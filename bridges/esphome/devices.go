package main

import (
	"fmt"
	"math"
	"slices"
	"sort"
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
	"clock":         clockBuilder{},
	"light":         lightBuilder{},
	"fan":           fanBuilder{},
	"standing_desk": standingDeskBuilder{},
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

// --- Fan ---

// fanToggleRoles are the Toggle-trait role names fanBuilder recognizes. A configured fan only
// needs to supply roles/entities for the toggles it actually has; Toggle.Attributes reflects
// exactly the subset present in config, not this full list.
var fanToggleRoles = []string{"oscillation", "beep", "display"}

// fanTimerLabels pairs each of the fan's fixed Tuya timer select options with its index; the
// duration in seconds is the index times one hour ("Cancel" = 0h = 0s, up to "12h" = 43200s).
// This is a fixed 13-value set specific to this device's enum-datapoint timer, not something a
// generic trait.Timer consumer should need to know about — kept local to fanBuilder.
var fanTimerLabels = []string{
	"Cancel", "1h", "2h", "3h", "4h", "5h", "6h", "7h", "8h", "9h", "10h", "11h", "12h",
}

func fanTimerDurations() []int32 {
	durations := make([]int32, len(fanTimerLabels))
	for i := range fanTimerLabels {
		durations[i] = int32(i) * 3600
	}
	return durations
}

// durationToTimerLabel converts a commanded duration in seconds to the select option that
// represents it. Returns false for any value not in the fixed set rather than rounding.
func durationToTimerLabel(seconds int32) (string, bool) {
	if seconds < 0 || seconds%3600 != 0 {
		return "", false
	}
	idx := seconds / 3600
	if int(idx) >= len(fanTimerLabels) {
		return "", false
	}
	return fanTimerLabels[idx], true
}

// timerLabelToDuration converts the timer select's current state string to seconds. An
// unrecognized label (including the entity's not-yet-received zero value) is treated as no
// timer running, matching remaining_seconds' documented "0 means not running" convention.
func timerLabelToDuration(label string) int32 {
	for i, l := range fanTimerLabels {
		if l == label {
			return int32(i) * 3600
		}
	}
	return 0
}

// fanBuilder builds a house Fan device from a native ESPHome fan entity (on/off + speed), a
// mode select, a fixed-option timer select, and a set of independently-configurable switches
// exposed via the Toggle trait (oscillation/beep/display), plus an optional temperature sensor.
type fanBuilder struct{}

func (fanBuilder) build(cfg deviceConfig, entities map[string]esphome.Entity) (*device.Device, error) {
	fe, ok := entities["fan"].(*esphome.FanEntity)
	if !ok {
		return nil, fmt.Errorf("missing required role %q", "fan")
	}
	modeEnt, ok := entities["mode"].(*esphome.SelectEntity)
	if !ok {
		return nil, fmt.Errorf("missing required role %q", "mode")
	}
	timerEnt, ok := entities["timer"].(*esphome.SelectEntity)
	if !ok {
		return nil, fmt.Errorf("missing required role %q", "timer")
	}

	maxSpeed := fe.SupportedSpeedCount
	if maxSpeed < 1 {
		maxSpeed = 1
	}

	fan := &device.Fan{
		OnOff: &trait.OnOff{
			Attributes: &trait.OnOff_Attributes{CanControl: true},
			State:      &trait.OnOff_State{IsOn: fe.State},
		},
		Speed: &trait.Speed{
			Attributes: &trait.Speed_Attributes{CanControl: true, MinimumSpeed: 1, MaximumSpeed: maxSpeed, SpeedIncrement: 1},
			State:      &trait.Speed_State{CurrentSpeed: fe.SpeedLevel},
		},
		Mode: &trait.Mode{
			Attributes: &trait.Mode_Attributes{CanControl: true, AvailableModes: modeEnt.Options},
			State:      &trait.Mode_State{CurrentMode: modeEnt.State},
		},
		Timer: &trait.Timer{
			Attributes: &trait.Timer_Attributes{CanControl: true, AvailableDurations: fanTimerDurations()},
			State:      &trait.Timer_State{RemainingSeconds: timerLabelToDuration(timerEnt.State)},
		},
	}

	var toggles []string
	settings := make(map[string]bool)
	for _, role := range fanToggleRoles {
		se, ok := entities[role].(*esphome.SwitchEntity)
		if !ok {
			continue
		}
		toggles = append(toggles, role)
		settings[role] = se.State
	}
	fan.Toggles = &trait.Toggle{
		Attributes: &trait.Toggle_Attributes{AvailableToggles: toggles},
		State:      &trait.Toggle_State{Settings: settings},
	}

	if te, ok := entities["temperature"].(*esphome.SensorEntity); ok {
		fan.Temperature = &trait.Temperature{
			Attributes: &trait.Temperature_Attributes{Unit: "celsius"},
			State:      &trait.Temperature_State{Value: te.State},
		}
	}

	return &device.Device{
		Manufacturer: "ESPHome",
		Details:      &device.Device_Fan{Fan: fan},
	}, nil
}

func (fanBuilder) applyState(d *device.Device, role string, msg proto.Message) {
	f := d.GetFan()

	switch role {
	case "fan":
		m, ok := msg.(*pb.FanStateResponse)
		if !ok {
			return
		}
		f.OnOff.State.IsOn = m.State
		f.Speed.State.CurrentSpeed = m.SpeedLevel

	case "mode":
		m, ok := msg.(*pb.SelectStateResponse)
		if !ok {
			return
		}
		f.Mode.State.CurrentMode = m.State

	case "timer":
		m, ok := msg.(*pb.SelectStateResponse)
		if !ok {
			return
		}
		f.Timer.State.RemainingSeconds = timerLabelToDuration(m.State)

	case "oscillation", "beep", "display":
		m, ok := msg.(*pb.SwitchStateResponse)
		if !ok {
			return
		}
		f.Toggles.State.Settings[role] = m.State

	case "temperature":
		m, ok := msg.(*pb.SensorStateResponse)
		if !ok || f.Temperature == nil {
			return
		}
		f.Temperature.State.Value = m.State
	}
}

func (fanBuilder) applyCommand(client *esphome.Client, roleKeys map[string]uint32, d *device.Device, cmd *command.Command) error {
	f := d.GetFan()

	switch {
	case cmd.GetOnOff() != nil:
		key, ok := roleKeys["fan"]
		if !ok {
			return bridge.ErrUnsupportedCommand
		}
		on := cmd.GetOnOff().On
		if err := client.SetFan(key, esphome.FanCommandOpts{HasState: true, State: on}); err != nil {
			return err
		}
		f.OnOff.State.IsOn = on

	case cmd.GetSpeed() != nil:
		key, ok := roleKeys["fan"]
		if !ok {
			return bridge.ErrUnsupportedCommand
		}
		level := clampSpeed(cmd.GetSpeed().Value, f.Speed.Attributes.MinimumSpeed, f.Speed.Attributes.MaximumSpeed)
		if err := client.SetFan(key, esphome.FanCommandOpts{HasSpeedLevel: true, SpeedLevel: level}); err != nil {
			return err
		}
		f.Speed.State.CurrentSpeed = level

	case cmd.GetMode() != nil:
		key, ok := roleKeys["mode"]
		value := cmd.GetMode().Value
		if !ok || !slices.Contains(f.Mode.Attributes.AvailableModes, value) {
			return bridge.ErrUnsupportedCommand
		}
		if err := client.SetSelect(key, value); err != nil {
			return err
		}
		f.Mode.State.CurrentMode = value

	case cmd.GetTimer() != nil:
		key, ok := roleKeys["timer"]
		if !ok {
			return bridge.ErrUnsupportedCommand
		}
		label, ok := durationToTimerLabel(cmd.GetTimer().DurationSeconds)
		if !ok {
			return bridge.ErrUnsupportedCommand
		}
		if err := client.SetSelect(key, label); err != nil {
			return err
		}
		f.Timer.State.RemainingSeconds = cmd.GetTimer().DurationSeconds

	case cmd.GetToggle() != nil:
		for name, val := range cmd.GetToggle().Settings {
			key, ok := roleKeys[name]
			if !ok {
				return bridge.ErrUnsupportedCommand
			}
			if err := client.SetSwitch(key, val); err != nil {
				return err
			}
			f.Toggles.State.Settings[name] = val
		}

	default:
		return bridge.ErrUnsupportedCommand
	}

	return nil
}

func clampSpeed(v, min, max int32) int32 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// --- Standing Desk ---

// standingDeskFixedRoles are the roles standingDeskBuilder claims independent of the presets a
// given desk is configured with. Any other configured role is treated as a preset: its name
// becomes an entry in Mode.Attributes.available_modes, and its value is the object_id of the
// button that triggers it — the same role-name-doubles-as-key pattern fanBuilder uses for Toggle.
var standingDeskFixedRoles = map[string]bool{"height": true, "move_up": true, "move_down": true}

// standingDeskBuilder builds a house StandingDesk device from a height sensor, two move
// switches, and a set of preset buttons. Movement is deliberately left unwired: the desk this
// was built against has a known crash bug when driven via its Move Up/Down switches, so this
// builder only exposes read-only Position telemetry and preset Mode commands until that's
// resolved — see the ApplyCommand Movement case and StandingDesk's proto doc comment.
type standingDeskBuilder struct{}

func (standingDeskBuilder) build(cfg deviceConfig, entities map[string]esphome.Entity) (*device.Device, error) {
	se, ok := entities["height"].(*esphome.SensorEntity)
	if !ok {
		return nil, fmt.Errorf("missing required role %q", "height")
	}
	if _, ok := entities["move_up"].(*esphome.SwitchEntity); !ok {
		return nil, fmt.Errorf("missing required role %q", "move_up")
	}
	if _, ok := entities["move_down"].(*esphome.SwitchEntity); !ok {
		return nil, fmt.Errorf("missing required role %q", "move_down")
	}
	if cfg.PositionMin == nil || cfg.PositionMax == nil {
		return nil, fmt.Errorf("standing_desk device %q requires position_min and position_max", cfg.ID)
	}
	min, max := *cfg.PositionMin, *cfg.PositionMax

	var presets []string
	for role, ent := range entities {
		if standingDeskFixedRoles[role] {
			continue
		}
		if _, ok := ent.(*esphome.ButtonEntity); !ok {
			return nil, fmt.Errorf("preset role %q must be a button entity, got %T", role, ent)
		}
		presets = append(presets, role)
	}
	sort.Strings(presets)

	return &device.Device{
		Manufacturer: "ESPHome",
		Details: &device.Device_StandingDesk{
			StandingDesk: &device.StandingDesk{
				Position: &trait.Position{
					Attributes: &trait.Position_Attributes{Unit: "cm", MinValue: min, MaxValue: max, SupportsSet: false},
					State:      &trait.Position_State{Value: se.State, Percent: positionPercent(se.State, min, max)},
				},
				Mode: &trait.Mode{
					Attributes: &trait.Mode_Attributes{CanControl: true, AvailableModes: presets},
					State:      &trait.Mode_State{},
				},
			},
		},
	}, nil
}

func (standingDeskBuilder) applyState(d *device.Device, role string, msg proto.Message) {
	if role != "height" {
		return
	}
	m, ok := msg.(*pb.SensorStateResponse)
	if !ok {
		return
	}
	p := d.GetStandingDesk().Position
	p.State.Value = m.State
	p.State.Percent = positionPercent(m.State, p.Attributes.MinValue, p.Attributes.MaxValue)
}

func (standingDeskBuilder) applyCommand(client *esphome.Client, roleKeys map[string]uint32, d *device.Device, cmd *command.Command) error {
	sd := d.GetStandingDesk()

	switch {
	case cmd.GetMode() != nil:
		value := cmd.GetMode().Value
		key, ok := roleKeys[value]
		if !ok {
			return bridge.ErrUnsupportedCommand
		}
		if err := client.PressButton(key); err != nil {
			return err
		}
		sd.Mode.State.CurrentMode = value

	case cmd.GetPosition() != nil:
		// supports_set is always false for this desk: the only way to change height is via
		// Movement, which isn't wired yet (see the case below and the StandingDesk proto doc).
		return bridge.ErrUnsupportedCommand

	case cmd.GetMovement() != nil:
		// TODO: wire this up once the desk's movement-command crash bug is resolved (see
		// esphome-device-profiles-extension-plan.md). Desk Move Up/Down are switch entities with
		// on-device repeat-send/auto-cutoff safety timeouts, so ApplyCommand would be a thin
		// pass-through (DIRECTION_POSITIVE/NEGATIVE -> SetSwitch(move_up/move_down key, true),
		// DIRECTION_STOPPED -> SetSwitch(false) on whichever is on) once that's safe to exercise
		// against real hardware.
		return bridge.ErrUnsupportedCommand

	default:
		return bridge.ErrUnsupportedCommand
	}

	return nil
}

// positionPercent normalizes value into Position.State.percent's documented 0-100 range. A NaN
// value - which a not-yet-reported ESPHome sensor can produce (confirmed live: the standing
// desk's height sensor stays NaN until its control box starts actively streaming status frames,
// which nothing in this device profile currently triggers) would otherwise slip past the
// range checks below unclamped, since every comparison against NaN is false in IEEE 754.
func positionPercent(value, min, max float32) float32 {
	if max <= min || math.IsNaN(float64(value)) {
		return 0
	}
	pct := (value - min) / (max - min) * 100
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
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
