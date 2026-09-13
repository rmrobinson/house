package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
)

// dimmerFullOnLevel is the Z-Wave Multilevel Switch level (0-99 scale) this bridge writes for a
// plain OnOff-only "on" command against a dimmer or RGB(W) bulb (i.e. one with no explicit
// brightness/colour in the command). 99, not 255 - the CC's "restore last value" sentinel isn't
// used since this bridge already tracks the device's last known level itself.
const dimmerFullOnLevel = 99

// deviceBuilder is implemented once per house device type this bridge supports. A node is
// dispatched to its builder by networkConn.classify, based on its Z-Wave generic device class
// (and, for Multilevel Switch, whether it also reports a Color Switch CC value) - see network.go.
type deviceBuilder interface {
	// build constructs the initial house Device for a discovered node, along with the
	// role->valueId table applyState/applyCommand route through. override carries this node's
	// config escape hatches, keyed by its DeviceID - see config.go.
	build(n nodeInfo, override deviceOverride) (*device.Device, map[string]valueID, error)

	// applyState mutates the device in response to a value update received for one of its roles.
	applyState(d *device.Device, role string, raw json.RawMessage)

	// applyCommand translates a house Command into one or more WriteValue calls against the
	// node's roles, and updates the device optimistically. The normal value-topic stream
	// corrects this if the node disagrees.
	applyCommand(ctx context.Context, m *mqttConn, roles map[string]valueID, d *device.Device, cmd *command.Command) error
}

// findValue looks up a node's cached value by its CC/endpoint/property/propertyKey, returning
// false if the node has no cached value there - which happens for a dead or never-interviewed
// node even when its generic device class implies the capability should exist. See the design
// doc's "Node/value discovery" and "Sensor capability discovery" sections for which callers
// should still assert the role unconditionally (the base capability a generic class always
// implies) versus only when a value is actually found here (optional sensor traits, whose
// presence genuinely varies by node).
func findValue(n nodeInfo, cc, endpoint int, property, propertyKey string) (nodeValue, bool) {
	for _, v := range n.Values {
		if v.CommandClass == cc && v.Endpoint == endpoint && v.Property == property && v.PropertyKey == propertyKey {
			return v, true
		}
	}
	return nodeValue{}, false
}

// nodeTopicBase resolves the "[<location>/]<nodeName-or-nodeID_N>" portion of every MQTT topic
// zwave-js-ui publishes for n, under its named-topics scheme: n.Loc verbatim (it may itself
// contain multiple "/"-separated segments, e.g. "second_floor/guest_bedroom") followed by n.Name,
// or - confirmed against a real broker for a node with no name assigned - "nodeID_<id>" in its
// place. Confirmed against a real zwave-js-ui broker for both a located, named node and an
// unlocated, unnamed one (the controller node); a located-but-unnamed node's exact topic shape is
// this bridge's extrapolation from that pattern, not independently confirmed.
func nodeTopicBase(n nodeInfo) string {
	name := n.Name
	if name == "" {
		name = fmt.Sprintf("nodeID_%d", n.ID)
	}
	if n.Loc == "" {
		return name
	}
	return n.Loc + "/" + name
}

// --- Switch ---

// switchBuilder builds a house Generic/OnOff device from a Binary Switch (CC 37) node. Generic,
// not a dedicated Switch/Outlet device type, since this repo has none yet - see the design doc's
// "One real gap, not blocking" note. classifyBinarySwitchLabel's result is surfaced via
// ModelDescription purely as a cosmetic hint (plug vs switch) since it doesn't change the trait
// shape either way.
type switchBuilder struct{}

