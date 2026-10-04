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
	"google.golang.org/protobuf/reflect/protoregistry"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
	"github.com/rmrobinson/house/service/lib/bridgeconn"
	"github.com/rmrobinson/house/service/lib/grpcutil"
	"github.com/rmrobinson/house/service/lib/protoreflectutil"
	"github.com/rmrobinson/house/service/policy"
)

// commandTimeout bounds ExecuteCommand: the connected endpoint is expected to
// resolve (or fail) a command against a real device in well under this, and
// without a deadline a wedged downstream bridge would hang the calling
// policy script's goroutine forever.
const commandTimeout = 10 * time.Second

// ErrNotReady is returned by SetLight and SetState - which must reach the
// connected endpoint to execute a command - when called before Start has
// run or while the connection to addr is down. GetLight and GetLastKnown
// only ever consult the engine's own cache, so they return ErrNotReady
// solely for the before-Start case: once populated, cached state remains
// readable through a later disconnect, which is exactly the point of
// reading from a cache rather than the live connection. GetSensor,
// GetHouseState and SetHouseState remain unconditionally unimplemented -
// see their own doc comments - and always return policy.ErrNotImplemented
// regardless.
var ErrNotReady = errors.New("bridgehome: adapter not started")

// commandDescriptor is command.Command's own message descriptor, used to
// resolve SetState's key against Command.details' oneof member field
// names (see SetState's doc comment).
var commandDescriptor = (&command.Command{}).ProtoReflect().Descriptor()

// updateQueueSize bounds Adapter's pending-update channel (see handleUpdate/
// processUpdates): large enough that an ordinary burst (e.g. a facade's
// per-device INITIAL replay) never blocks the connection's own Recv loop,
// while still bounded so a genuinely stuck applyUpdate (rather than just a
// slow one) eventually applies backpressure instead of growing without
// limit.
const updateQueueSize = 256

// Adapter is a policy.HomeAPI backed by exactly one BridgeService connection.
// It owns no local device->bridge routing table: if addr is a facade
// aggregating several upstream bridges, resolving which one owns a given
// device is that endpoint's job, not Adapter's.
type Adapter struct {
	logger *zap.Logger
	conn   *bridgeconn.Conn

	// updates queues Updates from handleUpdate (called synchronously from
	// conn.Run's stream.Recv() loop) for processUpdates to apply on its own
	// goroutine, in the same order they arrived - so a bulk InitialUpdate's
	// device-by-device application (which can take a while for a large
	// snapshot) never delays picking up the connection's next message, while
	// every update - InitialUpdate included - is still applied strictly in
	// receive order, exactly as if handleUpdate had applied it inline.
	updates chan *api2.Update

	mu     sync.Mutex
	ctx    context.Context
	engine *policy.Engine
	// live is true once the connection has actually delivered an Update, and
	// false again once it drops (see handleUpdate/onDrop) - a.conn.Client()
	// alone goes non-nil as soon as the dial succeeds, before the upstream
	// has confirmed it's actually there to talk to.
	live bool
}

// New creates an Adapter that will connect to addr once Start is called.
// Every HomeAPI method returns ErrNotReady until then. tlsCfg, if non-nil,
// is used for the connection; nil means plaintext gRPC.
func New(logger *zap.Logger, addr string, tlsCfg *grpcutil.ClientTLSConfig) *Adapter {
	return &Adapter{
		logger:  logger,
		conn:    bridgeconn.New(logger, addr, tlsCfg),
		updates: make(chan *api2.Update, updateQueueSize),
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

	go a.processUpdates(ctx)
	go a.conn.Run(ctx, a.handleUpdate, a.onDrop)
}

// processUpdates applies every Update handleUpdate enqueues, one at a time
// and in order, until ctx is done. Running on its own goroutine keeps
// applying an update (in particular, a bulk InitialUpdate's device loop)
// from ever delaying conn.Run's stream.Recv() loop.
func (a *Adapter) processUpdates(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case u := <-a.updates:
			a.applyUpdate(u)
		}
	}
}

// onDrop marks the connection no longer live. Called once each time the
// underlying connection is lost, including on shutdown while connected (see
// bridgeconn.Conn.Run).
func (a *Adapter) onDrop() {
	a.mu.Lock()
	a.live = false
	a.mu.Unlock()
}

// isLive reports whether the connection has delivered at least one Update
// since it last came up (see handleUpdate/onDrop).
func (a *Adapter) isLive() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.live
}

