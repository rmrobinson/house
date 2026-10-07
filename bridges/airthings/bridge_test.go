package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	airthings "github.com/rmrobinson/airthings-btle"
)

// fakeSensor implements airthings.Sensor with canned values, so sensorToDevice can be tested
// without real Bluetooth hardware.
type fakeSensor struct {
	serialNumber int
	address      string
	batteryLevel float32
	measurement  airthings.Measurement
}

func (f *fakeSensor) SerialNumber() int                                      { return f.serialNumber }
func (f *fakeSensor) Address() string                                        { return f.address }
func (f *fakeSensor) BatteryLevel() float32                                  { return f.batteryLevel }
func (f *fakeSensor) RSSI() int                                              { return 0 }
func (f *fakeSensor) CurrentMeasurement() airthings.Measurement              { return f.measurement }
func (f *fakeSensor) HistoricalMeasurements() []airthings.HistoryMeasurement { return nil }
func (f *fakeSensor) Disconnect()                                            {}
func (f *fakeSensor) Refresh(ctx context.Context) error                      { return nil }
func (f *fakeSensor) RefreshHistory(ctx context.Context, hours int) error    { return nil }
func (f *fakeSensor) GetDeviceProfile() ([]airthings.DeviceProfile, error)   { return nil, nil }

func TestSensorToDevice_BatteryLevelSetAsCapacityRemainingPct(t *testing.T) {
	s := &fakeSensor{serialNumber: 12345, address: "aa:bb:cc:dd:ee:ff", batteryLevel: 73}
	d := sensorToDevice(s)
	assert.EqualValues(t, 73, d.GetSensor().GetBattery().GetState().GetCapacityRemainingPct())
}

func TestSensorToDevice_OnBatteryAlwaysTrue(t *testing.T) {
	s := &fakeSensor{serialNumber: 12345, address: "aa:bb:cc:dd:ee:ff"}
	d := sensorToDevice(s)
	assert.True(t, d.GetSensor().GetMetadata().GetOnBattery(), "Wave Plus sensors always run on battery")
}

func TestSensorToDevice_AirQualityAndPropertiesPopulatedFromMeasurement(t *testing.T) {
	s := &fakeSensor{
		serialNumber: 12345,
		address:      "aa:bb:cc:dd:ee:ff",
		measurement: airthings.Measurement{
			Humidity:                    45.5,
			Temperature:                 21.3,
			RelativeAtmosphericPressure: 1013,
			CO2Level:                    650,
			VOCLevel:                    120,
			RadonLongTermAvg:            30,
		},
	}
	d := sensorToDevice(s)

	assert.EqualValues(t, 650, d.GetSensor().GetAirQuality().GetState().GetCo2Ppm())
	assert.EqualValues(t, 120, d.GetSensor().GetAirQuality().GetState().GetVolatileOrganicCompoundsPpb())
	assert.EqualValues(t, 30, d.GetSensor().GetAirQuality().GetState().GetRadonBqM3())
	assert.InDelta(t, 21.3, d.GetSensor().GetAirProperties().GetState().GetTemperatureC(), 0.001)
	assert.InDelta(t, 1013, d.GetSensor().GetAirProperties().GetState().GetPressureHpa(), 0.001)
	assert.InDelta(t, 45.5, d.GetSensor().GetAirProperties().GetState().GetHumidityPercentage(), 0.001)
}

func TestSensorToDevice_IDFromSerialNumber(t *testing.T) {
	s := &fakeSensor{serialNumber: 987654321, address: "aa:bb:cc:dd:ee:ff"}
	d := sensorToDevice(s)
	assert.Equal(t, "987654321", d.GetId())
	assert.Equal(t, "aa:bb:cc:dd:ee:ff", d.GetAddress().GetAddress())
	assert.True(t, d.GetAddress().GetIsReachable())
}
