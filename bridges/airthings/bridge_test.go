package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	airthings "github.com/rmrobinson/airthings-btle"

	api2 "github.com/rmrobinson/house/api"
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

	// historyFailures makes the first N RefreshHistory calls fail (a flaky connection that recovers);
	// refreshErr fails Refresh. The counters record how the bridge drove the sensor.
	historyFailures int
	refreshErr      error
	historyCalls    int
	disconnects     int

	// refreshFailures makes the first N Refresh calls fail, then succeed.
	refreshFailures int
	refreshCalls    int
}

func (f *fakeSensor) SerialNumber() int                         { return f.serialNumber }
func (f *fakeSensor) Address() string                           { return f.address }
func (f *fakeSensor) BatteryLevel() float32                     { return f.batteryLevel }
func (f *fakeSensor) RSSI() int                                 { return 0 }
func (f *fakeSensor) CurrentMeasurement() airthings.Measurement { return f.measurement }
func (f *fakeSensor) HistoricalMeasurements() []airthings.HistoryMeasurement {
	return f.history
}
func (f *fakeSensor) Disconnect() {
	f.disconnected = true
	f.disconnects++
}
func (f *fakeSensor) Refresh(ctx context.Context) error {
	f.refreshCalls++
	if f.refreshCalls <= f.refreshFailures {
		return errors.New("failed to read: Disconnected early")
	}
	return f.refreshErr
}
func (f *fakeSensor) RefreshHistory(ctx context.Context, hours int) error {
	f.requestedHours = hours
	f.historyCalls++
	if f.historyCalls <= f.historyFailures {
		return errors.New("failed to connect: Disconnected early")
	}
	return f.historyErr
}
func (f *fakeSensor) GetDeviceProfile() ([]airthings.DeviceProfile, error) { return nil, nil }

// fakeFinder hands back a canned sensor (or error) for any serial number it was built for.
type fakeFinder struct {
	sensors map[int]*fakeSensor
	err     error

	// failFirst makes the first N FindSensor calls fail, then succeed.
	failFirst int
	calls     int

	// unheard sensors are never found: like the real scan, FindSensor then blocks until its ctx ends.
	unheard map[int]bool
	byID    map[int]int
}