// handleUpdate is bridgeconn.Conn.Run's onUpdate callback: called
// synchronously from its stream.Recv() loop (see that doc comment), so it
// must return quickly. It marks the connection live, then hands u to
// processUpdates via a.updates for the actual (potentially slow, for a bulk
// InitialUpdate) application - a blocking send is used deliberately, so a
// full queue applies backpressure onto the connection rather than growing
// without bound, at the cost of delaying the next Recv() only when
// processUpdates has fallen far behind.
func (a *Adapter) handleUpdate(u *api2.Update) {
	a.mu.Lock()
	ctx := a.ctx
	a.live = true
	a.mu.Unlock()

	select {
	case a.updates <- u:
	case <-ctx.Done():
	}
}

// applyUpdate applies u to the engine's cache/bus, called only from
// processUpdates so every update - InitialUpdate included - is applied
// strictly in the order handleUpdate received it. It handles every shape
// bridge.proto documents as legal for Update, not just the one the currently
// configured addr happens to send: a bulk Update_InitialUpdate (what an
// individual bridge's own server sends) and a per-device Update_DeviceUpdate
// for any Action including INITIAL (what service/bridge/facade currently
// sends instead) are treated as equally expected, not one primary path and
// one fallback.
func (a *Adapter) applyUpdate(u *api2.Update) {
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

	// UpdateDeviceState itself publishes "device.updated.<id>" - the
	// trait-agnostic trigger for any condition watching this device's
	// attributes generically (see GetState) - on every update, including
	// the very first one, since it's just a signal to re-check, not itself
	// a fact.
	engine.UpdateDeviceState(id, deviceKind(d), d)
}

// hasMotion reports whether d's presence trait, wherever it lives, currently
// reports motion. It goes through resolveState rather than a hardcoded
// d.GetSensor().GetPresence() chain, so it works for any device kind whose
// details message happens to have a "presence" field - Sensor, Camera and
// Thermostat all do today - without a per-kind branch here, and without
// missing whichever kind adds one next.
func hasMotion(d *device.Device) bool {
	v, err := resolveState(d, "presence.state.motion_detected")
	if err != nil {
		return false
	}
	motion, _ := v.(bool)
	return motion
}

// isDischarging reports whether d's battery trait, wherever it lives,
// currently reports discharging. Like hasMotion, it goes through
// resolveState rather than a hardcoded per-kind chain, so it also covers a
// Generic device's optional battery trait (device/generic.proto), not just
// Sensor and Ups.
func isDischarging(d *device.Device) bool {
	v, err := resolveState(d, "battery.state.discharging")
	if err != nil {
		return false
	}
	discharging, _ := v.(bool)
	return discharging
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
	return a.runCommand(&command.Command{
		DeviceId: id,
		Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: on}},
	})
}

