package main

import (
	"context"
	"encoding/json"
	"math"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
)

// deviceBuilder is implemented once per house device type this bridge supports. A zigbee2mqtt
// device is dispatched to its builder by networkConn.classify, based on its exposes shape - see
// classify's doc comment for why this differs structurally from bridges/zwave's fixed
// generic-device-class dispatch.
//
// Unlike bridges/zwave's deviceBuilder, there is no role->valueId table to plumb through:
// zigbee2mqtt publishes a device's entire known state as one JSON object on one topic, so
// applyState and applyCommand work directly against that object/the device's friendly_name
// rather than a per-property topic.
type deviceBuilder interface {
	// build constructs the initial house Device for a discovered zigbee2mqtt device. zigbee2mqtt's
	// bridge/devices payload doesn't include a device's *current* property values (only its
	// capabilities) - unlike bridges/zwave's getNodes, which returns both in one call - so a
	// freshly built device starts at its trait's zero value and is corrected by the device's own
	// retained state message arriving shortly after, via applyState, the moment this bridge
	// subscribes to it. This is a real, documented behavioural difference from zwave: expect a
	// brief window after startup where a just-discovered device's state hasn't caught up yet.
	build(bd bridgeDevice) (*device.Device, error)

	// applyState mutates d from a (possibly partial) decoded device-state JSON object - a periodic
	// zigbee2mqtt report may include only "linkquality", for example, with none of the keys this
	// builder cares about, so every field access here must check for presence rather than assume
	// every property is included in a given update.
	applyState(d *device.Device, state map[string]any)

	// applyCommand translates a house Command into a single WriteState call against friendlyName,
	// and updates d optimistically. The normal state-topic stream corrects this if the device
	// disagrees.
	applyCommand(ctx context.Context, m *mqttConn, friendlyName string, d *device.Device, cmd *command.Command) error
}

// --- Generic on/off (switch, or plug w/ optional power metering) ---

// onOffBuilder builds a house Generic/OnOff device from a top-level "switch" composite expose
// (feature "state") or a bare top-level binary "state" property - zigbee2mqtt's universal
// convention for a controllable on/off device is a "state" property whose value is the string
// "ON" or "OFF" (not a JSON bool, unlike bridges/zwave's Binary Switch CC) - confirmed by
// zigbee2mqtt's own exposes documentation, not device-specific, so this bridge hardcodes it the
// same way bridges/zwave hardcodes "targetValue"/"currentValue" for CC 37. Generic, not a
// dedicated Switch/Outlet device type, since this repo has none yet - same gap
// bridges/zwave/devices.go's switchBuilder documents.
//
// If the device also exposes "power" (a smart plug's metering, reported in watts), that's folded
// onto the same Generic device's Power trait rather than building a separate Sensor - a metering
// plug is still fundamentally one controllable device, and Generic supports both traits directly.
type onOffBuilder struct{}

const (
	zigbeeStateOn  = "ON"
	zigbeeStateOff = "OFF"
)

func (onOffBuilder) build(bd bridgeDevice) (*device.Device, error) {
	g := &device.Generic{
		OnOff: &trait.OnOff{
			Attributes: &trait.OnOff_Attributes{CanControl: true},
			State:      &trait.OnOff_State{},
		},
	}

	if _, ok := findByProperty(bd.Definition.Exposes, "power"); ok {
		g.Power = &trait.Power{
			Attributes: &trait.Power_Attributes{},
			State:      &trait.Power_State{},
		}
	}

	return &device.Device{
		Manufacturer:     bd.Definition.Vendor,
		ModelId:          bd.Definition.Model,
		ModelDescription: &bd.Definition.Description,
		Details:          &device.Device_Generic{Generic: g},
	}, nil
}

func (onOffBuilder) applyState(d *device.Device, state map[string]any) {
	g := d.GetGeneric()

	if v, ok := state["state"].(string); ok {
		g.OnOff.State.IsOn = v == zigbeeStateOn
	}

	if g.Power == nil {
		return
	}
	if v, ok := numberValue(state["power"]); ok {
		g.Power.State.PowerW = v
	}
	if v, ok := numberValue(state["current"]); ok {
		g.Power.State.CurrentA = v
	}
	if v, ok := numberValue(state["voltage"]); ok {
		g.Power.State.VoltageV = v
	}
}

