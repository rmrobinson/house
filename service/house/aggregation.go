package house

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	apiTrait "github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
	"github.com/rmrobinson/house/service/house/db"
	"github.com/rmrobinson/house/service/lib/protoreflectutil"
)

// buildingOccupiedWindow is how recently a room must have had motion (per
// Room.Properties.occupied) for buildingOccupied to report its building as
// occupied - see Building.State.occupied's doc comment in api/house.proto.
const buildingOccupiedWindow = 10 * time.Minute

// aggregator maintains, per room, the live Room.Properties computed from
// that room's linked devices, and per building, the live Building.State.
// occupied derived from them (Building.State.mode is db-owned - see
// buildingMode) - see api/house.proto's AggregationConfig doc comment for
// the Properties metric/strategy contract, and computeProperties for which
// device kinds/traits actually contribute. It is kept current by feeding it
// every BridgeService Update (see handleUpdate) and every room/link/mode
// change Service itself makes (setDeviceRoom, removeDeviceRoom,
// setRoomAggregation, registerRoom, removeRoom, setBuildingMode) - it never
// queries the db on its own, so Service is responsible for keeping it in
// sync. Every room whose computed Properties actually changes (see
// recomputeRoomLocked) and every building whose occupied actually changes
// (see refreshBuildingOccupiedLocked) is republished on updates, the
// fan-out source Service.StreamHouseUpdates subscribes to - the same
// bridge.Source/Sink pub-sub primitive service/bridge/facade uses for
// BridgeService.StreamUpdates.
type aggregator struct {
	logger  *zap.Logger
	updates *bridge.Source

	mu sync.Mutex
	// deviceRoom and roomDevices are inverses of each other, kept in sync
	// together - deviceRoom for O(1) device->room lookup on an incoming
	// Update, roomDevices for O(room size) recompute instead of an O(all
	// devices) scan.
	deviceRoom   map[string]string                  // device_id -> room_id
	roomDevices  map[string]map[string]bool         // room_id -> set of device_id
	deviceState  map[string]*apiDevice.Device       // device_id -> latest known Device reading (see handleUpdate for which kinds)
	roomConfig   map[string]*api2.AggregationConfig // room_id -> override, nil = every metric uses its default
	roomBuilding map[string]string                  // room_id -> building_id, for StreamHouseUpdates' per-building scoping
	// buildingRooms is the inverse of roomBuilding (building_id -> set of
	// room_id), kept in sync alongside it via registerRoomLocked - lets
	// buildingOccupied/propertiesForBuilding look up a building's rooms
	// directly instead of scanning every room in the house on every call.
	buildingRooms map[string]map[string]bool
	properties    map[string]*api2.Room_Properties // room_id -> last computed Properties
	// lastMotion records, per room, the last time recomputeRoomLocked
	// computed that room's Properties.occupied as true - the basis for
	// buildingOccupied's "has had motion in the last
	// buildingOccupiedWindow" contract. A room with no entry here has never
	// had a device report occupied=true since this aggregator started (or
	// since the room was created) - never written back to false; the
	// window-based decay itself is still evaluated lazily (against the
	// current time, not a stored "will decay at" deadline) whenever
	// buildingOccupiedLocked runs, whether that's a direct GetBuilding-driven
	// call or the decay timer's own recheck (see armDecayTimerLocked).
	lastMotion map[string]time.Time
	// buildingMode mirrors each building's db.Building.Mode (set via
	// Service.SetHouseMode, never by the aggregator itself) - kept here
	// purely so a BuildingUpdate triggered by an occupied change (see
	// refreshBuildingOccupiedLocked) can report the building's full current
	// State without the aggregator reaching into the db it otherwise never
	// queries on its own. A building with no entry defaults to "", the same
	// "never set" zero value db.Building.Mode itself uses.
	buildingMode map[string]string
	// buildingOccupiedPublished is the occupied value last published in a
	// BuildingUpdate for each building, so refreshBuildingOccupiedLocked only
	// publishes when it actually changes - the same proto.Equal-style
	// dedup recomputeRoomLocked does for Room.Properties, but for a *bool.
	// No entry means nothing has been published for that building yet.
	buildingOccupiedPublished map[string]*bool
	// decayTimers holds each building's pending occupied-decay recheck
	// timer (see armDecayTimerLocked) - keyed so a fresh motion event can
	// replace a still-pending one instead of leaving two races in flight. A
	// fired callback only deletes its building's entry if it's still the
	// same *time.Timer the callback was armed with - otherwise a newer timer
	// has already replaced it (this one lost the race between firing and
	// being Stop()'d), and deleting unconditionally would orphan that newer
	// timer, leaving it untracked and un-cancelable.
	decayTimers map[string]*time.Timer
	// now is overridden in tests; defaults to time.Now.
	now func() time.Time
	// afterFunc schedules a decay recheck like time.AfterFunc; overridden in
	// tests so they don't have to wait out a real buildingOccupiedWindow.
	afterFunc func(d time.Duration, f func()) *time.Timer
}

