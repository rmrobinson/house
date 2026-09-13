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

func nv(cc, endpoint int, property, propertyKey string, value any) nodeValue {
	b, _ := json.Marshal(value)
	return nodeValue{CommandClass: cc, Endpoint: endpoint, Property: property, PropertyKey: propertyKey, Value: b}
}

// nvState builds a nodeValueState the same shape a real getNodes response uses (confirmed against
// a live AEON MultiSensor 6: [{"text":"idle","value":0},{"text":"Motion detection","value":8}]).
func nvState(text string, value any) nodeValueState {
	b, _ := json.Marshal(value)
	return nodeValueState{Text: text, Value: b}
}

// --- switchBuilder ---

func TestSwitchBuilder_Build(t *testing.T) {
	n := nodeInfo{ID: 7, Values: map[string]nodeValue{
		"v": nv(ccBinarySwitch, 0, "targetValue", "", true),
	}}
	n.DeviceClass.Generic = genericBinarySwitch
	n.DeviceConfig.Description = "Outdoor Smart Plug"

	d, roles, err := switchBuilder{}.build(n, deviceOverride{})
	require.NoError(t, err)
	assert.True(t, d.GetGeneric().OnOff.State.IsOn)
	assert.Equal(t, "plug", d.GetModelDescription())
	assert.Equal(t, valueID{nodeID: 7, topicBase: "nodeID_7", commandClass: ccBinarySwitch, endpoint: 0, property: "targetValue"}, roles["on_off"])
}

func TestSwitchBuilder_Build_LabelOverride(t *testing.T) {
	n := nodeInfo{ID: 7, DeviceID: "1-2-3"}
	n.DeviceClass.Generic = genericBinarySwitch

	d, _, err := switchBuilder{}.build(n, deviceOverride{BinarySwitchLabel: "switch"})
	require.NoError(t, err)
	assert.Equal(t, "switch", d.GetModelDescription())
}

func TestSwitchBuilder_ApplyState(t *testing.T) {
	n := nodeInfo{ID: 7}
	n.DeviceClass.Generic = genericBinarySwitch
	d, _, err := switchBuilder{}.build(n, deviceOverride{})
	require.NoError(t, err)

	switchBuilder{}.applyState(d, "on_off", []byte("true"))
	assert.True(t, d.GetGeneric().OnOff.State.IsOn)
	switchBuilder{}.applyState(d, "on_off", []byte("false"))
	assert.False(t, d.GetGeneric().OnOff.State.IsOn)
}

func TestSwitchBuilder_ApplyCommand(t *testing.T) {
	n := nodeInfo{ID: 7}
	n.DeviceClass.Generic = genericBinarySwitch
	d, roles, err := switchBuilder{}.build(n, deviceOverride{})
	require.NoError(t, err)

	mc, fc := newTestMQTTConn(t, mqttConfig{Prefix: "zwave"})
	topic := mc.topicFor(roles["on_off"])
	fc.respond(topic+"/set", topic, []byte("true"))

	cmd := &command.Command{Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}}}
	err = switchBuilder{}.applyCommand(context.Background(), mc, roles, d, cmd)
	require.NoError(t, err)
	assert.True(t, d.GetGeneric().OnOff.State.IsOn)
}

func TestSwitchBuilder_ApplyCommand_Unsupported(t *testing.T) {
	n := nodeInfo{ID: 7}
	n.DeviceClass.Generic = genericBinarySwitch
	d, roles, _ := switchBuilder{}.build(n, deviceOverride{})
	mc, _ := newTestMQTTConn(t, mqttConfig{Prefix: "zwave"})

	cmd := &command.Command{Details: &command.Command_BrightnessAbsolute{BrightnessAbsolute: &command.BrightnessAbsolute{BrightnessPercent: 50}}}
	err := switchBuilder{}.applyCommand(context.Background(), mc, roles, d, cmd)
	assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
}

// --- dimmerBuilder ---

func TestDimmerBuilder_Build(t *testing.T) {
	n := nodeInfo{ID: 3, Values: map[string]nodeValue{
		"v": nv(ccMultilevelSwitch, 0, "targetValue", "", 50),
	}}
	n.DeviceClass.Generic = genericMultilevelSwitch

	d, roles, err := dimmerBuilder{}.build(n, deviceOverride{})
	require.NoError(t, err)
	assert.True(t, d.GetLight().OnOff.State.IsOn)
	assert.Equal(t, zwaveLevelToPercent(50), d.GetLight().Brightness.State.Level)
	assert.Equal(t, valueID{nodeID: 3, topicBase: "nodeID_3", commandClass: ccMultilevelSwitch, endpoint: 0, property: "targetValue"}, roles["level"])
}

