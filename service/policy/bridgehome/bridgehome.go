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
	"sync"
	"time"

	"go.uber.org/zap"

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

	engine.UpdateDeviceState(id, d)
}

func hasMotion(d *device.Device) bool {
	return d.GetSensor().GetPresence().GetState().GetMotionDetected()
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
	engine.UpdateDeviceState(id, d)
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

// GetAttribute implements policy.HomeAPI. Deferred for the same reason as
// GetSensor.
func (a *Adapter) GetAttribute(id, key string) (any, error) {
	return nil, policy.ErrNotImplemented
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