func newAggregator(logger *zap.Logger) *aggregator {
	return &aggregator{
		logger:                    logger,
		updates:                   bridge.NewSource(logger),
		deviceRoom:                make(map[string]string),
		roomDevices:               make(map[string]map[string]bool),
		deviceState:               make(map[string]*apiDevice.Device),
		roomConfig:                make(map[string]*api2.AggregationConfig),
		roomBuilding:              make(map[string]string),
		buildingRooms:             make(map[string]map[string]bool),
		properties:                make(map[string]*api2.Room_Properties),
		lastMotion:                make(map[string]time.Time),
		buildingMode:              make(map[string]string),
		buildingOccupiedPublished: make(map[string]*bool),
		decayTimers:               make(map[string]*time.Timer),
		now:                       time.Now,
		afterFunc:                 time.AfterFunc,
	}
}

// load seeds the device->room index, every room's building_id, every room's
// aggregation override, and every building's current mode from the
// database. Call once at startup, before subscribing to bridge updates - a
// device update for a not-yet-loaded link would otherwise be silently
// dropped (see handleUpdate).
func (a *aggregator) load(ctx context.Context, database *db.Database) error {
	rooms, err := database.ListRooms(ctx, nil, nil)
	if err != nil {
		return err
	}
	links, err := database.ListDeviceLinks(ctx, nil, nil, nil)
	if err != nil {
		return err
	}
	buildings, err := database.GetBuildings(ctx)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range rooms {
		a.roomConfig[r.ID] = dbAggregationToAPI(r.Aggregation)
		a.registerRoomLocked(r.ID, r.BuildingID)
	}
	for _, l := range links {
		a.linkLocked(l.ID, l.RoomID)
	}
	for _, b := range buildings {
		a.buildingMode[b.ID] = b.Mode
	}
	return nil
}

// linkLocked records deviceID as linked to roomID, replacing any prior
// room it was linked to. Callers must hold a.mu.
func (a *aggregator) linkLocked(deviceID, roomID string) {
	if prevRoomID, ok := a.deviceRoom[deviceID]; ok {
		if prevRoomID == roomID {
			return
		}
		delete(a.roomDevices[prevRoomID], deviceID)
	}
	a.deviceRoom[deviceID] = roomID
	if a.roomDevices[roomID] == nil {
		a.roomDevices[roomID] = make(map[string]bool)
	}
	a.roomDevices[roomID][deviceID] = true
}

// setDeviceRoom links deviceID to roomID (replacing any prior link) and
// recomputes both the old and new room's Properties. Call from
// Service.LinkDevice after a successful link.
func (a *aggregator) setDeviceRoom(deviceID, roomID string) {
	a.mu.Lock()
	prevRoomID, hadPrev := a.deviceRoom[deviceID]
	a.linkLocked(deviceID, roomID)
	var prevUpdate *api2.RoomUpdate
	var prevBuildingUpdate *api2.BuildingUpdate
	if hadPrev && prevRoomID != roomID {
		prevUpdate = a.recomputeRoomLocked(prevRoomID)
		// Same "only a fresh occupied=true reading can have moved the
		// building's occupied value" reasoning handleUpdate documents -
		// relinking a device that's already reporting occupied=true can
		// move that signal into a different building immediately, which
		// must be published now rather than left to decay/arm on its own.
		if prevUpdate.GetProperties().GetOccupied() {
			prevBuildingUpdate = a.refreshBuildingOccupiedLocked(a.roomBuilding[prevRoomID])
		}
	}
	update := a.recomputeRoomLocked(roomID)
	var buildingUpdate *api2.BuildingUpdate
	if update.GetProperties().GetOccupied() {
		buildingUpdate = a.refreshBuildingOccupiedLocked(a.roomBuilding[roomID])
	}
	a.mu.Unlock()

	a.publish(prevUpdate)
	a.publish(update)
	a.publishBuilding(prevBuildingUpdate)
	a.publishBuilding(buildingUpdate)
}

// removeDeviceRoom unlinks deviceID from whatever room it was linked to (a
// no-op if it wasn't linked) and recomputes that room's Properties. Call
// from Service.UnlinkDevice.
func (a *aggregator) removeDeviceRoom(deviceID string) {
	a.mu.Lock()

	roomID, ok := a.deviceRoom[deviceID]
	if !ok {
		a.mu.Unlock()
		return
	}
	delete(a.deviceRoom, deviceID)
	delete(a.roomDevices[roomID], deviceID)
	delete(a.deviceState, deviceID)
	update := a.recomputeRoomLocked(roomID)
	a.mu.Unlock()

	a.publish(update)
}