func TestDimmerBuilder_Build_ZeroLevelIsOff(t *testing.T) {
	n := nodeInfo{ID: 3, Values: map[string]nodeValue{
		"v": nv(ccMultilevelSwitch, 0, "targetValue", "", 0),
	}}
	n.DeviceClass.Generic = genericMultilevelSwitch

	d, _, err := dimmerBuilder{}.build(n, deviceOverride{})
	require.NoError(t, err)
	assert.False(t, d.GetLight().OnOff.State.IsOn)
}

func TestDimmerBuilder_ApplyCommand_BrightnessAbsolute(t *testing.T) {
	n := nodeInfo{ID: 3}
	n.DeviceClass.Generic = genericMultilevelSwitch
	d, roles, err := dimmerBuilder{}.build(n, deviceOverride{})
	require.NoError(t, err)

	mc, fc := newTestMQTTConn(t, mqttConfig{Prefix: "zwave"})
	topic := mc.topicFor(roles["level"])
	wantLevel := percentToZwaveLevel(75)
	fc.respond(topic+"/set", topic, []byte(json.RawMessage(mustJSON(wantLevel))))

	cmd := &command.Command{Details: &command.Command_BrightnessAbsolute{BrightnessAbsolute: &command.BrightnessAbsolute{BrightnessPercent: 75}}}
	err = dimmerBuilder{}.applyCommand(context.Background(), mc, roles, d, cmd)
	require.NoError(t, err)
	assert.Equal(t, int32(75), d.GetLight().Brightness.State.Level)
	assert.True(t, d.GetLight().OnOff.State.IsOn)
}

func TestDimmerBuilder_ApplyCommand_OnOff(t *testing.T) {
	n := nodeInfo{ID: 3}
	n.DeviceClass.Generic = genericMultilevelSwitch
	d, roles, err := dimmerBuilder{}.build(n, deviceOverride{})
	require.NoError(t, err)

	mc, fc := newTestMQTTConn(t, mqttConfig{Prefix: "zwave"})
	topic := mc.topicFor(roles["level"])
	fc.respond(topic+"/set", topic, []byte("0"))

	cmd := &command.Command{Details: &command.Command_OnOff{OnOff: &command.OnOff{On: false}}}
	err = dimmerBuilder{}.applyCommand(context.Background(), mc, roles, d, cmd)
	require.NoError(t, err)
	assert.False(t, d.GetLight().OnOff.State.IsOn)
	assert.Equal(t, int32(0), d.GetLight().Brightness.State.Level)
}

// TestDimmerBuilder_ApplyCommand_BrightnessRelative covers resolveLevelCommand's
// BrightnessRelative branch - shared by dimmerBuilder and rgbLightBuilder - which had no test
// coverage at all despite being a real, reachable command path.
func TestDimmerBuilder_ApplyCommand_BrightnessRelative(t *testing.T) {
	n := nodeInfo{ID: 3, Values: map[string]nodeValue{
		"v": nv(ccMultilevelSwitch, 0, "targetValue", "", percentToZwaveLevel(40)),
	}}
	n.DeviceClass.Generic = genericMultilevelSwitch
	d, roles, err := dimmerBuilder{}.build(n, deviceOverride{})
	require.NoError(t, err)
	require.Equal(t, int32(40), d.GetLight().Brightness.State.Level)

	mc, fc := newTestMQTTConn(t, mqttConfig{Prefix: "zwave"})
	topic := mc.topicFor(roles["level"])
	wantLevel := percentToZwaveLevel(55)
	fc.respond(topic+"/set", topic, mustJSON(wantLevel))

	cmd := &command.Command{Details: &command.Command_BrightnessRelative{BrightnessRelative: &command.BrightnessRelative{ChangePercent: 15}}}
	err = dimmerBuilder{}.applyCommand(context.Background(), mc, roles, d, cmd)
	require.NoError(t, err)
	assert.Equal(t, int32(55), d.GetLight().Brightness.State.Level)
	assert.True(t, d.GetLight().OnOff.State.IsOn)
}

