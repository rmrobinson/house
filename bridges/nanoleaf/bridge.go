package main

import (
	"context"
	"sync"

	"go.uber.org/zap"

	"github.com/spf13/viper"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
)

// NanoleafBridge is the bridge.Handler implementation for a set of Nanoleaf Light Panels
// controllers. Like bridges/esphome (and unlike bridges/zwave's single shared MQTT connection),
// each configured panel gets its own connection - a Nanoleaf controller is an independent
// HTTP+SSE endpoint on its own IP, so there's no shared transport to multiplex.
type NanoleafBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	panels []*panelConn

	mu          sync.Mutex
	deviceOwner map[string]*panelConn
}

// NewNanoleafBridge creates a bridge for the supplied set of panels. Connections aren't
// established until Start is called.
func NewNanoleafBridge(logger *zap.Logger, svc *bridge.Service, deviceConfigs []deviceConfig) *NanoleafBridge {
	b := &api2.Bridge{
		Id:           viper.GetString("bridge.id"),
		IsReachable:  true,
		ModelId:      "NANL1",
		Manufacturer: "Faltung Networks",
		Config: &api2.Bridge_Config{
			Name:        viper.GetString("bridge.name"),
			Description: viper.GetString("bridge.description"),
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}

	nb := &NanoleafBridge{
		logger:      logger,
		svc:         svc,
		b:           b,
		deviceOwner: make(map[string]*panelConn),
	}

	for _, dc := range deviceConfigs {
		if dc.Port == 0 {
			dc.Port = defaultPort
		}
		nb.panels = append(nb.panels, newPanelConn(logger, svc, nb, dc))
	}

	return nb
}

// Bridge returns the static Bridge descriptor for this process.
func (nb *NanoleafBridge) Bridge() *api2.Bridge {
	return nb.b
}

// Start connects to every configured panel. Each panel connects and reconnects independently in
// the background (nanoleaf.Client.SubscribeEvents owns its own reconnect/backoff); Start returns
// once the connection goroutines have been launched, not once they're ready.
func (nb *NanoleafBridge) Start(ctx context.Context) {
	for _, pc := range nb.panels {
		go pc.run(ctx)
	}
}

// registerDevice records which panelConn owns a device so ProcessCommand can route to it. Called
// by a panelConn once it has built a device from its panel's initial state.
func (nb *NanoleafBridge) registerDevice(id string, pc *panelConn) {
	nb.mu.Lock()
	defer nb.mu.Unlock()
	nb.deviceOwner[id] = pc
}

// ProcessCommand takes a given command request and attempts to execute it.
func (nb *NanoleafBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	nb.mu.Lock()
	pc, ok := nb.deviceOwner[cmd.DeviceId]
	nb.mu.Unlock()

	if !ok {
		nb.logger.Error("received command for unknown device id", zap.String("device_id", cmd.DeviceId))
		return nil, bridge.ErrDeviceNotFound
	}

	return pc.applyCommand(ctx, cmd)
}

// ProcessCommandAsync is present to conform to the bridge.Handler interface. This bridge has no
// device traits eligible for asynchronous commands, so it always returns
// ErrAsyncCommandsNotSupported.
func (nb *NanoleafBridge) ProcessCommandAsync(ctx context.Context, cmd *command.Command) error {
	return bridge.ErrAsyncCommandsNotSupported
}

// Refresh is present to conform to the bridge.Handler interface. Panel state arrives over each
// panelConn's standing event stream, so there's no polling loop needed here.
func (nb *NanoleafBridge) Refresh(ctx context.Context) error {
	return nil
}

// SetBridgeConfig takes the supplied config params and saves them for future reference.
func (nb *NanoleafBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	nb.b.Config.Name = config.Name
	nb.b.Config.Description = config.Description

	viper.Set("bridge.name", config.Name)
	viper.Set("bridge.description", config.Description)
	return viper.WriteConfig()
}