// runCommand executes cmd (DeviceId already set) against the connected
// endpoint under a bounded context, then applies its response to the
// engine's cache immediately rather than waiting for the matching CHANGED
// update to arrive on the stream - gives read-your-writes consistency for a
// script that reads a device's state right after commanding it. The later
// stream CHANGED update for the same change is a harmless duplicate
// UpdateDeviceState call. Shared by SetLight and SetState.
func (a *Adapter) runCommand(cmd *command.Command) error {
	a.mu.Lock()
	ctx := a.ctx
	engine := a.engine
	live := a.live
	a.mu.Unlock()

	if !live || engine == nil {
		return ErrNotReady
	}

	client := a.conn.Client()
	if client == nil {
		return ErrNotReady
	}

	cmdCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	d, err := client.ExecuteCommand(cmdCtx, cmd)
	if err != nil {
		// Propagated as-is, whatever the connected endpoint returned -
		// meaningful to a script/caller already, no need to wrap.
		return err
	}

	engine.UpdateDeviceState(cmd.GetDeviceId(), deviceKind(d), d)
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

// GetState implements policy.HomeAPI as a generic dotted-path read over
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
func (a *Adapter) GetState(id, key string) (any, error) {
	d, err := a.getDevice(id)
	if err != nil {
		return nil, err
	}
	return resolveState(d, key)
}

// GetDeviceName implements policy.HomeAPI, returning id's configured
// display name (Device.Config.Name) - falling back to id itself if unset,
// so a policy script building a human-readable message never shows a
// blank. Unlike GetState's dotted path, which only walks Device.details'
// populated oneof branch, Config lives outside that oneof entirely and is
// otherwise unreachable from a script.
func (a *Adapter) GetDeviceName(id string) (string, error) {
	d, err := a.getDevice(id)
	if err != nil {
		return "", err
	}
	if name := d.GetConfig().GetName(); name != "" {
		return name, nil
	}
	return id, nil
}

// GetDeviceRoom implements policy.HomeAPI. Room linking is HouseService
// data (api/house.proto's DeviceRoomLink), entirely separate from
// BridgeService's own Device this Adapter wraps - unconditionally
// ErrNotImplemented, staying strictly scoped to bridge.proto's contract,
// the same convention GetHouseState/SetHouseState already follow. See
// service/policy/housestate.Adapter for the HouseService-backed answer.
func (a *Adapter) GetDeviceRoom(id string) (string, error) {
	return "", policy.ErrNotImplemented
}

// HasState implements policy.HomeAPI, walking the same dot-path as GetState
// (see resolveState) but reporting whether it's actually populated rather
// than reading its value - resolveState's Get() calls return a zero-value
// message for an *unset* optional field (proto3 explicit-presence
// semantics), so e.g. GetState(id, "battery.state.capacity_remaining_pct")
// on a device with no Battery trait silently returns 0, indistinguishable
// from "battery actually at 0%". HasState(id, "battery") is what tells
// those apart.
func (a *Adapter) HasState(id, key string) (bool, error) {
	d, err := a.getDevice(id)
	if err != nil {
		return false, err
	}
	return hasState(d, key)
}

// deviceKind returns the name of d's populated details oneof branch
// ("light", "sensor", "ups", "camera", ...), or "" if none is set. Passed to
// policy.Engine.UpdateDeviceState as the opaque "kind" tag policy scripts
// can later enumerate devices by via home.findDevices(kind) - the engine
// itself has no notion of device.Device's schema, this is just the most
// natural string bridgehome has on hand to tag devices with.
func deviceKind(d *device.Device) string {
	_, name, ok := protoreflectutil.OneofMessage(d, "details")
	if !ok {
		return ""
	}
	return string(name)
}

// resolveState walks key's dot-separated segments as literal field names
// starting from whichever device.Device.details oneof branch d has set,
// descending through singular message-typed fields until a scalar value is
// reached.
func resolveState(d *device.Device, key string) (any, error) {
	cur, _, ok := protoreflectutil.OneofMessage(d, "details")
	if !ok {
		return nil, fmt.Errorf("bridgehome: device has no details set")
	}

	segments := strings.Split(key, ".")
	for i, seg := range segments {
		fields := cur.Descriptor().Fields()
		fieldDesc := fields.ByName(protoreflect.Name(seg))
		if fieldDesc == nil {
			return nil, fmt.Errorf("bridgehome: state %q: no field %q on %s", key, seg, cur.Descriptor().FullName())
		}

		last := i == len(segments)-1

		if fieldDesc.Kind() != protoreflect.MessageKind && fieldDesc.Kind() != protoreflect.GroupKind {
			if !last {
				return nil, fmt.Errorf("bridgehome: state %q: %q is a scalar, but the path continues", key, seg)
			}
			return scalarToGo(cur.Get(fieldDesc), fieldDesc.Kind()), nil
		}

		if last {
			return nil, fmt.Errorf("bridgehome: state %q: %q is a message, not a scalar", key, seg)
		}
		if fieldDesc.IsList() || fieldDesc.IsMap() {
			return nil, fmt.Errorf("bridgehome: state %q: %q is a list/map, not supported", key, seg)
		}
		cur = cur.Get(fieldDesc).Message()
	}
	return nil, fmt.Errorf("bridgehome: state %q: empty path", key)
}

// hasState walks key's dot-separated segments the same way resolveState
// does, but reports presence at each step via protoreflect.Message.Has
// rather than reading a value, so it can tell "not populated" apart from
// "populated with a zero value" - something resolveState's Get() alone
// cannot (an unset optional message field and a present-but-empty one
// return the same thing from Get()). A segment absent partway through the
// path (e.g. "battery" itself unset, for key "battery.state.
// capacity_remaining_pct") reports false, nil rather than continuing to
// descend into it, since Get() would otherwise hand back a zero-value
// message to keep walking - exactly the ambiguity this function exists to
// avoid.
func hasState(d *device.Device, key string) (bool, error) {
	cur, _, ok := protoreflectutil.OneofMessage(d, "details")
	if !ok {
		return false, fmt.Errorf("bridgehome: state %q: device has no details set", key)
	}

	segments := strings.Split(key, ".")
	for i, seg := range segments {
		fields := cur.Descriptor().Fields()
		fieldDesc := fields.ByName(protoreflect.Name(seg))
		if fieldDesc == nil {
			return false, fmt.Errorf("bridgehome: state %q: no field %q on %s", key, seg, cur.Descriptor().FullName())
		}

		last := i == len(segments)-1
		// Path-shape validity is checked before presence, and so is
		// deterministic regardless of the field's current value: otherwise
		// a malformed path like "battery.state.discharging.extra" would
		// only be caught as an error when discharging happened to be true,
		// since Has() on a proto3 implicit-presence bool false-value field
		// reports false exactly like an absent one - the field being
		// checked here, not the field discharging actually is.
		if !last {
			if fieldDesc.Kind() != protoreflect.MessageKind && fieldDesc.Kind() != protoreflect.GroupKind {
				return false, fmt.Errorf("bridgehome: state %q: %q is a scalar, but the path continues", key, seg)
			}
			if fieldDesc.IsList() || fieldDesc.IsMap() {
				return false, fmt.Errorf("bridgehome: state %q: %q is a list/map, not supported", key, seg)
			}
		}

		if !cur.Has(fieldDesc) {
			return false, nil
		}
		if last {
			return true, nil
		}
		cur = cur.Get(fieldDesc).Message()
	}
	return false, fmt.Errorf("bridgehome: state %q: empty path", key)
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

// SetState implements policy.HomeAPI: a policy script is asking for id's
// state to become a particular value, not asking to "run a command" as
// such - that a command.Command is how this Adapter happens to get there is
// an implementation detail of talking to BridgeService, not part of
// HomeAPI's own contract (a different HomeAPI backing, e.g. a direct
// house-state write, need not involve a command at all). key is the literal
// name of one of Command.details' oneof fields (see api/command/command.proto
// for the full list - "on_off",
// "brightness_absolute", "colour", "set_thermostat_mode", "toggle", ...),
// and value is a map[string]any of that command message's own field values,
// resolved against its descriptor via protoreflect exactly the way
// GetState reads device state - so every current and future command
// variant is reachable with no code change here. A nested message field
// (e.g. Colour.rgb) takes a nested map; a map field (e.g. Toggle.settings)
// takes a map of its own entries, string-keyed only; a field that's itself
// part of a oneof inside the command message (e.g. Colour's own
// rgb/hsb/colour_temperature_k) is set exactly like any other field, since
// proto3 makes no reflection-level distinction there - setting exactly one
// of them is the caller's own responsibility, matching that oneof's
// semantics. An enum field's value is its literal enum value name as a
// string (e.g. "HEAT" for set_thermostat_mode's mode). key must name a
// Command.details member specifically - "device_id"/"id"/"version" (not
// part of that oneof) are rejected, same as any unknown key.
func (a *Adapter) SetState(id, key string, value any) error {
	params, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("bridgehome: setState %q: value must be a table of the command's own fields, got %T", key, value)
	}

	fd := commandDescriptor.Fields().ByName(protoreflect.Name(key))
	if fd == nil || fd.ContainingOneof() == nil || fd.ContainingOneof().Name() != "details" {
		return fmt.Errorf("bridgehome: setState: %q is not a known command (see Command.details in api/command/command.proto)", key)
	}

	msg, err := buildCommandMessage(fd.Message(), params)
	if err != nil {
		return fmt.Errorf("bridgehome: setState %q: %w", key, err)
	}

	cmd := &command.Command{DeviceId: id}
	cmd.ProtoReflect().Set(fd, protoreflect.ValueOfMessage(msg))

	return a.runCommand(cmd)
}

// buildCommandMessage constructs a new message of desc's own registered Go
// type, populated from params via protoreflect: each key is a literal field
// name on desc. It recurses into nested message fields and handles
// string-keyed map fields, covering every command sub-message shape
// api/command/*.proto defines today with no per-message Go code - see
// SetState's doc comment for the full convention. Repeated fields aren't
// supported (none exist in api/command/*.proto today); encountering one
// fails loudly rather than silently mishandling it.
func buildCommandMessage(desc protoreflect.MessageDescriptor, params map[string]any) (protoreflect.Message, error) {
	mt, err := protoregistry.GlobalTypes.FindMessageByName(desc.FullName())
	if err != nil {
		return nil, fmt.Errorf("no registered message type for %s: %w", desc.FullName(), err)
	}
	msg := mt.New()

	for key, v := range params {
		fieldDesc := desc.Fields().ByName(protoreflect.Name(key))
		if fieldDesc == nil {
			return nil, fmt.Errorf("%s has no field %q", desc.FullName(), key)
		}

		switch {
		case fieldDesc.IsMap():
			if fieldDesc.MapKey().Kind() != protoreflect.StringKind {
				return nil, fmt.Errorf("field %q: only string-keyed maps are supported", key)
			}
			entries, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("field %q is a map, want a table, got %T", key, v)
			}
			mapVal := msg.Mutable(fieldDesc).Map()
			for mk, mv := range entries {
				sv, err := scalarCommandValue(fieldDesc.MapValue(), mv)
				if err != nil {
					return nil, fmt.Errorf("field %q[%q]: %w", key, mk, err)
				}
				mapVal.Set(protoreflect.ValueOfString(mk).MapKey(), sv)
			}
		case fieldDesc.IsList():
			return nil, fmt.Errorf("field %q: repeated fields aren't supported", key)
		case fieldDesc.Kind() == protoreflect.MessageKind || fieldDesc.Kind() == protoreflect.GroupKind:
			sub, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("field %q is a message, want a table, got %T", key, v)
			}
			subMsg, err := buildCommandMessage(fieldDesc.Message(), sub)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", key, err)
			}
			msg.Set(fieldDesc, protoreflect.ValueOfMessage(subMsg))
		default:
			sv, err := scalarCommandValue(fieldDesc, v)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", key, err)
			}
			msg.Set(fieldDesc, sv)
		}
	}

	return msg, nil
}