func (onOffBuilder) applyCommand(ctx context.Context, m *mqttConn, friendlyName string, d *device.Device, cmd *command.Command) error {
	if cmd.GetOnOff() == nil {
		return bridge.ErrUnsupportedCommand
	}

	on := cmd.GetOnOff().On
	state := zigbeeStateOff
	if on {
		state = zigbeeStateOn
	}
	if err := m.WriteState(ctx, friendlyName, map[string]any{"state": state}); err != nil {
		return err
	}
	d.GetGeneric().OnOff.State.IsOn = on
	return nil
}

// --- Light ---

// lightBuilder builds a house Light device from a top-level "light" composite expose. Brightness
// (if the light supports dimming) and Colour (if it supports hue/saturation and/or colour
// temperature) are only added when the light's own exposes report them - mirroring
// bridges/zwave/devices.go's sensorBuilder's "only add a trait if the device actually reports it"
// pattern, just applied to a light's optional capabilities instead of a sensor's.
//
// Colour: only the "color_hs" feature (hue 0-360 / saturation 0-100) is modeled, matching
// trait.Colour_State_HSB exactly with no unit conversion needed - the same MODE_HSB choice
// bridges/nanoleaf makes, for the same reason (the device's own colour representation is
// hue/saturation, not RGB). A light that only exposes "color_xy" (CIE 1931 chromaticity
// coordinates) gets no Colour trait at all - converting xy to hue/saturation is lossy and this
// bridge has no live device to validate a conversion against, so rather than guess (or, like
// bridges/zwave's rgbLightBuilder, refuse to build the whole device) this simply omits colour
// control for that light while still building it for on/off/brightness - see README's "Known
// limitations". Colour temperature ("color_temp") is independent of the hue/saturation
// capability, per trait.Colour's own doc comment, and is modeled whenever present regardless of
// whether color_hs is also present.
// lightBuilder carries the device-specific brightness scale resolved at classify-time
// (newLightBuilder), rather than assuming zigbeeDefaultBrightnessMax (254) unconditionally -
// confirmed 254 for every light on the real broker this bridge was checked against, but the
// exposes API doesn't guarantee that's universal, and an earlier version of this bridge silently
// hardcoded it in both directions (state decode and command encode) regardless of what a
// device's own "brightness" expose actually reported, which would have miscalculated brightness
// for any device whose real range differs.
type lightBuilder struct {
	// brightnessMax is 0 if the light has no "brightness" feature at all (see build's guard on
	// this being nonzero), else the feature's own value_max, falling back to
	// zigbeeDefaultBrightnessMax only if that field is absent from the expose.
	brightnessMax int32
}

// zigbeeDefaultBrightnessMax is zigbee2mqtt's near-universal brightness scale (the Zigbee Level
// Control cluster's native 0-254 range) - used only as a fallback if a light's own "brightness"
// expose doesn't report value_max, which the exposes API documents as always present for a
// numeric expose in practice.
const zigbeeDefaultBrightnessMax = 254

// newLightBuilder resolves a lightBuilder from a device's exposes, or reports false if the device
// has no top-level "light" composite expose at all.
func newLightBuilder(exposes []expose) (lightBuilder, bool) {
	light, ok := findTopLevel(exposes, "light")
	if !ok {
		return lightBuilder{}, false
	}

	lb := lightBuilder{}
	if brightnessFeature, ok := findFeature(light.Features, "brightness"); ok {
		lb.brightnessMax = zigbeeDefaultBrightnessMax
		if brightnessFeature.ValueMax != nil && *brightnessFeature.ValueMax > 0 {
			lb.brightnessMax = int32(*brightnessFeature.ValueMax)
		}
	}
	return lb, true
}