func (f *fakeFinder) FindSensor(ctx context.Context, serialNumber int) (airthings.Sensor, error) {
	f.calls++
	if f.byID == nil {
		f.byID = map[int]int{}
	}
	f.byID[serialNumber]++
	if f.unheard[serialNumber] {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.calls <= f.failFirst {
		return nil, errors.New("scan timed out")
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
	// Every attempt is made, and every connection released.
	assert.Equal(t, historyAttempts, sensor.historyCalls)
	assert.Equal(t, historyAttempts, sensor.disconnects)
}

func TestGetHistory_SensorNotFoundIsUnavailable(t *testing.T) {
	finder := &fakeFinder{err: errors.New("scan timed out")}
	ab := newHistoryTestBridge(t, finder, 42)

	_, err := ab.GetHistory(context.Background(), "42", time.Time{}, time.Now())
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Equal(t, historyAttempts, finder.calls)
}

func TestGetHistory_RetriesFlakyDownloadUntilItSucceeds(t *testing.T) {
	sensor := &fakeSensor{serialNumber: 42, historyFailures: historyAttempts - 1}
	ab := newHistoryTestBridge(t, &fakeFinder{sensors: map[int]*fakeSensor{42: sensor}}, 42)

	_, err := ab.GetHistory(context.Background(), "42", time.Now().Add(-time.Hour), time.Now())
	require.NoError(t, err)
	assert.Equal(t, historyAttempts, sensor.historyCalls)
	// Each attempt's connection, failed or not, is released.
	assert.Equal(t, historyAttempts, sensor.disconnects)
}

func TestGetHistory_RetriesSensorNotFoundUntilItIsFound(t *testing.T) {
	sensor := &fakeSensor{serialNumber: 42}
	finder := &fakeFinder{sensors: map[int]*fakeSensor{42: sensor}, failFirst: historyAttempts - 1}
	ab := newHistoryTestBridge(t, finder, 42)

	_, err := ab.GetHistory(context.Background(), "42", time.Now().Add(-time.Hour), time.Now())
	require.NoError(t, err)
	assert.Equal(t, historyAttempts, finder.calls)
	assert.Equal(t, 1, sensor.historyCalls)
}

func TestGetHistory_StopsRetryingOnceTheCallerGivesUp(t *testing.T) {
	sensor := &fakeSensor{serialNumber: 42, historyErr: errors.New("notification timeout")}
	ab := newHistoryTestBridge(t, &fakeFinder{sensors: map[int]*fakeSensor{42: sensor}}, 42)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ab.GetHistory(ctx, "42", time.Now().Add(-time.Hour), time.Now())
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Equal(t, 1, sensor.historyCalls)
}

func TestRefresh_DisconnectsEvenWhenTheRefreshFails(t *testing.T) {
	sensor := &fakeSensor{serialNumber: 42, refreshErr: errors.New("read failed")}
	ab := newHistoryTestBridge(t, &fakeFinder{sensors: map[int]*fakeSensor{42: sensor}}, 42)

	require.NoError(t, ab.Refresh(context.Background()))
	// Every attempt's connection is released, including the ones that failed.
	assert.Equal(t, refreshAttempts, sensor.disconnects)
}

// newRefreshTestBridge is newHistoryTestBridge plus a real Service, so Refresh can publish devices.
func newRefreshTestBridge(t *testing.T, finder sensorFinder, ids ...int) (*AirthingsBridge, *bridge.Service) {
	t.Helper()
	ab := newHistoryTestBridge(t, finder, ids...)
	svc := bridge.NewService(zaptest.NewLogger(t))
	ab.svc = svc
	svc.RegisterHandler(ab, &api2.Bridge{Id: "test-bridge"})
	return ab, svc
}

func publishedIDs(t *testing.T, svc *bridge.Service) []string {
	t.Helper()
	resp, err := svc.API().ListDevices(context.Background(), &api2.ListDevicesRequest{})
	require.NoError(t, err)
	var ids []string
	for _, d := range resp.GetDevices() {
		ids = append(ids, d.GetId())
	}
	return ids
}

func TestRefresh_AnUnheardSensorDoesNotStallTheOthers(t *testing.T) {
	good := &fakeSensor{serialNumber: 43}
	finder := &fakeFinder{sensors: map[int]*fakeSensor{43: good}, unheard: map[int]bool{42: true}}
	// The unheard sensor is first, so everything after it is only reached if its scan is bounded.
	ab, svc := newRefreshTestBridge(t, finder, 42, 43)
	ab.scanTimeout = 20 * time.Millisecond

	done := make(chan struct{})
	go func() {
		defer close(done)
		assert.NoError(t, ab.Refresh(context.Background()))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Refresh blocked on a sensor that is never heard")
	}

	assert.Equal(t, []string{"43"}, publishedIDs(t, svc))
	// A timed-out scan has already cost its whole window, so it isn't repeated straight away.
	assert.Equal(t, 1, finder.byID[42])
}

func TestRefresh_RetriesAFlakyReadUntilItSucceeds(t *testing.T) {
	sensor := &fakeSensor{serialNumber: 42, refreshFailures: refreshAttempts - 1}
	ab, svc := newRefreshTestBridge(t, &fakeFinder{sensors: map[int]*fakeSensor{42: sensor}}, 42)

	require.NoError(t, ab.Refresh(context.Background()))
	assert.Equal(t, []string{"42"}, publishedIDs(t, svc))
	assert.Equal(t, refreshAttempts, sensor.refreshCalls)
	assert.Equal(t, refreshAttempts, sensor.disconnects)
}

func TestRefresh_RetriesAFlakyFindUntilItSucceeds(t *testing.T) {
	sensor := &fakeSensor{serialNumber: 42}
	finder := &fakeFinder{sensors: map[int]*fakeSensor{42: sensor}, failFirst: refreshAttempts - 1}
	ab, svc := newRefreshTestBridge(t, finder, 42)

	require.NoError(t, ab.Refresh(context.Background()))
	assert.Equal(t, []string{"42"}, publishedIDs(t, svc))
	assert.Equal(t, refreshAttempts, finder.calls)
}

func TestRefresh_GivesUpAfterTheAttemptBudgetAndPublishesNothing(t *testing.T) {
	sensor := &fakeSensor{serialNumber: 42, refreshErr: errors.New("read failed")}
	ab, svc := newRefreshTestBridge(t, &fakeFinder{sensors: map[int]*fakeSensor{42: sensor}}, 42)

	require.NoError(t, ab.Refresh(context.Background()))
	assert.Empty(t, publishedIDs(t, svc))
	assert.Equal(t, refreshAttempts, sensor.refreshCalls)
}

func TestGetHistory_AnUnheardSensorFailsAtTheCallersDeadlineWithoutRetrying(t *testing.T) {
	finder := &fakeFinder{unheard: map[int]bool{42: true}}
	ab := newHistoryTestBridge(t, finder, 42)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := ab.GetHistory(ctx, "42", time.Now().Add(-time.Hour), time.Now())
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Equal(t, 1, finder.byID[42])
}

func TestRetryable(t *testing.T) {
	assert.True(t, retryable(errors.New("failed to connect: Disconnected early")))
	assert.False(t, retryable(fmt.Errorf("unable to find sensor: %w", context.DeadlineExceeded)))
	assert.False(t, retryable(context.Canceled))
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