// TestDimmerBuilder_ApplyCommand_BrightnessRelative_ClampsAtZero confirms resolveLevelCommand's
// clampPercent call actually stops a large negative ChangePercent from underflowing below 0.
func TestDimmerBuilder_ApplyCommand_BrightnessRelative_ClampsAtZero(t *testing.T) {
	n := nodeInfo{ID: 3, Values: map[string]nodeValue{
		"v": nv(ccMultilevelSwitch, 0, "targetValue", "", percentToZwaveLevel(10)),
	}}
	n.DeviceClass.Generic = genericMultilevelSwitch
	d, roles, err := dimmerBuilder{}.build(n, deviceOverride{})
	require.NoError(t, err)

	mc, fc := newTestMQTTConn(t, mqttConfig{Prefix: "zwave"})
	topic := mc.topicFor(roles["level"])
	fc.respond(topic+"/set", topic, []byte("0"))

	cmd := &command.Command{Details: &command.Command_BrightnessRelative{BrightnessRelative: &command.BrightnessRelative{ChangePercent: -50}}}
	err = dimmerBuilder{}.applyCommand(context.Background(), mc, roles, d, cmd)
	require.NoError(t, err)
	assert.Equal(t, int32(0), d.GetLight().Brightness.State.Level)
	assert.False(t, d.GetLight().OnOff.State.IsOn)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// --- sensorBuilder ---

func TestSensorBuilder_Build_FullSensor(t *testing.T) {
	n := nodeInfo{ID: 12, Values: map[string]nodeValue{
		"motion":  nv(ccNotification, 0, "Home Security", "Motion sensor status", 8),
		"temp":    nv(ccMultilevelSensor, 0, "Air temperature", "", 21.5),
		"humid":   nv(ccMultilevelSensor, 0, "Humidity", "", 45.0),
		"lux":     nv(ccMultilevelSensor, 0, "Illuminance", "", 100.0),
		"uv":      nv(ccMultilevelSensor, 0, "Ultraviolet", "", 3),
		"batt":    nv(ccBattery, 0, "level", "", 80),
		"battLow": nv(ccBattery, 0, "isLow", "", false),
	}}
	n.DeviceClass.Generic = genericSensorNotification

	d, roles, err := sensorBuilder{}.build(n, deviceOverride{})
	require.NoError(t, err)

	s := d.GetSensor()
	require.NotNil(t, s.Presence)
	assert.True(t, s.Presence.State.MotionDetected)
	require.NotNil(t, s.AirProperties)
	assert.InDelta(t, 21.5, s.AirProperties.State.TemperatureC, 0.01)
	assert.InDelta(t, 45.0, s.AirProperties.State.HumidityPercentage, 0.01)
	require.NotNil(t, s.LightLevel)
	assert.InDelta(t, 100.0, s.LightLevel.State.Lux, 0.01)
	require.NotNil(t, s.LightLevel.State.UvIndex)
	assert.Equal(t, int32(3), *s.LightLevel.State.UvIndex)
	require.NotNil(t, s.Battery)
	assert.Equal(t, int32(80), s.Battery.State.CapacityRemainingPct)
	require.NotNil(t, s.Metadata)
	assert.True(t, s.Metadata.OnBattery)
	assert.False(t, s.Metadata.LowBattery)

	assert.Len(t, roles, 7)
}

func TestSensorBuilder_Build_DeadNode_PresenceOnlyByConvention(t *testing.T) {
	n := nodeInfo{ID: 12} // no Values at all - dead/never-interviewed
	n.DeviceClass.Generic = genericSensorNotification

	d, roles, err := sensorBuilder{}.build(n, deviceOverride{})
	require.NoError(t, err)

	s := d.GetSensor()
	require.NotNil(t, s.Presence, "presence must be asserted unconditionally from the generic class convention")
	assert.False(t, s.Presence.State.MotionDetected)
	assert.Nil(t, s.AirProperties)
	assert.Nil(t, s.LightLevel)
	assert.Nil(t, s.Battery)
	assert.Len(t, roles, 1)
	assert.Contains(t, roles, "presence")
}

func TestSensorBuilder_Build_BinarySensorFallback(t *testing.T) {
	n := nodeInfo{ID: 12, Values: map[string]nodeValue{
		"motion": nv(ccBinarySensor, 0, "Motion", "", true),
	}}
	n.DeviceClass.Generic = genericSensorNotification

	d, roles, err := sensorBuilder{}.build(n, deviceOverride{})
	require.NoError(t, err)
	assert.True(t, d.GetSensor().Presence.State.MotionDetected)
	assert.Equal(t, valueID{nodeID: 12, topicBase: "nodeID_12", commandClass: ccBinarySensor, endpoint: 0, property: "Motion"}, roles["presence"],
		"a node with no Notification CC value but a Binary Sensor CC 'Motion' value must route through that instead")
}

func TestSensorBuilder_Build_NotificationPreferredOverBinarySensor(t *testing.T) {
	n := nodeInfo{ID: 12, Values: map[string]nodeValue{
		"notif":  nv(ccNotification, 0, "Home Security", "Motion sensor status", 8),
		"binary": nv(ccBinarySensor, 0, "Motion", "", false),
	}}
	n.DeviceClass.Generic = genericSensorNotification

	_, roles, err := sensorBuilder{}.build(n, deviceOverride{})
	require.NoError(t, err)
	assert.Equal(t, ccNotification, roles["presence"].commandClass)
}

func TestSensorBuilder_ApplyState_MotionAcceptsBothEncodings(t *testing.T) {
	n := nodeInfo{ID: 12}
	n.DeviceClass.Generic = genericSensorNotification
	d, _, err := sensorBuilder{}.build(n, deviceOverride{})
	require.NoError(t, err)

	// Binary Sensor CC (48/0x30) encoding: bare boolean.
	sensorBuilder{}.applyState(d, "presence", []byte("true"))
	assert.True(t, d.GetSensor().Presence.State.MotionDetected)
	sensorBuilder{}.applyState(d, "presence", []byte("false"))
	assert.False(t, d.GetSensor().Presence.State.MotionDetected)

	// Notification CC (113) encoding: bare number.
	sensorBuilder{}.applyState(d, "presence", []byte("8"))
	assert.True(t, d.GetSensor().Presence.State.MotionDetected)
	sensorBuilder{}.applyState(d, "presence", []byte("0"))
	assert.False(t, d.GetSensor().Presence.State.MotionDetected)
}

func TestSensorBuilder_Build_NotificationStatesLabelOverridesRawNonzero(t *testing.T) {
	v := nv(ccNotification, 0, "Home Security", "Motion sensor status", 3)
	v.States = []nodeValueState{nvState("idle", 0), nvState("Event cleared", 3)}
	n := nodeInfo{ID: 12, Values: map[string]nodeValue{"m": v}}
	n.DeviceClass.Generic = genericSensorNotification

	d, _, err := sensorBuilder{}.build(n, deviceOverride{})
	require.NoError(t, err)
	assert.False(t, d.GetSensor().Presence.State.MotionDetected,
		"a states label positively indicating idle/cleared should override the raw nonzero value")
}

func TestSensorBuilder_Build_NotificationStatesLabelConfirmsDetected(t *testing.T) {
	v := nv(ccNotification, 0, "Home Security", "Motion sensor status", 7)
	v.States = []nodeValueState{nvState("idle", 0), nvState("Motion detected (location provided)", 7)}
	n := nodeInfo{ID: 12, Values: map[string]nodeValue{"m": v}}
	n.DeviceClass.Generic = genericSensorNotification

	d, _, err := sensorBuilder{}.build(n, deviceOverride{})
	require.NoError(t, err)
	assert.True(t, d.GetSensor().Presence.State.MotionDetected)
}

func TestSensorBuilder_ApplyState(t *testing.T) {
	n := nodeInfo{ID: 12, Values: map[string]nodeValue{
		"temp": nv(ccMultilevelSensor, 0, "Air temperature", "", 20.0),
	}}
	n.DeviceClass.Generic = genericSensorNotification
	d, _, err := sensorBuilder{}.build(n, deviceOverride{})
	require.NoError(t, err)

	sensorBuilder{}.applyState(d, "air_temperature", []byte("22.5"))
	assert.InDelta(t, 22.5, d.GetSensor().AirProperties.State.TemperatureC, 0.01)

	sensorBuilder{}.applyState(d, "presence", []byte("8"))
	assert.True(t, d.GetSensor().Presence.State.MotionDetected)
	sensorBuilder{}.applyState(d, "presence", []byte("0"))
	assert.False(t, d.GetSensor().Presence.State.MotionDetected)
}

func TestSensorBuilder_ApplyCommand_AlwaysUnsupported(t *testing.T) {
	n := nodeInfo{ID: 12}
	n.DeviceClass.Generic = genericSensorNotification
	d, roles, _ := sensorBuilder{}.build(n, deviceOverride{})
	mc, _ := newTestMQTTConn(t, mqttConfig{Prefix: "zwave"})

	err := sensorBuilder{}.applyCommand(context.Background(), mc, roles, d, &command.Command{Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}}})
	assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
}

