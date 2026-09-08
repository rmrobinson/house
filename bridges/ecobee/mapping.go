package main

import (
	"encoding/json"
	"fmt"

	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/bridges/lib/homekitctrl"
)

// Characteristic IDs captured from the specific ecobee Smart Thermostat Premium (firmware
// 4.10.330030) this bridge was built and tested against - see docs/ecobee-hap-dump.txt. The
// standard (Apple-defined) ones are part of the public HAP spec, but iid numbering is still
// assigned per-unit/firmware, not fixed by the spec itself; the vendor ones (undocumented UUIDs)
// are unit/firmware-specific in both iid AND meaning. If this bridge is ever pointed at a
// different ecobee, redo the docs/ecobee-hap-dump.txt capture (see README) and update this table
// - don't assume it carries over.
const thermostatAID uint64 = 1

const (
	charCurrentHeatingCoolingState uint64 = 17
	charTargetHeatingCoolingState  uint64 = 18
	charCurrentTemperature         uint64 = 19
	charTargetTemperature          uint64 = 20
	charTemperatureDisplayUnits    uint64 = 21
	charCoolingThreshold           uint64 = 22
	charHeatingThreshold           uint64 = 23
	charCurrentRelativeHumidity    uint64 = 24
	charTargetRelativeHumidity     uint64 = 25

	// The thermostat's own built-in occupancy/motion sensing (Premium's radar "SmartSensor"),
	// living on the same aid as the thermostat itself - not a separately paired remote sensor.
	charBuiltinMotionDetected    uint64 = 66
	charBuiltinOccupancyDetected uint64 = 65

	charTargetFanState  uint64 = 75
	charCurrentFanState uint64 = 76

	// Vendor comfort-profile characteristics. Their meaning was confirmed by directly changing
	// the thermostat's active profile on the physical unit and diffing before/after captures -
	// not guessed from static values alone. See README for what wasn't confirmed this way.
	charActiveComfortProfile uint64 = 33 // read-only index: 0=Home, 1=Sleep, 2=Away
	charHomeHeatSetpoint     uint64 = 34
	charHomeCoolSetpoint     uint64 = 35
	charAwayHeatSetpoint     uint64 = 36
	charAwayCoolSetpoint     uint64 = 37
	charSleepHeatSetpoint    uint64 = 38
	charSleepCoolSetpoint    uint64 = 39
	charSetComfortProfile    uint64 = 40 // write-only; takes the same index as charActiveComfortProfile
	charHoldUntil            uint64 = 41 // empty string = no active hold
	charClearHold            uint64 = 48 // write-only bool; true resumes the schedule
	charOnSchedule           uint64 = 51 // read-only: 1 = following schedule, 0 = hold active
	charVentilatorFilterMsg  uint64 = 54 // read-only string
)

const (
	thermostatModelID      = "ECB601"
	thermostatManufacturer = "ecobee Inc."
)

// Setpoint/humidity bounds, from the real characteristic metadata captured in
// docs/ecobee-hap-dump.txt. Shared between buildThermostatDevice (advertised as Attributes) and
// command.go (enforced before writing) so the two can't drift apart.
const (
	minSetpointCelsius  float32 = 7.2
	maxSetpointCelsius  float32 = 33.3
	setpointStepCelsius float32 = 0.1
)

// requiredThermostatChars are checked for presence after every read. A missing value here can't
// be distinguished from a real zero/false/CELSIUS by the charValues accessors below, and several
// device.Thermostat fields have no "unknown" representation to fall back on (current_temperature
// _celsius is a plain, non-optional float) - so rather than silently publish a device built from
// a value that's actually just absent, Refresh treats a miss on any of these as a failed poll
// entirely. This is deliberately a short list of the fields where a wrong-but-plausible default
// would be actively misleading (e.g. 0°C, or "on" from a missing mode) - not a check on
// everything this bridge reads.
var requiredThermostatChars = []uint64{
	charCurrentHeatingCoolingState,
	charTargetHeatingCoolingState,
	charCurrentTemperature,
	charTemperatureDisplayUnits,
}

