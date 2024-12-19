package main

import (
	"context"

	"go.uber.org/zap"

	"github.com/google/uuid"
	"github.com/spf13/viper"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
)

// ClockBridge acts as the handler for Bridge requests for this Clock.
type ClockBridge struct {
	logger *zap.Logger

	svc *bridge.Service
	c   *Clock

	b *api2.Bridge
	d *device.Device
}

// NewClockBridge creates a new bridge for the clock implementation
func NewClockBridge(logger *zap.Logger, svc *bridge.Service, c *Clock) *ClockBridge {
	// Get IDs and configured valeus from viper
	viper.SetConfigName("raspi-clock")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("/etc/house")
	viper.AddConfigPath("$HOME/house")
	viper.AddConfigPath(".")

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			deviceID := uuid.New().String()
			bridgeID := uuid.New().String()

			logger.Debug("config missing, saving new device and bridge ids", zap.String("bridge_id", bridgeID), zap.String("device_id", deviceID))

			viper.Set("bridge.id", bridgeID)
			viper.Set("device.id", deviceID)

			viper.WriteConfig()
		}
		logger.Fatal("unable to read viper config", zap.Error(err))
	}

	bridgeModelName := "Adafruit 1269"
	bridgeModelDescription := "4 digit 7 segment display"
	b := &api2.Bridge{
		Id:               viper.GetString("bridge.id"),
		IsReachable:      true,
		ModelId:          "RPiClk1",
		Manufacturer:     "Faltung Networks",
		ModelName:        &bridgeModelName,
		ModelDescription: &bridgeModelDescription,
		Config: &api2.Bridge_Config{
			Address: &api2.Address{
				I2C: &api2.Address_I2C{
					Address: i2cAddress,
				},
			},
			Timezone: c.timezone.String(),
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}

	deviceModelName := "Adafruit 7 Segment Display"
	deviceMode := trait.Time_TIME_FORMAT_24H
	if c.timeMode == TwelveHour {
		deviceMode = trait.Time_TIME_FORMAT_12H
	}

	d := &device.Device{
		Id:           viper.GetString("device.id"),
		ModelId:      "ADA-879",
		Manufacturer: "Adafruit",
		ModelName:    &deviceModelName,
		Details: &device.Device_Clock{
			Clock: &device.Clock{
				OnOff: &trait.OnOff{
					Attributes: &trait.OnOff_Attributes{
						CanControl: true,
					},
					State: &trait.OnOff_State{
						IsOn: true,
					},
				},
				Brightness: &trait.Brightness{
					Attributes: &trait.Brightness_Attributes{
						CanControl: true,
					},
					State: &trait.Brightness_State{
						Level: 100,
					},
				},
				Time: &trait.Time{
					Attributes: &trait.Time_Attributes{
						CanControl:  true,
						SupportsNtp: true,
					},
					State: &trait.Time_State{
						SetTimezoneAutomatically: false,
						Timezone:                 c.timezone.String(),
						TimeFormat:               deviceMode,
					},
				},
			},
		},
	}

	cb := &ClockBridge{
		logger: logger,
		svc:    svc,
		c:      c,
		b:      b,
		d:      d,
	}

	svc.RegisterHandler(cb, cb.b)

	return cb
}

// ProcessCommand takes a given command request and attempts to execute it.
// We only worry about processing valid commands for the given device traits.
func (cb *ClockBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	// The clock supports the Brightness, OnOff and Time commands.
	if cmd.GetOnOff() != nil {
		cb.c.SetOnOff(cmd.GetOnOff().On)

		cb.d.GetClock().OnOff.State.IsOn = cmd.GetOnOff().On
	} else if cmd.GetBrightnessAbsolute() != nil {
		cb.c.SetBrightness(int(cmd.GetBrightnessAbsolute().BrightnessPercent))

		cb.d.GetClock().Brightness.State.Level = cmd.GetBrightnessAbsolute().BrightnessPercent
	} else if cmd.GetBrightnessRelative() != nil {
		cb.c.ChangeBrightness(int(cmd.GetBrightnessRelative().ChangePercent))

		cb.d.GetClock().Brightness.State.Level += cmd.GetBrightnessRelative().ChangePercent
	} else if cmd.GetTime() != nil {
		if cmd.GetTime().GetFormat() == trait.Time_TIME_FORMAT_12H {
			cb.c.SetTimeMode(TwelveHour)

			cb.d.GetClock().Time.State.TimeFormat = trait.Time_TIME_FORMAT_12H
		} else if cmd.GetTime().GetFormat() == trait.Time_TIME_FORMAT_24H {
			cb.c.SetTimeMode(TwentyFourHour)

			cb.d.GetClock().Time.State.TimeFormat = trait.Time_TIME_FORMAT_24H
		}

		if cmd.GetTime().GetTimezone() != "" {
			if err := cb.c.SetTimeZone(cmd.GetTime().GetTimezone()); err != nil {
				cb.logger.Error("invalid timezone specified", zap.String("timezone", cmd.GetTime().GetTimezone()))
				return nil, bridge.ErrInvalidTimezone
			}

			cb.d.GetClock().Time.State.Timezone = cmd.GetTime().GetTimezone()
		}
	} else {
		cb.logger.Error("received unsupported command - shouldn't happen")
		return nil, bridge.ErrUnsupportedCommand
	}

	return cb.d, nil
}

// SetBridgeConfig takes the supplied config params and saves them for future reference.
func (cb *ClockBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	cb.b.Config.Name = config.Name
	cb.b.Config.Description = config.Description

	viper.Set("bridge.name", config.Name)
	viper.Set("bridge.description", config.Description)
	viper.WriteConfig()

	return nil
}

// Refresh is present to conform to the bridge.Handler interface. In this implementation it does nothing
// since there isn't 'remote' state which needs to be refreshed.
func (cb *ClockBridge) Refresh(ctx context.Context) error {
	return nil
}