// setRoomAggregation records roomID's aggregation override (nil = every
// metric uses its default) and recomputes its Properties. Call from
// Service.CreateRoom/UpdateRoom.
func (a *aggregator) setRoomAggregation(roomID string, cfg *api2.AggregationConfig) {
	a.mu.Lock()
	a.roomConfig[roomID] = cfg
	update := a.recomputeRoomLocked(roomID)
	a.mu.Unlock()

	a.publish(update)
}

// registerRoomLocked records roomID's building_id in roomBuilding and its
// reverse index buildingRooms. Callers must hold a.mu.
func (a *aggregator) registerRoomLocked(roomID, buildingID string) {
	a.roomBuilding[roomID] = buildingID
	if a.buildingRooms[buildingID] == nil {
		a.buildingRooms[buildingID] = make(map[string]bool)
	}
	a.buildingRooms[buildingID][roomID] = true
}

// registerRoom records roomID's building_id. Call from Service.CreateRoom
// once the room's id/building are known - a room's building never changes
// after creation (see db.Room's UpdateRoom doc comment: "Room-to-floor
// reassignment isn't supported here"), so this is otherwise a one-time
// registration, not something UpdateRoom needs to repeat.
func (a *aggregator) registerRoom(roomID, buildingID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.registerRoomLocked(roomID, buildingID)
}

// removeRoom drops all cached state for a deleted room. Call from
// Service.DeleteRoom - DeleteRoom only succeeds when no devices are still
// linked to the room (ErrHasChildren otherwise), so there's no roomDevices
// entry left to clean up by the time this runs.
func (a *aggregator) removeRoom(roomID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	buildingID := a.roomBuilding[roomID]
	delete(a.roomConfig, roomID)
	delete(a.properties, roomID)
	delete(a.roomDevices, roomID)
	delete(a.roomBuilding, roomID)
	delete(a.lastMotion, roomID)
	delete(a.buildingRooms[buildingID], roomID)
}

// removeBuilding drops all cached per-building state for a deleted building.
// Call from Service.DeleteBuilding - DeleteBuilding only succeeds once every
// floor/room referencing buildingID is already gone (ErrHasChildren
// otherwise), so buildingRooms[buildingID] is already empty by the time this
// runs; this only needs to clean up the building-level entries
// refreshBuildingOccupiedLocked/setBuildingMode/armDecayTimerLocked populate
// directly, which would otherwise leak for the life of the process (a stale
// map entry forever, or a pending *time.Timer leaked if one was armed at
// deletion time).
func (a *aggregator) removeBuilding(buildingID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.decayTimers[buildingID]; ok {
		t.Stop()
		delete(a.decayTimers, buildingID)
	}
	delete(a.buildingMode, buildingID)
	delete(a.buildingOccupiedPublished, buildingID)
	delete(a.buildingRooms, buildingID)
}

// getProperties returns the cached Properties for roomID, or nil if the
// room has no linked device that has ever reported.
func (a *aggregator) getProperties(roomID string) *api2.Room_Properties {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.properties[roomID]
}

// buildingOf returns the building_id roomID belongs to, or "" if roomID is
// unknown (never registered, or since removed).
func (a *aggregator) buildingOf(roomID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.roomBuilding[roomID]
}

// propertiesForBuilding returns every room in buildingID that currently has
// a computed Properties (a room with no linked device that's ever reported
// is left out entirely, same as getProperties returning nil for it) -
// Service.StreamHouseUpdates' initial snapshot for a newly subscribed
// client.
func (a *aggregator) propertiesForBuilding(buildingID string) map[string]*api2.Room_Properties {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make(map[string]*api2.Room_Properties)
	for roomID := range a.buildingRooms[buildingID] {
		if p := a.properties[roomID]; p != nil {
			out[roomID] = p
		}
	}
	return out
}

// buildingOccupied reports whether any room belonging to buildingID has had
// motion (per lastMotion) within buildingOccupiedWindow of now, as *bool -
// nil if no room belonging to buildingID has ever recorded any occupancy
// signal at all, the same "no data yet" contract getProperties/
// computeProperties use for Room.Properties.occupied. Once at least one room
// in the building has ever reported occupied=true, the result is always
// true or false, never reverting to nil - evaluated lazily against the
// current time rather than on a timer, so a building correctly "goes
// unoccupied" the next time anything asks, without a background goroutine.
func (a *aggregator) buildingOccupied(buildingID string) *bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.buildingOccupiedLocked(buildingID)
}

