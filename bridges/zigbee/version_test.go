package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeVersion_ChangesWithControllableState(t *testing.T) {
	d, err := onOffBuilder{}.build(switchDevice(false))
	require.NoError(t, err)

	v1 := computeVersion(d)
	d.GetGeneric().OnOff.State.IsOn = true
	v2 := computeVersion(d)
	assert.NotEqual(t, v1, v2)
}

func TestComputeVersion_StableAcrossPowerTelemetry(t *testing.T) {
	d, err := onOffBuilder{}.build(switchDevice(true))
	require.NoError(t, err)

	v1 := computeVersion(d)
	d.GetGeneric().Power.State.PowerW = 42.0
	v2 := computeVersion(d)
	assert.Equal(t, v1, v2, "read-only Power telemetry must not affect version, per computeVersion's documented contract")
}

func TestComputeVersion_LightIncludesColourAndBrightness(t *testing.T) {
	fixture := lightDevice(true, true, true)
	lb := mustLightBuilder(t, fixture)
	d, err := lb.build(fixture)
	require.NoError(t, err)

	base := computeVersion(d)

	d.GetLight().Brightness.State.Level = 50
	assert.NotEqual(t, base, computeVersion(d))

	d2, err := lb.build(fixture)
	require.NoError(t, err)
	base2 := computeVersion(d2)
	d2.GetLight().Colour.State.Hsb.Hue = 200
	assert.NotEqual(t, base2, computeVersion(d2))
}

func TestComputeVersion_SensorConstant(t *testing.T) {
	d, err := sensorBuilder{}.build(sensorDevice("temperature"))
	require.NoError(t, err)

	v1 := computeVersion(d)
	d.GetSensor().AirProperties.State.TemperatureC = 30
	v2 := computeVersion(d)
	assert.Equal(t, v1, v2, "Sensor has no command-target fields, so its version must stay constant across state changes")
}

// TestComputeVersion_FanIncludesOnOffModeAndToggles is a regression test: computeVersion had no
// Fan branch at all for a while after fanBuilder was added, which would have left every Fan
// device's version constant regardless of any applied command - defeating Command.version's
// whole optimistic-concurrency purpose for that device type. Speed is deliberately excluded (see
// computeVersion's doc comment) since fanBuilder always builds it read-only.
func TestComputeVersion_FanIncludesOnOffModeAndToggles(t *testing.T) {
	bd := starkvindDevice()
	fb, ok := newFanBuilder(bd.Definition.Exposes)
	require.True(t, ok)

	d, err := fb.build(bd)
	require.NoError(t, err)
	base := computeVersion(d)

	d.GetFan().OnOff.State.IsOn = true
	assert.NotEqual(t, base, computeVersion(d), "on_off must affect version")

	d2, err := fb.build(bd)
	require.NoError(t, err)
	base2 := computeVersion(d2)
	d2.GetFan().Mode.State.CurrentMode = "auto"
	assert.NotEqual(t, base2, computeVersion(d2), "mode must affect version")

	d3, err := fb.build(bd)
	require.NoError(t, err)
	base3 := computeVersion(d3)
	d3.GetFan().Toggles.State.Settings["led_enable"] = true
	assert.NotEqual(t, base3, computeVersion(d3), "toggles must affect version")

	d4, err := fb.build(bd)
	require.NoError(t, err)
	base4 := computeVersion(d4)
	d4.GetFan().Speed.State.CurrentSpeed = 5
	assert.Equal(t, base4, computeVersion(d4), "speed is read-only telemetry and must not affect version")
}