// scalarCommandValue converts v - as produced by luaToGo (bool, string, or
// float64) for a Lua caller, or a plain Go value for one calling SetState
// directly - into the protoreflect.Value fd's kind expects, covering every
// scalar/enum kind api/command/*.proto uses today.
func scalarCommandValue(fd protoreflect.FieldDescriptor, v any) (protoreflect.Value, error) {
	if fd.Kind() == protoreflect.EnumKind {
		if s, ok := v.(string); ok {
			evd := fd.Enum().Values().ByName(protoreflect.Name(s))
			if evd == nil {
				return protoreflect.Value{}, fmt.Errorf("unknown value %q for enum %s", s, fd.Enum().FullName())
			}
			return protoreflect.ValueOfEnum(evd.Number()), nil
		}
		n, err := commandNumericValue(v)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("enum value must be its literal name (a string) or a number: %w", err)
		}
		return protoreflect.ValueOfEnum(protoreflect.EnumNumber(int32(n))), nil
	}

	switch fd.Kind() {
	case protoreflect.BoolKind:
		b, ok := v.(bool)
		if !ok {
			return protoreflect.Value{}, fmt.Errorf("want bool, got %T", v)
		}
		return protoreflect.ValueOfBool(b), nil
	case protoreflect.StringKind:
		s, ok := v.(string)
		if !ok {
			return protoreflect.Value{}, fmt.Errorf("want string, got %T", v)
		}
		return protoreflect.ValueOfString(s), nil
	case protoreflect.BytesKind:
		switch b := v.(type) {
		case []byte:
			return protoreflect.ValueOfBytes(b), nil
		case string:
			return protoreflect.ValueOfBytes([]byte(b)), nil
		default:
			return protoreflect.Value{}, fmt.Errorf("want bytes, got %T", v)
		}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		n, err := commandNumericValue(v)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfInt32(int32(n)), nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		n, err := commandNumericValue(v)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfInt64(int64(n)), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		n, err := commandNumericValue(v)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfUint32(uint32(n)), nil
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		n, err := commandNumericValue(v)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfUint64(uint64(n)), nil
	case protoreflect.FloatKind:
		n, err := commandNumericValue(v)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfFloat32(float32(n)), nil
	case protoreflect.DoubleKind:
		n, err := commandNumericValue(v)
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfFloat64(n), nil
	default:
		return protoreflect.Value{}, fmt.Errorf("unsupported field kind %s", fd.Kind())
	}
}

// commandNumericValue accepts every numeric Go kind a caller might
// plausibly pass - float64 (what every Lua number becomes via luaToGo) plus
// the concrete int/uint/float kinds a Go caller might use directly.
func commandNumericValue(v any) (float64, error) {
	switch t := v.(type) {
	case float64:
		return t, nil
	case float32:
		return float64(t), nil
	case int:
		return float64(t), nil
	case int8:
		return float64(t), nil
	case int16:
		return float64(t), nil
	case int32:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case uint:
		return float64(t), nil
	case uint8:
		return float64(t), nil
	case uint16:
		return float64(t), nil
	case uint32:
		return float64(t), nil
	case uint64:
		return float64(t), nil
	default:
		return 0, fmt.Errorf("want a number, got %T", v)
	}
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