func (switchBuilder) build(n nodeInfo, override deviceOverride) (*device.Device, map[string]valueID, error) {
	v := valueID{nodeID: n.ID, topicBase: nodeTopicBase(n), commandClass: ccBinarySwitch, endpoint: 0, property: "targetValue"}

	isOn := false
	if nv, ok := findValue(n, ccBinarySwitch, 0, "targetValue", ""); ok {
		_ = json.Unmarshal(nv.Value, &isOn)
	} else if nv, ok := findValue(n, ccBinarySwitch, 0, "currentValue", ""); ok {
		_ = json.Unmarshal(nv.Value, &isOn)
	}

	label := classifyBinarySwitchLabel(n, override)

	d := &device.Device{
		Manufacturer:     "Z-Wave",
		ModelDescription: &label,
		Details: &device.Device_Generic{
			Generic: &device.Generic{
				OnOff: &trait.OnOff{
					Attributes: &trait.OnOff_Attributes{CanControl: true},
					State:      &trait.OnOff_State{IsOn: isOn},
				},
			},
		},
	}
	return d, map[string]valueID{"on_off": v}, nil
}

func (switchBuilder) applyState(d *device.Device, role string, raw json.RawMessage) {
	if role != "on_off" {
		return
	}
	var isOn bool
	if err := json.Unmarshal(raw, &isOn); err != nil {
		return
	}
	d.GetGeneric().OnOff.State.IsOn = isOn
}

func (switchBuilder) applyCommand(ctx context.Context, m *mqttConn, roles map[string]valueID, d *device.Device, cmd *command.Command) error {
	if cmd.GetOnOff() == nil {
		return bridge.ErrUnsupportedCommand
	}
	v, ok := roles["on_off"]
	if !ok {
		return bridge.ErrUnsupportedCommand
	}

	on := cmd.GetOnOff().On
	if err := m.WriteValue(ctx, v, on); err != nil {
		return err
	}
	d.GetGeneric().OnOff.State.IsOn = on
	return nil
}

// classifyBinarySwitchLabel resolves the "does this binary switch drive a light or a plug"
// ambiguity the design doc's "Disambiguating device role from device class" section describes:
// deviceClass.specific doesn't reliably distinguish them, so this first tries a config override
// (one entry per distinct product, keyed by DeviceID), then falls back to a keyword match against
// the embedded zwave-js-ui device database's description/label.
func classifyBinarySwitchLabel(n nodeInfo, override deviceOverride) string {
	if override.BinarySwitchLabel != "" {
		return override.BinarySwitchLabel
	}
	desc := strings.ToLower(n.DeviceConfig.Description + " " + n.DeviceConfig.Label)
	if strings.Contains(desc, "plug") || strings.Contains(desc, "outlet") {
		return "plug"
	}
	return "switch"
}

// --- Dimmer ---

// dimmerBuilder builds a house Light/OnOff+Brightness device from a Multilevel Switch (CC 38)
// node with no Color Switch (CC 51) value - see networkConn.classify.
type dimmerBuilder struct{}

func (dimmerBuilder) build(n nodeInfo, _ deviceOverride) (*device.Device, map[string]valueID, error) {
	v := valueID{nodeID: n.ID, topicBase: nodeTopicBase(n), commandClass: ccMultilevelSwitch, endpoint: 0, property: "targetValue"}
	level := readLevel(n)

	d := &device.Device{
		Manufacturer: "Z-Wave",
		Details: &device.Device_Light{
			Light: &device.Light{
				OnOff: &trait.OnOff{
					Attributes: &trait.OnOff_Attributes{CanControl: true},
					State:      &trait.OnOff_State{IsOn: level > 0},
				},
				Brightness: &trait.Brightness{
					Attributes: &trait.Brightness_Attributes{CanControl: true},
					State:      &trait.Brightness_State{Level: zwaveLevelToPercent(level)},
				},
			},
		},
	}
	return d, map[string]valueID{"level": v}, nil
}

func (dimmerBuilder) applyState(d *device.Device, role string, raw json.RawMessage) {
	if role != "level" {
		return
	}
	var level int
	if err := json.Unmarshal(raw, &level); err != nil {
		return
	}
	l := d.GetLight()
	l.OnOff.State.IsOn = level > 0
	l.Brightness.State.Level = zwaveLevelToPercent(level)
}

