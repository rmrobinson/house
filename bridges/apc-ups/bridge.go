package main

import (
	"context"
	"time"

	"github.com/mdlayher/apcupsd"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
)

func statusToDevice(s *apcupsd.Status) *device.Device {
	return &device.Device{
		Id:           s.SerialNumber,
		ModelId:      s.Model,
		Manufacturer: "APC",
		ModelName:    &s.Model,
		LastSeen:     timestamppb.New(s.EndAPC),
		Details: &device.Device_Ups{
			Ups: &device.UPS{
				OnOff: &trait.OnOff{
					Attributes: &trait.OnOff_Attributes{
						CanControl: false,
					},
					State: &trait.OnOff_State{
						IsOn: true,
					},
				},
				Battery: &trait.Battery{
					State: &trait.Battery_State{
						Discharging:           s.Status == "ONBATT",
						Status:                s.Status,
						CapacityRemainingPct:  100 - int32(s.BatteryChargePercent),
						CapacityRemainingMins: int32(s.TimeLeft.Minutes()),
					},
				},
				Power: &trait.Power{
					Attributes: &trait.Power_Attributes{},
					State: &trait.Power_State{
						VoltageV: s.LineVoltage,
					},
				},
			},
		},
	}
}

// APCUPSBridge is a bridge to an APC UPS status daemon
type APCUPSBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	client *apcupsd.Client
}

// NewAPCUPSBridge creates a new bridge to the specified APC UPS daemon
func NewAPCUPSBridge(logger *zap.Logger, svc *bridge.Service, client *apcupsd.Client, upsIPAddr string, upsPort int) *APCUPSBridge {
	b := &api2.Bridge{
		Id:           viper.GetString("bridge.id"),
		IsReachable:  true,
		ModelId:      "APCUPS11",
		Manufacturer: "Faltung Networks",
		Config: &api2.Bridge_Config{
			Name:        viper.GetString("bridge.name"),
			Description: viper.GetString("bridge.description"),
			Address: &api2.Address{
				Ip: &api2.Address_Ip{
					Host: upsIPAddr,
					Port: int32(upsPort),
				},
			},
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}

	aub := &APCUPSBridge{
		logger: logger,
		svc:    svc,
		b:      b,
		client: client,
	}

	return aub
}

// ProcessCommand takes a given command request and attempts to execute it.
// We only worry about processing valid commands for the given device traits.
func (aub *APCUPSBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	aub.logger.Error("received unsupported command - shouldn't happen")
	return nil, bridge.ErrUnsupportedCommand
}

// SetBridgeConfig takes the supplied config params and saves them for future reference.
func (aub *APCUPSBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	aub.b.Config.Name = config.Name
	aub.b.Config.Description = config.Description

	viper.Set("bridge.name", config.Name)
	viper.Set("bridge.description", config.Description)
	viper.WriteConfig()

	return nil
}

// Refresh is present to conform to the bridge.Handler interface. In this implementation it queries
// the UPS API and returns the current state of the UPS.
func (aub *APCUPSBridge) Refresh(ctx context.Context) error {
	s, err := aub.client.Status()
	if err != nil {
		aub.logger.Error("unable to get status from ups",
			zap.Error(err))
		return status.Error(codes.Internal, "unable to get status from ups")
	}

	aub.svc.UpdateDevice(statusToDevice(s))
	return nil
}

// Run begins the process of polling the sensor and reporting back the state.
func (aub *APCUPSBridge) Run(ctx context.Context) {
	aub.Refresh(ctx)

	refreshTimer := time.NewTicker(time.Second * time.Duration(viper.GetInt("bridge.refresh_interval")))
	aub.logger.Info("beginning refresh loop", zap.Int("refresh_interval", viper.GetInt("bridge.refresh_interval")))
	for {
		select {
		case <-refreshTimer.C:
			if err := aub.Refresh(ctx); err != nil {
				aub.logger.Error("unable to refresh sensors",
					zap.Error(err))
				continue
			}
			aub.logger.Debug("refreshed")
		case <-ctx.Done():
			aub.logger.Info("run context cancelled")
			return
		}
	}
}
