package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/service/bridge"
)

func f64(v float64) *float64 { return &v }

func switchDevice(withPower bool) bridgeDevice {
	exposes := []expose{
		{Type: "switch", Features: []expose{
			{Type: "binary", Name: "state", Property: "state"},
		}},
	}
	if withPower {
		exposes = append(exposes, expose{Type: "numeric", Name: "power", Property: "power", Unit: "W"})
		exposes = append(exposes, expose{Type: "numeric", Name: "current", Property: "current", Unit: "A"})
		exposes = append(exposes, expose{Type: "numeric", Name: "voltage", Property: "voltage", Unit: "V"})
	}
	return bridgeDevice{
		IEEEAddress:  "0xabc",
		FriendlyName: "plug1",
		Type:         "Router",
		Supported:    true,
		Definition:   &deviceDefiniton{Model: "S31", Vendor: "Sonoff", Description: "Smart plug", Exposes: exposes},
	}
}

func TestOnOffBuilder_Build(t *testing.T) {
	d, err := onOffBuilder{}.build(switchDevice(false))
	require.NoError(t, err)
	g := d.GetGeneric()
	require.NotNil(t, g)
	assert.True(t, g.OnOff.Attributes.CanControl)
	assert.False(t, g.OnOff.State.IsOn)
	assert.Nil(t, g.Power)
}

func TestOnOffBuilder_Build_WithPower(t *testing.T) {
	d, err := onOffBuilder{}.build(switchDevice(true))
	require.NoError(t, err)
	g := d.GetGeneric()
	require.NotNil(t, g.Power)
}

func TestOnOffBuilder_ApplyState(t *testing.T) {
	d, err := onOffBuilder{}.build(switchDevice(true))
	require.NoError(t, err)

	onOffBuilder{}.applyState(d, map[string]any{"state": "ON", "power": 12.5, "current": 0.1, "voltage": 120.0})
	g := d.GetGeneric()
	assert.True(t, g.OnOff.State.IsOn)
	assert.EqualValues(t, 12.5, g.Power.State.PowerW)
	assert.EqualValues(t, 0.1, g.Power.State.CurrentA)
	assert.EqualValues(t, 120.0, g.Power.State.VoltageV)

	// A partial update (only linkquality, no "state") must not clobber existing fields.
	onOffBuilder{}.applyState(d, map[string]any{"linkquality": 60.0})
	assert.True(t, g.OnOff.State.IsOn)
}

func TestOnOffBuilder_ApplyCommand(t *testing.T) {
	mc, fc := newTestMQTTConn(t)
	fc.respond("zigbee2mqtt/plug1/set", "zigbee2mqtt/plug1", []byte(`{"state":"ON"}`))

	d, err := onOffBuilder{}.build(switchDevice(false))
	require.NoError(t, err)

	err = onOffBuilder{}.applyCommand(context.Background(), mc, "plug1", d, &command.Command{
		Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
	})
	require.NoError(t, err)
	assert.True(t, d.GetGeneric().OnOff.State.IsOn)
}

func TestOnOffBuilder_ApplyCommand_Unsupported(t *testing.T) {
	mc, _ := newTestMQTTConn(t)
	d, err := onOffBuilder{}.build(switchDevice(false))
	require.NoError(t, err)

	err = onOffBuilder{}.applyCommand(context.Background(), mc, "plug1", d, &command.Command{
		Details: &command.Command_BrightnessAbsolute{BrightnessAbsolute: &command.BrightnessAbsolute{BrightnessPercent: 50}},
	})
	assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
}

// --- Light ---

func lightDevice(brightness, colorTemp, colorHS bool) bridgeDevice {
	features := []expose{
		{Type: "binary", Name: "state", Property: "state"},
	}
	if brightness {
		features = append(features, expose{Type: "numeric", Name: "brightness", Property: "brightness", ValueMin: f64(0), ValueMax: f64(254)})
	}
	if colorTemp {
		features = append(features, expose{Type: "numeric", Name: "color_temp", Property: "color_temp", Unit: "mired", ValueMin: f64(150), ValueMax: f64(500)})
	}
	if colorHS {
		features = append(features, expose{Type: "composite", Name: "color_hs", Property: "color", Features: []expose{
			{Type: "numeric", Name: "hue", Property: "hue", ValueMin: f64(0), ValueMax: f64(360)},
			{Type: "numeric", Name: "saturation", Property: "saturation", ValueMin: f64(0), ValueMax: f64(100)},
		}})
	}
	return bridgeDevice{
		IEEEAddress:  "0xbulb",
		FriendlyName: "lamp1",
		Type:         "Router",
		Supported:    true,
		Definition: &deviceDefiniton{
			Model: "LCT001", Vendor: "Philips", Description: "Hue bulb",
			Exposes: []expose{{Type: "light", Features: features}},
		},
	}
}

