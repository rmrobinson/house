package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func actionExpose(values ...string) expose {
	return expose{Type: "enum", Name: "action", Property: "action", Values: values}
}

// hueDimmerDevice mirrors the real Hue dimmer switch: battery + action, no light/switch composite.
func hueDimmerDevice() bridgeDevice {
	return bridgeDevice{
		IEEEAddress:  "0x0017880102e30055",
		FriendlyName: "remote/switch2",
		Type:         "EndDevice",
		Supported:    true,
		Definition: &deviceDefiniton{Model: "324131092621", Vendor: "Philips", Exposes: []expose{
			{Type: "numeric", Name: "battery", Property: "battery", Unit: "%"},
			actionExpose("on_press", "on_press_release", "up_press", "off_press"),
		}},
	}
}

func TestSensorBuilder_Buttons_BuildAndEvents(t *testing.T) {
	d, err := sensorBuilder{}.build(hueDimmerDevice())
	require.NoError(t, err)
	b := d.GetSensor().GetButtons()
	require.NotNil(t, b)
	assert.Equal(t, []string{"on_press", "on_press_release", "up_press", "off_press"}, b.Attributes.Actions)
	assert.Equal(t, int64(0), b.State.EventCount)

	sensorBuilder{}.applyState(d, map[string]any{"action": "on_press"})
	assert.Equal(t, "on_press", b.State.LastAction)
	assert.Equal(t, int64(1), b.State.EventCount)
	assert.NotNil(t, b.State.LastActionTime)

	// z2m's reset message and unrelated reports are not events.
	sensorBuilder{}.applyState(d, map[string]any{"action": ""})
	sensorBuilder{}.applyState(d, map[string]any{"battery": 80.0, "linkquality": 120.0})
	assert.Equal(t, "on_press", b.State.LastAction)
	assert.Equal(t, int64(1), b.State.EventCount)

	// An identical action is still a distinct event.
	sensorBuilder{}.applyState(d, map[string]any{"action": "on_press"})
	assert.Equal(t, int64(2), b.State.EventCount)
}

func TestNetworkConn_Classify_ActionOnlyDeviceIsSensor(t *testing.T) {
	bd := bridgeDevice{
		IEEEAddress: "0x1", FriendlyName: "remote/x", Type: "EndDevice",
		Definition: &deviceDefiniton{Exposes: []expose{actionExpose("single")}},
	}
	builder, ok := (&networkConn{}).classify(bd)
	require.True(t, ok)
	assert.IsType(t, sensorBuilder{}, builder)
}

func TestLightBuilder_Buttons_InovelliTaps(t *testing.T) {
	bd := bridgeDevice{
		IEEEAddress: "0x2", FriendlyName: "first_floor/kitchen/island_light", Type: "Router",
		Definition: &deviceDefiniton{Model: "VZM31-SN", Vendor: "Inovelli", Exposes: []expose{
			{Type: "light", Features: []expose{
				{Type: "binary", Name: "state", Property: "state"},
				{Type: "numeric", Name: "brightness", Property: "brightness", ValueMax: f64(254)},
			}},
			actionExpose("up_single", "up_double", "down_triple"),
		}},
	}
	lb, ok := newLightBuilder(bd.Definition.Exposes)
	require.True(t, ok)
	d, err := lb.build(bd)
	require.NoError(t, err)
	b := d.GetLight().GetButtons()
	require.NotNil(t, b)

	lb.applyState(d, map[string]any{"state": "ON", "action": "up_double"})
	assert.True(t, d.GetLight().OnOff.State.IsOn)
	assert.Equal(t, "up_double", b.State.LastAction)
	assert.Equal(t, int64(1), b.State.EventCount)
}

func TestLightBuilder_NoActionExpose_NoButtons(t *testing.T) {
	exposes := []expose{{Type: "light", Features: []expose{{Type: "binary", Name: "state", Property: "state"}}}}
	lb, _ := newLightBuilder(exposes)
	d, err := lb.build(bridgeDevice{Definition: &deviceDefiniton{Exposes: exposes}})
	require.NoError(t, err)
	assert.Nil(t, d.GetLight().GetButtons())
	lb.applyState(d, map[string]any{"action": "up_single"}) // must not panic
}
