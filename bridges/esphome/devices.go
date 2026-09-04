package main

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	esphome "github.com/richard87/esphome-apiclient"
	"github.com/richard87/esphome-apiclient/pb"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
)

// deviceBuilder is implemented once per house device type this bridge supports. A device is
// dispatched to its builder by deviceConfig.Type, matching the config's `type:` string against
// the deviceBuilders registry below.
type deviceBuilder interface {
	// build constructs the initial house Device from the entities matched to a device's
	// configured roles (role name -> entity).
	build(cfg deviceConfig, entities map[string]esphome.Entity) (*device.Device, error)

	// applyState mutates the device in response to a state message received for one of its
	// roles. Whether anything actually changed is left to bridge.Service's own diffing.
	applyState(d *device.Device, role string, msg proto.Message)

	// applyCommand translates a house Command into an outbound ESPHome command and updates the
	// device optimistically. roleKeys maps this device's role names to their ESPHome entity keys.
	applyCommand(client *esphome.Client, roleKeys map[string]uint32, d *device.Device, cmd *command.Command) error
}

// deviceBuilders is the registry of device types this bridge knows how to build. Config `type:`
// values that aren't in this map are rejected at connect time with a logged error.
var deviceBuilders = map[string]deviceBuilder{
	"clock": clockBuilder{},
	"light": lightBuilder{},
}

// --- Light ---

// lightBuilder builds a house Light device from a single ESPHome light entity claiming the
// "light" role. Only OnOff and Brightness are mapped for now; colour/effects are a follow-on.
type lightBuilder struct{}

func (lightBuilder) build(cfg deviceConfig, entities map[string]esphome.Entity) (*device.Device, error) {
	le, err := requireLightEntity(entities)
	if err != nil {
		return nil, err
	}

	return &device.Device{
		Manufacturer: "ESPHome",
		Details: &device.Device_Light{
			Light: &device.Light{
				OnOff: &trait.OnOff{
					Attributes: &trait.OnOff_Attributes{CanControl: true},
					State:      &trait.OnOff_State{IsOn: le.State},
				},
				Brightness: &trait.Brightness{
					Attributes: &trait.Brightness_Attributes{CanControl: true},
					State:      &trait.Brightness_State{Level: brightnessToPercent(le.Brightness)},
				},
			},
		},
	}, nil
}

func (lightBuilder) applyState(d *device.Device, role string, msg proto.Message) {
	if role != "light" {
		return
	}
	m, ok := msg.(*pb.LightStateResponse)
	if !ok {
		return
	}
	l := d.GetLight()
	l.OnOff.State.IsOn = m.State
	l.Brightness.State.Level = brightnessToPercent(m.Brightness)
}

func (lightBuilder) applyCommand(client *esphome.Client, roleKeys map[string]uint32, d *device.Device, cmd *command.Command) error {
	key, ok := roleKeys["light"]
	if !ok {
		return bridge.ErrUnsupportedCommand
	}
	l := d.GetLight()

	switch {
	case cmd.GetOnOff() != nil:
		on := cmd.GetOnOff().On
		if err := client.SetLight(key, esphome.LightCommandOpts{HasState: true, State: on}); err != nil {
			return err
		}
		l.OnOff.State.IsOn = on

	case cmd.GetBrightnessAbsolute() != nil:
		pct := cmd.GetBrightnessAbsolute().BrightnessPercent
		if err := client.SetLight(key, esphome.LightCommandOpts{HasBrightness: true, Brightness: percentToBrightness(pct)}); err != nil {
			return err
		}
		l.Brightness.State.Level = pct

	case cmd.GetBrightnessRelative() != nil:
		pct := clampPercent(l.Brightness.State.Level + cmd.GetBrightnessRelative().ChangePercent)
		if err := client.SetLight(key, esphome.LightCommandOpts{HasBrightness: true, Brightness: percentToBrightness(pct)}); err != nil {
			return err
		}
		l.Brightness.State.Level = pct

	default:
		return bridge.ErrUnsupportedCommand
	}

	return nil
}

// --- Clock ---

// clockBuilder builds a house Clock device from a single ESPHome light entity (claiming the
// "light" role) driving the clock's OnOff/Brightness, plus a bridge-tracked Time trait.
// ESPHome's native API has no message for pushing a display time-format live to a node — the
// node only ever asks the bridge for the current time (GetTimeRequest, handled in node.go).
// Time.State here is therefore bridge-side bookkeeping, updated by command but never sent
// on the wire beyond what GetTimeResponse already carries (UTC + timezone).
type clockBuilder struct{}

