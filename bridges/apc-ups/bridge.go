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

type APCUPSBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	client *apcupsd.Client
}

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
	return &APCUPSBridge{
		logger: logger,
		svc:    svc,
		b:      b,
		client: client,
	}
}

// ProcessCommand takes a given command request and attempts to execute it.
// We only worry about processing valid commands for the given device traits.
func (ab *APCUPSBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	ab.logger.Error("received unsupported command - shouldn't happen")
	return nil, bridge.ErrUnsupportedCommand
}

// SetBridgeConfig takes the supplied config params and saves them for future reference.
func (ab *APCUPSBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	ab.b.Config.Name = config.Name
	ab.b.Config.Description = config.Description

	viper.Set("bridge.name", config.Name)
	viper.Set("bridge.description", config.Description)
	viper.WriteConfig()

	return nil
}

// Refresh is present to conform to the bridge.Handler interface. In this implementation it queries
// the charger API and returns the current state of the charger.
func (ab *APCUPSBridge) Refresh(ctx context.Context) error {
	s, err := ab.client.Status()
	if err != nil {
		ab.logger.Error("unable to get status from ups",
			zap.Error(err))
		return status.Error(codes.Internal, "unable to get status from ups")
	}

	ab.svc.UpdateDevice(statusToDevice(s))
	return nil
}

// Run begins the process of polling the sensor and reporting back the state.
func (ab *APCUPSBridge) Run() {
	refreshTimer := time.NewTicker(time.Minute * 5)
	for {
		select {
		case <-refreshTimer.C:
			status, err := ab.client.Status()
			if err != nil {
				ab.logger.Error("unable to get status from ups",
					zap.Error(err))
				continue
			}

			ab.svc.UpdateDevice(statusToDevice(status))
		}
	}
}
