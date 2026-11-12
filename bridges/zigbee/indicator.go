package main

import (
	"context"
	"math"
	"slices"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
)

// Inovelli's LED bar colour is a 0-255 value in which 0-254 sweep the hue wheel and 255 is white.
const (
	indicatorHueMax      = 254
	indicatorColourWhite = 255
)

// Inovelli packs an effect's duration into one byte with a unit per range: 1-60 are seconds,
// 61-120 are 1-60 minutes, 121-254 are 1-134 hours, and 255 is until cleared.
const (
	indicatorDurationMinutesBase = 60
	indicatorDurationHoursBase   = 120
	indicatorDurationMaxHours    = 134
	indicatorDurationIndefinite  = 255
)

// findIndicatorExpose returns a device's top-level "led_effect" composite - zigbee2mqtt's
// whole-bar effect on an Inovelli VZM31-SN. The sibling "individual_led_effect" composite (one LED
// of the bar) is deliberately not modeled; see api/command/indicator.proto.
func findIndicatorExpose(exposes []expose) (expose, bool) {
	for _, e := range exposes {
		if e.Property == "led_effect" && e.Endpoint == "" && e.Type == "composite" {
			return e, true
		}
	}
	return expose{}, false
}

// newIndicatorTrait returns an Indicator trait listing the device's effects if it exposes a
// "led_effect" composite with an "effect" enum, else nil.
func newIndicatorTrait(exposes []expose) *trait.Indicator {
	e, ok := findIndicatorExpose(exposes)
	if !ok {
		return nil
	}
	effect, ok := findFeature(e.Features, "effect")
	if !ok || len(effect.Values) == 0 {
		return nil
	}
	return &trait.Indicator{
		Attributes: &trait.Indicator_Attributes{
			CanControl: true,
			Effects:    append([]string(nil), effect.Values...),
		},
		State: &trait.Indicator_State{},
	}
}

// applyIndicatorCommand plays an IndicatorEffect on the device's LED bar. The effect is not kept
// in house state: it ends on its own, and the device's state report echoing it back (which is what
// WriteState waits for) is not a lasting property of the device.
func applyIndicatorCommand(ctx context.Context, m *mqttConn, friendlyName string, l *device.Light, c *command.IndicatorEffect) error {
	if l.Indicator == nil {
		return bridge.ErrUnsupportedCommand
	}
	if !slices.Contains(l.Indicator.Attributes.Effects, c.Effect) {
		return bridge.ErrArgumentNotSupportedByDevice
	}

	color, err := indicatorColour(c)
	if err != nil {
		return err
	}
	level := int32(100)
	if c.LevelPercent != nil {
		level = clampPercent(c.GetLevelPercent())
	}

	return m.WriteState(ctx, friendlyName, map[string]any{
		"led_effect": map[string]any{
			"effect":   c.Effect,
			"color":    color,
			"level":    level,
			"duration": indicatorDuration(c.DurationSeconds),
		},
	})
}

// indicatorColour converts the command's hue (degrees) or white flag to the device's colour scale.
// An effect which shows no colour (it ends or clears the bar) may leave both unset; any other must
// say which colour it wants rather than silently get a default.
func indicatorColour(c *command.IndicatorEffect) (int32, error) {
	switch {
	case c.White:
		return indicatorColourWhite, nil
	case c.Hue != nil:
		hue := clampRange(c.GetHue(), 0, 360)
		return int32(math.Round(float64(hue) * indicatorHueMax / 360)), nil
	case c.Effect == "off" || c.Effect == "clear_effect":
		return 0, nil
	default:
		return 0, bridge.ErrArgumentNotSupportedByDevice
	}
}

// indicatorDuration encodes seconds into the device's one-byte duration (see the constants above).
// A value too long for the largest unit saturates at the longest representable duration; one that
// does not divide evenly into minutes or hours is rounded up, so an effect never ends early.
func indicatorDuration(seconds int32) int32 {
	switch {
	case seconds <= 0:
		return indicatorDurationIndefinite
	case seconds <= 60:
		return seconds
	case seconds <= 60*60:
		return indicatorDurationMinutesBase + ceilDiv(seconds, 60)
	default:
		hours := ceilDiv(seconds, 60*60)
		if hours > indicatorDurationMaxHours {
			hours = indicatorDurationMaxHours
		}
		return indicatorDurationHoursBase + hours
	}
}

func ceilDiv(a, b int32) int32 { return (a + b - 1) / b }