// buildingOccupiedLocked is buildingOccupied's implementation. Callers must
// hold a.mu.
func (a *aggregator) buildingOccupiedLocked(buildingID string) *bool {
	now := a.now()
	haveSignal := false
	occupied := false
	for roomID := range a.buildingRooms[buildingID] {
		last, ok := a.lastMotion[roomID]
		if !ok {
			continue
		}
		haveSignal = true
		if now.Sub(last) <= buildingOccupiedWindow {
			occupied = true
		}
	}
	if !haveSignal {
		return nil
	}
	return &occupied
}

// boolPtrEqual reports whether a and b are both nil or both non-nil with the
// same value.
func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// refreshBuildingOccupiedLocked recomputes buildingID's current occupied
// value and returns a BuildingUpdate to publish if it actually changed from
// the last one published for this building, or nil otherwise - the same
// "only fan out on real change" dedup recomputeRoomLocked does for
// Room.Properties, but for *bool. Does not itself arm the decay timer (see
// armDecayTimerLocked) - that's anchored to the actual motion event inside
// recomputeRoomLocked, not to whenever this happens to be called, so an
// unrelated device update elsewhere in the building can't push the real
// decay deadline out. Callers must hold a.mu, and must publish the result
// only after releasing it - same convention as recomputeRoomLocked/a.publish.
func (a *aggregator) refreshBuildingOccupiedLocked(buildingID string) *api2.BuildingUpdate {
	occupied := a.buildingOccupiedLocked(buildingID)
	if boolPtrEqual(a.buildingOccupiedPublished[buildingID], occupied) {
		return nil
	}
	a.buildingOccupiedPublished[buildingID] = occupied
	return &api2.BuildingUpdate{
		BuildingId: buildingID,
		State:      &api2.Building_State{Occupied: occupied, Mode: a.buildingMode[buildingID]},
	}
}

// armDecayTimerLocked (re)schedules a recheck of buildingID's occupied value
// for buildingOccupiedWindow from now, replacing any pending one. Call only
// when a room in buildingID just recorded fresh motion (see
// recomputeRoomLocked) - occupied is driven by the room with the most
// recent motion, so each such event needs the recheck pushed back out to
// cover it; once no further motion arrives before the timer fires,
// refreshBuildingOccupiedLocked finds occupied has decayed to false and
// publishes the transition that nothing else would otherwise trigger.
// Callers must hold a.mu.
func (a *aggregator) armDecayTimerLocked(buildingID string) {
	if t, ok := a.decayTimers[buildingID]; ok {
		t.Stop()
	}

	var timer *time.Timer
	timer = a.afterFunc(buildingOccupiedWindow, func() {
		a.mu.Lock()
		if a.decayTimers[buildingID] == timer {
			delete(a.decayTimers, buildingID)
		}
		update := a.refreshBuildingOccupiedLocked(buildingID)
		a.mu.Unlock()
		a.publishBuilding(update)
	})
	a.decayTimers[buildingID] = timer
}

// setBuildingMode records buildingID's new mode and publishes a
// BuildingUpdate reflecting it combined with the building's current
// occupied value. Call from Service.SetHouseMode after a successful write -
// mode is db-owned, so Service, not the aggregator, is what learns of a
// change; unlike an occupied change this always publishes, since it only
// runs when a caller actually asked to change the mode.
func (a *aggregator) setBuildingMode(buildingID, mode string) {
	a.mu.Lock()
	a.buildingMode[buildingID] = mode
	occupied := a.buildingOccupiedLocked(buildingID)
	a.buildingOccupiedPublished[buildingID] = occupied
	update := &api2.BuildingUpdate{
		BuildingId: buildingID,
		State:      &api2.Building_State{Occupied: occupied, Mode: mode},
	}
	a.mu.Unlock()

	a.publishBuilding(update)
}

// aggregatingDeviceKind reports whether d has a Device.details kind set at
// all - deliberately not "does extractDeviceTraits currently find a trait
// on it", so a device whose kind can aggregate doesn't get silently
// skipped (its deviceState entry left stale) by one Update that happens to
// carry none of its traits this time (e.g. a Sensor momentarily reporting
// only battery/metadata). A kind that never carries any aggregating trait
// (Light, MediaPlayer, ConnectedDevice, ...) is still cached here, same as
// before this file read any trait types generically - a cheap, bounded
// no-op cost (see computeProperties/extractDeviceTraits, which contribute
// nothing for such a device regardless).
func aggregatingDeviceKind(d *apiDevice.Device) bool {
	_, _, ok := protoreflectutil.OneofMessage(d, "details")
	return ok
}

