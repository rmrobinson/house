package main

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/spf13/viper"

	airthings "github.com/rmrobinson/airthings-btle"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
)

func sensorToDevice(s *airthings.Sensor) *device.Device {
	modelName := "Wave Plus"
	vocLevel := int32(s.VOCLevel)
	co2Level := int32(s.CO2Level)
	radonLTAvg := int32(s.RadonLongTermAvg)
	return &device.Device{
		Id:           fmt.Sprintf("%d", s.SerialNumber),
		ModelId:      "Wave Plus",
		Manufacturer: "Airthings",
		ModelName:    &modelName,
		LastSeen:     timestamppb.Now(),
		Details: &device.Device_Sensor{
			Sensor: &device.Sensor{
				AirQuality: &trait.AirQuality{
					State: &trait.AirQuality_State{
						VolatileOrganicCompoundsPpb: &vocLevel,
						Co2Ppm:                      &co2Level,
						RadonBqM3:                   &radonLTAvg,
					},
				},
				AirProperties: &trait.AirProperties{
					State: &trait.AirProperties_State{
						TemperatureC:       s.Temperature,
						PressureHpa:        s.RelativeAtmosphericPressure,
						HumidityPercentage: s.Humidity,
					},
				},
			},
		},
	}
}

// AirthingsBridge acts as the handler for Bridge requests for the Airthings sensor.
type AirthingsBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	s *airthings.Sensor
}

// NewAirthingsBridge creates a new bridge to an Airthings sensor
func NewAirthingsBridge(logger *zap.Logger, svc *bridge.Service, s *airthings.Sensor) *AirthingsBridge {
	b := &api2.Bridge{
		Id:           viper.GetString("bridge.id"),
		IsReachable:  true,
		ModelId:      "ATS1",
		Manufacturer: "Faltung Networks",
		Config: &api2.Bridge_Config{
			Name:        viper.GetString("bridge.name"),
			Description: viper.GetString("bridge.description"),
			Address: &api2.Address{
				Bluetooth: &api2.Address_Bluetooth{
					Address: s.Address(),
				},
			},
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}

	cb := &AirthingsBridge{
		logger: logger,
		svc:    svc,
		s:      s,
		b:      b,
	}

	return cb
}

// ProcessCommand takes a given command request and attempts to execute it.
// We only worry about processing valid commands for the given device traits.
func (ab *AirthingsBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	ab.logger.Error("received unsupported command - shouldn't happen")
	return nil, bridge.ErrUnsupportedCommand
}

// SetBridgeConfig takes the supplied config params and saves them for future reference.
func (ab *AirthingsBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	ab.b.Config.Name = config.Name
	ab.b.Config.Description = config.Description

	viper.Set("bridge.name", config.Name)
	viper.Set("bridge.description", config.Description)
	viper.WriteConfig()

	return nil
}

// Refresh is present to conform to the bridge.Handler interface. In this implementation it queries
// the charger API and returns the current state of the charger.
func (ab *AirthingsBridge) Refresh(ctx context.Context) error {
	err := ab.s.Refresh()
	if err != nil {
		ab.logger.Error("unable to refresh sensor",
			zap.Error(err))
		return status.Error(codes.Internal, "unable to refresh sensor state")
	}

	ab.svc.UpdateDevice(sensorToDevice(ab.s))
	return nil
}

// Run begins the process of polling the sensor and reporting back the state.
func (ab *AirthingsBridge) Run() {
	refreshTimer := time.NewTicker(time.Minute * 5)
	for {
		select {
		case <-refreshTimer.C:
			err := ab.s.Refresh()
			if err != nil {
				ab.logger.Error("unable to refresh sensor",
					zap.Error(err))
				continue
			}

			ab.svc.UpdateDevice(sensorToDevice(ab.s))
		}
	}
}
