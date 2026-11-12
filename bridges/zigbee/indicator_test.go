package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/service/bridge"
)

// inovelliDevice mirrors the exposes of a real VZM31-SN that matter here: the dimmer's light
// composite, the whole-bar led_effect composite, and the per-LED composite this bridge ignores.
func inovelliLEDBarDevice() bridgeDevice {
	return bridgeDevice{
		IEEEAddress:  "0x5c3aa2fffe45345e",
		FriendlyName: "dimmer1",
		Type:         "Router",
		Supported:    true,
		Definition: &deviceDefiniton{Model: "VZM31-SN", Vendor: "Inovelli", Exposes: []expose{
			{Type: "light", Features: []expose{
				{Type: "binary", Name: "state", Property: "state"},
				{Type: "numeric", Name: "brightness", Property: "brightness", ValueMax: f64(254)},
			}},
			{Type: "composite", Name: "led_effect", Property: "led_effect", Features: []expose{
				{Type: "enum", Name: "effect", Property: "effect", Values: []string{"off", "solid", "pulse", "clear_effect"}},
				{Type: "numeric", Name: "color", Property: "color"},
				{Type: "numeric", Name: "level", Property: "level"},
				{Type: "numeric", Name: "duration", Property: "duration"},
			}},
			{Type: "composite", Name: "individual_led_effect", Property: "individual_led_effect", Features: []expose{
				{Type: "enum", Name: "led", Property: "led", Values: []string{"1", "2"}},
				{Type: "enum", Name: "effect", Property: "effect", Values: []string{"off", "solid"}},
			}},
		}},
	}
}

func TestLightBuilder_Indicator_Build(t *testing.T) {
	fixture := inovelliLEDBarDevice()
	lb := mustLightBuilder(t, fixture)
	d, err := lb.build(fixture)
	require.NoError(t, err)

	ind := d.GetLight().GetIndicator()
	require.NotNil(t, ind)
	assert.True(t, ind.Attributes.CanControl)
	assert.Equal(t, []string{"off", "solid", "pulse", "clear_effect"}, ind.Attributes.Effects)
}

func TestLightBuilder_NoLedEffectExpose_NoIndicator(t *testing.T) {
	fixture := lightDevice(true, true, true)
	lb := mustLightBuilder(t, fixture)
	d, err := lb.build(fixture)
	require.NoError(t, err)
	assert.Nil(t, d.GetLight().GetIndicator())
}

func indicatorCommand(c *command.IndicatorEffect) *command.Command {
	return &command.Command{Details: &command.Command_IndicatorEffect{IndicatorEffect: c}}
}

func i32(v int32) *int32 { return &v }

func TestLightBuilder_ApplyCommand_IndicatorEffect(t *testing.T) {
	mc, fc := newTestMQTTConn(t)
	// The echo must reflect the whole led_effect object, as zigbee2mqtt does (confirmed live).
	fc.respond("zigbee2mqtt/dimmer1/set", "zigbee2mqtt/dimmer1",
		[]byte(`{"led_effect":{"effect":"pulse","color":0,"level":80,"duration":30}}`))

	fixture := inovelliLEDBarDevice()
	lb := mustLightBuilder(t, fixture)
	d, err := lb.build(fixture)
	require.NoError(t, err)

	err = lb.applyCommand(context.Background(), mc, "dimmer1", d, indicatorCommand(&command.IndicatorEffect{
		Effect: "pulse", DurationSeconds: 30, Hue: i32(0), LevelPercent: i32(80),
	}))
	require.NoError(t, err)
}

func TestLightBuilder_ApplyCommand_IndicatorEffect_PublishedPayload(t *testing.T) {
	mc, fc := newTestMQTTConn(t)
	// Not echoed, so WriteState times out; what matters here is what was published.
	prev := writeTimeout
	writeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { writeTimeout = prev })

	fixture := inovelliLEDBarDevice()
	lb := mustLightBuilder(t, fixture)
	d, err := lb.build(fixture)
	require.NoError(t, err)

	_ = lb.applyCommand(context.Background(), mc, "dimmer1", d, indicatorCommand(&command.IndicatorEffect{
		Effect: "solid", Hue: i32(120), DurationSeconds: 90,
	}))

	require.Equal(t, []string{"zigbee2mqtt/dimmer1/set"}, fc.publishedTopics())
	var got map[string]any
	require.NoError(t, json.Unmarshal(fc.lastPayload(), &got))
	assert.Equal(t, map[string]any{"led_effect": map[string]any{
		"effect": "solid", "color": 85.0, "level": 100.0, "duration": 62.0,
	}}, got)
}

func TestLightBuilder_ApplyCommand_IndicatorEffect_Rejected(t *testing.T) {
	mc, _ := newTestMQTTConn(t)
	fixture := inovelliLEDBarDevice()
	lb := mustLightBuilder(t, fixture)
	d, err := lb.build(fixture)
	require.NoError(t, err)

	// An effect the device doesn't list.
	err = lb.applyCommand(context.Background(), mc, "dimmer1", d, indicatorCommand(&command.IndicatorEffect{
		Effect: "chase_the_dragon", White: true,
	}))
	assert.ErrorIs(t, err, bridge.ErrArgumentNotSupportedByDevice)

	// A coloured effect with no colour chosen.
	err = lb.applyCommand(context.Background(), mc, "dimmer1", d, indicatorCommand(&command.IndicatorEffect{Effect: "solid"}))
	assert.ErrorIs(t, err, bridge.ErrArgumentNotSupportedByDevice)

	// A light with no indicator at all.
	plain := lightDevice(true, true, true)
	plb := mustLightBuilder(t, plain)
	pd, err := plb.build(plain)
	require.NoError(t, err)
	err = plb.applyCommand(context.Background(), mc, "lamp1", pd, indicatorCommand(&command.IndicatorEffect{Effect: "solid", White: true}))
	assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
}

func TestIndicatorColour(t *testing.T) {
	cases := []struct {
		name string
		cmd  *command.IndicatorEffect
		want int32
	}{
		{"red", &command.IndicatorEffect{Effect: "solid", Hue: i32(0)}, 0},
		{"green", &command.IndicatorEffect{Effect: "solid", Hue: i32(120)}, 85},
		{"max hue stays on the wheel", &command.IndicatorEffect{Effect: "solid", Hue: i32(360)}, 254},
		{"over-range hue clamps", &command.IndicatorEffect{Effect: "solid", Hue: i32(900)}, 254},
		{"white wins over hue", &command.IndicatorEffect{Effect: "solid", Hue: i32(120), White: true}, 255},
		{"clear needs no colour", &command.IndicatorEffect{Effect: "clear_effect"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := indicatorColour(tc.cmd)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestIndicatorDuration(t *testing.T) {
	cases := []struct{ seconds, want int32 }{
		{0, 255},    // until cleared
		{-5, 255},   // nonsense reads as until cleared, never as an instant effect
		{1, 1},      // seconds
		{60, 60},    // top of the seconds range
		{61, 62},    // rounds up to 2 minutes rather than ending early
		{120, 62},   // exactly 2 minutes
		{3600, 120}, // top of the minutes range: 60 minutes
		{3601, 122}, // rounds up to 2 hours
		{7200, 122}, // exactly 2 hours
		{134 * 3600, 254},
		{1000 * 3600, 254}, // saturates at the longest representable duration
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, indicatorDuration(tc.seconds), "seconds=%d", tc.seconds)
	}
}