// handleUpdate applies one BridgeService Update. Non-device updates and
// device updates for a non-aggregating-kind (see aggregatingDeviceKind) or
// unlinked device are ignored - only a device whose kind contributes a
// measurement trait, linked to a room (via setDeviceRoom), affects
// aggregation.
func (a *aggregator) handleUpdate(u *api2.Update) {
	du := u.GetDeviceUpdate()
	if du == nil {
		return
	}
	device := du.GetDevice()
	if !aggregatingDeviceKind(device) {
		return
	}

	a.mu.Lock()

	roomID, ok := a.deviceRoom[device.GetId()]
	if !ok {
		a.mu.Unlock()
		return
	}
	// Cloned per AGENTS.md's proto-ownership convention: the aggregator
	// caches this pointer indefinitely across recomputes, so it must not
	// share memory with whatever the bridge update pipeline does with u
	// after handleUpdate returns.
	a.deviceState[device.GetId()] = proto.Clone(device).(*apiDevice.Device)
	update := a.recomputeRoomLocked(roomID)
	// Only a fresh occupied=true reading can have moved the building's
	// occupied value (lastMotion is only ever written on one, and
	// buildingOccupiedLocked only reads lastMotion) - every other update
	// leaves it unchanged, so skip the building-wide rescan for those.
	var buildingUpdate *api2.BuildingUpdate
	if update.GetProperties().GetOccupied() {
		buildingUpdate = a.refreshBuildingOccupiedLocked(a.roomBuilding[roomID])
	}
	a.mu.Unlock()

	a.publish(update)
	a.publishBuilding(buildingUpdate)
}

// recomputeRoomLocked rebuilds roomID's cached Properties from the current
// deviceState of every device in roomDevices[roomID], returning the
// RoomUpdate to publish on a.updates if it actually changed from what was
// cached before, or nil otherwise. Callers must hold a.mu, and must publish
// the result via a.publish only after releasing it - a.updates.SendMessage
// synchronously invokes each subscriber's filter (e.g.
// Service.StreamHouseUpdates' per-building filter, which calls
// a.buildingOf), and a.mu is not reentrant.
func (a *aggregator) recomputeRoomLocked(roomID string) *api2.RoomUpdate {
	var devices []*apiDevice.Device
	for deviceID := range a.roomDevices[roomID] {
		if d, ok := a.deviceState[deviceID]; ok {
			devices = append(devices, d)
		}
	}

	newProps := computeProperties(a.roomConfig[roomID], devices)
	if newProps.GetOccupied() {
		a.lastMotion[roomID] = a.now()
		// Arm the decay timer here, anchored to this actual motion event,
		// not wherever handleUpdate next happens to recheck the building -
		// an unrelated device update elsewhere in the building must never
		// push the real decay deadline out (see armDecayTimerLocked).
		a.armDecayTimerLocked(a.roomBuilding[roomID])
	}
	oldProps := a.properties[roomID]
	a.properties[roomID] = newProps

	// proto.Equal treats two nil messages as equal, so a room with no
	// linked device that's ever reported (oldProps and newProps both nil)
	// correctly produces no update - the same "only fan out when something
	// actually changed" diffing service/bridge.Service does for device
	// updates (see AGENTS.md).
	if proto.Equal(oldProps, newProps) {
		return nil
	}
	return &api2.RoomUpdate{RoomId: roomID, Properties: newProps}
}

// publish sends update on a.updates wrapped in a HouseUpdate, a no-op if
// update is nil. Must be called without holding a.mu (see
// recomputeRoomLocked).
func (a *aggregator) publish(update *api2.RoomUpdate) {
	if update == nil {
		return
	}
	a.updates.SendMessage(&api2.HouseUpdate{Update: &api2.HouseUpdate_Room{Room: update}})
}

// publishBuilding sends update on a.updates wrapped in a HouseUpdate, a
// no-op if update is nil. Must be called without holding a.mu (see
// refreshBuildingOccupiedLocked).
func (a *aggregator) publishBuilding(update *api2.BuildingUpdate) {
	if update == nil {
		return
	}
	a.updates.SendMessage(&api2.HouseUpdate{Update: &api2.HouseUpdate_Building{Building: update}})
}

/* ----- pure aggregation math ----- */

// strategyOrDefault returns cfg's strategy for one metric, falling back to
// def when cfg is nil or the field is left at STRATEGY_UNSPECIFIED - the
// "unset = use per-metric defaults" contract documented on
// Room.Config.aggregation.
func strategyOrDefault(s, def api2.AggregationConfig_Strategy) api2.AggregationConfig_Strategy {
	if s == api2.AggregationConfig_STRATEGY_UNSPECIFIED {
		return def
	}
	return s
}

// numericSample is one device's contribution to a numeric metric, paired
// with its deviceLastReported time (0 if unknown) so LATEST can pick the
// most recent one.
type numericSample struct {
	value        float64
	lastReported int64 // unix nanos; 0 if unknown
}

