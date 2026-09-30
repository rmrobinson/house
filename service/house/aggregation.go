package house

import (
	"context"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
	"github.com/rmrobinson/house/service/house/db"
)

// aggregator maintains, per room, the live Room.Properties computed from
// that room's linked Sensor-kind devices - see api/house.proto's
// AggregationConfig doc comment for the metric/strategy contract. It is
// kept current by feeding it every BridgeService Update (see handleUpdate)
// and every room/link change Service itself makes (setDeviceRoom,
// removeDeviceRoom, setRoomAggregation, registerRoom, removeRoom) - it
// never queries the db on its own, so Service is responsible for keeping it
// in sync. Every room whose computed Properties actually changes (see
// recomputeRoomLocked) is republished on updates, the fan-out source
// Service.StreamHouseUpdates subscribes to - the same bridge.Source/Sink
// pub-sub primitive service/bridge/facade uses for BridgeService.
// StreamUpdates.
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
	deviceState  map[string]*apiDevice.Sensor       // device_id -> latest known Sensor reading
	roomConfig   map[string]*api2.AggregationConfig // room_id -> override, nil = every metric uses its default
	roomBuilding map[string]string                  // room_id -> building_id, for StreamHouseUpdates' per-building scoping
	properties   map[string]*api2.Room_Properties   // room_id -> last computed Properties
}

func newAggregator(logger *zap.Logger) *aggregator {
	return &aggregator{
		logger:       logger,
		updates:      bridge.NewSource(logger),
		deviceRoom:   make(map[string]string),
		roomDevices:  make(map[string]map[string]bool),
		deviceState:  make(map[string]*apiDevice.Sensor),
		roomConfig:   make(map[string]*api2.AggregationConfig),
		roomBuilding: make(map[string]string),
		properties:   make(map[string]*api2.Room_Properties),
	}
}

// load seeds the device->room index, every room's building_id, and every
// room's aggregation override from the database. Call once at startup,
// before subscribing to bridge updates - a device update for a
// not-yet-loaded link would otherwise be silently dropped (see
// handleUpdate).
func (a *aggregator) load(ctx context.Context, database *db.Database) error {
	rooms, err := database.ListRooms(ctx, nil, nil)
	if err != nil {
		return err
	}
	links, err := database.ListDeviceLinks(ctx, nil, nil, nil)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range rooms {
		a.roomConfig[r.ID] = dbAggregationToAPI(r.Aggregation)
		a.roomBuilding[r.ID] = r.BuildingID
	}
	for _, l := range links {
		a.linkLocked(l.ID, l.RoomID)
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
	defer a.mu.Unlock()

	prevRoomID, hadPrev := a.deviceRoom[deviceID]
	a.linkLocked(deviceID, roomID)
	if hadPrev && prevRoomID != roomID {
		a.recomputeRoomLocked(prevRoomID)
	}
	a.recomputeRoomLocked(roomID)
}

// removeDeviceRoom unlinks deviceID from whatever room it was linked to (a
// no-op if it wasn't linked) and recomputes that room's Properties. Call
// from Service.UnlinkDevice.
func (a *aggregator) removeDeviceRoom(deviceID string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	roomID, ok := a.deviceRoom[deviceID]
	if !ok {
		return
	}
	delete(a.deviceRoom, deviceID)
	delete(a.roomDevices[roomID], deviceID)
	delete(a.deviceState, deviceID)
	a.recomputeRoomLocked(roomID)
}

// setRoomAggregation records roomID's aggregation override (nil = every
// metric uses its default) and recomputes its Properties. Call from
// Service.CreateRoom/UpdateRoom.
func (a *aggregator) setRoomAggregation(roomID string, cfg *api2.AggregationConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.roomConfig[roomID] = cfg
	a.recomputeRoomLocked(roomID)
}

// registerRoom records roomID's building_id. Call from Service.CreateRoom
// once the room's id/building are known - a room's building never changes
// after creation (see db.Room's UpdateRoom doc comment: "Room-to-floor
// reassignment isn't supported here"), so this is otherwise a one-time
// registration, not something UpdateRoom needs to repeat.
func (a *aggregator) registerRoom(roomID, buildingID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.roomBuilding[roomID] = buildingID
}

// removeRoom drops all cached state for a deleted room. Call from
// Service.DeleteRoom - DeleteRoom only succeeds when no devices are still
// linked to the room (ErrHasChildren otherwise), so there's no roomDevices
// entry left to clean up by the time this runs.
func (a *aggregator) removeRoom(roomID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.roomConfig, roomID)
	delete(a.properties, roomID)
	delete(a.roomDevices, roomID)
	delete(a.roomBuilding, roomID)
}