// mustLightBuilder resolves a lightBuilder for bd via newLightBuilder, the same path classify()
// uses - a bare lightBuilder{} literal (as earlier versions of these tests used directly) has a
// zero brightnessMax, which is indistinguishable from "no brightness feature" and would silently
// mask the exact bug TestLightBuilder_ApplyCommand_Brightness_UsesDeviceValueMax below guards
// against.
func mustLightBuilder(t *testing.T, bd bridgeDevice) lightBuilder {
	t.Helper()
	lb, ok := newLightBuilder(bd.Definition.Exposes)
	require.True(t, ok)
	return lb
}

// setBrightnessValueMax mutates bd's "light" composite's "brightness" feature's ValueMax in
// place, looked up by name rather than a fixed index so it doesn't silently break if
// lightDevice's feature ordering ever changes.
func setBrightnessValueMax(bd bridgeDevice, max float64) {
	for i := range bd.Definition.Exposes {
		if bd.Definition.Exposes[i].Type != "light" {
			continue
		}
		for j := range bd.Definition.Exposes[i].Features {
			if bd.Definition.Exposes[i].Features[j].Name == "brightness" {
				bd.Definition.Exposes[i].Features[j].ValueMax = f64(max)
				return
			}
		}
	}
}

func TestLightBuilder_Build_FullyFeatured(t *testing.T) {
	lb := mustLightBuilder(t, lightDevice(true, true, true))
	d, err := lb.build(lightDevice(true, true, true))
	require.NoError(t, err)
	l := d.GetLight()
	require.NotNil(t, l)
	assert.True(t, l.OnOff.Attributes.CanControl)
	require.NotNil(t, l.Brightness)
	require.NotNil(t, l.Colour)
	assert.Equal(t, int32(2000), l.Colour.Attributes.ColourTemperatureRange.MinK) // 1e6/500
	assert.Equal(t, int32(6667), l.Colour.Attributes.ColourTemperatureRange.MaxK) // round(1e6/150)
	require.NotNil(t, l.Colour.State.Hsb)
}

func TestLightBuilder_Build_ColorTempOnly_NoHSB(t *testing.T) {
	lb := mustLightBuilder(t, lightDevice(true, true, false))
	d, err := lb.build(lightDevice(true, true, false))
	require.NoError(t, err)
	l := d.GetLight()
	require.NotNil(t, l.Colour)
	assert.Nil(t, l.Colour.State.Hsb)
	assert.Equal(t, l.Colour.Attributes.Mode.String(), "MODE_UNSPECIFIED")
}

func TestLightBuilder_Build_OnOffOnly_NoBrightnessNoColour(t *testing.T) {
	lb := mustLightBuilder(t, lightDevice(false, false, false))
	d, err := lb.build(lightDevice(false, false, false))
	require.NoError(t, err)
	l := d.GetLight()
	assert.Nil(t, l.Brightness)
	assert.Nil(t, l.Colour)
}

func TestLightBuilder_ApplyState(t *testing.T) {
	fixture := lightDevice(true, true, true)
	lb := mustLightBuilder(t, fixture)
	d, err := lb.build(fixture)
	require.NoError(t, err)

	lb.applyState(d, map[string]any{
		"state": "ON", "brightness": 127.0, "color_temp": 250.0,
		"color": map[string]any{"hue": 200.0, "saturation": 50.0},
	})
	l := d.GetLight()
	assert.True(t, l.OnOff.State.IsOn)
	assert.EqualValues(t, 50, l.Brightness.State.Level)            // round(127*100/254)
	assert.EqualValues(t, 4000, l.Colour.State.ColourTemperatureK) // round(1e6/250)
	assert.EqualValues(t, 200, l.Colour.State.Hsb.Hue)
	assert.EqualValues(t, 50, l.Colour.State.Hsb.Saturation)
}