func (clockBuilder) build(cfg deviceConfig, entities map[string]esphome.Entity) (*device.Device, error) {
	le, err := requireLightEntity(entities)
	if err != nil {
		return nil, err
	}

	tz := cfg.Timezone
	if tz == "" {
		tz = time.Local.String()
	}
	format := trait.Time_TIME_FORMAT_24H
	if cfg.TimeFormat == "12h" {
		format = trait.Time_TIME_FORMAT_12H
	}

	now := time.Now().UTC()

	return &device.Device{
		Manufacturer: "ESPHome",
		Details: &device.Device_Clock{
			Clock: &device.Clock{
				OnOff: &trait.OnOff{
					Attributes: &trait.OnOff_Attributes{CanControl: true},
					State:      &trait.OnOff_State{IsOn: le.State},
				},
				Brightness: &trait.Brightness{
					Attributes: &trait.Brightness_Attributes{CanControl: true},
					State:      &trait.Brightness_State{Level: brightnessToPercent(le.Brightness)},
				},
				Time: &trait.Time{
					Attributes: &trait.Time_Attributes{CanControl: true, SupportsNtp: false},
					State: &trait.Time_State{
						Timezone:   tz,
						TimeFormat: format,
						Utc:        timestamppb.New(now),
						LocalTime:  now.Format(time.RFC3339),
					},
				},
			},
		},
	}, nil
}

func (clockBuilder) applyState(d *device.Device, role string, msg proto.Message) {
	if role != "light" {
		return
	}
	m, ok := msg.(*pb.LightStateResponse)
	if !ok {
		return
	}
	c := d.GetClock()
	c.OnOff.State.IsOn = m.State
	c.Brightness.State.Level = brightnessToPercent(m.Brightness)
}

func (clockBuilder) applyCommand(client *esphome.Client, roleKeys map[string]uint32, d *device.Device, cmd *command.Command) error {
	key, ok := roleKeys["light"]
	if !ok {
		return bridge.ErrUnsupportedCommand
	}
	c := d.GetClock()

	switch {
	case cmd.GetOnOff() != nil:
		on := cmd.GetOnOff().On
		if err := client.SetLight(key, esphome.LightCommandOpts{HasState: true, State: on}); err != nil {
			return err
		}
		c.OnOff.State.IsOn = on

	case cmd.GetBrightnessAbsolute() != nil:
		pct := cmd.GetBrightnessAbsolute().BrightnessPercent
		if err := client.SetLight(key, esphome.LightCommandOpts{HasBrightness: true, Brightness: percentToBrightness(pct)}); err != nil {
			return err
		}
		c.Brightness.State.Level = pct

	case cmd.GetBrightnessRelative() != nil:
		pct := clampPercent(c.Brightness.State.Level + cmd.GetBrightnessRelative().ChangePercent)
		if err := client.SetLight(key, esphome.LightCommandOpts{HasBrightness: true, Brightness: percentToBrightness(pct)}); err != nil {
			return err
		}
		c.Brightness.State.Level = pct

	case cmd.GetTime() != nil:
		if tz := cmd.GetTime().GetTimezone(); tz != "" {
			if _, err := time.LoadLocation(tz); err != nil {
				return bridge.ErrInvalidTimezone
			}
			c.Time.State.Timezone = tz
		}
		if cmd.GetTime().Format != nil {
			c.Time.State.TimeFormat = cmd.GetTime().GetFormat()
		}

	default:
		return bridge.ErrUnsupportedCommand
	}

	return nil
}

// --- shared helpers ---

func requireLightEntity(entities map[string]esphome.Entity) (*esphome.LightEntity, error) {
	ent, ok := entities["light"]
	if !ok {
		return nil, fmt.Errorf("missing required role %q", "light")
	}
	le, ok := ent.(*esphome.LightEntity)
	if !ok {
		return nil, fmt.Errorf("role %q must be a light entity, got %T", "light", ent)
	}
	return le, nil
}

// brightnessToPercent converts ESPHome's 0.0-1.0 brightness to house's 0-100 percent scale.
func brightnessToPercent(b float32) int32 {
	return int32(b*100 + 0.5)
}

// percentToBrightness converts house's 0-100 percent scale to ESPHome's 0.0-1.0 brightness.
func percentToBrightness(pct int32) float32 {
	return float32(pct) / 100
}

func clampPercent(pct int32) int32 {
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}
