package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/spf13/viper"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
)

type dev struct {
	id string

	isLight  bool
	isSensor bool

	// for d1
	isOn       bool
	brightness int

	// for d2
	luxLevel float32
}

func (d *dev) toDevice() *device.Device {
	modelName := "Example Device 1"
	ret := &device.Device{
		Id:           d.id,
		ModelId:      " EX1",
		Manufacturer: "Faltung Systems",
		ModelName:    &modelName,
	}

	if d.isLight {
		ret.Details = &device.Device_Light{
			Light: &device.Light{
				OnOff: &trait.OnOff{
					Attributes: &trait.OnOff_Attributes{
						CanControl: true,
					},
					State: &trait.OnOff_State{
						IsOn: d.isOn,
					},
				},
				Brightness: &trait.Brightness{
					Attributes: &trait.Brightness_Attributes{
						CanControl: true,
					},
					State: &trait.Brightness_State{
						Level: int32(d.brightness),
					},
				},
			},
		}
	} else if d.isSensor {
		ret.Details = &device.Device_Sensor{
			Sensor: &device.Sensor{
				// Advertises that GetHistory can be asked for samples; see
				// ExampleBridge.GetHistory.
				Metadata: &device.Sensor_Metadata{
					History: &device.Sensor_Metadata_History{
						MaxWindow:      durationpb.New(historyMaxWindow),
						SampleInterval: durationpb.New(historySampleInterval),
					},
				},
				LightLevel: &trait.LightLevel{
					Attributes: &trait.LightLevel_Attributes{
						CanControl: false,
					},
					State: &trait.LightLevel_State{
						Lux:        d.luxLevel,
						IsDaylight: true,
					},
				},
			},
		}
	}

	return ret
}

const (
	// historyMaxWindow and historySampleInterval describe the sensor's
	// pretend on-device log, advertised via Sensor.Metadata.history.
	historyMaxWindow      = 24 * time.Hour
	historySampleInterval = 5 * time.Minute
)

// GetHistory implements bridge.HistoryHandler for the sensor, demonstrating
// the optional history API: a device that logs samples itself lets the bridge
// be polled rarely and the gaps fetched afterwards. The log here is made up -
// a lux value derived from the wall-clock time of each 5 minute boundary - so
// it is stable across calls, and asking for an overlapping window returns the
// same samples again.
func (b *ExampleBridge) GetHistory(ctx context.Context, deviceID string, since, until time.Time) ([]*api2.HistorySample, error) {
	if deviceID != b.d2.id {
		return nil, bridge.ErrDeviceNotFound
	}

	// The device only remembers historyMaxWindow.
	oldest := until.Add(-historyMaxWindow)
	if since.Before(oldest) {
		since = oldest
	}

	var samples []*api2.HistorySample
	for t := since.Truncate(historySampleInterval).Add(historySampleInterval); !t.After(until); t = t.Add(historySampleInterval) {
		lux := float32(100 + (t.Unix()/int64(historySampleInterval.Seconds()))%50)
		samples = append(samples, &api2.HistorySample{
			Time: timestamppb.New(t),
			Snapshot: &device.Device{
				Id: b.d2.id,
				Details: &device.Device_Sensor{Sensor: &device.Sensor{
					LightLevel: &trait.LightLevel{State: &trait.LightLevel_State{Lux: lux}},
				}},
			},
		})
	}
	return samples, nil
}

// ExampleBridge contains an implementation of a bridge with 2 virtual devices.
type ExampleBridge struct {
	logger *zap.Logger
	svc    *bridge.Service

	b  *api2.Bridge
	d1 *dev
	d2 *dev
}