// TestLightBuilder_ApplyState_UsesDeviceValueMax is a regression test: an earlier version of this
// bridge hardcoded zigbeeDefaultBrightnessMax (254) in both applyState and applyCommand
// regardless of what a light's own "brightness" expose reported, contradicting README's own
// documented claim that the expose's value_max is used - masked in live testing because every
// light on the real broker happened to report 254. This fixture reports value_max: 100 instead.
func TestLightBuilder_ApplyState_UsesDeviceValueMax(t *testing.T) {
	fixture := lightDevice(true, false, false)
	setBrightnessValueMax(fixture, 100)
	lb := mustLightBuilder(t, fixture)
	require.EqualValues(t, 100, lb.brightnessMax)

	d, err := lb.build(fixture)
	require.NoError(t, err)
	lb.applyState(d, map[string]any{"brightness": 50.0})
	assert.EqualValues(t, 50, d.GetLight().Brightness.State.Level) // 50/100, NOT 50/254
}

func TestLightBuilder_ApplyCommand_OnOff(t *testing.T) {
	mc, fc := newTestMQTTConn(t)
	fc.respond("zigbee2mqtt/lamp1/set", "zigbee2mqtt/lamp1", []byte(`{"state":"ON"}`))

	fixture := lightDevice(true, true, true)
	lb := mustLightBuilder(t, fixture)
	d, err := lb.build(fixture)
	require.NoError(t, err)

	err = lb.applyCommand(context.Background(), mc, "lamp1", d, &command.Command{
		Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
	})
	require.NoError(t, err)
	assert.True(t, d.GetLight().OnOff.State.IsOn)
	// A plain on/off command must not touch brightness - see lightBuilder.applyCommand's doc
	// comment on why no restore-on-level bookkeeping is needed for Zigbee, unlike zwave.
	require.Len(t, fc.published, 1)
	assert.NotContains(t, string(fc.published[0].payload), "brightness")
}

func TestLightBuilder_ApplyCommand_Brightness(t *testing.T) {
	mc, fc := newTestMQTTConn(t)
	fc.respond("zigbee2mqtt/lamp1/set", "zigbee2mqtt/lamp1", []byte(`{"brightness":127}`))

	fixture := lightDevice(true, true, true)
	lb := mustLightBuilder(t, fixture)
	d, err := lb.build(fixture)
	require.NoError(t, err)

	err = lb.applyCommand(context.Background(), mc, "lamp1", d, &command.Command{
		Details: &command.Command_BrightnessAbsolute{BrightnessAbsolute: &command.BrightnessAbsolute{BrightnessPercent: 50}},
	})
	require.NoError(t, err)
	assert.EqualValues(t, 50, d.GetLight().Brightness.State.Level)
	assert.True(t, d.GetLight().OnOff.State.IsOn)
}

// TestLightBuilder_ApplyCommand_Brightness_UsesDeviceValueMax is applyCommand's counterpart to
// TestLightBuilder_ApplyState_UsesDeviceValueMax - see its doc comment.
func TestLightBuilder_ApplyCommand_Brightness_UsesDeviceValueMax(t *testing.T) {
	mc, fc := newTestMQTTConn(t)
	fc.respond("zigbee2mqtt/lamp1/set", "zigbee2mqtt/lamp1", []byte(`{"brightness":50}`))

	fixture := lightDevice(true, false, false)
	setBrightnessValueMax(fixture, 100)
	lb := mustLightBuilder(t, fixture)
	d, err := lb.build(fixture)
	require.NoError(t, err)

	err = lb.applyCommand(context.Background(), mc, "lamp1", d, &command.Command{
		Details: &command.Command_BrightnessAbsolute{BrightnessAbsolute: &command.BrightnessAbsolute{BrightnessPercent: 50}},
	})
	require.NoError(t, err)
	require.Len(t, fc.published, 1)
	assert.JSONEq(t, `{"brightness":50}`, string(fc.published[0].payload)) // 50% of 100, NOT of 254
}