// --- rgbLightBuilder ---

func TestRGBLightBuilder_Build_MissingOverride(t *testing.T) {
	n := nodeInfo{ID: 4, Values: map[string]nodeValue{
		"c": nv(ccColorSwitch, 0, "currentColor", "2", 255),
	}}
	n.DeviceClass.Generic = genericMultilevelSwitch

	_, _, err := rgbLightBuilder{}.build(n, deviceOverride{})
	require.Error(t, err)
}

func TestRGBLightBuilder_Build_IncompleteOverride(t *testing.T) {
	n := nodeInfo{ID: 4}
	n.DeviceClass.Generic = genericMultilevelSwitch

	_, _, err := rgbLightBuilder{}.build(n, deviceOverride{ColourChannels: map[string]string{"red": "2"}})
	require.Error(t, err)
}

func TestRGBLightBuilder_Build_Success(t *testing.T) {
	n := nodeInfo{ID: 4, Values: map[string]nodeValue{
		"level": nv(ccMultilevelSwitch, 0, "targetValue", "", 99),
		"red":   nv(ccColorSwitch, 0, "currentColor", "2", 255),
		"green": nv(ccColorSwitch, 0, "currentColor", "3", 128),
		"blue":  nv(ccColorSwitch, 0, "currentColor", "4", 0),
	}}
	n.DeviceClass.Generic = genericMultilevelSwitch
	override := deviceOverride{ColourChannels: map[string]string{"red": "2", "green": "3", "blue": "4"}}

	d, roles, err := rgbLightBuilder{}.build(n, override)
	require.NoError(t, err)
	require.NotNil(t, d.GetLight().Colour)
	assert.Equal(t, int32(255), d.GetLight().Colour.State.Rgb.Red)
	assert.Equal(t, int32(128), d.GetLight().Colour.State.Rgb.Green)
	assert.Equal(t, int32(0), d.GetLight().Colour.State.Rgb.Blue)
	assert.True(t, d.GetLight().OnOff.State.IsOn)
	assert.Len(t, roles, 4)
}