// getProperties returns the cached Properties for roomID, or nil if the
// room has no linked Sensor device that has ever reported.
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
// a computed Properties (a room with no linked Sensor that's ever reported
// is left out entirely, same as getProperties returning nil for it) -
// Service.StreamHouseUpdates' initial snapshot for a newly subscribed
// client.
func (a *aggregator) propertiesForBuilding(buildingID string) map[string]*api2.Room_Properties {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make(map[string]*api2.Room_Properties)
	for roomID, bID := range a.roomBuilding {
		if bID != buildingID {
			continue
		}
		if p := a.properties[roomID]; p != nil {
			out[roomID] = p
		}
	}
	return out
}

// handleUpdate applies one BridgeService Update. Non-device updates and
// device updates for a non-Sensor or unlinked device are ignored - only a
// Sensor-kind device linked to a room (via setDeviceRoom) contributes to
// aggregation.
func (a *aggregator) handleUpdate(u *api2.Update) {
	du := u.GetDeviceUpdate()
	if du == nil {
		return
	}
	device := du.GetDevice()
	sensor := device.GetSensor()
	if sensor == nil {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	roomID, ok := a.deviceRoom[device.GetId()]
	if !ok {
		return
	}
	a.deviceState[device.GetId()] = sensor
	a.recomputeRoomLocked(roomID)
}

// recomputeRoomLocked rebuilds roomID's cached Properties from the current
// deviceState of every device in roomDevices[roomID], and republishes it on
// a.updates - the fan-out Service.StreamHouseUpdates reads from - if it
// actually changed from what was cached before. Callers must hold a.mu.
func (a *aggregator) recomputeRoomLocked(roomID string) {
	var sensors []*apiDevice.Sensor
	for deviceID := range a.roomDevices[roomID] {
		if s, ok := a.deviceState[deviceID]; ok {
			sensors = append(sensors, s)
		}
	}

	newProps := computeProperties(a.roomConfig[roomID], sensors)
	oldProps := a.properties[roomID]
	a.properties[roomID] = newProps

	// proto.Equal treats two nil messages as equal, so a room with no
	// linked Sensor that's ever reported (oldProps and newProps both nil)
	// correctly produces no update - the same "only fan out when something
	// actually changed" diffing service/bridge.Service does for device
	// updates (see AGENTS.md).
	if proto.Equal(oldProps, newProps) {
		return
	}
	a.updates.SendMessage(&api2.RoomUpdate{RoomId: roomID, Properties: newProps})
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

// numericSample is one Sensor's contribution to a numeric metric, paired
// with its Metadata.LastReported time (zero value if the sensor has no
// Metadata) so LATEST can pick the most recent one.
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

// computeProperties builds a room's Properties from the current reading of
// every one of its linked Sensor devices. cfg may be nil (every metric uses
// its default strategy). A metric with no contributing sensor is left unset
// on the result, never defaulted to zero.
func computeProperties(cfg *api2.AggregationConfig, sensors []*apiDevice.Sensor) *api2.Room_Properties {
	var occupancy, temp, light, aqi, power []numericSample

	for _, s := range sensors {
		lastReported := int64(0)
		if ts := s.GetMetadata().GetLastReported(); ts != nil {
			lastReported = ts.AsTime().UnixNano()
		}

		if p := s.GetPresence(); p != nil {
			v := p.GetState().GetMotionDetected()
			if p.GetState().OccupancyDetected != nil {
				v = p.GetState().GetOccupancyDetected()
			}
			val := 0.0
			if v {
				val = 1
			}
			occupancy = append(occupancy, numericSample{val, lastReported})
		}
		if ap := s.GetAirProperties(); ap != nil {
			temp = append(temp, numericSample{celsiusToFahrenheit(float64(ap.GetState().GetTemperatureC())), lastReported})
		}
		if ll := s.GetLightLevel(); ll != nil {
			light = append(light, numericSample{float64(ll.GetState().GetLux()), lastReported})
		}
		if aq := s.GetAirQuality(); aq != nil && aq.GetState().Aqi != nil {
			aqi = append(aqi, numericSample{float64(aq.GetState().GetAqi()), lastReported})
		}
		if pw := s.GetPower(); pw != nil {
			power = append(power, numericSample{pw.GetState().GetPowerW(), lastReported})
		} else if pc := s.GetPowerConsumption(); pc != nil {
			power = append(power, numericSample{float64(pc.GetPowerUsageW()), lastReported})
		}
	}

	props := &api2.Room_Properties{}
	props.Occupied = aggregateBool(strategyOrDefault(cfg.GetOccupancyStrategy(), api2.AggregationConfig_ANY), occupancy)
	if v := aggregateNumeric(strategyOrDefault(cfg.GetTemperatureStrategy(), api2.AggregationConfig_AVERAGE), temp); v != nil {
		props.TemperatureF = v
	}
	if v := aggregateNumeric(strategyOrDefault(cfg.GetLightStrategy(), api2.AggregationConfig_AVERAGE), light); v != nil {
		lux := int32(*v)
		props.LightLevelLux = &lux
	}
	if v := aggregateNumeric(strategyOrDefault(cfg.GetAirQualityStrategy(), api2.AggregationConfig_AVERAGE), aqi); v != nil {
		idx := int32(*v)
		props.AirQualityIndex = &idx
	}
	if v := aggregateNumeric(strategyOrDefault(cfg.GetPowerStrategy(), api2.AggregationConfig_SUM), power); v != nil {
		props.PowerDrawW = v
	}

	// A room with no linked Sensor that has ever reported any metric gets
	// no Properties at all, not an all-fields-unset one - so a fresh link
	// (setDeviceRoom, before any Update has arrived for it) leaves
	// getProperties returning nil rather than a misleadingly "computed"
	// empty result.
	if props.Occupied == nil && props.TemperatureF == nil && props.LightLevelLux == nil && props.AirQualityIndex == nil && props.PowerDrawW == nil {
		return nil
	}
	return props
}

func celsiusToFahrenheit(c float64) float64 {
	return c*9/5 + 32
}

/* ----- db <-> API AggregationConfig conversion ----- */

func dbStrategyToAPI(s db.AggregationStrategy) api2.AggregationConfig_Strategy {
	switch s {
	case db.AggregationLatest:
		return api2.AggregationConfig_LATEST
	case db.AggregationAverage:
		return api2.AggregationConfig_AVERAGE
	case db.AggregationMin:
		return api2.AggregationConfig_MIN
	case db.AggregationMax:
		return api2.AggregationConfig_MAX
	case db.AggregationSum:
		return api2.AggregationConfig_SUM
	case db.AggregationAny:
		return api2.AggregationConfig_ANY
	default:
		return api2.AggregationConfig_STRATEGY_UNSPECIFIED
	}
}

func apiStrategyToDB(s api2.AggregationConfig_Strategy) db.AggregationStrategy {
	switch s {
	case api2.AggregationConfig_LATEST:
		return db.AggregationLatest
	case api2.AggregationConfig_AVERAGE:
		return db.AggregationAverage
	case api2.AggregationConfig_MIN:
		return db.AggregationMin
	case api2.AggregationConfig_MAX:
		return db.AggregationMax
	case api2.AggregationConfig_SUM:
		return db.AggregationSum
	case api2.AggregationConfig_ANY:
		return db.AggregationAny
	default:
		return db.AggregationUnspecified
	}
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