// NewExampleBridge creates a bridge with a hardcoded ID, and 2 hardcoded devices.
func NewExampleBridge(logger *zap.Logger, svc *bridge.Service) *ExampleBridge {
	bridgeModelName := "Example Bridge 1"
	bridgeModelDescription := "Example bridge implementation for testing"

	b := &api2.Bridge{
		Id:               viper.GetString("bridge.id"),
		IsReachable:      true,
		ModelId:          "EXB1",
		Manufacturer:     "Faltung Networks",
		ModelName:        &bridgeModelName,
		ModelDescription: &bridgeModelDescription,
		Config: &api2.Bridge_Config{
			Name:        viper.GetString("bridge.name"),
			Description: viper.GetString("bridge.description"),
			Timezone:    time.Local.String(),
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}

	return &ExampleBridge{
		logger: logger,
		svc:    svc,
		b:      b,
		d1: &dev{
			id:      "dev1",
			isLight: true,
		},
		d2: &dev{
			id:       "dev2",
			isSensor: true,
			luxLevel: 400.0,
		},
	}
}

// ProcessCommand takes a given command request and attempts to execute it.
// We only worry about processing valid commands for the given device traits.
func (b *ExampleBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	devID := cmd.DeviceId
	if devID == b.d1.id {
		// The example device 1 supports the Brightness and OnOff commands.
		if cmd.GetOnOff() != nil {
			b.d1.isOn = cmd.GetOnOff().On
		} else if cmd.GetBrightnessAbsolute() != nil {
			b.d1.brightness = int(cmd.GetBrightnessAbsolute().BrightnessPercent)
		} else if cmd.GetBrightnessRelative() != nil {
			b.d1.brightness += int(cmd.GetBrightnessRelative().ChangePercent)
		} else {
			b.logger.Error("received unsupported command - shouldn't happen")
			return nil, bridge.ErrUnsupportedCommand
		}
		return b.d1.toDevice(), nil
	} else if devID == b.d2.id {
		// The sensor has no supported commands
		b.logger.Error("received unsupported command - shouldn't happen")
		return nil, bridge.ErrUnsupportedCommand
	} else {
		b.logger.Error("received command for unknown device id", zap.String("device_id", devID))
		return nil, bridge.ErrDeviceNotFound
	}
}

// ProcessCommandAsync is present to conform to the bridge.Handler interface. This bridge has no
// device traits eligible for asynchronous commands, so it always returns ErrAsyncCommandsNotSupported.
func (b *ExampleBridge) ProcessCommandAsync(ctx context.Context, cmd *command.Command) error {
	return bridge.ErrAsyncCommandsNotSupported
}

// Refresh is present to conform to the bridge.Handler interface. In this implementation it does nothing
// since there isn't 'remote' state which needs to be refreshed.
func (b *ExampleBridge) Refresh(ctx context.Context) error {
	return nil
}

// SetBridgeConfig takes the supplied config params and saves them for future reference.
func (b *ExampleBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	b.b.Config.Name = config.Name
	b.b.Config.Description = config.Description

	viper.Set("bridge.name", config.Name)
	viper.Set("bridge.description", config.Description)
	viper.WriteConfig()

	return nil
}

// Run begins processing async updates - instead of interfacing with real-world devices
// instead the SIGUSR1 and SIGUSR2 signals are listened as triggers for state changes.
// This also starts a timer which updates the lux of the second device every 30 seconds.
func (b *ExampleBridge) Run(ctx context.Context) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGUSR1, syscall.SIGUSR2)

	devTimer := time.NewTicker(30 * time.Second)

	for {
		select {
		case sig := <-sigChan:
			b.logger.Debug("received signal", zap.String("value", sig.String()))
			if sig == syscall.SIGUSR1 {
				// Toggle the light on or off
				b.d1.isOn = !b.d1.isOn
				b.svc.UpdateDevice(b.d1.toDevice())
			} else if sig == syscall.SIGUSR2 {
				// Reset the lux level to a low starting value
				b.d2.luxLevel = 100
				b.svc.UpdateDevice(b.d2.toDevice())
			}
		case <-devTimer.C:
			b.d2.luxLevel++

			b.svc.UpdateDevice(b.d2.toDevice())
		case <-ctx.Done():
			b.logger.Info("context complete")
			return
		}
	}
}