func TestLightBuilder_ApplyCommand_Hsb(t *testing.T) {
	mc, fc := newTestMQTTConn(t)
	fc.respond("zigbee2mqtt/lamp1/set", "zigbee2mqtt/lamp1", []byte(`{"color":{"hue":200,"saturation":75}}`))

	fixture := lightDevice(true, true, true)
	lb := mustLightBuilder(t, fixture)
	d, err := lb.build(fixture)
	require.NoError(t, err)

	err = lb.applyCommand(context.Background(), mc, "lamp1", d, &command.Command{
		Details: &command.Command_Colour{Colour: &command.Colour{
			Value: &command.Colour_Hsb{Hsb: &command.Colour_HSB{Hue: 200, Saturation: 75}},
		}},
	})
	require.NoError(t, err)
	assert.EqualValues(t, 200, d.GetLight().Colour.State.Hsb.Hue)
	assert.EqualValues(t, 75, d.GetLight().Colour.State.Hsb.Saturation)
}

func TestLightBuilder_ApplyCommand_ColourTemperature_ClampedToRange(t *testing.T) {
	mc, fc := newTestMQTTConn(t)
	fc.respond("zigbee2mqtt/lamp1/set", "zigbee2mqtt/lamp1", []byte(`{"color_temp":150}`))

	fixture := lightDevice(true, true, true)
	lb := mustLightBuilder(t, fixture)
	d, err := lb.build(fixture)
	require.NoError(t, err)

	err = lb.applyCommand(context.Background(), mc, "lamp1", d, &command.Command{
		Details: &command.Command_Colour{Colour: &command.Colour{
			Value: &command.Colour_ColourTemperatureK{ColourTemperatureK: 9000},
		}},
	})
	require.NoError(t, err)
	assert.EqualValues(t, 6667, d.GetLight().Colour.State.ColourTemperatureK) // clamped to range max
}

func TestLightBuilder_ApplyCommand_Rgb_Unsupported(t *testing.T) {
	mc, _ := newTestMQTTConn(t)
	fixture := lightDevice(true, true, true)
	lb := mustLightBuilder(t, fixture)
	d, err := lb.build(fixture)
	require.NoError(t, err)

	err = lb.applyCommand(context.Background(), mc, "lamp1", d, &command.Command{
		Details: &command.Command_Colour{Colour: &command.Colour{
			Value: &command.Colour_Rgb{Rgb: &command.Colour_RGB{Red: 255}},
		}},
	})
	assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
}

// --- Sensor ---

func sensorDevice(props ...string) bridgeDevice {
	var exposes []expose
	for _, p := range props {
		typ := "numeric"
		if p == "occupancy" || p == "contact" || p == "water_leak" || p == "smoke" || p == "battery_low" {
			typ = "binary"
		}
		exposes = append(exposes, expose{Type: typ, Name: p, Property: p})
	}
	return bridgeDevice{
		IEEEAddress:  "0xsensor",
		FriendlyName: "sensor1",
		Type:         "EndDevice",
		PowerSource:  "Battery",
		Supported:    true,
		Definition:   &deviceDefiniton{Model: "SNZB-02", Vendor: "SONOFF", Description: "Temp/humidity sensor", Exposes: exposes},
	}
}

func TestSensorBuilder_Build_AllTraits(t *testing.T) {
	d, err := sensorBuilder{}.build(sensorDevice("occupancy", "contact", "temperature", "humidity", "illuminance", "battery", "water_leak", "smoke"))
	require.NoError(t, err)
	s := d.GetSensor()
	require.NotNil(t, s.Presence)
	require.NotNil(t, s.OpenedClosed)
	require.NotNil(t, s.AirProperties)
	require.NotNil(t, s.LightLevel)
	require.NotNil(t, s.Battery)
	require.NotNil(t, s.Water)
	require.NotNil(t, s.Fire)
	require.NotNil(t, s.Metadata)
	assert.True(t, s.Metadata.OnBattery)
}

func TestSensorBuilder_Build_OnlyTemperature(t *testing.T) {
	d, err := sensorBuilder{}.build(sensorDevice("temperature"))
	require.NoError(t, err)
	s := d.GetSensor()
	require.NotNil(t, s.AirProperties)
	assert.Nil(t, s.Presence)
	assert.Nil(t, s.OpenedClosed)
	assert.Nil(t, s.Battery)
}