func aggregateNumeric(strategy api2.AggregationConfig_Strategy, samples []numericSample) *float64 {
	if len(samples) == 0 {
		return nil
	}

	var result float64
	switch strategy {
	case api2.AggregationConfig_LATEST:
		latest := samples[0]
		for _, s := range samples[1:] {
			if s.lastReported > latest.lastReported {
				latest = s
			}
		}
		result = latest.value
	case api2.AggregationConfig_MIN:
		result = samples[0].value
		for _, s := range samples[1:] {
			if s.value < result {
				result = s.value
			}
		}
	case api2.AggregationConfig_MAX:
		result = samples[0].value
		for _, s := range samples[1:] {
			if s.value > result {
				result = s.value
			}
		}
	case api2.AggregationConfig_SUM:
		for _, s := range samples {
			result += s.value
		}
	case api2.AggregationConfig_AVERAGE, api2.AggregationConfig_ANY:
		// ANY has no numeric meaning; AVERAGE is the fallback for it too.
		for _, s := range samples {
			result += s.value
		}
		result /= float64(len(samples))
	default:
		for _, s := range samples {
			result += s.value
		}
		result /= float64(len(samples))
	}
	return &result
}

// aggregateBool combines occupancy readings. ANY (the default) is true if
// any sensor is; LATEST picks the most recently reported sensor's value.
// Every other strategy falls back to ANY - none of them have a meaningful
// definition for a boolean metric.
func aggregateBool(strategy api2.AggregationConfig_Strategy, samples []numericSample) *bool {
	if len(samples) == 0 {
		return nil
	}

	var result bool
	switch strategy {
	case api2.AggregationConfig_LATEST:
		latest := samples[0]
		for _, s := range samples[1:] {
			if s.lastReported > latest.lastReported {
				latest = s
			}
		}
		result = latest.value != 0
	default:
		for _, s := range samples {
			if s.value != 0 {
				result = true
				break
			}
		}
	}
	return &result
}

// deviceLastReported returns, as unix nanos (0 if unknown), the best
// available "when was this measurement taken" timestamp for d - used so
// LATEST can pick the most recently-reported sample. A Sensor's own
// Metadata.LastReported, when a bridge actually sets it, is a true
// per-reading timestamp; no bridge in this repo does today, and no other
// aggregating kind has an equivalent field at all, so Device.LastSeen
// (last time the bridge heard from the device at all) is the fallback -
// checked whenever Metadata.LastReported is absent, not only for non-Sensor
// kinds, so today's actual (LastSeen-only) bridges still get a real
// timestamp instead of silently falling through to 0.
func deviceLastReported(d *apiDevice.Device) int64 {
	if s := d.GetSensor(); s != nil {
		if ts := s.GetMetadata().GetLastReported(); ts != nil {
			return ts.AsTime().UnixNano()
		}
	}
	if ts := d.GetLastSeen(); ts != nil {
		return ts.AsTime().UnixNano()
	}
	return 0
}

// deviceTraits holds whichever of the shared measurement traits one device
// contributes to room aggregation - nil/empty fields mean this particular
// device didn't report that trait. Built by extractDeviceTraits.
type deviceTraits struct {
	presence      *apiTrait.Presence
	airProperties *apiTrait.AirProperties
	airQuality    *apiTrait.AirQuality
	lightLevel    *apiTrait.LightLevel
	// powerW is every real wattage reading this device contributes - more
	// than one for an EVCharger (wall + vehicle), already-converted-to-float
	// for Sensor.PowerConsumption since it's not a trait.Power.
	powerW []float64
	// temperatureC is set directly (bypassing airProperties) for a Fan's
	// Temperature trait (api/trait/temperature.proto) - a different message
	// type with its own unit, already validated as Celsius here.
	temperatureC *float64
}

// evChargerFullName/evChargerExteriorConditionsFieldNumber are the one known
// case where a trait field found by extractDeviceTraits's generic scan must
// still be excluded
// by (containing message, field number) rather than by name alone -
// field number is proto's own stable identity for a field, so a routine
// wire-compatible rename of exterior_conditions can't silently stop this
// matching: EVCharger.exterior_conditions is an AirProperties, the same
// type as every genuine ambient reading, but it measures conditions near
// the charger for its own charge-rate throttling, not the room it happens
// to be mounted in. Every other trait field extractDeviceTraits finds is
// trusted at face value - this is the sole exception.
var evChargerFullName = (&apiDevice.EVCharger{}).ProtoReflect().Descriptor().FullName()

const evChargerExteriorConditionsFieldNumber = protoreflect.FieldNumber(5)