func (dimmerBuilder) applyCommand(ctx context.Context, m *mqttConn, roles map[string]valueID, d *device.Device, cmd *command.Command) error {
	v, ok := roles["level"]
	if !ok {
		return bridge.ErrUnsupportedCommand
	}
	l := d.GetLight()

	level, pct, err := resolveLevelCommand(cmd, l.Brightness.State.Level)
	if err != nil {
		return err
	}

	if err := m.WriteValue(ctx, v, level); err != nil {
		return err
	}
	l.Brightness.State.Level = pct
	l.OnOff.State.IsOn = level > 0
	return nil
}

// resolveLevelCommand translates the OnOff/BrightnessAbsolute/BrightnessRelative commands shared
// by dimmerBuilder and rgbLightBuilder into a Z-Wave level (0-99) and the resulting house
// brightness percent, given the light's current percent (needed for BrightnessRelative).
func resolveLevelCommand(cmd *command.Command, currentPct int32) (level int, pct int32, err error) {
	switch {
	case cmd.GetOnOff() != nil:
		if cmd.GetOnOff().On {
			return dimmerFullOnLevel, zwaveLevelToPercent(dimmerFullOnLevel), nil
		}
		return 0, 0, nil

	case cmd.GetBrightnessAbsolute() != nil:
		pct = clampPercent(cmd.GetBrightnessAbsolute().BrightnessPercent)
		return percentToZwaveLevel(pct), pct, nil

	case cmd.GetBrightnessRelative() != nil:
		pct = clampPercent(currentPct + cmd.GetBrightnessRelative().ChangePercent)
		return percentToZwaveLevel(pct), pct, nil

	default:
		return 0, 0, bridge.ErrUnsupportedCommand
	}
}

func readLevel(n nodeInfo) int {
	level := 0
	if nv, ok := findValue(n, ccMultilevelSwitch, 0, "targetValue", ""); ok {
		_ = json.Unmarshal(nv.Value, &level)
	} else if nv, ok := findValue(n, ccMultilevelSwitch, 0, "currentValue", ""); ok {
		_ = json.Unmarshal(nv.Value, &level)
	}
	return level
}

// zwaveLevelToPercent converts a Multilevel Switch level (0-99) to house's 0-100 percent scale.
func zwaveLevelToPercent(level int) int32 {
	if level > 99 {
		level = 99
	}
	if level < 0 {
		level = 0
	}
	return int32(math.Round(float64(level) * 100 / 99))
}

