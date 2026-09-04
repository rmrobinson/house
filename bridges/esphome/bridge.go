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

// nodeConfig describes a single ESPHome node and the house devices built from its entities.
type nodeConfig struct {
	MAC      string         `mapstructure:"mac"`
	Address  string         `mapstructure:"address"`
	NoisePSK string         `mapstructure:"noise_psk"`
	Devices  []deviceConfig `mapstructure:"devices"`
}

// deviceConfig describes a single house device claimed from a subset of its node's entities.
// Roles maps a builder-defined role name (e.g. "light") to the ESPHome entity object_id backing it.
type deviceConfig struct {
	ID         string            `mapstructure:"id"`
	Type       string            `mapstructure:"type"`
	Roles      map[string]string `mapstructure:"roles"`
	Timezone   string            `mapstructure:"timezone"`
	TimeFormat string            `mapstructure:"time_format"`
}

// EsphomeBridge is the bridge.Handler implementation for ESPHome nodes.
type EsphomeBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	mu          sync.Mutex
	nodes       []*nodeConn
	deviceOwner map[string]*nodeConn
}

// NewEsphomeBridge creates a bridge for the supplied set of ESPHome nodes.
// Node connections aren't established until Start is called.
func NewEsphomeBridge(logger *zap.Logger, svc *bridge.Service, nodeConfigs []nodeConfig) *EsphomeBridge {
	b := &api2.Bridge{
		Id:           viper.GetString("bridge.id"),
		IsReachable:  true,
		ModelId:      "ESPH1",
		Manufacturer: "Faltung Networks",
		Config: &api2.Bridge_Config{
			Name:        viper.GetString("bridge.name"),
			Description: viper.GetString("bridge.description"),
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}

	eb := &EsphomeBridge{
		logger:      logger,
		svc:         svc,
		b:           b,
		deviceOwner: make(map[string]*nodeConn),
	}

	for _, nc := range nodeConfigs {
		eb.nodes = append(eb.nodes, newNodeConn(logger, svc, eb, nc))
	}

	return eb
}

// Bridge returns the static Bridge descriptor for this process.
func (eb *EsphomeBridge) Bridge() *api2.Bridge {
	return eb.b
}

// Start connects to every configured node. Each node connects and reconnects independently in the
// background; Start returns once the connection goroutines have been launched, not once they're ready.
func (eb *EsphomeBridge) Start(ctx context.Context) {
	for _, nc := range eb.nodes {
		go nc.run(ctx)
	}
}

// registerDevice records which node owns a device so ProcessCommand can route to it.
// Called by a nodeConn once it has built a device from its matched entities.
func (eb *EsphomeBridge) registerDevice(id string, nc *nodeConn) {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	eb.deviceOwner[id] = nc
}

// ProcessCommand takes a given command request and attempts to execute it.
func (eb *EsphomeBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	eb.mu.Lock()
	nc, ok := eb.deviceOwner[cmd.DeviceId]
	eb.mu.Unlock()

	if !ok {
		eb.logger.Error("received command for unknown device id", zap.String("device_id", cmd.DeviceId))
		return nil, bridge.ErrDeviceNotFound
	}

	return nc.applyCommand(cmd)
}

// Refresh is present to conform to the bridge.Handler interface. ESPHome nodes push state changes
// over their SubscribeStates stream, so there's no polling loop needed here.
func (eb *EsphomeBridge) Refresh(ctx context.Context) error {
	return nil
}

// SetBridgeConfig takes the supplied config params and saves them for future reference.
func (eb *EsphomeBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	eb.b.Config.Name = config.Name
	eb.b.Config.Description = config.Description

	viper.Set("bridge.name", config.Name)
	viper.Set("bridge.description", config.Description)
	viper.WriteConfig()

	return nil
}
