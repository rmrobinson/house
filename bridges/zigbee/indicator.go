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
//
// Arguments are validated, not corrected: a hue, level or duration outside what the command
// documents is a caller bug, and quietly clamping it would show the wrong thing on the bar (or end
// an effect early) while reporting success.
func applyIndicatorCommand(ctx context.Context, m *mqttConn, friendlyName string, l *device.Light, c *command.IndicatorEffect) error {
	if l.GetIndicator() == nil {
		return bridge.ErrUnsupportedCommand
	}
	if !slices.Contains(l.GetIndicator().GetAttributes().GetEffects(), c.Effect) {
		return bridge.ErrArgumentNotSupportedByDevice
	}

	color, err := indicatorColour(c)
	if err != nil {
		return err
	}
	level := int32(100)
	if c.LevelPercent != nil {
		level = c.GetLevelPercent()
		if level < 0 || level > 100 {
			return bridge.ErrArgumentNotSupportedByDevice
		}
	}
	duration, err := indicatorDuration(c.DurationSeconds)
	if err != nil {
		return err
	}

	return m.WriteState(ctx, friendlyName, map[string]any{
		"led_effect": map[string]any{
			"effect":   c.Effect,
			"color":    color,
			"level":    level,
			"duration": duration,
		},
	})
}

// indicatorColour converts the command's hue (degrees) or white flag to the device's colour scale.
// An effect which shows no colour may leave both unset; any other must say which colour it wants
// rather than silently get a default. Which effects show no colour is Inovelli's own vocabulary
// ("off" and "clear_effect" end the effect on the bar) - this is the one place that is encoded.
func indicatorColour(c *command.IndicatorEffect) (int32, error) {
	switch {
	case c.White:
		return indicatorColourWhite, nil
	case c.Hue != nil:
		hue := c.GetHue()
		if hue < 0 || hue > 360 {
			return 0, bridge.ErrArgumentNotSupportedByDevice
		}
		return int32(math.Round(float64(hue) * indicatorHueMax / 360)), nil
	case c.Effect == "off" || c.Effect == "clear_effect":
		return 0, nil
	default:
		return 0, bridge.ErrArgumentNotSupportedByDevice
	}
}

// indicatorDuration encodes seconds into the device's one-byte duration (see the constants above).
// Zero means until cleared. A value that does not divide evenly into minutes or hours is rounded
// up, so an effect never ends early; one the device cannot represent at all (negative, or beyond
// the longest hours value) is rejected rather than quietly shortened.
func indicatorDuration(seconds int32) (int32, error) {
	switch {
	case seconds < 0:
		return 0, bridge.ErrArgumentNotSupportedByDevice
	case seconds == 0:
		return indicatorDurationIndefinite, nil
	case seconds <= 60:
		return seconds, nil
	case seconds <= 60*60:
		return indicatorDurationMinutesBase + ceilDiv(seconds, 60), nil
	}
	hours := ceilDiv(seconds, 60*60)
	if hours > indicatorDurationMaxHours {
		return 0, bridge.ErrArgumentNotSupportedByDevice
	}
	return indicatorDurationHoursBase + hours, nil
}

// ceilDiv is a/b rounded up, for positive a and b. Written as a quotient plus a remainder check
// rather than (a+b-1)/b, which overflows int32 for a near math.MaxInt32.
func ceilDiv(a, b int32) int32 {
	q := a / b
	if a%b != 0 {
		q++
	}
	return q
}