func (lb lightBuilder) build(bd bridgeDevice) (*device.Device, error) {
	light, _ := findTopLevel(bd.Definition.Exposes, "light")

	l := &device.Light{
		OnOff: &trait.OnOff{
			Attributes: &trait.OnOff_Attributes{CanControl: true},
			State:      &trait.OnOff_State{},
		},
	}

	if lb.brightnessMax > 0 {
		l.Brightness = &trait.Brightness{
			Attributes: &trait.Brightness_Attributes{CanControl: true},
			State:      &trait.Brightness_State{},
		}
	}

	ctFeature, hasCT := findFeature(light.Features, "color_temp")
	hsFeature, hasHS := findFeature(light.Features, "color_hs")

	if hasCT || hasHS {
		attrs := &trait.Colour_Attributes{CanControl: true}
		if hasHS {
			attrs.Mode = trait.Colour_Attributes_MODE_HSB
			_ = hsFeature // composite itself carries no extra range info beyond its hue/saturation sub-features, which are fixed 0-360/0-100 by the Zigbee spec
		}
		if hasCT && ctFeature.ValueMin != nil && ctFeature.ValueMax != nil && *ctFeature.ValueMin > 0 && *ctFeature.ValueMax > 0 {
			// Mireds and Kelvin are inversely related - the expose's smallest mired value is the
			// warmest-*capable* i.e. highest Kelvin bound, and vice versa.
			attrs.ColourTemperatureRange = &trait.Colour_Attributes_ColourTemperatureRange{
				MinK: miredToKelvin(*ctFeature.ValueMax),
				MaxK: miredToKelvin(*ctFeature.ValueMin),
			}
		}
		l.Colour = &trait.Colour{Attributes: attrs, State: &trait.Colour_State{}}
		if hasHS {
			l.Colour.State.Hsb = &trait.Colour_State_HSB{}
		}
	}

	return &device.Device{
		Manufacturer:     bd.Definition.Vendor,
		ModelId:          bd.Definition.Model,
		ModelDescription: &bd.Definition.Description,
		Details:          &device.Device_Light{Light: l},
	}, nil
}

func (lb lightBuilder) applyState(d *device.Device, state map[string]any) {
	l := d.GetLight()

	if v, ok := state["state"].(string); ok {
		l.OnOff.State.IsOn = v == zigbeeStateOn
	}

	if l.Brightness != nil {
		if v, ok := numberValue(state["brightness"]); ok {
			l.Brightness.State.Level = brightnessToPercent(v, lb.brightnessMax)
		}
	}

	if l.Colour == nil {
		return
	}
	if v, ok := numberValue(state["color_temp"]); ok && v > 0 {
		l.Colour.State.ColourTemperatureK = miredToKelvin(v)
	}
	if l.Colour.State.Hsb != nil {
		if colour, ok := state["color"].(map[string]any); ok {
			if v, ok := numberValue(colour["hue"]); ok {
				l.Colour.State.Hsb.Hue = int32(v)
			}
			if v, ok := numberValue(colour["saturation"]); ok {
				l.Colour.State.Hsb.Saturation = int32(v)
			}
		}
	}
}

func (lb lightBuilder) applyCommand(ctx context.Context, m *mqttConn, friendlyName string, d *device.Device, cmd *command.Command) error {
	l := d.GetLight()

	switch {
	case cmd.GetOnOff() != nil:
		on := cmd.GetOnOff().On
		// A plain on/off command deliberately never touches brightness: unlike bridges/zwave's
		// Multilevel Switch CC (whose "on" has no independent memory of the last level and needs
		// bridge-side bookkeeping - see resolveRestoreOnLevel), the Zigbee Level Control cluster
		// is independent of the On/Off cluster and a compliant bulb restores its own last level
		// on its own when just told "on" - no equivalent bookkeeping is needed here.
		state := zigbeeStateOff
		if on {
			state = zigbeeStateOn
		}
		if err := m.WriteState(ctx, friendlyName, map[string]any{"state": state}); err != nil {
			return err
		}
		l.OnOff.State.IsOn = on
		return nil

	case cmd.GetBrightnessAbsolute() != nil || cmd.GetBrightnessRelative() != nil:
		if l.Brightness == nil {
			return bridge.ErrUnsupportedCommand
		}
		pct := resolveBrightnessPercent(cmd, l.Brightness.State.Level)
		brightness := percentToBrightness(pct, lb.brightnessMax)
		if err := m.WriteState(ctx, friendlyName, map[string]any{"brightness": brightness}); err != nil {
			return err
		}
		l.Brightness.State.Level = pct
		l.OnOff.State.IsOn = pct > 0
		return nil

	case cmd.GetColour() != nil:
		return applyColourCommand(ctx, m, friendlyName, l, cmd.GetColour())

	default:
		return bridge.ErrUnsupportedCommand
	}
}

