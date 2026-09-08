package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/bridges/lib/homekitctrl"
)

// rawValues builds a charValues fixture from Go values, mirroring what ReadCharacteristics
// returns off the wire (JSON-encoded values keyed by characteristic ID).
func rawValues(t *testing.T, byID map[uint64]any) charValues {
	t.Helper()

	var values []homekitctrl.CharacteristicValue
	for id, v := range byID {
		b, err := json.Marshal(v)
		require.NoError(t, err)
		values = append(values, homekitctrl.CharacteristicValue{AccessoryID: thermostatAID, CharacteristicID: id, Value: b})
	}
	return indexCharValues(values, thermostatAID)
}

// baselineThermostatValues mirrors the real capture in docs/ecobee-hap-dump.txt: Home profile
// active, Cool mode, no hold.
func baselineThermostatValues(t *testing.T) map[uint64]any {
	t.Helper()
	return map[uint64]any{
		charCurrentHeatingCoolingState: 2,
		charTargetHeatingCoolingState:  2,
		charCurrentTemperature:         22.8,
		charTargetTemperature:          23.0,
		charTemperatureDisplayUnits:    0,
		charCoolingThreshold:           23.0,
		charHeatingThreshold:           21.0,
		charCurrentRelativeHumidity:    68.0,
		charTargetRelativeHumidity:     36.0,
		charBuiltinMotionDetected:      false,
		charBuiltinOccupancyDetected:   1,
		charTargetFanState:             1,
		charCurrentFanState:            2,
		charActiveComfortProfile:       0,
		charHomeHeatSetpoint:           21.0,
		charHomeCoolSetpoint:           23.0,
		charAwayHeatSetpoint:           18.0,
		charAwayCoolSetpoint:           25.0,
		charSleepHeatSetpoint:          18.0,
		charSleepCoolSetpoint:          24.0,
		charHoldUntil:                  "",
		charOnSchedule:                 1,
		charVentilatorFilterMsg:        "Time to change your ventilator filter.  It was last changed on May 30, 2026.",
	}
}

func TestBuildThermostatDeviceBaseline(t *testing.T) {
	values := rawValues(t, baselineThermostatValues(t))

	d := buildThermostatDevice("ecobee-main", "Main Floor Thermostat", values)

	require.Equal(t, "ecobee-main", d.Id)
	require.Equal(t, thermostatModelID, d.ModelId)

	th := d.GetThermostat()
	require.NotNil(t, th)

	assert.True(t, th.OnOff.State.IsOn)
	assert.Equal(t, trait.Thermostat_COOL, th.Thermostat.State.CurrentMode)
	assert.Equal(t, trait.Thermostat_COOL, th.Thermostat.State.TargetMode)
	assert.Equal(t, float32(22.8), th.Thermostat.State.CurrentTemperatureCelsius)

	// COOL mode: the single TargetTemperature characteristic becomes the cool setpoint; heat
	// setpoint is left unset since there's nothing meaningful to report.
	require.NotNil(t, th.Thermostat.State.CoolSetpointCelsius)
	assert.Equal(t, float32(23.0), *th.Thermostat.State.CoolSetpointCelsius)
	assert.Nil(t, th.Thermostat.State.HeatSetpointCelsius)

	require.NotNil(t, th.Thermostat.State.ActiveComfortProfile)
	assert.Equal(t, "Home", *th.Thermostat.State.ActiveComfortProfile)

	require.NotNil(t, th.Thermostat.State.HoldActive)
	assert.False(t, *th.Thermostat.State.HoldActive)

	assert.Equal(t, "auto", th.Thermostat.State.FanMode)

	assert.False(t, th.Presence.State.MotionDetected)
	require.NotNil(t, th.Presence.State.OccupancyDetected)
	assert.True(t, *th.Presence.State.OccupancyDetected)

	require.NotNil(t, th.Ventilation)
	require.NotNil(t, th.Ventilation.State.FilterStatusMessage)
	assert.Contains(t, *th.Ventilation.State.FilterStatusMessage, "ventilator filter")
	assert.False(t, th.Ventilation.Attributes.CanControl)
}

func TestBuildThermostatDeviceAutoModeUsesThresholdPair(t *testing.T) {
	raw := baselineThermostatValues(t)
	raw[charTargetHeatingCoolingState] = 3 // AUTO
	values := rawValues(t, raw)

	th := buildThermostatDevice("ecobee-main", "Main Floor Thermostat", values).GetThermostat()

	require.NotNil(t, th.Thermostat.State.HeatSetpointCelsius)
	require.NotNil(t, th.Thermostat.State.CoolSetpointCelsius)
	assert.Equal(t, float32(21.0), *th.Thermostat.State.HeatSetpointCelsius)
	assert.Equal(t, float32(23.0), *th.Thermostat.State.CoolSetpointCelsius)
}

func TestBuildThermostatDeviceHoldActiveWhenTimestampPresent(t *testing.T) {
	raw := baselineThermostatValues(t)
	raw[charActiveComfortProfile] = 2 // Away
	raw[charHoldUntil] = "2035-01-03T00:00:00-04:00"
	values := rawValues(t, raw)

	th := buildThermostatDevice("ecobee-main", "Main Floor Thermostat", values).GetThermostat()

	require.NotNil(t, th.Thermostat.State.ActiveComfortProfile)
	assert.Equal(t, "Away", *th.Thermostat.State.ActiveComfortProfile)
	require.NotNil(t, th.Thermostat.State.HoldActive)
	assert.True(t, *th.Thermostat.State.HoldActive)
}

func TestBuildRemoteSensorDevice(t *testing.T) {
	var values []homekitctrl.CharacteristicValue
	set := func(id uint64, v any) {
		b, err := json.Marshal(v)
		require.NoError(t, err)
		values = append(values, homekitctrl.CharacteristicValue{AccessoryID: 4297248826, CharacteristicID: id, Value: b})
	}
	set(remoteSensorCharCurrentTemperature, 23.3)
	set(remoteSensorCharBatteryLevel, 100)
	set(remoteSensorCharBatteryChargingState, 2)
	set(remoteSensorCharMotionDetected, false)
	set(remoteSensorCharOccupancyDetected, 0)

	cv := indexCharValues(values, 4297248826)
	d := buildRemoteSensorDevice("ecobee-downstairs", "Downstairs", cv)

	s := d.GetSensor()
	require.NotNil(t, s)
	assert.Equal(t, float32(23.3), s.AirProperties.State.TemperatureC)
	assert.Equal(t, int32(100), s.Battery.State.CapacityRemainingPct)
	assert.True(t, s.Battery.State.Discharging)
	assert.False(t, s.Presence.State.MotionDetected)
	require.NotNil(t, s.Presence.State.OccupancyDetected)
	assert.False(t, *s.Presence.State.OccupancyDetected)
}

func TestValidateRequiredCatchesIncompleteRead(t *testing.T) {
	complete := rawValues(t, baselineThermostatValues(t))
	assert.NoError(t, complete.validateRequired(requiredThermostatChars))

	raw := baselineThermostatValues(t)
	delete(raw, charCurrentTemperature)
	incomplete := rawValues(t, raw)

	err := incomplete.validateRequired(requiredThermostatChars)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "19") // charCurrentTemperature's iid
}
