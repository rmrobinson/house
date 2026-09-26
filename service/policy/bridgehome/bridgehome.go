// Package bridgehome implements policy.HomeAPI against a live BridgeService
// endpoint, so policy conditions are driven by real device events instead of
// polling. It is coded strictly to bridge.proto's documented contract: the
// endpoint it connects to may be a single bridge, a standalone bridgefacaded,
// or a housed process with the facade embedded, and this package neither
// knows nor cares which — it doesn't assume any behavior beyond what
// BridgeService itself guarantees.
package bridgehome

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/reflect/protoreflect"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/bridgeconn"
	"github.com/rmrobinson/house/service/bridge"
	"github.com/rmrobinson/house/service/policy"
)

// commandTimeout bounds ExecuteCommand: the connected endpoint is expected to
// resolve (or fail) a command against a real device in well under this, and
// without a deadline a wedged downstream bridge would hang the calling
// policy script's goroutine forever.
const commandTimeout = 10 * time.Second

// ErrNotReady is returned by GetLight, SetLight and GetLastKnown when called
// before Start has run or while the connection to addr is down. The other
// HomeAPI methods (GetSensor, GetAttribute, SetAttribute, GetHouseState,
// SetHouseState) are unconditionally unimplemented - see their own doc
// comments - and always return policy.ErrNotImplemented regardless.
var ErrNotReady = errors.New("bridgehome: adapter not started")

// Adapter is a policy.HomeAPI backed by exactly one BridgeService connection.
// It owns no local device->bridge routing table: if addr is a facade
// aggregating several upstream bridges, resolving which one owns a given
// device is that endpoint's job, not Adapter's.
type Adapter struct {
	logger *zap.Logger
	conn   *bridgeconn.Conn

	mu     sync.Mutex
	ctx    context.Context
	engine *policy.Engine
}

// New creates an Adapter that will connect to addr once Start is called.
// Every HomeAPI method returns ErrNotReady until then.
func New(logger *zap.Logger, addr string) *Adapter {
	return &Adapter{
		logger: logger,
		conn:   bridgeconn.New(logger, addr),
	}
}

// Start records engine (for cache/bus access) and begins connecting to addr
// in the background, reconnecting with jittered exponential backoff on
// disconnect. It returns immediately; ctx governs the connection's lifetime,
// and also bounds every RPC the adapter issues afterwards (e.g. SetLight).
func (a *Adapter) Start(ctx context.Context, engine *policy.Engine) {
	a.mu.Lock()
	a.ctx = ctx
	a.engine = engine
	a.mu.Unlock()

	go a.conn.Run(ctx, a.handleUpdate, nil)
}

// handleUpdate applies u to the engine's cache/bus. It handles every shape
// bridge.proto documents as legal for Update, not just the one the currently
// configured addr happens to send: a bulk Update_InitialUpdate (what an
// individual bridge's own server sends) and a per-device Update_DeviceUpdate
// for any Action including INITIAL (what service/bridge/facade currently
// sends instead) are treated as equally expected, not one primary path and
// one fallback.
func (a *Adapter) handleUpdate(u *api2.Update) {
	a.mu.Lock()
	engine := a.engine
	a.mu.Unlock()
	if engine == nil {
		return
	}

	if iu := u.GetInitialUpdate(); iu != nil {
		for _, d := range iu.GetDevices() {
			applyDeviceUpdate(engine, d.GetId(), d)
		}
		return
	}

	if du := u.GetDeviceUpdate(); du != nil {
		if u.Action == api2.Update_REMOVED {
			engine.RemoveDeviceState(du.GetDeviceId())
			return
		}
		if d := du.GetDevice(); d != nil {
			// DeviceUpdate.device is optional even for a non-REMOVED update
			// (bridge.proto); a DeviceUpdate with no Device carries nothing
			// about device state, so there's nothing to apply - in
			// particular, it must never overwrite an already-cached device
			// with a nil one.
			applyDeviceUpdate(engine, du.GetDeviceId(), d)
		}
		return
	}

	// Update_BridgeUpdate / Update_CommandUpdate: policy conditions care
	// about device state, not bridge-level or async-command-outcome state;
	// nothing here needs them yet.
}

// applyDeviceUpdate derives the sys.* system events from d's trait state
// relative to whatever was previously cached for id, then applies d as id's
// new last-known state. Events are only derived when id was already cached:
// otherwise d is the device's first-ever observed state, not a transition
// into it, so e.g. a motion sensor whose very first update already reports
// motion active must not be treated as a rising edge.
func applyDeviceUpdate(engine *policy.Engine, id string, d *device.Device) {
	prevVal, hadPrev := engine.GetLastKnown(id)
	prev, _ := prevVal.(*device.Device) // nil if a different type; fine, nil-safe below

	if hadPrev {
		if hasMotion(d) && !hasMotion(prev) {
			engine.Bus().Publish(policy.Event{Topic: "motion.detected", Payload: d})
		}
		// discharging means "running on battery" - so a discharging->not
		// transition is exactly "mains power came back".
		if !isDischarging(d) && isDischarging(prev) {
			engine.Bus().Publish(policy.Event{Topic: "power.restored", Payload: d})
		}
	}

	engine.UpdateDeviceState(id, deviceKind(d), d)

	// Trait-agnostic trigger for any condition watching this device's
	// attributes generically (see GetAttribute) - fires on every update,
	// including the very first one, since it's just a signal to re-check,
	// not itself a fact.
	engine.Bus().Publish(policy.Event{Topic: "device.updated." + id, Payload: d})
}