func TestSensorBuilder_ApplyState(t *testing.T) {
	d, err := sensorBuilder{}.build(sensorDevice("occupancy", "contact", "temperature", "humidity", "illuminance", "battery", "water_leak", "smoke"))
	require.NoError(t, err)

	sensorBuilder{}.applyState(d, map[string]any{
		"occupancy": true, "contact": false, "temperature": 21.5, "humidity": 45.0,
		"illuminance": 130.0, "battery": 87.0, "water_leak": true, "smoke": false,
	})
	s := d.GetSensor()
	require.NotNil(t, s.Presence.State.OccupancyDetected)
	assert.True(t, *s.Presence.State.OccupancyDetected)
	// contact:false means the contact is broken (open) - opened_closed.is_active is the negation.
	assert.True(t, s.OpenedClosed.IsActive)
	assert.EqualValues(t, 21.5, s.AirProperties.State.TemperatureC)
	assert.EqualValues(t, 45.0, s.AirProperties.State.HumidityPercentage)
	assert.EqualValues(t, 130.0, s.LightLevel.State.Lux)
	assert.EqualValues(t, 87, s.Battery.State.CapacityRemainingPct)
	assert.True(t, s.Water.IsActive)
	assert.False(t, s.Fire.IsActive)
}

func TestSensorBuilder_ApplyState_ContactClosed(t *testing.T) {
	d, err := sensorBuilder{}.build(sensorDevice("contact"))
	require.NoError(t, err)

	sensorBuilder{}.applyState(d, map[string]any{"contact": true})
	assert.False(t, d.GetSensor().OpenedClosed.IsActive)
}

// TestSensorBuilder_PrefersIlluminanceLux is a regression test for a real Philips Hue motion
// sensor (9290012607) recon'd against a live zigbee2mqtt broker: its exposes document
// "illuminance" as "Raw measured illuminance" (not lux) and "illuminance_lux" as the actual
// calibrated reading - an earlier version of this bridge read "illuminance" directly as lux,
// which would silently report the wrong scale for any device exposing both. See
// sensorBuilder's doc comment.
func TestSensorBuilder_PrefersIlluminanceLux(t *testing.T) {
	bd := sensorDevice("illuminance", "illuminance_lux")

	d, err := sensorBuilder{}.build(bd)
	require.NoError(t, err)
	require.NotNil(t, d.GetSensor().LightLevel)

	// A raw, unscaled illuminance count alongside the real lux value in the same update -
	// illuminance_lux must win.
	sensorBuilder{}.applyState(d, map[string]any{"illuminance": 30000.0, "illuminance_lux": 130.0})
	assert.EqualValues(t, 130, d.GetSensor().LightLevel.State.Lux)
}

func TestSensorBuilder_IlluminanceOnly_FallsBack(t *testing.T) {
	d, err := sensorBuilder{}.build(sensorDevice("illuminance"))
	require.NoError(t, err)

	sensorBuilder{}.applyState(d, map[string]any{"illuminance": 130.0})
	assert.EqualValues(t, 130, d.GetSensor().LightLevel.State.Lux)
}

// TestSensorBuilder_BatteryLow is a regression test for a real Aqara water-leak sensor
// (SJCGQ11LM) recon'd against a live zigbee2mqtt broker, which exposes a "battery_low" binary
// property alongside "battery" - an earlier version of this bridge didn't read it at all.
func TestSensorBuilder_BatteryLow(t *testing.T) {
	bd := sensorDevice("battery", "battery_low")

	d, err := sensorBuilder{}.build(bd)
	require.NoError(t, err)
	require.NotNil(t, d.GetSensor().Metadata)

	sensorBuilder{}.applyState(d, map[string]any{"battery_low": true})
	assert.True(t, d.GetSensor().Metadata.LowBattery)
}

func TestSensorBuilder_ApplyCommand_AlwaysUnsupported(t *testing.T) {
	d, err := sensorBuilder{}.build(sensorDevice("temperature"))
	require.NoError(t, err)

	err = sensorBuilder{}.applyCommand(context.Background(), nil, "sensor1", d, &command.Command{
		Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
	})
	assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
}

// --- Conversion helpers ---