func applyColourCommand(ctx context.Context, m *mqttConn, friendlyName string, l *device.Light, c *command.Colour) error {
	if l.Colour == nil {
		return bridge.ErrUnsupportedCommand
	}

	switch v := c.GetValue().(type) {
	case *command.Colour_Hsb:
		if l.Colour.State.Hsb == nil {
			return bridge.ErrUnsupportedCommand
		}
		hue := clampRange(v.Hsb.Hue, 0, 360)
		sat := clampRange(v.Hsb.Saturation, 0, 100)
		set := map[string]any{"color": map[string]any{"hue": hue, "saturation": sat}}
		if err := m.WriteState(ctx, friendlyName, set); err != nil {
			return err
		}
		l.Colour.State.Hsb.Hue = hue
		l.Colour.State.Hsb.Saturation = sat
		return nil

	case *command.Colour_ColourTemperatureK:
		if l.Colour.Attributes.ColourTemperatureRange == nil {
			return bridge.ErrUnsupportedCommand
		}
		ct := clampRange(v.ColourTemperatureK, l.Colour.Attributes.ColourTemperatureRange.MinK, l.Colour.Attributes.ColourTemperatureRange.MaxK)
		mired := kelvinToMired(ct)
		if err := m.WriteState(ctx, friendlyName, map[string]any{"color_temp": mired}); err != nil {
			return err
		}
		l.Colour.State.ColourTemperatureK = ct
		return nil

	default:
		// Only MODE_HSB (color_hs) is supported by this builder - see lightBuilder's doc comment
		// for why color_xy (the only other colour representation zigbee2mqtt exposes) isn't
		// modeled at all, so a Colour.Rgb command has nothing to translate to here either.
		return bridge.ErrUnsupportedCommand
	}
}

func resolveBrightnessPercent(cmd *command.Command, currentPct int32) int32 {
	if a := cmd.GetBrightnessAbsolute(); a != nil {
		return clampPercent(a.BrightnessPercent)
	}
	return clampPercent(currentPct + cmd.GetBrightnessRelative().ChangePercent)
}

// brightnessToPercent converts zigbee2mqtt's device-native brightness scale (0-max, typically
// 0-254) to house's 0-100 percent scale.
func brightnessToPercent(brightness float64, max int32) int32 {
	if max <= 0 {
		max = zigbeeDefaultBrightnessMax
	}
	pct := int32(math.Round(brightness * 100 / float64(max)))
	return clampPercent(pct)
}

// percentToBrightness converts house's 0-100 percent scale to zigbee2mqtt's device-native
// brightness scale.
func percentToBrightness(pct int32, max int32) int32 {
	if max <= 0 {
		max = zigbeeDefaultBrightnessMax
	}
	return int32(math.Round(float64(clampPercent(pct)) * float64(max) / 100))
}

// miredToKelvin converts a Zigbee colour-temperature reading (mireds, a.k.a. "mirek") to Kelvin.
// Mireds and Kelvin are inversely related (mired = 1,000,000 / kelvin), a fixed physical
// conversion rather than a device-specific assumption.
func miredToKelvin(mired float64) int32 {
	if mired <= 0 {
		return 0
	}
	return int32(math.Round(1000000 / mired))
}

// kelvinToMired is miredToKelvin's inverse, used when writing a Colour_ColourTemperatureK command
// back out to zigbee2mqtt's "color_temp" property.
func kelvinToMired(kelvin int32) int32 {
	if kelvin <= 0 {
		return 0
	}
	return int32(math.Round(1000000 / float64(kelvin)))
}

func clampPercent(pct int32) int32 {
	return clampRange(pct, 0, 100)
}