// hasMotion reports whether d's presence trait, wherever it lives, currently
// reports motion. It goes through resolveAttribute rather than a hardcoded
// d.GetSensor().GetPresence() chain, so it works for any device kind whose
// details message happens to have a "presence" field - Sensor, Camera and
// Thermostat all do today - without a per-kind branch here, and without
// missing whichever kind adds one next.
func hasMotion(d *device.Device) bool {
	v, err := resolveAttribute(d, "presence.state.motion_detected")
	if err != nil {
		return false
	}
	motion, _ := v.(bool)
	return motion
}

func isDischarging(d *device.Device) bool {
	// A Device is exactly one of Sensor/Ups (device.proto's details oneof),
	// so at most one of these chains is ever non-default.
	return d.GetSensor().GetBattery().GetState().GetDischarging() ||
		d.GetUps().GetBattery().GetState().GetDischarging()
}

// GetLight implements policy.HomeAPI.
func (a *Adapter) GetLight(id string) (bool, error) {
	d, err := a.getDevice(id)
	if err != nil {
		return false, err
	}
	return d.GetLight().GetOnOff().GetState().GetIsOn(), nil
}

// SetLight implements policy.HomeAPI. It calls ExecuteCommand on the single
// connected BridgeServiceClient with just the device ID - resolving which
// upstream bridge actually owns id (if addr is a facade aggregating several)
// is that endpoint's job, not Adapter's.
func (a *Adapter) SetLight(id string, on bool) error {
	a.mu.Lock()
	ctx := a.ctx
	engine := a.engine
	a.mu.Unlock()

	client := a.conn.Client()
	if client == nil || engine == nil {
		return ErrNotReady
	}

	cmdCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	d, err := client.ExecuteCommand(cmdCtx, &command.Command{
		DeviceId: id,
		Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: on}},
	})
	if err != nil {
		// Propagated as-is, whatever the connected endpoint returned -
		// meaningful to a script/caller already, no need to wrap.
		return err
	}

	// Apply the RPC's own response immediately rather than waiting for the
	// matching CHANGED update to arrive on the stream - gives read-your-writes
	// consistency for a script calling home.setLight() then home.getLight()
	// in the same run. The later stream CHANGED update for the same change is
	// a harmless duplicate UpdateDeviceState call.
	engine.UpdateDeviceState(id, deviceKind(d), d)
	return nil
}

// GetSensor implements policy.HomeAPI. Deferred: Sensor (api/device/sensor.proto)
// reports many optional traits at once (presence, battery, power, air
// quality, ...) with no single scalar "the" value - returns
// policy.ErrNotImplemented until a specific policy/condition needs a specific
// trait.
func (a *Adapter) GetSensor(id string) (float64, error) {
	return 0, policy.ErrNotImplemented
}

// GetAttribute implements policy.HomeAPI as a generic dotted-path read over
// id's cached last-known device state - the escape hatch GetSensor can't be
// (see GetSensor's own doc comment: Sensor alone reports many optional
// traits with no single "the" value). key is a literal dot-path of proto
// field names resolved against whichever device.Device.details oneof branch
// is populated (Light, Sensor, ...), with no implicit hops of any kind: e.g.
// "presence.state.motion_detected" or "air_properties.state.temperature_c"
// for a real api/trait/*.proto-backed field, "water.is_active" for a
// Sensor-local BinarySensor field that has no State indirection at all.
// Every current and future trait field is addressable this way without a
// code change here.
func (a *Adapter) GetAttribute(id, key string) (any, error) {
	d, err := a.getDevice(id)
	if err != nil {
		return nil, err
	}
	return resolveAttribute(d, key)
}

// detailsMessage returns d's populated device.Device.details oneof branch
// (Light, Sensor, Camera, ...) as a protoreflect.Message, and that branch
// field's name ("light", "sensor", "camera", ...), or ok=false if d has no
// details set. Shared by resolveAttribute (which descends into the message)
// and deviceKind (which only wants the branch name).
func detailsMessage(d *device.Device) (msg protoreflect.Message, name protoreflect.Name, ok bool) {
	m := d.ProtoReflect()

	oneof := m.Descriptor().Oneofs().ByName("details")
	if oneof == nil {
		return nil, "", false
	}
	fd := m.WhichOneof(oneof)
	if fd == nil {
		return nil, "", false
	}
	return m.Get(fd).Message(), fd.Name(), true
}

