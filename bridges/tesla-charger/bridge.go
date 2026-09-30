package main

import (
	"context"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/spf13/viper"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
)

// chargerClient is the subset of *Charger this bridge needs, narrowed to an interface so Refresh
// can be tested against a fake - mirrors bridges/apc-ups's statusClient.
type chargerClient interface {
	State() (*ChargerState, error)
}

// ChargerBridge acts as the handler for Bridge requests for this charger.
type ChargerBridge struct {
	logger *zap.Logger
	svc    *bridge.Service

	charger chargerClient
	b       *api2.Bridge

	// lastDevice is the most recently published device, kept so Refresh can republish it with
	// Address.IsReachable = false when a poll fails.
	lastDevice *device.Device
}

// NewChargerBridge creates a new charger bridge
func NewChargerBridge(logger *zap.Logger, svc *bridge.Service, charger *Charger) *ChargerBridge {
	b := &api2.Bridge{
		Id:           viper.GetString("bridge.id"),
		IsReachable:  true,
		ModelId:      "TWC1",
		Manufacturer: "Faltung Networks",
		Config: &api2.Bridge_Config{
			Name:        viper.GetString("bridge.name"),
			Description: viper.GetString("bridge.description"),
			Address: &api2.Address{
				Ip: &api2.Address_Ip{
					Host: charger.ipAddr,
					Port: 80,
				},
			},
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}

	cb := &ChargerBridge{
		logger:  logger,
		svc:     svc,
		charger: charger,
		b:       b,
	}

	return cb
}

// ProcessCommand takes a given command request and attempts to execute it.
// We only worry about processing valid commands for the given device traits.
func (cb *ChargerBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	cb.logger.Error("received unsupported command - shouldn't happen")
	return nil, bridge.ErrUnsupportedCommand
}

// SetBridgeConfig takes the supplied config params and saves them for future reference.
func (cb *ChargerBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	cb.b.Config.Name = config.Name
	cb.b.Config.Description = config.Description

	viper.Set("bridge.name", config.Name)
	viper.Set("bridge.description", config.Description)
	viper.WriteConfig()

	return nil
}

// ProcessCommandAsync is present to conform to the bridge.Handler interface. This bridge has no
// device traits eligible for asynchronous commands, so it always returns ErrAsyncCommandsNotSupported.
func (cb *ChargerBridge) ProcessCommandAsync(ctx context.Context, cmd *command.Command) error {
	return bridge.ErrAsyncCommandsNotSupported
}

// Refresh is present to conform to the bridge.Handler interface. In this implementation it queries
// the charger API and returns the current state of the charger.
func (cb *ChargerBridge) Refresh(ctx context.Context) error {
	chargerState, err := cb.charger.State()
	if err != nil {
		cb.logger.Error("unable to get charger state",
			zap.Error(err))
		cb.markUnreachable()
		return status.Error(codes.Internal, "unable to refresh charger state")
	}

	d := chargerState.toDevice()
	if d == nil {
		// A partial response (missing vitals/version/lifetime) isn't trustworthy enough to
		// build a device from - and passing nil straight to UpdateDevice would be fatal, since
		// it treats a nil device as a programming error, not a data problem. Mark the charger
		// unreachable instead of crashing the whole bridge process over one bad poll.
		cb.logger.Warn("charger state incomplete, skipping this refresh")
		cb.markUnreachable()
		return nil
	}

	cb.lastDevice = d
	cb.svc.UpdateDevice(d)
	return nil
}

// markUnreachable republishes the last-known device with Address.IsReachable = false. A no-op
// if Refresh has never successfully published a device yet.
func (cb *ChargerBridge) markUnreachable() {
	if cb.lastDevice == nil {
		return
	}

	gone := proto.Clone(cb.lastDevice).(*device.Device)
	gone.Address.IsReachable = false
	cb.lastDevice = gone
	cb.svc.UpdateDevice(gone)
}

// Run begins the process of polling the charger API and reporting back the state.
func (cb *ChargerBridge) Run(ctx context.Context) {
	cb.Refresh(ctx)

	refreshTimer := time.NewTicker(time.Second * time.Duration(viper.GetInt("bridge.refresh_interval")))
	cb.logger.Info("beginning refresh loop", zap.Int("refresh_interval", viper.GetInt("bridge.refresh_interval")))
	for {
		select {
		case <-refreshTimer.C:
			if err := cb.Refresh(ctx); err != nil {
				cb.logger.Error("unable to refresh charger",
					zap.Error(err))
				continue
			}
			cb.logger.Debug("refreshed")
		case <-ctx.Done():
			cb.logger.Info("run context cancelled")
			return
		}
	}
}
