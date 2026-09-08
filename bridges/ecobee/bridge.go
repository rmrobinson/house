package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/spf13/viper"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/bridges/lib/homekitctrl"
	"github.com/rmrobinson/house/service/bridge"
)

// EcobeeBridge is the bridge.Handler implementation for a single paired ecobee thermostat and
// its remote sensors.
type EcobeeBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	conn   *ecobeeConn
	config ecobeeConfig

	mu             sync.Mutex
	lastThermostat *device.Device
	lastSensors    map[string]*device.Device // keyed by house device ID
}

// NewEcobeeBridge creates a bridge for the ecobee described by cfg. The connection to it isn't
// established until Run is called.
func NewEcobeeBridge(logger *zap.Logger, svc *bridge.Service, store homekitctrl.Store, cfg ecobeeConfig) *EcobeeBridge {
	b := &api2.Bridge{
		Id:           viper.GetString("bridge.id"),
		IsReachable:  true,
		ModelId:      "ECOBEE1",
		Manufacturer: "Faltung Networks",
		Config: &api2.Bridge_Config{
			Name:        viper.GetString("bridge.name"),
			Description: viper.GetString("bridge.description"),
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}

	return &EcobeeBridge{
		logger:      logger,
		svc:         svc,
		b:           b,
		conn:        newEcobeeConn(store, cfg.AccessoryName),
		config:      cfg,
		lastSensors: make(map[string]*device.Device),
	}
}

// Bridge returns the static Bridge descriptor for this process.
func (eb *EcobeeBridge) Bridge() *api2.Bridge {
	return eb.b
}

// Run polls the ecobee on a fixed interval until ctx is cancelled. A failed poll is left for the
// next tick to retry rather than driving its own backoff loop - the ticker interval already
// provides that, matching every other poll-based bridge in this repo.
func (eb *EcobeeBridge) Run(ctx context.Context) {
	if err := eb.Refresh(ctx); err != nil {
		eb.logger.Error("initial refresh failed", zap.Error(err))
	}

	interval := time.Duration(viper.GetInt("bridge.refresh_interval")) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	eb.logger.Info("beginning refresh loop", zap.Duration("interval", interval))
	for {
		select {
		case <-ticker.C:
			if err := eb.Refresh(ctx); err != nil {
				eb.logger.Error("unable to refresh ecobee state", zap.Error(err))
				continue
			}
			eb.logger.Debug("refreshed")
		case <-ctx.Done():
			eb.logger.Info("run context cancelled")
			return
		}
	}
}

// Refresh re-fetches every watched characteristic in a single request and rebuilds every device
// from the result, always re-reading the entire watched set rather than diffing against a
// partial cache - see the design notes on why (a real ecobee has been observed to silently stop
// updating individual characteristics after a reconnect while others keep working fine).
func (eb *EcobeeBridge) Refresh(ctx context.Context) error {
	ctrl, err := eb.conn.ensureConnected(ctx)
	if err != nil {
		eb.markAllUnreachable()
		return fmt.Errorf("connect: %w", err)
	}

	ids := thermostatCharIDs()
	for _, s := range eb.config.Sensors {
		ids = append(ids, remoteSensorCharIDs(s.AccessoryID)...)
	}

	values, err := ctrl.ReadCharacteristics(ctx, ids)
	if err != nil {
		eb.conn.invalidate()
		eb.markAllUnreachable()
		return fmt.Errorf("read characteristics: %w", err)
	}

	thermostatValues := indexCharValues(values, thermostatAID)
	if err := thermostatValues.validateRequired(requiredThermostatChars); err != nil {
		// A partial read isn't trustworthy enough to build any device from - see
		// requiredThermostatChars' doc comment. Deliberately doesn't invalidate the connection:
		// this is a data-completeness problem, not evidence the connection itself is bad, so
		// there's no reason to force a reconnect over it.
		eb.markAllUnreachable()
		return fmt.Errorf("incomplete characteristic read: %w", err)
	}
	d := buildThermostatDevice(eb.config.Thermostat.ID, eb.config.Thermostat.Name, thermostatValues)
	d.Address = &device.Device_Address{IsReachable: true}

	eb.mu.Lock()
	eb.lastThermostat = d
	eb.mu.Unlock()
	eb.svc.UpdateDevice(d)

	for _, s := range eb.config.Sensors {
		sensorValues := indexCharValues(values, s.AccessoryID)
		sd := buildRemoteSensorDevice(s.ID, s.Name, sensorValues)
		sd.Address = &device.Device_Address{IsReachable: true}

		eb.mu.Lock()
		eb.lastSensors[s.ID] = sd
		eb.mu.Unlock()
		eb.svc.UpdateDevice(sd)
	}

	return nil
}

// markAllUnreachable flips every known device's reachability off in place, preserving its last
// known state otherwise. Called when the connection to the ecobee itself is down - individual
// characteristic read failures are handled by Refresh failing outright instead, since a partial
// read isn't trustworthy enough to build any device from (see Refresh's doc comment).
func (eb *EcobeeBridge) markAllUnreachable() {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	if eb.lastThermostat != nil {
		d := proto.Clone(eb.lastThermostat).(*device.Device)
		d.Address = &device.Device_Address{IsReachable: false}
		eb.lastThermostat = d
		eb.svc.UpdateDevice(d)
	}
	for id, d := range eb.lastSensors {
		dc := proto.Clone(d).(*device.Device)
		dc.Address = &device.Device_Address{IsReachable: false}
		eb.lastSensors[id] = dc
		eb.svc.UpdateDevice(dc)
	}
}

// ProcessCommand takes a given command request and attempts to execute it. Only the thermostat
// device accepts commands - remote sensors are read-only.
func (eb *EcobeeBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	eb.mu.Lock()
	current := eb.lastThermostat
	_, isSensor := eb.lastSensors[cmd.DeviceId]
	eb.mu.Unlock()

	if current == nil {
		return nil, bridge.ErrDeviceNotFound
	}
	if cmd.DeviceId != current.Id {
		if isSensor {
			// The device exists, it's just read-only - distinct from not existing at all.
			return nil, bridge.ErrUnsupportedCommand
		}
		return nil, bridge.ErrDeviceNotFound
	}

	ctrl, err := eb.conn.ensureConnected(ctx)
	if err != nil {
		eb.logger.Error("unable to connect to ecobee to process command", zap.Error(err))
		return nil, status.Error(codes.Unavailable, "unable to connect to ecobee")
	}

	optimistic := proto.Clone(current).(*device.Device)

	// SetTemperature's routing (HEAT vs COOL) depends on the thermostat's *current* target mode,
	// which can be up to bridge.refresh_interval seconds stale in the cached device - a live read
	// here avoids routing a write against a mode the accessory has since left (e.g. changed
	// directly on the thermostat, or by another controller, between poll cycles). Best-effort:
	// if it fails, fall back to the cached mode rather than blocking the command entirely.
	if _, ok := cmd.Details.(*command.Command_SetTemperature); ok {
		if liveMode, err := eb.readLiveTargetMode(ctx, ctrl); err != nil {
			eb.logger.Warn("unable to confirm live thermostat mode before SetTemperature, using last polled value", zap.Error(err))
		} else {
			optimistic.GetThermostat().GetThermostat().GetState().TargetMode = liveMode
		}
	}

	writes, err := thermostatCommandWrites(cmd, optimistic)
	if err != nil {
		// thermostatCommandWrites returns bridge.ErrUnsupportedCommand (already a gRPC status
		// error) for command types this bridge doesn't implement; anything else is a plain Go
		// error describing a bad argument (out-of-range setpoint, unknown comfort profile, ...).
		// Passing the former through unmodified preserves its FailedPrecondition code instead of
		// flattening it to InvalidArgument.
		if _, ok := status.FromError(err); ok {
			return nil, err
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if err := ctrl.WriteCharacteristics(ctx, writes); err != nil {
		eb.logger.Error("unable to write command to ecobee", zap.String("device_id", cmd.DeviceId), zap.Error(err))
		eb.conn.invalidate()
		return nil, status.Error(codes.Internal, "unable to write command to ecobee")
	}

	// optimistic (mutated by thermostatCommandWrites above) is applied without a synchronous
	// verify re-read: ecobee's own HAP writes aren't reliably reflected in an immediate re-read
	// (see README's write-reliability notes - the delay has been observed to be longer than a
	// single poll interval specifically for HomeKit-originated writes). The next Refresh cycle
	// (at most bridge.refresh_interval seconds away) re-reads the real state and corrects this if
	// the accessory disagreed, the same way ESPHome's bridge handles optimistic command
	// application.
	eb.mu.Lock()
	eb.lastThermostat = optimistic
	eb.mu.Unlock()
	eb.svc.UpdateDevice(optimistic)

	return optimistic, nil
}

// readLiveTargetMode reads the thermostat's current TargetHeatingCoolingState directly, bypassing
// the poll cache - see ProcessCommand's use of it for why.
func (eb *EcobeeBridge) readLiveTargetMode(ctx context.Context, ctrl hapController) (trait.Thermostat_Mode, error) {
	values, err := ctrl.ReadCharacteristics(ctx, []homekitctrl.CharID{{AccessoryID: thermostatAID, CharacteristicID: charTargetHeatingCoolingState}})
	if err != nil {
		return trait.Thermostat_MODE_UNSPECIFIED, err
	}
	mode, ok := indexCharValues(values, thermostatAID).int(charTargetHeatingCoolingState)
	if !ok {
		return trait.Thermostat_MODE_UNSPECIFIED, fmt.Errorf("target mode missing from live read")
	}
	return hvacStateToMode(mode, true), nil
}

// SetBridgeConfig takes the supplied config params and saves them for future reference.
func (eb *EcobeeBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	eb.b.Config.Name = config.Name
	eb.b.Config.Description = config.Description

	viper.Set("bridge.name", config.Name)
	viper.Set("bridge.description", config.Description)
	return viper.WriteConfig()
}