func TestRGBLightBuilder_Build_DefaultColourPropertyNames(t *testing.T) {
	n := nodeInfo{ID: 4}
	n.DeviceClass.Generic = genericMultilevelSwitch
	override := deviceOverride{ColourChannels: map[string]string{"red": "2", "green": "3", "blue": "4"}}

	_, roles, err := rgbLightBuilder{}.build(n, override)
	require.NoError(t, err)
	assert.Equal(t, "targetColor", roles["colour_red"].property)
}

func TestRGBLightBuilder_Build_ColourPropertyOverride(t *testing.T) {
	n := nodeInfo{ID: 4, Values: map[string]nodeValue{
		"red": nv(ccColorSwitch, 0, "currentValue", "2", 200),
	}}
	n.DeviceClass.Generic = genericMultilevelSwitch
	override := deviceOverride{
		ColourChannels:        map[string]string{"red": "2", "green": "3", "blue": "4"},
		ColourTargetProperty:  "targetValue",
		ColourCurrentProperty: "currentValue",
	}

	d, roles, err := rgbLightBuilder{}.build(n, override)
	require.NoError(t, err)
	assert.Equal(t, "targetValue", roles["colour_red"].property,
		"colour_target_property override must replace the default 'targetColor' property name")
	assert.Equal(t, int32(200), d.GetLight().Colour.State.Rgb.Red,
		"colour_current_property override must be used to read the initial value too")
}