func TestMiredKelvinConversion(t *testing.T) {
	assert.Equal(t, int32(2000), miredToKelvin(500))
	assert.Equal(t, int32(6667), miredToKelvin(150))
	assert.Equal(t, int32(250), kelvinToMired(4000))
}

func TestBrightnessPercentConversion(t *testing.T) {
	assert.EqualValues(t, 50, brightnessToPercent(127, 254))
	assert.EqualValues(t, 127, percentToBrightness(50, 254))
	assert.EqualValues(t, 254, percentToBrightness(100, 254))
	assert.EqualValues(t, 0, percentToBrightness(0, 254))
}

// --- Fan ---

// starkvindDevice mirrors a real IKEA STARKVIND air purifier's (E2007) exposes, recon'd against a
// live zigbee2mqtt broker - see README's "Verified against a live broker". In particular: the fan
// composite's "state" feature has wire property "fan_state" (not "state"), its "mode" feature is
// an enum ("off"/"auto"/"1".."9") under "fan_mode", the current-speed telemetry is a separate
// read-only top-level "fan_speed" numeric, and the two toggles use different JSON encodings for
// the same logical on/off concept (led_enable: bool, child_lock: string).
func starkvindDevice() bridgeDevice {
	return bridgeDevice{
		IEEEAddress:  "0xfan1",
		FriendlyName: "office/air_purifier",
		Type:         "Router",
		Supported:    true,
		Definition: &deviceDefiniton{
			Model: "E2007", Vendor: "IKEA", Description: "STARKVIND air purifier",
			Exposes: []expose{
				{Type: "fan", Features: []expose{
					{Type: "binary", Name: "state", Property: "fan_state", ValueOn: []byte(`"ON"`), ValueOff: []byte(`"OFF"`)},
					{Type: "enum", Name: "mode", Property: "fan_mode", Values: []string{"off", "auto", "1", "2", "3", "4", "5", "6", "7", "8", "9"}},
				}},
				{Type: "numeric", Name: "fan_speed", Property: "fan_speed", ValueMin: f64(0), ValueMax: f64(9)},
				{Type: "binary", Name: "led_enable", Property: "led_enable", ValueOn: []byte(`true`), ValueOff: []byte(`false`)},
				{Type: "binary", Name: "child_lock", Property: "child_lock", ValueOn: []byte(`"LOCK"`), ValueOff: []byte(`"UNLOCK"`)},
			},
		},
	}
}

func TestFanBuilder_Classify(t *testing.T) {
	fb, ok := newFanBuilder(starkvindDevice().Definition.Exposes)
	require.True(t, ok)
	assert.Equal(t, "fan_state", fb.stateProperty)
	assert.Equal(t, "fan_mode", fb.modeProperty)
	assert.Equal(t, "fan_speed", fb.speedProperty)
	require.Len(t, fb.toggles, 2)
}

func TestFanBuilder_NoFanComposite(t *testing.T) {
	_, ok := newFanBuilder(switchDevice(false).Definition.Exposes)
	assert.False(t, ok)
}

func TestFanBuilder_Build(t *testing.T) {
	bd := starkvindDevice()
	fb, ok := newFanBuilder(bd.Definition.Exposes)
	require.True(t, ok)

	d, err := fb.build(bd)
	require.NoError(t, err)
	f := d.GetFan()
	require.NotNil(t, f)
	assert.True(t, f.OnOff.Attributes.CanControl)
	require.NotNil(t, f.Mode)
	assert.Equal(t, []string{"off", "auto", "1", "2", "3", "4", "5", "6", "7", "8", "9"}, f.Mode.Attributes.AvailableModes)
	require.NotNil(t, f.Speed)
	assert.EqualValues(t, 0, f.Speed.Attributes.MinimumSpeed)
	assert.EqualValues(t, 9, f.Speed.Attributes.MaximumSpeed)
	require.NotNil(t, f.Toggles)
	assert.ElementsMatch(t, []string{"led_enable", "child_lock"}, f.Toggles.Attributes.AvailableToggles)
}