// deviceKind returns the name of d's populated details oneof branch
// ("light", "sensor", "ups", "camera", ...), or "" if none is set. Passed to
// policy.Engine.UpdateDeviceState as the opaque "kind" tag policy scripts
// can later enumerate devices by via home.findDevices(kind) - the engine
// itself has no notion of device.Device's schema, this is just the most
// natural string bridgehome has on hand to tag devices with.
func deviceKind(d *device.Device) string {
	_, name, ok := detailsMessage(d)
	if !ok {
		return ""
	}
	return string(name)
}

// resolveAttribute walks key's dot-separated segments as literal field names
// starting from whichever device.Device.details oneof branch d has set,
// descending through singular message-typed fields until a scalar value is
// reached.
func resolveAttribute(d *device.Device, key string) (any, error) {
	cur, _, ok := detailsMessage(d)
	if !ok {
		return nil, fmt.Errorf("bridgehome: device has no details set")
	}

	segments := strings.Split(key, ".")
	for i, seg := range segments {
		fields := cur.Descriptor().Fields()
		fieldDesc := fields.ByName(protoreflect.Name(seg))
		if fieldDesc == nil {
			return nil, fmt.Errorf("bridgehome: attribute %q: no field %q on %s", key, seg, cur.Descriptor().FullName())
		}

		last := i == len(segments)-1

		if fieldDesc.Kind() != protoreflect.MessageKind && fieldDesc.Kind() != protoreflect.GroupKind {
			if !last {
				return nil, fmt.Errorf("bridgehome: attribute %q: %q is a scalar, but the path continues", key, seg)
			}
			return scalarToGo(cur.Get(fieldDesc), fieldDesc.Kind()), nil
		}

		if last {
			return nil, fmt.Errorf("bridgehome: attribute %q: %q is a message, not a scalar", key, seg)
		}
		if fieldDesc.IsList() || fieldDesc.IsMap() {
			return nil, fmt.Errorf("bridgehome: attribute %q: %q is a list/map, not supported", key, seg)
		}
		cur = cur.Get(fieldDesc).Message()
	}
	return nil, fmt.Errorf("bridgehome: attribute %q: empty path", key)
}

// scalarToGo converts a resolved leaf protoreflect.Value into a plain Go
// value, matching the underlying Go kinds bindings.go's goToLua already
// knows how to convert to Lua.
func scalarToGo(v protoreflect.Value, kind protoreflect.Kind) any {
	switch kind {
	case protoreflect.BoolKind:
		return v.Bool()
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return v.Int()
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return v.Uint()
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return v.Float()
	case protoreflect.StringKind:
		return v.String()
	case protoreflect.EnumKind:
		return int64(v.Enum())
	case protoreflect.BytesKind:
		return v.Bytes()
	default:
		return v.Interface()
	}
}

// SetAttribute implements policy.HomeAPI. Deferred: command.Command has no
// generic "set an arbitrary key/value" variant - every command is a specific
// typed one (OnOff, Colour, BrightnessAbsolute, ...) - so there's no way to
// map an arbitrary (key, value) pair onto one without a convention that
// doesn't exist yet.
func (a *Adapter) SetAttribute(id, key string, value any) error {
	return policy.ErrNotImplemented
}

// GetHouseState implements policy.HomeAPI. No house-state stream exists yet
// (see bindings.go's HomeAPI doc comment).
func (a *Adapter) GetHouseState(key string) (any, error) {
	return nil, policy.ErrNotImplemented
}

// SetHouseState implements policy.HomeAPI. See GetHouseState.
func (a *Adapter) SetHouseState(key string, value any) error {
	return policy.ErrNotImplemented
}

// GetLastKnown implements policy.HomeAPI, reading through to the engine's own
// cache. A missing entry is (nil, nil), not an error - same convention as
// the in-memory HomeAPI test doubles elsewhere in this package.
func (a *Adapter) GetLastKnown(id string) (any, error) {
	a.mu.Lock()
	engine := a.engine
	a.mu.Unlock()
	if engine == nil {
		return nil, ErrNotReady
	}

	v, ok := engine.GetLastKnown(id)
	if !ok {
		return nil, nil
	}
	return v, nil
}

// Notify implements policy.HomeAPI by logging - there's no BridgeService RPC
// for notifying a human, so this is the same behavior as the stub HomeAPI
// used before a real adapter existed.
func (a *Adapter) Notify(event string, payload map[string]any) error {
	a.logger.Info("policy notification", zap.String("event", event), zap.Any("payload", payload))
	return nil
}

func (a *Adapter) getDevice(id string) (*device.Device, error) {
	a.mu.Lock()
	engine := a.engine
	a.mu.Unlock()
	if engine == nil {
		return nil, ErrNotReady
	}

	v, ok := engine.GetLastKnown(id)
	if !ok {
		return nil, bridge.ErrDeviceNotFound
	}
	d, ok := v.(*device.Device)
	if !ok {
		return nil, bridge.ErrDeviceNotFound
	}
	return d, nil
}

var _ policy.HomeAPI = (*Adapter)(nil)