// extractDeviceTraits reads every one of d's populated Device.details
// fields whose type is one of the shared measurement traits - Presence,
// AirProperties, AirQuality, LightLevel, Power, Temperature (see
// api/trait/*.proto) - regardless of which device kind d actually is. A
// device kind newly gaining one of these trait fields in the future
// contributes to aggregation with no change needed here, the same "code to
// the documented contract, not hardcoded behavior" principle
// service/policy/bridgehome's generic state access already follows (see
// protoreflectutil.OneofMessage). This also means a kind nobody expected to
// contribute can start to - e.g. Camera.presence (api/device/camera.proto)
// is a trait.Presence, so a camera with motion detection genuinely feeds
// room occupancy now, which is correct, not a bug (see
// TestComputeProperties_CameraPresence). ok is false if d has no details
// set, or its kind carries none of these trait types at all (Light,
// MediaPlayer, ConnectedDevice, ...).
func extractDeviceTraits(d *apiDevice.Device) (t deviceTraits, ok bool) {
	msg, _, has := protoreflectutil.OneofMessage(d, "details")
	if !has {
		return deviceTraits{}, false
	}

	fields := msg.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.Kind() != protoreflect.MessageKind || !msg.Has(fd) {
			continue
		}
		if fd.Number() == evChargerExteriorConditionsFieldNumber && fd.ContainingMessage().FullName() == evChargerFullName {
			continue
		}

		switch v := msg.Get(fd).Message().Interface().(type) {
		case *apiTrait.Presence:
			t.presence = v
			ok = true
		case *apiTrait.AirProperties:
			// Attributes/State can arrive in separate updates - don't treat
			// an unpopulated State as a genuine 0.0°C reading (same
			// nil-State guard as Presence/AirQuality/Temperature below).
			if v.GetState() != nil {
				t.airProperties = v
				ok = true
			}
		case *apiTrait.AirQuality:
			t.airQuality = v
			ok = true
		case *apiTrait.LightLevel:
			if v.GetState() != nil {
				t.lightLevel = v
				ok = true
			}
		case *apiTrait.Power:
			if st := v.GetState(); st != nil {
				t.powerW = append(t.powerW, st.GetPowerW())
				ok = true
			}
		case *apiTrait.Temperature:
			// Carries its own unit, unlike AirProperties.State.TemperatureC -
			// only trust it as a room-temperature sample when State is
			// actually populated (same nil-State guard as every other trait
			// here - Attributes/State can arrive in separate updates) and
			// that unit is actually Celsius, matching
			// Room.Properties.temperature_c.
			if st := v.GetState(); st != nil && strings.EqualFold(v.GetAttributes().GetUnit(), "celsius") {
				cv := float64(st.GetValue())
				t.temperatureC = &cv
				ok = true
			}
		}
	}

	// Sensor.PowerConsumption isn't a shared api/trait/*.proto type (it's
	// Sensor-local), so the generic scan above can't find it - and it's
	// only a fallback for a Sensor with no Power trait of its own, the same
	// precedence the two had before this function existed.
	if s := d.GetSensor(); s.GetPower() == nil {
		if pc := s.GetPowerConsumption(); pc != nil {
			t.powerW = append(t.powerW, float64(pc.GetPowerUsageW()))
			ok = true
		}
	}

	return t, ok
}

