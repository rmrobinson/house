package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	airthings "github.com/rmrobinson/airthings-btle"

	"github.com/rmrobinson/house/service/bridge"
)

// fakeSensor implements airthings.Sensor with canned values, so sensorToDevice can be tested
// without real Bluetooth hardware.
type fakeSensor struct {
	serialNumber int
	address      string
	batteryLevel float32
	measurement  airthings.Measurement

	// history is returned by HistoricalMeasurements; historyErr is returned by RefreshHistory.
	history        []airthings.HistoryMeasurement
	historyErr     error
	requestedHours int
	disconnected   bool
}

func (f *fakeSensor) SerialNumber() int                         { return f.serialNumber }
func (f *fakeSensor) Address() string                           { return f.address }
func (f *fakeSensor) BatteryLevel() float32                     { return f.batteryLevel }
func (f *fakeSensor) RSSI() int                                 { return 0 }
func (f *fakeSensor) CurrentMeasurement() airthings.Measurement { return f.measurement }
func (f *fakeSensor) HistoricalMeasurements() []airthings.HistoryMeasurement {
	return f.history
}
func (f *fakeSensor) Disconnect()                         { f.disconnected = true }
func (f *fakeSensor) Refresh(ctx context.Context) error { return nil }
func (f *fakeSensor) RefreshHistory(ctx context.Context, hours int) error {
	f.requestedHours = hours
	return f.historyErr
}
func (f *fakeSensor) GetDeviceProfile() ([]airthings.DeviceProfile, error) { return nil, nil }

// fakeFinder hands back a canned sensor (or error) for any serial number it was built for.
type fakeFinder struct {
	sensors map[int]*fakeSensor
	err     error
}

func (f *fakeFinder) FindSensor(ctx context.Context, serialNumber int) (airthings.Sensor, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.sensors[serialNumber], nil
}

func newHistoryTestBridge(t *testing.T, finder sensorFinder, ids ...int) *AirthingsBridge {
	return &AirthingsBridge{logger: zaptest.NewLogger(t), scanner: finder, ids: ids}
}

func TestHistoryToSamples_ExpandsBlocksIntoFiveMinuteSamples(t *testing.T) {
	start := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	hm := airthings.HistoryMeasurement{
		Timestamp:         start,
		RadonShortTermAvg: 12,
		RadonLongTermAvg:  34,
	}
	for i := 0; i < 12; i++ {
		hm.Temperature[i] = 20 + float32(i)
		hm.CO2Level[i] = 400 + float32(i)
	}

	samples := historyToSamples("42", []airthings.HistoryMeasurement{hm})
	require.Len(t, samples, 12)

	assert.Equal(t, start, samples[0].GetTime().AsTime())
	assert.Equal(t, start.Add(55*time.Minute), samples[11].GetTime().AsTime())
	assert.Equal(t, "42", samples[3].GetSnapshot().GetId())

	last := samples[11].GetSnapshot().GetSensor()
	assert.InDelta(t, 31, last.GetAirProperties().GetState().GetTemperatureC(), 0.001)
	assert.EqualValues(t, 411, last.GetAirQuality().GetState().GetCo2Ppm())
	assert.EqualValues(t, 34, last.GetAirQuality().GetState().GetRadonBqM3())
	assert.EqualValues(t, 12, last.GetAirQuality().GetState().GetRadonShortTermBqM3())
}

func TestGetHistory_RequestsWindowPlusPartialBlockAndDisconnects(t *testing.T) {
	sensor := &fakeSensor{serialNumber: 42}
	ab := newHistoryTestBridge(t, &fakeFinder{sensors: map[int]*fakeSensor{42: sensor}}, 42)

	_, err := ab.GetHistory(context.Background(), "42", time.Now().Add(-6*time.Hour), time.Now())
	require.NoError(t, err)

	// 6h window rounds up (the call itself takes a moment) to 7h, plus the block in progress.
	assert.Equal(t, 8, sensor.requestedHours)
	assert.True(t, sensor.disconnected)
}

func TestGetHistory_ClampsToMaxWindow(t *testing.T) {
	sensor := &fakeSensor{serialNumber: 42}
	ab := newHistoryTestBridge(t, &fakeFinder{sensors: map[int]*fakeSensor{42: sensor}}, 42)

	// An unset since means "as far back as possible".
	_, err := ab.GetHistory(context.Background(), "42", time.Time{}, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 25, sensor.requestedHours)

	_, err = ab.GetHistory(context.Background(), "42", time.Now().Add(-72*time.Hour), time.Now())
	require.NoError(t, err)
	assert.Equal(t, 25, sensor.requestedHours)
}

func TestGetHistory_UnknownDevice(t *testing.T) {
	ab := newHistoryTestBridge(t, &fakeFinder{}, 42)

	_, err := ab.GetHistory(context.Background(), "99", time.Time{}, time.Now())
	assert.ErrorIs(t, err, bridge.ErrDeviceNotFound)
}

func TestGetHistory_DownloadFailureIsUnavailableAndStillDisconnects(t *testing.T) {
	sensor := &fakeSensor{serialNumber: 42, historyErr: errors.New("notification timeout")}
	ab := newHistoryTestBridge(t, &fakeFinder{sensors: map[int]*fakeSensor{42: sensor}}, 42)

	_, err := ab.GetHistory(context.Background(), "42", time.Now().Add(-time.Hour), time.Now())
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.True(t, sensor.disconnected)
}

func TestGetHistory_SensorNotFoundIsUnavailable(t *testing.T) {
	ab := newHistoryTestBridge(t, &fakeFinder{err: errors.New("scan timed out")}, 42)

	_, err := ab.GetHistory(context.Background(), "42", time.Time{}, time.Now())
	assert.Equal(t, codes.Unavailable, status.Code(err))
}

func TestSensorToDevice_AdvertisesHistoryAndShortTermRadon(t *testing.T) {
	s := &fakeSensor{serialNumber: 12345, measurement: airthings.Measurement{RadonShortTermAvg: 17}}
	d := sensorToDevice(s)

	h := d.GetSensor().GetMetadata().GetHistory()
	require.NotNil(t, h)
	assert.Equal(t, 24*time.Hour, h.GetMaxWindow().AsDuration())
	assert.Equal(t, 5*time.Minute, h.GetSampleInterval().AsDuration())
	assert.EqualValues(t, 17, d.GetSensor().GetAirQuality().GetState().GetRadonShortTermBqM3())
}

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
