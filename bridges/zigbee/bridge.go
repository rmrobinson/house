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

// ZigbeeBridge is the bridge.Handler implementation for a zigbee2mqtt gateway. Like
// bridges/zwave's ZwaveBridge, it owns a single mqttConn/networkConn pair shared by every device
// on the Zigbee network - see network.go's doc comment for why.
type ZigbeeBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	mqtt *mqttConn
	net  *networkConn

	mu          sync.Mutex
	deviceOwner map[string]*networkConn
}

// NewZigbeeBridge creates a bridge for the given config. The MQTT connection isn't established
// until Start is called.
func NewZigbeeBridge(logger *zap.Logger, svc *bridge.Service, cfg zigbeeConfig) (*ZigbeeBridge, error) {
	b := &api2.Bridge{
		Id:           viper.GetString("bridge.id"),
		IsReachable:  true,
		ModelId:      "ZBMQ1",
		Manufacturer: "Faltung Networks",
		Config: &api2.Bridge_Config{
			Name:        viper.GetString("bridge.name"),
			Description: viper.GetString("bridge.description"),
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}

	zb := &ZigbeeBridge{
		logger:      logger,
		svc:         svc,
		b:           b,
		deviceOwner: make(map[string]*networkConn),
	}

	mc, err := newMQTTConn(logger, cfg.MQTT)
	if err != nil {
		return nil, err
	}
	zb.mqtt = mc
	zb.net = newNetworkConn(logger, svc, zb, mc, cfg)

	return zb, nil
}

// Bridge returns the static Bridge descriptor for this process.
func (zb *ZigbeeBridge) Bridge() *api2.Bridge {
	return zb.b
}

// Start connects to the MQTT broker; the initial connection attempt is synchronous, but every
// reconnect after that - and the discovery pass a fresh retained bridge/devices delivery triggers
// - happens in the background via the client library's own auto-reconnect.
func (zb *ZigbeeBridge) Start(ctx context.Context) error {
	return zb.mqtt.connect(ctx)
}

// registerDevice records which networkConn owns a device so ProcessCommand can route to it.
// Called by networkConn once it has built a device. This bridge only ever has one networkConn -
// the indirection mirrors bridges/zwave/bridge.go's registerDevice.
func (zb *ZigbeeBridge) registerDevice(id string, nc *networkConn) {
	zb.mu.Lock()
	defer zb.mu.Unlock()
	zb.deviceOwner[id] = nc
}

// unregisterDevice is registerDevice's mirror, called by networkConn when a previously-built
// device no longer appears in a bridge/devices discovery pass.
func (zb *ZigbeeBridge) unregisterDevice(id string) {
	zb.mu.Lock()
	defer zb.mu.Unlock()
	delete(zb.deviceOwner, id)
}

// ProcessCommand takes a given command request and attempts to execute it.
func (zb *ZigbeeBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	zb.mu.Lock()
	nc, ok := zb.deviceOwner[cmd.DeviceId]
	zb.mu.Unlock()

	if !ok {
		zb.logger.Error("received command for unknown device id", zap.String("device_id", cmd.DeviceId))
		return nil, bridge.ErrDeviceNotFound
	}

	return nc.applyCommand(ctx, cmd)
}

// ProcessCommandAsync is present to conform to the bridge.Handler interface. This bridge has no
// device traits eligible for asynchronous commands, so it always returns
// ErrAsyncCommandsNotSupported.
func (zb *ZigbeeBridge) ProcessCommandAsync(ctx context.Context, cmd *command.Command) error {
	return bridge.ErrAsyncCommandsNotSupported
}

// Refresh is present to conform to the bridge.Handler interface. Zigbee state arrives via the
// standing MQTT subscription, so there's no polling loop needed here.
func (zb *ZigbeeBridge) Refresh(ctx context.Context) error {
	return nil
}

// SetBridgeConfig takes the supplied config params and saves them for future reference.
func (zb *ZigbeeBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	zb.b.Config.Name = config.Name
	zb.b.Config.Description = config.Description

	viper.Set("bridge.name", config.Name)
	viper.Set("bridge.description", config.Description)
	return viper.WriteConfig()
}