// computeProperties builds a room's Properties from the current reading of
// every one of its linked devices (see extractDeviceTraits for which kinds
// contribute and which trait fields each carries). cfg may be nil (every
// metric uses its default strategy). A metric with no contributing device
// is left unset on the result, never defaulted to zero.
func computeProperties(cfg *api2.AggregationConfig, devices []*apiDevice.Device) *api2.Room_Properties {
	var occupancy, temp, light, aqi, co2, voc, radon, power []numericSample

	for _, d := range devices {
		traits, ok := extractDeviceTraits(d)
		if !ok {
			continue
		}
		lastReported := deviceLastReported(d)

		if st := traits.presence.GetState(); st != nil {
			v := st.GetMotionDetected()
			if st.OccupancyDetected != nil {
				v = st.GetOccupancyDetected()
			}
			val := 0.0
			if v {
				val = 1
			}
			occupancy = append(occupancy, numericSample{val, lastReported})
		}
		if traits.airProperties != nil {
			temp = append(temp, numericSample{float64(traits.airProperties.GetState().GetTemperatureC()), lastReported})
		}
		if traits.temperatureC != nil {
			temp = append(temp, numericSample{*traits.temperatureC, lastReported})
		}
		if traits.lightLevel != nil {
			light = append(light, numericSample{float64(traits.lightLevel.GetState().GetLux()), lastReported})
		}
		if st := traits.airQuality.GetState(); st != nil {
			if st.Aqi != nil {
				aqi = append(aqi, numericSample{float64(st.GetAqi()), lastReported})
			}
			if st.Co2Ppm != nil {
				co2 = append(co2, numericSample{float64(st.GetCo2Ppm()), lastReported})
			}
			if st.VolatileOrganicCompoundsPpb != nil {
				voc = append(voc, numericSample{float64(st.GetVolatileOrganicCompoundsPpb()), lastReported})
			}
			if st.RadonBqM3 != nil {
				radon = append(radon, numericSample{float64(st.GetRadonBqM3()), lastReported})
			}
		}
		for _, w := range traits.powerW {
			power = append(power, numericSample{w, lastReported})
		}
	}

	props := &api2.Room_Properties{}
	props.Occupied = aggregateBool(strategyOrDefault(cfg.GetOccupancyStrategy(), api2.AggregationConfig_ANY), occupancy)
	if v := aggregateNumeric(strategyOrDefault(cfg.GetTemperatureStrategy(), api2.AggregationConfig_AVERAGE), temp); v != nil {
		props.TemperatureC = v
	}
	if v := aggregateNumeric(strategyOrDefault(cfg.GetLightStrategy(), api2.AggregationConfig_AVERAGE), light); v != nil {
		lux := int32(*v)
		props.LightLevelLux = &lux
	}
	airQualityStrategy := strategyOrDefault(cfg.GetAirQualityStrategy(), api2.AggregationConfig_AVERAGE)
	if v := aggregateNumeric(airQualityStrategy, aqi); v != nil {
		idx := int32(*v)
		props.AirQualityIndex = &idx
	}
	if v := aggregateNumeric(airQualityStrategy, co2); v != nil {
		ppm := int32(*v)
		props.Co2Ppm = &ppm
	}
	if v := aggregateNumeric(airQualityStrategy, voc); v != nil {
		ppb := int32(*v)
		props.VocPpb = &ppb
	}
	if v := aggregateNumeric(airQualityStrategy, radon); v != nil {
		bqM3 := int32(*v)
		props.RadonBqM3 = &bqM3
	}
	if v := aggregateNumeric(strategyOrDefault(cfg.GetPowerStrategy(), api2.AggregationConfig_SUM), power); v != nil {
		props.PowerDrawW = v
	}

	// A room with no linked device that has ever reported any metric gets
	// no Properties at all, not an all-fields-unset one - so a fresh link
	// (setDeviceRoom, before any Update has arrived for it) leaves
	// getProperties returning nil rather than a misleadingly "computed"
	// empty result.
	if props.Occupied == nil && props.TemperatureC == nil && props.LightLevelLux == nil &&
		props.AirQualityIndex == nil && props.Co2Ppm == nil && props.VocPpb == nil &&
		props.RadonBqM3 == nil && props.PowerDrawW == nil {
		return nil
	}
	return props
}

/* ----- db <-> API AggregationConfig conversion ----- */

// dbStrategyToAPI and apiStrategyToDB are plain casts, not switches, because
// db.AggregationStrategy and api2.AggregationConfig_Strategy are declared in
// the same order (both: UNSPECIFIED, LATEST, AVERAGE, MIN, MAX, SUM, ANY) -
// the same convention service.go's db.RoomType(...) cast uses for
// Room.Config.type. Keep the two enums' orderings in sync if either grows a
// new value.
func dbStrategyToAPI(s db.AggregationStrategy) api2.AggregationConfig_Strategy {
	return api2.AggregationConfig_Strategy(s)
}

func apiStrategyToDB(s api2.AggregationConfig_Strategy) db.AggregationStrategy {
	return db.AggregationStrategy(s)
}

// dbAggregationToAPI converts a db.AggregationConfig to its API
// representation. nil in, nil out.
func dbAggregationToAPI(a *db.AggregationConfig) *api2.AggregationConfig {
	if a == nil {
		return nil
	}
	return &api2.AggregationConfig{
		OccupancyStrategy:   dbStrategyToAPI(a.OccupancyStrategy),
		TemperatureStrategy: dbStrategyToAPI(a.TemperatureStrategy),
		LightStrategy:       dbStrategyToAPI(a.LightStrategy),
		AirQualityStrategy:  dbStrategyToAPI(a.AirQualityStrategy),
		PowerStrategy:       dbStrategyToAPI(a.PowerStrategy),
	}
}

// apiAggregationToDB converts an API AggregationConfig to its db
// representation. nil in, nil out.
func apiAggregationToDB(a *api2.AggregationConfig) *db.AggregationConfig {
	if a == nil {
		return nil
	}
	return &db.AggregationConfig{
		OccupancyStrategy:   apiStrategyToDB(a.GetOccupancyStrategy()),
		TemperatureStrategy: apiStrategyToDB(a.GetTemperatureStrategy()),
		LightStrategy:       apiStrategyToDB(a.GetLightStrategy()),
		AirQualityStrategy:  apiStrategyToDB(a.GetAirQualityStrategy()),
		PowerStrategy:       apiStrategyToDB(a.GetPowerStrategy()),
	}
}