func TestFanBuilder_ApplyState(t *testing.T) {
	bd := starkvindDevice()
	fb, ok := newFanBuilder(bd.Definition.Exposes)
	require.True(t, ok)
	d, err := fb.build(bd)
	require.NoError(t, err)

	fb.applyState(d, map[string]any{
		"fan_state": "ON", "fan_mode": "auto", "fan_speed": 4.0,
		"led_enable": true, "child_lock": "LOCK",
	})
	f := d.GetFan()
	assert.True(t, f.OnOff.State.IsOn)
	assert.Equal(t, "auto", f.Mode.State.CurrentMode)
	assert.EqualValues(t, 4, f.Speed.State.CurrentSpeed)
	assert.True(t, f.Toggles.State.Settings["led_enable"])
	assert.True(t, f.Toggles.State.Settings["child_lock"])

	fb.applyState(d, map[string]any{"fan_state": "OFF", "child_lock": "UNLOCK"})
	assert.False(t, f.OnOff.State.IsOn)
	assert.False(t, f.Toggles.State.Settings["child_lock"])
	// A partial update must not disturb fields it doesn't mention.
	assert.True(t, f.Toggles.State.Settings["led_enable"])
}

func TestFanBuilder_ApplyCommand_OnOff(t *testing.T) {
	mc, fc := newTestMQTTConn(t)
	fc.respond("zigbee2mqtt/office/air_purifier/set", "zigbee2mqtt/office/air_purifier", []byte(`{"fan_state":"ON"}`))

	bd := starkvindDevice()
	fb, _ := newFanBuilder(bd.Definition.Exposes)
	d, err := fb.build(bd)
	require.NoError(t, err)

	err = fb.applyCommand(context.Background(), mc, "office/air_purifier", d, &command.Command{
		Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
	})
	require.NoError(t, err)
	assert.True(t, d.GetFan().OnOff.State.IsOn)
}

func TestFanBuilder_ApplyCommand_Mode(t *testing.T) {
	mc, fc := newTestMQTTConn(t)
	fc.respond("zigbee2mqtt/office/air_purifier/set", "zigbee2mqtt/office/air_purifier", []byte(`{"fan_mode":"auto"}`))

	bd := starkvindDevice()
	fb, _ := newFanBuilder(bd.Definition.Exposes)
	d, err := fb.build(bd)
	require.NoError(t, err)

	err = fb.applyCommand(context.Background(), mc, "office/air_purifier", d, &command.Command{
		Details: &command.Command_Mode{Mode: &command.Mode{Value: "auto"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "auto", d.GetFan().Mode.State.CurrentMode)
}

func TestFanBuilder_ApplyCommand_Toggle_MixedEncoding(t *testing.T) {
	mc, fc := newTestMQTTConn(t)
	fc.respond("zigbee2mqtt/office/air_purifier/set", "zigbee2mqtt/office/air_purifier",
		[]byte(`{"led_enable":false,"child_lock":"LOCK"}`))

	bd := starkvindDevice()
	fb, _ := newFanBuilder(bd.Definition.Exposes)
	d, err := fb.build(bd)
	require.NoError(t, err)

	err = fb.applyCommand(context.Background(), mc, "office/air_purifier", d, &command.Command{
		Details: &command.Command_Toggle{Toggle: &command.Toggle{Settings: map[string]bool{
			"led_enable": false, "child_lock": true,
		}}},
	})
	require.NoError(t, err)
	f := d.GetFan()
	assert.False(t, f.Toggles.State.Settings["led_enable"])
	assert.True(t, f.Toggles.State.Settings["child_lock"])
}

func TestFanBuilder_ApplyCommand_Speed_Unsupported(t *testing.T) {
	mc, _ := newTestMQTTConn(t)
	bd := starkvindDevice()
	fb, _ := newFanBuilder(bd.Definition.Exposes)
	d, err := fb.build(bd)
	require.NoError(t, err)

	err = fb.applyCommand(context.Background(), mc, "office/air_purifier", d, &command.Command{
		Details: &command.Command_BrightnessAbsolute{BrightnessAbsolute: &command.BrightnessAbsolute{BrightnessPercent: 50}},
	})
	assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
}

func TestDecodeAny(t *testing.T) {
	assert.Equal(t, true, decodeAny([]byte(`true`), false))
	assert.Equal(t, "LOCK", decodeAny([]byte(`"LOCK"`), "fallback"))
	assert.Equal(t, "fallback", decodeAny(nil, "fallback"))
	assert.Equal(t, "fallback", decodeAny([]byte(`not json`), "fallback"))
}