// percentToZwaveLevel converts house's 0-100 percent scale to a Multilevel Switch level (0-99).
func percentToZwaveLevel(pct int32) int {
	pct = clampPercent(pct)
	return int(math.Round(float64(pct) * 99 / 100))
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

// clampColourChannel clamps a command.Colour.RGB channel value to the 0-255 range its own proto
// doc comment promises, but that nothing upstream of rgbLightBuilder actually enforces.
func clampColourChannel(v int32) int32 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

// --- Sensor ---

// sensorBuilder builds a house Sensor device from a Sensor Notification (generic class 0x07)
// node. Presence is asserted unconditionally, from the generic class's own convention (per the
// design doc's "Sensor capability discovery" section, this is the one base capability the generic
// class always implies); AirProperties/LightLevel/Battery are only populated - and only get
// role entries at all - for whichever CC 49/128 values are actually present in this node's
// getNodes response, since not every Sensor Notification node is a full multi-capability sensor.
type sensorBuilder struct{}

func (sensorBuilder) build(n nodeInfo, _ deviceOverride) (*device.Device, map[string]valueID, error) {
	roles := make(map[string]valueID)
	sensor := &device.Sensor{}

	presenceID, motion := findMotionValue(n)
	roles["presence"] = presenceID
	sensor.Presence = &trait.Presence{Attributes: &trait.Presence_Attributes{}, State: &trait.Presence_State{MotionDetected: motion}}

	if nv, ok := findValue(n, ccMultilevelSensor, 0, "Air temperature", ""); ok {
		roles["air_temperature"] = nv.id(n)
		ensureAirProperties(sensor)
		sensor.AirProperties.State.TemperatureC = decodeFloat32(nv.Value)
	}
	if nv, ok := findValue(n, ccMultilevelSensor, 0, "Humidity", ""); ok {
		roles["air_humidity"] = nv.id(n)
		ensureAirProperties(sensor)
		sensor.AirProperties.State.HumidityPercentage = decodeFloat32(nv.Value)
	}

	if nv, ok := findValue(n, ccMultilevelSensor, 0, "Illuminance", ""); ok {
		roles["light_illuminance"] = nv.id(n)
		ensureLightLevel(sensor)
		lux := decodeFloat32(nv.Value)
		sensor.LightLevel.State.Lux = lux
		sensor.LightLevel.State.LightLevel = luxToLightLevel(lux)
	}
	if nv, ok := findValue(n, ccMultilevelSensor, 0, "Ultraviolet", ""); ok {
		roles["light_uv"] = nv.id(n)
		ensureLightLevel(sensor)
		uv := int32(decodeFloat32(nv.Value))
		sensor.LightLevel.State.UvIndex = &uv
	}

	if nv, ok := findValue(n, ccBattery, 0, "level", ""); ok {
		roles["battery_level"] = nv.id(n)
		ensureBattery(sensor)
		var pct int32
		_ = json.Unmarshal(nv.Value, &pct)
		sensor.Battery.State.CapacityRemainingPct = pct
	}
	if nv, ok := findValue(n, ccBattery, 0, "isLow", ""); ok {
		roles["battery_low"] = nv.id(n)
		ensureMetadata(sensor)
		var low bool
		_ = json.Unmarshal(nv.Value, &low)
		sensor.Metadata.LowBattery = low
	}
	if sensor.Battery != nil {
		ensureMetadata(sensor)
		sensor.Metadata.OnBattery = true
	}

	d := &device.Device{
		Manufacturer: "Z-Wave",
		Details:      &device.Device_Sensor{Sensor: sensor},
	}
	return d, roles, nil
}

func (sensorBuilder) applyState(d *device.Device, role string, raw json.RawMessage) {
	s := d.GetSensor()
	switch role {
	case "presence":
		s.Presence.State.MotionDetected = decodeMotionDetected(raw)
	case "air_temperature":
		s.AirProperties.State.TemperatureC = decodeFloat32(raw)
	case "air_humidity":
		s.AirProperties.State.HumidityPercentage = decodeFloat32(raw)
	case "light_illuminance":
		lux := decodeFloat32(raw)
		s.LightLevel.State.Lux = lux
		s.LightLevel.State.LightLevel = luxToLightLevel(lux)
	case "light_uv":
		uv := int32(decodeFloat32(raw))
		s.LightLevel.State.UvIndex = &uv
	case "battery_level":
		var pct int32
		if err := json.Unmarshal(raw, &pct); err != nil {
			return
		}
		s.Battery.State.CapacityRemainingPct = pct
	case "battery_low":
		var low bool
		if err := json.Unmarshal(raw, &low); err != nil {
			return
		}
		s.Metadata.LowBattery = low
	}
}

// applyCommand always fails: every trait a Sensor device can carry (Presence, AirProperties,
// LightLevel, Battery) is read-only, per the design doc's phase 1 scope (locks/thermostats/scenes
// are explicitly out of scope, and none of the in-scope sensor traits are controllable).
func (sensorBuilder) applyCommand(_ context.Context, _ *mqttConn, _ map[string]valueID, _ *device.Device, _ *command.Command) error {
	return bridge.ErrUnsupportedCommand
}

// findMotionValue locates a Sensor Notification-class node's motion-detection value, preferring
// Notification CC (113)'s "Home Security"/"Motion sensor status" - the device variant the design
// doc verified against real hardware - and falling back to Binary Sensor CC (48/0x30)'s flat
// "Motion" boolean for older/simpler devices that report motion that way instead. These are
// genuinely different wire encodings (an event-style small integer with a per-device states
// label list, vs. a plain boolean with no label metadata at all), not just a naming difference,
// so build's initial-state decode and applyState's live-update decode both need to know which
// encoding a given node actually uses - the returned valueID's commandClass records that choice
// for applyState to key off of via decodeMotionDetected.
//
// If neither is present yet (a dead/never-interviewed node), returns the Notification CC
// convention valueID with motionDetected false, so routing/topic construction is still
// well-defined once the node completes its first interview - same "build regardless of live
// status" convention used for the always-present base traits elsewhere in this bridge.
func findMotionValue(n nodeInfo) (valueID, bool) {
	if nv, ok := findValue(n, ccNotification, 0, "Home Security", "Motion sensor status"); ok {
		return nv.id(n), motionDetectedFromNotification(nv.Value, nv.States)
	}
	if nv, ok := findValue(n, ccBinarySensor, 0, "Motion", ""); ok {
		return nv.id(n), decodeMotionDetected(nv.Value)
	}
	return valueID{nodeID: n.ID, topicBase: nodeTopicBase(n), commandClass: ccNotification, endpoint: 0, property: "Home Security", propertyKey: "Motion sensor status"}, false
}

// motionDetectedFromNotification decodes a Notification CC "Motion sensor status" value using its
// own device-reported states label when available, rather than assuming a fixed numeric code (the
// design doc found the specific "detected" value varies by device - e.g. 7 vs 8 depending on
// model). 0 is a fixed idle/cleared sentinel across every Notification CC event type per the
// Z-Wave specification, so that check doesn't need a label. For a nonzero value, if a states label
// is available and its wording positively reads as idle/cleared, this trusts the label over the
// raw nonzero-ness; with no label at all, nonzero still means detected, since this is only ever
// called against the "Motion sensor status" propertyKey specifically (not the general "Home
// Security" notification, which also carries unrelated tamper/glass-break events under other
// propertyKeys) - a propertyKey this scoped shouldn't have a non-idle, non-motion value.
//
// States is only present on getNodes' cached value snapshot (used here, at build time), never on
// a live value-topic update - see decodeMotionDetected, which applyState uses instead for exactly
// that reason. That means this label-aware check can't be repeated for a live update to the same
// value, only for the node's state as of the last discovery pass; accepted as a known limitation
// rather than plumbing the states list through to every future update just for this one case.
func motionDetectedFromNotification(raw json.RawMessage, states []nodeValueState) bool {
	var v int
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	if v == 0 {
		return false
	}
	for _, s := range states {
		if !valuesEqual(s.Value, v) {
			continue
		}
		lower := strings.ToLower(s.Text)
		if strings.Contains(lower, "idle") || strings.Contains(lower, "clear") {
			return false
		}
		break
	}
	return true
}

// decodeMotionDetected decodes a live presence-role value-topic update, where no states label is
// available (see motionDetectedFromNotification's doc comment). Notification CC (113) publishes
// this as a bare number (0 = idle, nonzero = detected); Binary Sensor CC (48/0x30) publishes it as
// a bare boolean. Trying both keeps this correct regardless of which encoding the specific node
// findMotionValue matched uses.
func decodeMotionDetected(raw json.RawMessage) bool {
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	var v int
	if err := json.Unmarshal(raw, &v); err == nil {
		return v != 0
	}
	return false
}

func decodeFloat32(raw json.RawMessage) float32 {
	var f float64
	_ = json.Unmarshal(raw, &f)
	return float32(f)
}

// luxToLightLevel derives LightLevel.State.light_level from a lux reading, per the formula
// documented on that field: 10000*log10(lux)+1.
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

func ensureLightLevel(s *device.Sensor) {
	if s.LightLevel == nil {
		s.LightLevel = &trait.LightLevel{Attributes: &trait.LightLevel_Attributes{CanControl: false}, State: &trait.LightLevel_State{}}
	}
}

func ensureBattery(s *device.Sensor) {
	if s.Battery == nil {
		s.Battery = &trait.Battery{Attributes: &trait.Battery_Attributes{}, State: &trait.Battery_State{}}
	}
}

func ensureMetadata(s *device.Sensor) {
	if s.Metadata == nil {
		s.Metadata = &device.Sensor_Metadata{}
	}
}

// --- RGB(W) Light ---

// rgbLightBuilder builds a house Light/OnOff+Brightness+Colour device from a Multilevel Switch
// (CC 38, on/off + brightness) node that also reports a Color Switch (CC 51) value. Per the
// design doc, CC 51's propertyKey numbering for color components is not fixed across devices, so
// this builder refuses to guess: it requires a colour_channels config override (see config.go)
// naming this product's actual "red"/"green"/"blue" propertyKeys, confirmed against the bulb's
// own nodeinfo/valueId list rather than assumed from a datasheet. A bulb with no override
// configured fails to build with an explanatory error rather than silently writing to the wrong
// channel.
type rgbLightBuilder struct{}

var rgbRequiredChannels = []string{"red", "green", "blue"}

func (rgbLightBuilder) build(n nodeInfo, override deviceOverride) (*device.Device, map[string]valueID, error) {
	if len(override.ColourChannels) == 0 {
		return nil, nil, fmt.Errorf("zwave: node %d (device_id %q) is an RGB(W) bulb but has no colour_channels override configured - confirm this bulb's actual CC 51 propertyKey numbering against its nodeinfo/valueId list and add a colour_channels entry for device_id %q before it can be controlled (see README)", n.ID, n.DeviceID, n.DeviceID)
	}
	for _, ch := range rgbRequiredChannels {
		if _, ok := override.ColourChannels[ch]; !ok {
			return nil, nil, fmt.Errorf("zwave: node %d colour_channels override is missing required channel %q", n.ID, ch)
		}
	}

	roles := make(map[string]valueID)

	levelID := valueID{nodeID: n.ID, topicBase: nodeTopicBase(n), commandClass: ccMultilevelSwitch, endpoint: 0, property: "targetValue"}
	roles["level"] = levelID
	level := readLevel(n)

	// "targetColor"/"currentColor" are this bridge's best-guess default for CC 51's zwave-js-ui
	// property names - not independently verified against live hardware, same as the propertyKey
	// numbers above, but with a working default rather than failing closed (see
	// deviceOverride.ColourTargetProperty's doc comment for the override escape hatch).
	targetProp := override.ColourTargetProperty
	if targetProp == "" {
		targetProp = "targetColor"
	}
	currentProp := override.ColourCurrentProperty
	if currentProp == "" {
		currentProp = "currentColor"
	}

	rgb := &trait.Colour_State_RGB{}
	for _, ch := range rgbRequiredChannels {
		pk := override.ColourChannels[ch]
		roles["colour_"+ch] = valueID{nodeID: n.ID, topicBase: nodeTopicBase(n), commandClass: ccColorSwitch, endpoint: 0, property: targetProp, propertyKey: pk}
		if nv, ok := findValue(n, ccColorSwitch, 0, currentProp, pk); ok {
			var val int32
			_ = json.Unmarshal(nv.Value, &val)
			setColourChannel(rgb, ch, val)
		}
	}

	d := &device.Device{
		Manufacturer: "Z-Wave",
		Details: &device.Device_Light{
			Light: &device.Light{
				OnOff: &trait.OnOff{
					Attributes: &trait.OnOff_Attributes{CanControl: true},
					State:      &trait.OnOff_State{IsOn: level > 0},
				},
				Brightness: &trait.Brightness{
					Attributes: &trait.Brightness_Attributes{CanControl: true},
					State:      &trait.Brightness_State{Level: zwaveLevelToPercent(level)},
				},
				Colour: &trait.Colour{
					Attributes: &trait.Colour_Attributes{CanControl: true, Mode: trait.Colour_Attributes_MODE_RGB},
					State:      &trait.Colour_State{Rgb: rgb},
				},
			},
		},
	}
	return d, roles, nil
}

func (rgbLightBuilder) applyState(d *device.Device, role string, raw json.RawMessage) {
	l := d.GetLight()
	switch {
	case role == "level":
		var level int
		if err := json.Unmarshal(raw, &level); err != nil {
			return
		}
		l.OnOff.State.IsOn = level > 0
		l.Brightness.State.Level = zwaveLevelToPercent(level)
	case strings.HasPrefix(role, "colour_"):
		var val int32
		if err := json.Unmarshal(raw, &val); err != nil {
			return
		}
		setColourChannel(l.Colour.State.Rgb, strings.TrimPrefix(role, "colour_"), val)
	}
}

func (rgbLightBuilder) applyCommand(ctx context.Context, m *mqttConn, roles map[string]valueID, d *device.Device, cmd *command.Command) error {
	l := d.GetLight()

	if cmd.GetColour() != nil {
		rgb := cmd.GetColour().GetRgb()
		if rgb == nil {
			// Only MODE_RGB is supported by this builder today (colour_temperature_k/hsb would
			// need their own config override shape) - see Colour.Attributes.mode on the built
			// device.
			return bridge.ErrUnsupportedCommand
		}
		redID, okR := roles["colour_red"]
		greenID, okG := roles["colour_green"]
		blueID, okB := roles["colour_blue"]
		if !okR || !okG || !okB {
			return bridge.ErrUnsupportedCommand
		}

		// command.Colour.RGB's fields are documented as 0-255 but nothing upstream of this builder
		// enforces that - clamp before writing so an out-of-range component (a buggy caller, or a
		// BrightnessRelative-style accumulation elsewhere) can't put an arbitrary int32 on the wire
		// for a CC 51 property that a real device may not validate itself.
		red := clampColourChannel(rgb.Red)
		green := clampColourChannel(rgb.Green)
		blue := clampColourChannel(rgb.Blue)

		// Partial failure (e.g. 2 of 3 channels accepted) is possible and isn't rolled back -
		// physical Z-Wave devices don't support transactional multi-CC writes. See the README's
		// known limitations section. Because WriteValue's echo correlation is per-topic, these
		// three calls need no shared serialization.
		g, gctx := errgroup.WithContext(ctx)
		g.Go(func() error { return m.WriteValue(gctx, redID, red) })
		g.Go(func() error { return m.WriteValue(gctx, greenID, green) })
		g.Go(func() error { return m.WriteValue(gctx, blueID, blue) })
		if err := g.Wait(); err != nil {
			return err
		}
		l.Colour.State.Rgb = &trait.Colour_State_RGB{Red: red, Green: green, Blue: blue}
		return nil
	}

	v, ok := roles["level"]
	if !ok {
		return bridge.ErrUnsupportedCommand
	}
	level, pct, err := resolveLevelCommand(cmd, l.Brightness.State.Level)
	if err != nil {
		return err
	}
	if err := m.WriteValue(ctx, v, level); err != nil {
		return err
	}
	l.Brightness.State.Level = pct
	l.OnOff.State.IsOn = level > 0
	return nil
}

func setColourChannel(rgb *trait.Colour_State_RGB, channel string, val int32) {
	switch channel {
	case "red":
		rgb.Red = val
	case "green":
		rgb.Green = val
	case "blue":
		rgb.Blue = val
	}
}
