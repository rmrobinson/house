package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
)

// testDeviceWithMode builds a minimal device.Device with just enough shape for
// thermostatCommandWrites to read/mutate its thermostat state.
func testDeviceWithMode(mode trait.Thermostat_Mode) *device.Device {
	return &device.Device{
		Id: "ecobee-main",
		Details: &device.Device_Thermostat{
			Thermostat: &device.Thermostat{
				Thermostat: &trait.Thermostat{
					State: &trait.Thermostat_State{TargetMode: mode},
				},
			},
		},
	}
}

func TestThermostatCommandWritesSetThermostatMode(t *testing.T) {
	cmd := &command.Command{Details: &command.Command_SetThermostatMode{
		SetThermostatMode: &command.SetThermostatMode{Mode: trait.Thermostat_HEAT},
	}}

	d := testDeviceWithMode(trait.Thermostat_COOL)
	writes, err := thermostatCommandWrites(cmd, d)
	require.NoError(t, err)
	require.Len(t, writes, 1)
	assert.Equal(t, thermostatAID, writes[0].AccessoryID)
	assert.Equal(t, charTargetHeatingCoolingState, writes[0].CharacteristicID)
	assert.Equal(t, 1, writes[0].Value) // HEAT

	assert.Equal(t, trait.Thermostat_HEAT, d.GetThermostat().GetThermostat().GetState().TargetMode)
}

func TestThermostatCommandWritesSetTemperatureInCoolMode(t *testing.T) {
	cmd := &command.Command{Details: &command.Command_SetTemperature{
		SetTemperature: &command.SetTemperature{SetpointCelsius: 24.5},
	}}

	d := testDeviceWithMode(trait.Thermostat_COOL)
	writes, err := thermostatCommandWrites(cmd, d)
	require.NoError(t, err)
	require.Len(t, writes, 1)
	assert.Equal(t, charTargetTemperature, writes[0].CharacteristicID)
	assert.Equal(t, float32(24.5), writes[0].Value)

	require.NotNil(t, d.GetThermostat().GetThermostat().GetState().CoolSetpointCelsius)
	assert.Equal(t, float32(24.5), *d.GetThermostat().GetThermostat().GetState().CoolSetpointCelsius)
}

func TestThermostatCommandWritesSetTemperatureRejectsAutoMode(t *testing.T) {
	cmd := &command.Command{Details: &command.Command_SetTemperature{
		SetTemperature: &command.SetTemperature{SetpointCelsius: 24.5},
	}}

	_, err := thermostatCommandWrites(cmd, testDeviceWithMode(trait.Thermostat_AUTO))
	assert.Error(t, err)
}

func TestThermostatCommandWritesSetTemperatureRejectsOutOfRange(t *testing.T) {
	cmd := &command.Command{Details: &command.Command_SetTemperature{
		SetTemperature: &command.SetTemperature{SetpointCelsius: 100},
	}}

	_, err := thermostatCommandWrites(cmd, testDeviceWithMode(trait.Thermostat_COOL))
	assert.Error(t, err)
}

func TestThermostatCommandWritesSetHeatCoolSetpoints(t *testing.T) {
	cmd := &command.Command{Details: &command.Command_SetHeatCoolSetpoints{
		SetHeatCoolSetpoints: &command.SetHeatCoolSetpoints{HeatSetpointCelsius: 20, CoolSetpointCelsius: 25},
	}}

	writes, err := thermostatCommandWrites(cmd, testDeviceWithMode(trait.Thermostat_AUTO))
	require.NoError(t, err)
	require.Len(t, writes, 2)
	assert.Equal(t, charHeatingThreshold, writes[0].CharacteristicID)
	assert.Equal(t, float32(20), writes[0].Value)
	assert.Equal(t, charCoolingThreshold, writes[1].CharacteristicID)
	assert.Equal(t, float32(25), writes[1].Value)
}

func TestThermostatCommandWritesSetHeatCoolSetpointsRejectsOutOfRange(t *testing.T) {
	cmd := &command.Command{Details: &command.Command_SetHeatCoolSetpoints{
		SetHeatCoolSetpoints: &command.SetHeatCoolSetpoints{HeatSetpointCelsius: 5, CoolSetpointCelsius: 25},
	}}

	_, err := thermostatCommandWrites(cmd, testDeviceWithMode(trait.Thermostat_AUTO))
	assert.Error(t, err)
}

func TestThermostatCommandWritesSetComfortProfile(t *testing.T) {
	cmd := &command.Command{Details: &command.Command_SetComfortProfile{
		SetComfortProfile: &command.SetComfortProfile{ProfileName: "Away"},
	}}

	d := testDeviceWithMode(trait.Thermostat_COOL)
	writes, err := thermostatCommandWrites(cmd, d)
	require.NoError(t, err)
	require.Len(t, writes, 1)
	assert.Equal(t, charSetComfortProfile, writes[0].CharacteristicID)
	assert.Equal(t, 2, writes[0].Value) // Away = index 2

	require.NotNil(t, d.GetThermostat().GetThermostat().GetState().ActiveComfortProfile)
	assert.Equal(t, "Away", *d.GetThermostat().GetThermostat().GetState().ActiveComfortProfile)
}

func TestThermostatCommandWritesSetComfortProfileRejectsUnknownName(t *testing.T) {
	cmd := &command.Command{Details: &command.Command_SetComfortProfile{
		SetComfortProfile: &command.SetComfortProfile{ProfileName: "Vacation"},
	}}

	_, err := thermostatCommandWrites(cmd, testDeviceWithMode(trait.Thermostat_COOL))
	assert.Error(t, err)
}

func TestThermostatCommandWritesRejectsSetHumidity(t *testing.T) {
	cmd := &command.Command{Details: &command.Command_SetHumidity{
		SetHumidity: &command.SetHumidity{TargetHumidityPercent: 40},
	}}

	_, err := thermostatCommandWrites(cmd, testDeviceWithMode(trait.Thermostat_COOL))
	require.Error(t, err)
	assert.Equal(t, bridge.ErrUnsupportedCommand, err)
}

func TestThermostatCommandWritesRejectsSetVentilation(t *testing.T) {
	cmd := &command.Command{Details: &command.Command_SetVentilation{
		SetVentilation: &command.SetVentilation{On: true},
	}}

	_, err := thermostatCommandWrites(cmd, testDeviceWithMode(trait.Thermostat_COOL))
	require.Error(t, err)
	assert.Equal(t, bridge.ErrUnsupportedCommand, err)
}
