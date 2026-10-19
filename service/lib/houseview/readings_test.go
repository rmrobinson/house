package houseview

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/proto"

	apiDevice "github.com/rmrobinson/house/api/device"
	apiTrait "github.com/rmrobinson/house/api/trait"
)

func TestReadings(t *testing.T) {
	assert.Empty(t, Readings(&apiDevice.Device{}))

	ups := &apiDevice.Device{Details: &apiDevice.Device_Ups{Ups: &apiDevice.UPS{
		OnOff:   &apiTrait.OnOff{},
		Battery: &apiTrait.Battery{State: &apiTrait.Battery_State{CapacityRemainingPct: 100, CapacityRemainingMins: 75}},
		Power:   &apiTrait.Power{State: &apiTrait.Power_State{VoltageV: 121.5, PowerW: 80}},
	}}}
	assert.Equal(t, []Reading{
		{Label: "Battery", Value: "100%", Metric: "battery.capacity_remaining_pct"},
		{Label: "Runtime", Value: "1h 15m", Metric: "battery.capacity_remaining_mins"},
		{Label: "Power", Value: "80.0 W", Metric: "power.power_w"},
		{Label: "Voltage", Value: "121.5 V", Metric: "power.voltage_v"},
	}, Readings(ups))

	energy := 3.5
	light := &apiDevice.Device{Details: &apiDevice.Device_Light{Light: &apiDevice.Light{
		OnOff: &apiTrait.OnOff{},
		Power: &apiTrait.Power{State: &apiTrait.Power_State{PowerW: 42.5, EnergyKwh: &energy}},
	}}}
	assert.Equal(t, []Reading{
		{Label: "Power", Value: "42.5 W", Metric: "power.power_w"},
		{Label: "Energy", Value: "3.50 kWh", Metric: "power.energy_kwh"},
	}, Readings(light))

	fan := &apiDevice.Device{Details: &apiDevice.Device_Fan{Fan: &apiDevice.Fan{
		Temperature: &apiTrait.Temperature{State: &apiTrait.Temperature_State{Value: 23.5}},
	}}}
	assert.Equal(t, []Reading{{Label: "Temp", Value: "23.5°C", Metric: "temperature.value"}}, Readings(fan))

	sensor := &apiDevice.Device{Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
		AirProperties: &apiTrait.AirProperties{State: &apiTrait.AirProperties_State{TemperatureC: 21, HumidityPercentage: 44}},
		AirQuality:    &apiTrait.AirQuality{State: &apiTrait.AirQuality_State{Co2Ppm: proto.Int32(650)}},
		Water:         &apiDevice.Sensor_BinarySensor{IsActive: true},
	}}}
	assert.Equal(t, []Reading{
		{Label: "Temp", Value: "21.0°C", Metric: "air_properties.temperature_c"},
		{Label: "Humidity", Value: "44%", Metric: "air_properties.humidity_percentage"},
		{Label: "CO2", Value: "650 ppm", Metric: "air_quality.co2_ppm"},
		{Label: "Water", Value: "Detected", Alert: true},
	}, Readings(sensor))
}

// A device holding several traits of one kind prefixes the label, but the
// metric key follows the trait's own field name, which is what metricsd tags.
func TestReadingsMetricKeys(t *testing.T) {
	ev := &apiDevice.Device{Details: &apiDevice.Device_EvCharger{EvCharger: &apiDevice.EVCharger{
		WallPower: &apiTrait.Power{State: &apiTrait.Power_State{PowerW: 1200}},
	}}}
	assert.Equal(t, []Reading{{Label: "Wall power Power", Value: "1200.0 W", Metric: "wall_power.power_w"}}, Readings(ev))

	// Text readings are not time series.
	status := &apiDevice.Device{Details: &apiDevice.Device_Ups{Ups: &apiDevice.UPS{
		Battery: &apiTrait.Battery{State: &apiTrait.Battery_State{Status: "charging"}},
	}}}
	assert.Equal(t, []Reading{{Label: "Status", Value: "charging"}}, Readings(status))
}