// validateRequired reports which of the given characteristic IDs are missing from c entirely.
func (c charValues) validateRequired(required []uint64) error {
	var missing []uint64
	for _, id := range required {
		if _, ok := c[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required characteristics: %v", missing)
	}
	return nil
}

// comfortProfileNames are the three built-in ecobee climates, indexed to match
// charActiveComfortProfile's value (0=Home, 1=Sleep, 2=Away - confirmed against the real unit).
// Note this is a DIFFERENT order than the heat/cool setpoint characteristics below - ecobee's own
// internal profile enumeration doesn't match its setpoint storage order.
var comfortProfileNames = []string{"Home", "Sleep", "Away"}

func comfortProfileIndex(name string) (int, bool) {
	for i, n := range comfortProfileNames {
		if n == name {
			return i, true
		}
	}
	return 0, false
}

// thermostatCharIDs returns every characteristic this bridge reads on each poll cycle for the
// thermostat accessory (aid=thermostatAID) itself - not remote sensors, see remoteSensorCharIDs.
func thermostatCharIDs() []homekitctrl.CharID {
	ids := []uint64{
		charCurrentHeatingCoolingState, charTargetHeatingCoolingState,
		charCurrentTemperature, charTargetTemperature, charTemperatureDisplayUnits,
		charCoolingThreshold, charHeatingThreshold,
		charCurrentRelativeHumidity, charTargetRelativeHumidity,
		charBuiltinMotionDetected, charBuiltinOccupancyDetected,
		charTargetFanState, charCurrentFanState,
		charActiveComfortProfile,
		charHomeHeatSetpoint, charHomeCoolSetpoint,
		charAwayHeatSetpoint, charAwayCoolSetpoint,
		charSleepHeatSetpoint, charSleepCoolSetpoint,
		charHoldUntil, charOnSchedule, charVentilatorFilterMsg,
	}
	out := make([]homekitctrl.CharID, len(ids))
	for i, id := range ids {
		out[i] = homekitctrl.CharID{AccessoryID: thermostatAID, CharacteristicID: id}
	}
	return out
}

// charValues indexes a single accessory's characteristic values by characteristic ID, for
// convenient lookup while building a device. Values are the raw JSON HAP returned, decoded
// lazily by the typed accessors below.
type charValues map[uint64]json.RawMessage

func indexCharValues(values []homekitctrl.CharacteristicValue, aid uint64) charValues {
	m := make(charValues, len(values))
	for _, v := range values {
		if v.AccessoryID == aid {
			m[v.CharacteristicID] = v.Value
		}
	}
	return m
}

func (c charValues) float(id uint64) (float64, bool) {
	raw, ok := c[id]
	if !ok || len(raw) == 0 {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, false
	}
	return f, true
}

func (c charValues) int(id uint64) (int, bool) {
	f, ok := c.float(id)
	return int(f), ok
}

// boolean characteristics on this unit sometimes report format "bool" but encode their value as
// a JSON number (0/1) rather than a JSON boolean literal - tolerate both rather than panicking or
// silently misreading, the same fix applied to the standalone capture tooling in
// bridges/ecobee/docs.
func (c charValues) bool(id uint64) (bool, bool) {
	raw, ok := c[id]
	if !ok || len(raw) == 0 {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, true
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n != 0, true
	}
	return false, false
}

func (c charValues) string(id uint64) (string, bool) {
	raw, ok := c[id]
	if !ok || len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func ptr[T any](v T) *T {
	return &v
}

func hvacStateToMode(v int, ok bool) trait.Thermostat_Mode {
	if !ok {
		return trait.Thermostat_MODE_UNSPECIFIED
	}
	switch v {
	case 0:
		return trait.Thermostat_OFF
	case 1:
		return trait.Thermostat_HEAT
	case 2:
		return trait.Thermostat_COOL
	case 3:
		return trait.Thermostat_AUTO
	default:
		return trait.Thermostat_MODE_UNSPECIFIED
	}
}

// buildThermostatDevice translates the thermostat accessory's current characteristic values
// (aid=thermostatAID) into a house device.Device. id is the house device ID to stamp onto it;
// name comes from the bridge's config, not the accessory (its AccessoryInformation Name
// characteristic is a room label like "Main Floor", not something the operator necessarily wants
// to use verbatim as the house device name).
//
// Assumes values already passed validateRequired(requiredThermostatChars) - current_mode,
// target_mode, current_temperature_celsius and display_units have no "unknown" representation to
// fall back on if their source characteristic is missing (current_temperature_celsius in
// particular is a plain, non-optional float, so a missing read and a genuine 0°C reading would
// otherwise be indistinguishable), so this function trusts the caller already rejected an
// incomplete read rather than guarding each field here too.
func buildThermostatDevice(id, name string, values charValues) *device.Device {
	currentMode := hvacStateToMode(values.int(charCurrentHeatingCoolingState))
	targetMode := hvacStateToMode(values.int(charTargetHeatingCoolingState))

	currentTempC, _ := values.float(charCurrentTemperature) // presence guaranteed - see doc comment above
	currentHumidity, hasHumidity := values.float(charCurrentRelativeHumidity)

	state := &trait.Thermostat_State{
		CurrentMode:               currentMode,
		TargetMode:                targetMode,
		CurrentTemperatureCelsius: float32(currentTempC),
	}
	if hasHumidity {
		state.CurrentHumidityPercent = ptr(float32(currentHumidity))
	}
	if v, ok := values.float(charTargetRelativeHumidity); ok {
		state.TargetHumidityPercent = ptr(float32(v))
	}
	// Presence guaranteed - see doc comment above - so the else branch is a real 0/CELSIUS
	// reading, not a stand-in for "missing."
	if v, _ := values.int(charTemperatureDisplayUnits); v == 1 {
		state.DisplayUnits = trait.Thermostat_FAHRENHEIT
	} else {
		state.DisplayUnits = trait.Thermostat_CELSIUS
	}

	// heat/cool setpoints are populated based on target_mode: HEAT/COOL use the single
	// TargetTemperature characteristic as the active setpoint; AUTO uses the two threshold
	// characteristics as the low/high band. OFF has nothing to control.
	targetTemp, hasTargetTemp := values.float(charTargetTemperature)
	switch targetMode {
	case trait.Thermostat_HEAT:
		if hasTargetTemp {
			state.HeatSetpointCelsius = ptr(float32(targetTemp))
		}
	case trait.Thermostat_COOL:
		if hasTargetTemp {
			state.CoolSetpointCelsius = ptr(float32(targetTemp))
		}
	case trait.Thermostat_AUTO:
		if v, ok := values.float(charHeatingThreshold); ok {
			state.HeatSetpointCelsius = ptr(float32(v))
		}
		if v, ok := values.float(charCoolingThreshold); ok {
			state.CoolSetpointCelsius = ptr(float32(v))
		}
	}

	if index, ok := values.int(charActiveComfortProfile); ok && index >= 0 && index < len(comfortProfileNames) {
		state.ActiveComfortProfile = ptr(comfortProfileNames[index])
	}
	if holdUntil, ok := values.string(charHoldUntil); ok {
		state.HoldActive = ptr(holdUntil != "")
	}

	if v, ok := values.int(charTargetFanState); ok {
		if v == 1 {
			state.FanMode = "auto"
		} else {
			state.FanMode = "on"
		}
	}

	d := &device.Device{
		Id:           id,
		ModelId:      thermostatModelID,
		Manufacturer: thermostatManufacturer,
		Config:       &device.Device_Config{Name: name},
		Details: &device.Device_Thermostat{
			Thermostat: &device.Thermostat{
				OnOff: &trait.OnOff{
					Attributes: &trait.OnOff_Attributes{CanControl: true},
					State:      &trait.OnOff_State{IsOn: targetMode != trait.Thermostat_OFF},
				},
				Thermostat: &trait.Thermostat{
					Attributes: &trait.Thermostat_Attributes{
						CanControl: true,
						SupportedModes: []trait.Thermostat_Mode{
							trait.Thermostat_OFF, trait.Thermostat_HEAT, trait.Thermostat_COOL, trait.Thermostat_AUTO,
						},
						MinSetpointCelsius:  minSetpointCelsius,
						MaxSetpointCelsius:  maxSetpointCelsius,
						SetpointStepCelsius: setpointStepCelsius,
						ComfortProfiles:     comfortProfileNames,
						// Deliberately no Min/Max/TargetHumidityStepPercent here: this bridge
						// doesn't implement SetHumidity (no humidifier attached to this house's
						// ecobee - see command.go), and trait.Thermostat has no per-feature
						// can_control flag the way trait.Ventilation does, so advertising a
						// controllable-looking range here would be misleading. State.
						// target_humidity_percent below is still populated as read-only telemetry.
					},
					State: state,
				},
				AirProperties: &trait.AirProperties{
					Attributes: &trait.AirProperties_Attributes{},
					State: &trait.AirProperties_State{
						TemperatureC:       float32(currentTempC),
						HumidityPercentage: float32(currentHumidity),
					},
				},
				Presence: &trait.Presence{
					Attributes: &trait.Presence_Attributes{},
					State:      &trait.Presence_State{},
				},
			},
		},
	}

	presence := d.GetThermostat().Presence
	if v, ok := values.bool(charBuiltinMotionDetected); ok {
		presence.State.MotionDetected = v
	}
	if v, ok := values.bool(charBuiltinOccupancyDetected); ok {
		presence.State.OccupancyDetected = ptr(v)
	}

	// The ventilator filter reminder is read-only informational text - see README for why
	// on/off control isn't wired here (this ecobee doesn't expose it over HAP at all).
	if msg, ok := values.string(charVentilatorFilterMsg); ok && msg != "" {
		d.GetThermostat().Ventilation = &trait.Ventilation{
			Attributes: &trait.Ventilation_Attributes{CanControl: false},
			State:      &trait.Ventilation_State{FilterStatusMessage: ptr(msg)},
		}
	}

	return d
}
