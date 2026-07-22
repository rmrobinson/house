package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
	"tinygo.org/x/bluetooth"

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
		Address: &device.Device_Address{
			Address:     s.Address(),
			IsReachable: true,
		},
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

// AirthingsBridge acts as the handler for Bridge requests for the Airthings sensors registered.
type AirthingsBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	scanner     *airthings.Scanner
	refreshLock sync.Mutex
	ids         []int
}

// NewAirthingsBridge creates a new bridge to a collection Airthings sensors
func NewAirthingsBridge(logger *zap.Logger, svc *bridge.Service, btAdapter *bluetooth.Adapter, sensorIDs []int) *AirthingsBridge {
	btMac, err := btAdapter.Address()
	if err != nil {
		logger.Fatal("unable to get address from bluetooth adapter", zap.Error(err))
	}

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
					Address: btMac.MAC.String(),
				},
			},
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}

	ab := &AirthingsBridge{
		logger:  logger,
		svc:     svc,
		scanner: airthings.NewScanner(btAdapter),
		ids:     sensorIDs,
		b:       b,
	}

	return ab
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
	ab.refreshLock.Lock()
	defer ab.refreshLock.Unlock()

	for _, id := range ab.ids {
		ab.logger.Debug("scanning for sensor", zap.Int("sensor_id", id))
		sensor, err := ab.scanner.FindSensor(ctx, id)
		if err != nil {
			ab.logger.Error("unable to find sensor",
				zap.Error(err))
			continue
		}
		if err = sensor.Refresh(); err != nil {
			ab.logger.Error("unable to refresh sensor",
				zap.Error(err))
			continue
		}
		sensor.Disconnect()

		ab.svc.UpdateDevice(sensorToDevice(sensor))
	}
	return nil
}

// Run begins the process of polling the sensor and reporting back the state.
func (ab *AirthingsBridge) Run(ctx context.Context) {
	ab.Refresh(ctx)

	refreshTimer := time.NewTicker(time.Second * time.Duration(viper.GetInt("bridge.refresh_interval")))
	ab.logger.Info("beginning refresh loop", zap.Int("refresh_interval", viper.GetInt("bridge.refresh_interval")))
	for {
		select {
		case <-refreshTimer.C:
			if err := ab.Refresh(ctx); err != nil {
				ab.logger.Error("unable to refresh sensors",
					zap.Error(err))
				continue
			}
			ab.logger.Debug("refreshed")
		case <-ctx.Done():
			ab.logger.Info("run context cancelled")
			return
		}
	}
}