func clampRange(v, min, max int32) int32 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// numberValue reads a JSON-decoded numeric value (always float64 after json.Unmarshal into
// map[string]any) out of a state map entry, reporting false for a missing key or a value that
// isn't a number at all (e.g. a device briefly reporting an error string in a field this bridge
// expects to be numeric).
func numberValue(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// --- Sensor ---

// sensorProperties lists every top-level property this bridge recognizes as making a device
// classify as a Sensor - see networkConn.classify. Order doesn't matter; it's only used as a
// membership check there.
var sensorProperties = []string{
	"occupancy", "contact", "temperature", "humidity", "illuminance", "illuminance_lux", "battery",
	"water_leak", "smoke",
}

// sensorBuilder builds a house Sensor device from whichever of the read-only capabilities in
// sensorProperties a device's exposes list actually reports - mirroring
// bridges/zwave/devices.go's sensorBuilder's "only add a trait if the node actually reports it"
// pattern. Every capability here is read-only in zigbee2mqtt's own exposes access model, so
// applyCommand always rejects, same as zwave's Sensor.
//
// contact's semantics are inverted from Sensor.opened_closed's: confirmed against a real
// zigbee2mqtt broker (a Philips Hue motion sensor and an IKEA/Aqara contact sensor, among others -
// see README's "Verified against a live broker") that the "contact" binary expose's own
// value_on/value_off metadata documents value_on: false, value_off: true - i.e. zigbee-herdsman-
// converters treats contact:true (the magnet is present, door/window closed) as the expose's
// logical "off" state and contact:false (open) as "on" - so opened_closed.is_active (documented
// "true if open") is set to the logical negation of the raw "contact" value, not passed through
// directly. Confirmed from the expose's own metadata, not merely inferred from behaviour.
//
// illuminance_lux is preferred over illuminance when both are present: confirmed against a real
// Philips Hue motion sensor's exposes that "illuminance" is documented there as "Raw measured
// illuminance" (an uncalibrated sensor count, not a lux value) while "illuminance_lux" is the
// actual calibrated lux reading - treating "illuminance" as lux, as an earlier version of this
// bridge did, would silently report the wrong scale for any device exposing both. A device
// reporting only "illuminance" (no "_lux" sibling) is assumed to be reporting real lux directly
// under that name, per zigbee2mqtt's general convention for simpler light sensors - unconfirmed
// against such a device specifically, since none was available on the broker this was checked
// against, but consistent with how every "illuminance"-only device's exposes description reads.
type sensorBuilder struct{}

// illuminanceLuxProperty returns whichever of "illuminance_lux"/"illuminance" a device's exposes
// list reports as its real lux reading, preferring "illuminance_lux" - see sensorBuilder's doc
// comment for why.
func illuminanceLuxProperty(exposes []expose) (string, bool) {
	if _, ok := findByProperty(exposes, "illuminance_lux"); ok {
		return "illuminance_lux", true
	}
	if _, ok := findByProperty(exposes, "illuminance"); ok {
		return "illuminance", true
	}
	return "", false
}

func (sensorBuilder) build(bd bridgeDevice) (*device.Device, error) {
	exposes := bd.Definition.Exposes
	s := &device.Sensor{}

	if _, ok := findByProperty(exposes, "occupancy"); ok {
		s.Presence = &trait.Presence{Attributes: &trait.Presence_Attributes{}, State: &trait.Presence_State{}}
	}
	if _, ok := findByProperty(exposes, "contact"); ok {
		s.OpenedClosed = &device.Sensor_BinarySensor{}
	}
	if _, ok := findByProperty(exposes, "temperature"); ok {
		ensureAirProperties(s)
	}
	if _, ok := findByProperty(exposes, "humidity"); ok {
		ensureAirProperties(s)
	}
	if _, ok := illuminanceLuxProperty(exposes); ok {
		s.LightLevel = &trait.LightLevel{Attributes: &trait.LightLevel_Attributes{CanControl: false}, State: &trait.LightLevel_State{}}
	}
	if _, ok := findByProperty(exposes, "battery"); ok {
		s.Battery = &trait.Battery{Attributes: &trait.Battery_Attributes{}, State: &trait.Battery_State{}}
	}
	if _, ok := findByProperty(exposes, "water_leak"); ok {
		s.Water = &device.Sensor_BinarySensor{}
	}
	if _, ok := findByProperty(exposes, "smoke"); ok {
		s.Fire = &device.Sensor_BinarySensor{}
	}
	if _, ok := findByProperty(exposes, "battery_low"); ok {
		ensureMetadata(s)
	}

	if bd.PowerSource == "Battery" {
		ensureMetadata(s)
		s.Metadata.OnBattery = true
	}

	return &device.Device{
		Manufacturer:     bd.Definition.Vendor,
		ModelId:          bd.Definition.Model,
		ModelDescription: &bd.Definition.Description,
		Details:          &device.Device_Sensor{Sensor: s},
	}, nil
}

func (sensorBuilder) applyState(d *device.Device, state map[string]any) {
	s := d.GetSensor()

	if s.Presence != nil {
		if v, ok := state["occupancy"].(bool); ok {
			s.Presence.State.OccupancyDetected = &v
		}
	}
	if s.OpenedClosed != nil {
		if v, ok := state["contact"].(bool); ok {
			s.OpenedClosed.IsActive = !v
		}
	}
	if s.AirProperties != nil {
		if v, ok := numberValue(state["temperature"]); ok {
			s.AirProperties.State.TemperatureC = float32(v)
		}
		if v, ok := numberValue(state["humidity"]); ok {
			s.AirProperties.State.HumidityPercentage = float32(v)
		}
	}
	if s.LightLevel != nil {
		// Prefer illuminance_lux (real lux) over illuminance (a raw, uncalibrated count on
		// devices that report both) - see sensorBuilder's doc comment. Checked per-message
		// against whichever keys this particular update actually carries, rather than a choice
		// fixed at build time, since zigbee2mqtt's default output republishes the device's whole
		// known state on every change - both keys are expected together whenever either changes.
		raw, ok := state["illuminance_lux"]
		if !ok {
			raw, ok = state["illuminance"]
		}
		if v, numOK := numberValue(raw); ok && numOK {
			lux := float32(v)
			s.LightLevel.State.Lux = lux
			s.LightLevel.State.LightLevel = luxToLightLevel(lux)
		}
	}
	if s.Battery != nil {
		if v, ok := numberValue(state["battery"]); ok {
			s.Battery.State.CapacityRemainingPct = int32(v)
		}
	}
	if s.Water != nil {
		if v, ok := state["water_leak"].(bool); ok {
			s.Water.IsActive = v
		}
	}
	if s.Fire != nil {
		if v, ok := state["smoke"].(bool); ok {
			s.Fire.IsActive = v
		}
	}
	if s.Metadata != nil {
		if v, ok := state["battery_low"].(bool); ok {
			s.Metadata.LowBattery = v
		}
	}
}

// applyCommand always fails: every trait a Sensor device can carry here is read-only, per
// zigbee2mqtt's own exposes access model - none of sensorProperties' exposes report access & 2
// (settable). Mirrors bridges/zwave/devices.go's sensorBuilder.applyCommand.
func (sensorBuilder) applyCommand(_ context.Context, _ *mqttConn, _ string, _ *device.Device, _ *command.Command) error {
	return bridge.ErrUnsupportedCommand
}

// luxToLightLevel derives LightLevel.State.light_level from a lux reading, per the formula
// documented on that field: 10000*log10(lux)+1. Shared verbatim with
// bridges/zwave/devices.go's identical helper - not factored into a common package since each
// bridge under bridges/ is deliberately self-contained (see bridges/zwave's own precedent).
func luxToLightLevel(lux float32) float32 {
	if lux <= 0 {
		return 0
	}
	return float32(10000*math.Log10(float64(lux)) + 1)
}

func ensureAirProperties(s *device.Sensor) {
	if s.AirProperties == nil {
		s.AirProperties = &trait.AirProperties{Attributes: &trait.AirProperties_Attributes{}, State: &trait.AirProperties_State{}}
	}
}

func ensureMetadata(s *device.Sensor) {
	if s.Metadata == nil {
		s.Metadata = &device.Sensor_Metadata{}
	}
}

// --- Fan ---

// fanToggleCandidates lists the toggle-shaped binary exposes this bridge recognizes as Fan
// toggles - confirmed against a real IKEA STARKVIND air purifier (E2007), whose "led_enable"
// (an LED indicator) and "child_lock" (physical input lock) both fit trait.Toggle's
// "independently-addressable named boolean feature" shape exactly. Unlike sensorProperties, this
// list isn't a generic Zigbee convention this bridge is confident generalizes - z2m has no single
// well-known property name for "a fan's LED" or "a fan's child lock" the way "contact"/
// "occupancy" are for sensors - so it's deliberately narrow (only what's confirmed on real
// hardware) rather than a guess at other fan products' property names. See README's "Known
// limitations".
var fanToggleCandidates = []string{"led_enable", "child_lock"}

// fanToggle resolves one binary config expose (from fanToggleCandidates) onto one entry of
// Fan.Toggles.
type fanToggle struct {
	name     string
	property string
	valueOn  any
	valueOff any
}

// fanBuilder builds a house Fan device from a top-level "fan" composite expose. Unlike
// onOffBuilder/lightBuilder, which hardcode "state" as the wire property name (a convention
// confirmed universal for every switch/light on the real broker this bridge was checked against -
// see README), a fan's property names are NOT universal: a real IKEA STARKVIND air purifier
// (E2007) exposes its fan composite's "state" feature under wire property "fan_state" (not
// "state") and its "mode" feature under "fan_mode" - so fanBuilder resolves and remembers each
// device's actual property names at classify-time (via newFanBuilder), rather than assuming a
// fixed convention. Per-value encoding is resolved the same way: the STARKVIND's own "state"
// feature reports "ON"/"OFF" strings (matching zigbeeStateOn/Off), but this isn't assumed either
// - stateValueOn/Off are read from the expose's own value_on/value_off, mirroring how
// fanToggle's valueOn/Off are resolved (see expose.ValueOn's doc comment).
//
// Speed is modeled as read-only telemetry, not a second way to control the fan: the STARKVIND's
// only settable speed control is its "mode" enum ("off"/"auto"/"1".."9"); its separate top-level
// "fan_speed" numeric (confirmed via that expose's own access value lacking the SET bit) only
// reports the fan's current actual speed, which can legitimately differ from the last commanded
// mode (e.g. while ramping, or in "auto" mode). No other fan-speed property shape has been
// checked against real hardware, so only this exact top-level "fan_speed" sibling convention is
// supported - see README's "Known limitations".
type fanBuilder struct {
	stateProperty string
	stateValueOn  any
	stateValueOff any
	// modeProperty is empty if the device's fan composite has no "mode" feature - Fan.Mode is
	// then left nil entirely (same "only add a trait the device actually reports" pattern used
	// throughout this bridge).
	modeProperty string
	// speedProperty is empty if the device has no top-level "fan_speed" sibling property.
	speedProperty string
	toggles       []fanToggle
}

// newFanBuilder resolves a fanBuilder from a device's exposes, or reports false if the device has
// no top-level "fan" composite, or that composite has no "state" feature at all (on/off is the
// one capability every fan must have for this bridge to do anything useful with it).
func newFanBuilder(exposes []expose) (fanBuilder, bool) {
	fan, ok := findTopLevel(exposes, "fan")
	if !ok {
		return fanBuilder{}, false
	}
	stateFeature, ok := findFeature(fan.Features, "state")
	if !ok {
		return fanBuilder{}, false
	}

	fb := fanBuilder{
		stateProperty: stateFeature.Property,
		stateValueOn:  decodeAny(stateFeature.ValueOn, zigbeeStateOn),
		stateValueOff: decodeAny(stateFeature.ValueOff, zigbeeStateOff),
	}

	if modeFeature, ok := findFeature(fan.Features, "mode"); ok {
		fb.modeProperty = modeFeature.Property
	}
	if _, ok := findByProperty(exposes, "fan_speed"); ok {
		fb.speedProperty = "fan_speed"
	}

	for _, name := range fanToggleCandidates {
		e, ok := findByProperty(exposes, name)
		if !ok {
			continue
		}
		fb.toggles = append(fb.toggles, fanToggle{
			name:     name,
			property: e.Property,
			valueOn:  decodeAny(e.ValueOn, true),
			valueOff: decodeAny(e.ValueOff, false),
		})
	}

	return fb, true
}

func (fb fanBuilder) build(bd bridgeDevice) (*device.Device, error) {
	exposes := bd.Definition.Exposes
	f := &device.Fan{
		OnOff: &trait.OnOff{
			Attributes: &trait.OnOff_Attributes{CanControl: true},
			State:      &trait.OnOff_State{},
		},
	}

	if fb.modeProperty != "" {
		fan, _ := findTopLevel(exposes, "fan")
		modeFeature, _ := findFeature(fan.Features, "mode")
		f.Mode = &trait.Mode{
			Attributes: &trait.Mode_Attributes{CanControl: true, AvailableModes: modeFeature.Values},
			State:      &trait.Mode_State{},
		}
	}

	if fb.speedProperty != "" {
		speed := &trait.Speed{Attributes: &trait.Speed_Attributes{}, State: &trait.Speed_State{}}
		if e, ok := findByProperty(exposes, fb.speedProperty); ok {
			if e.ValueMin != nil {
				speed.Attributes.MinimumSpeed = int32(*e.ValueMin)
			}
			if e.ValueMax != nil {
				speed.Attributes.MaximumSpeed = int32(*e.ValueMax)
			}
		}
		f.Speed = speed
	}

	if len(fb.toggles) > 0 {
		names := make([]string, len(fb.toggles))
		for i, t := range fb.toggles {
			names[i] = t.name
		}
		f.Toggles = &trait.Toggle{
			Attributes: &trait.Toggle_Attributes{AvailableToggles: names},
			State:      &trait.Toggle_State{Settings: make(map[string]bool, len(fb.toggles))},
		}
	}

	return &device.Device{
		Manufacturer:     bd.Definition.Vendor,
		ModelId:          bd.Definition.Model,
		ModelDescription: &bd.Definition.Description,
		Details:          &device.Device_Fan{Fan: f},
	}, nil
}

func (fb fanBuilder) applyState(d *device.Device, state map[string]any) {
	f := d.GetFan()

	if v, ok := state[fb.stateProperty]; ok {
		f.OnOff.State.IsOn = v == fb.stateValueOn
	}
	if f.Mode != nil {
		if v, ok := state[fb.modeProperty].(string); ok {
			f.Mode.State.CurrentMode = v
		}
	}
	if f.Speed != nil {
		if v, ok := numberValue(state[fb.speedProperty]); ok {
			f.Speed.State.CurrentSpeed = int32(v)
		}
	}
	if f.Toggles != nil {
		for _, t := range fb.toggles {
			if v, ok := state[t.property]; ok {
				f.Toggles.State.Settings[t.name] = v == t.valueOn
			}
		}
	}
}

func (fb fanBuilder) applyCommand(ctx context.Context, m *mqttConn, friendlyName string, d *device.Device, cmd *command.Command) error {
	f := d.GetFan()

	switch {
	case cmd.GetOnOff() != nil:
		on := cmd.GetOnOff().On
		val := fb.stateValueOff
		if on {
			val = fb.stateValueOn
		}
		if err := m.WriteState(ctx, friendlyName, map[string]any{fb.stateProperty: val}); err != nil {
			return err
		}
		f.OnOff.State.IsOn = on
		return nil

	case cmd.GetMode() != nil:
		if f.Mode == nil {
			return bridge.ErrUnsupportedCommand
		}
		value := cmd.GetMode().Value
		if err := m.WriteState(ctx, friendlyName, map[string]any{fb.modeProperty: value}); err != nil {
			return err
		}
		f.Mode.State.CurrentMode = value
		return nil

	case cmd.GetToggle() != nil:
		if f.Toggles == nil {
			return bridge.ErrUnsupportedCommand
		}
		set := make(map[string]any, len(cmd.GetToggle().Settings))
		applied := make(map[string]bool, len(cmd.GetToggle().Settings))
		for name, want := range cmd.GetToggle().Settings {
			t, ok := findFanToggle(fb.toggles, name)
			if !ok {
				continue // an unrecognized toggle name is ignored, not an error - Toggle is a
				// partial update, and a caller may legitimately name a toggle another device on
				// the same command stream supports but this one doesn't.
			}
			val := t.valueOff
			if want {
				val = t.valueOn
			}
			set[t.property] = val
			applied[name] = want
		}
		if len(set) == 0 {
			return bridge.ErrUnsupportedCommand
		}
		if err := m.WriteState(ctx, friendlyName, set); err != nil {
			return err
		}
		for name, want := range applied {
			f.Toggles.State.Settings[name] = want
		}
		return nil

	default:
		return bridge.ErrUnsupportedCommand
	}
}

func findFanToggle(toggles []fanToggle, name string) (fanToggle, bool) {
	for _, t := range toggles {
		if t.name == name {
			return t, true
		}
	}
	return fanToggle{}, false
}

// decodeAny decodes raw (an expose's value_on/value_off field) into a comparable Go value -
// bool/string/float64/nil depending on its JSON type, matching what a live state update's own
// json.Unmarshal into map[string]any produces for the same property, so the two can be compared
// directly with ==. Falls back to fallback if raw is absent (an older zigbee-herdsman-converters
// version, or a leaf expose type that doesn't document value_on/value_off) or malformed.
func decodeAny(raw json.RawMessage, fallback any) any {
	if len(raw) == 0 {
		return fallback
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fallback
	}
	return v
}