func TestRGBLightBuilder_ApplyCommand_Colour(t *testing.T) {
	n := nodeInfo{ID: 4}
	n.DeviceClass.Generic = genericMultilevelSwitch
	override := deviceOverride{ColourChannels: map[string]string{"red": "2", "green": "3", "blue": "4"}}
	d, roles, err := rgbLightBuilder{}.build(n, override)
	require.NoError(t, err)

	mc, fc := newTestMQTTConn(t, mqttConfig{Prefix: "zwave"})
	for _, ch := range []string{"red", "green", "blue"} {
		topic := mc.topicFor(roles["colour_"+ch])
		var val []byte
		switch ch {
		case "red":
			val = []byte("10")
		case "green":
			val = []byte("20")
		case "blue":
			val = []byte("30")
		}
		fc.respond(topic+"/set", topic, val)
	}

	cmd := &command.Command{Details: &command.Command_Colour{Colour: &command.Colour{
		Value: &command.Colour_Rgb{Rgb: &command.Colour_RGB{Red: 10, Green: 20, Blue: 30}},
	}}}
	err = rgbLightBuilder{}.applyCommand(context.Background(), mc, roles, d, cmd)
	require.NoError(t, err)
	assert.Equal(t, int32(10), d.GetLight().Colour.State.Rgb.Red)
	assert.Equal(t, int32(20), d.GetLight().Colour.State.Rgb.Green)
	assert.Equal(t, int32(30), d.GetLight().Colour.State.Rgb.Blue)
}

func TestRGBLightBuilder_ApplyCommand_Colour_ClampsOutOfRangeChannels(t *testing.T) {
	n := nodeInfo{ID: 4}
	n.DeviceClass.Generic = genericMultilevelSwitch
	override := deviceOverride{ColourChannels: map[string]string{"red": "2", "green": "3", "blue": "4"}}
	d, roles, err := rgbLightBuilder{}.build(n, override)
	require.NoError(t, err)

	mc, fc := newTestMQTTConn(t, mqttConfig{Prefix: "zwave"})
	// Arm each channel to echo back the clamped value this bridge should actually write, not the
	// out-of-range value the command requested.
	fc.respond(mc.topicFor(roles["colour_red"])+"/set", mc.topicFor(roles["colour_red"]), mustJSON(0))
	fc.respond(mc.topicFor(roles["colour_green"])+"/set", mc.topicFor(roles["colour_green"]), mustJSON(255))
	fc.respond(mc.topicFor(roles["colour_blue"])+"/set", mc.topicFor(roles["colour_blue"]), mustJSON(255))

	cmd := &command.Command{Details: &command.Command_Colour{Colour: &command.Colour{
		Value: &command.Colour_Rgb{Rgb: &command.Colour_RGB{Red: -5, Green: 300, Blue: 256}},
	}}}
	err = rgbLightBuilder{}.applyCommand(context.Background(), mc, roles, d, cmd)
	require.NoError(t, err)
	assert.Equal(t, int32(0), d.GetLight().Colour.State.Rgb.Red, "a negative channel must be clamped to 0 before writing")
	assert.Equal(t, int32(255), d.GetLight().Colour.State.Rgb.Green, "a >255 channel must be clamped to 255 before writing")
	assert.Equal(t, int32(255), d.GetLight().Colour.State.Rgb.Blue, "a >255 channel must be clamped to 255 before writing")
}

func TestRGBLightBuilder_ApplyCommand_PartialFailure(t *testing.T) {
	orig := writeTimeout
	writeTimeout = 50 * time.Millisecond
	defer func() { writeTimeout = orig }()

	n := nodeInfo{ID: 4}
	n.DeviceClass.Generic = genericMultilevelSwitch
	override := deviceOverride{ColourChannels: map[string]string{"red": "2", "green": "3", "blue": "4"}}
	d, roles, err := rgbLightBuilder{}.build(n, override)
	require.NoError(t, err)

	mc, fc := newTestMQTTConn(t, mqttConfig{Prefix: "zwave"})
	// Only arm red and green - blue never gets an echo, simulating a partially-accepted write.
	fc.respond(mc.topicFor(roles["colour_red"])+"/set", mc.topicFor(roles["colour_red"]), []byte("10"))
	fc.respond(mc.topicFor(roles["colour_green"])+"/set", mc.topicFor(roles["colour_green"]), []byte("20"))

	cmd := &command.Command{Details: &command.Command_Colour{Colour: &command.Colour{
		Value: &command.Colour_Rgb{Rgb: &command.Colour_RGB{Red: 10, Green: 20, Blue: 30}},
	}}}
	err = rgbLightBuilder{}.applyCommand(context.Background(), mc, roles, d, cmd)
	assert.ErrorIs(t, err, bridge.ErrCommandTimeout)
}
