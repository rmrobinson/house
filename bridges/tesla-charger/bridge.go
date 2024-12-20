package main

import (
	"context"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/spf13/viper"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
)

// ChargerBridge acts as the handler for Bridge requests for this charger.
type ChargerBridge struct {
	logger *zap.Logger
	svc    *bridge.Service

	charger *Charger
	b       *api2.Bridge
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

// Refresh is present to conform to the bridge.Handler interface. In this implementation it queries
// the charger API and returns the current state of the charger.
func (cb *ChargerBridge) Refresh(ctx context.Context) error {
	chargerState, err := cb.charger.State()
	if err != nil {
		cb.logger.Error("unable to get charger state",
			zap.Error(err))
		return status.Error(codes.Internal, "unable to refresh charger state")
	}

	cb.svc.UpdateDevice(chargerState.toDevice())
	return nil
}

// Run begins the process of polling the charger API and reporting back the state.
func (cb *ChargerBridge) Run() {
	refreshTimer := time.NewTicker(time.Minute * 5)
	for {
		select {
		case <-refreshTimer.C:
			chargerState, err := cb.charger.State()
			if err != nil {
				cb.logger.Error("unable to get charger state",
					zap.Error(err))
				continue
			}

			cb.svc.UpdateDevice(chargerState.toDevice())
		}
	}
}
