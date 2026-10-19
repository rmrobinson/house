package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"tinygo.org/x/bluetooth"

	"github.com/spf13/viper"

	airthings "github.com/rmrobinson/airthings-btle"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
)

const (
	// historySampleInterval is the spacing of the samples inside each hourly history block the
	// sensor reports.
	historySampleInterval = 5 * time.Minute
	// historyMaxWindow is how far back a single GetHistory call reaches. 24h is the window
	// that has been exercised against real sensors; the protocol itself may allow more.
	historyMaxWindow = 24 * time.Hour
	// historyTimeout bounds one BLE history download, after which the sensor is considered
	// unresponsive and the bridge releases the adapter.
	historyTimeout = 30 * time.Second
	// historyAttempts is how many times GetHistory tries a download before giving up. A BLE
	// connect to a sensor at the edge of range, or right after another session, fails often enough
	// ("Disconnected early") that one attempt is not a reliable backfill; the failures are quick,
	// so a few tries still fit well inside the caller's own deadline.
	historyAttempts = 3
	// historyRetryDelay lets the adapter and the sensor settle between attempts.
	historyRetryDelay = 2 * time.Second

	// scanTimeout bounds how long Refresh listens for one sensor to advertise. A sensor that is
	// out of range is simply never heard, so without a bound it would hold the whole refresh (and
	// refreshLock, which GetHistory also needs) until it happened to be.
	scanTimeout = 30 * time.Second
	// readTimeout bounds connecting to a found sensor and reading its current values.
	readTimeout = 30 * time.Second
	// refreshAttempts is how many times Refresh tries one sensor before moving on to the next. A
	// missed refresh is otherwise a whole refresh_interval of lost data.
	refreshAttempts = 2
)

// retryable reports whether a failed BLE attempt is worth repeating straight away. A timeout is not:
// it has already cost the full window, and the next refresh is the better time to try again.
func retryable(err error) bool {
	return !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled)
}

// withTimeout bounds ctx by d, where zero means no extra bound.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// sensorFinder is the part of airthings.Scanner the bridge uses, split out so tests can supply
// sensors without Bluetooth hardware.
type sensorFinder interface {
	FindSensor(ctx context.Context, serialNumber int) (airthings.Sensor, error)
}

// historySnapshot builds the device carrying just the air readings a history sample records.
// Battery and reachability aren't part of the sensor's history, so they're left unset.
func historySnapshot(id string, vocPpb, co2Ppm, radonLT, radonST int32, temperatureC, pressureHpa, humidityPct float32) *device.Device {
	return &device.Device{
		Id: id,
		Details: &device.Device_Sensor{
			Sensor: &device.Sensor{
				AirQuality: &trait.AirQuality{
					State: &trait.AirQuality_State{
						VolatileOrganicCompoundsPpb: &vocPpb,
						Co2Ppm:                      &co2Ppm,
						RadonBqM3:                   &radonLT,
						RadonShortTermBqM3:          &radonST,
					},
				},
				AirProperties: &trait.AirProperties{
					State: &trait.AirProperties_State{
						TemperatureC:       temperatureC,
						PressureHpa:        pressureHpa,
						HumidityPercentage: humidityPct,
					},
				},
			},
		},
	}
}

// historyToSamples expands the sensor's hourly history blocks into one sample per
// historySampleInterval. A block's radon averages apply to all of its samples.
func historyToSamples(deviceID string, blocks []airthings.HistoryMeasurement) []*api2.HistorySample {
	samples := make([]*api2.HistorySample, 0, len(blocks)*12)
	for _, hm := range blocks {
		for i := 0; i < len(hm.Temperature); i++ {
			samples = append(samples, &api2.HistorySample{
				Time: timestamppb.New(hm.Timestamp.Add(time.Duration(i) * historySampleInterval)),
				Snapshot: historySnapshot(deviceID,
					int32(hm.VOCLevel[i]), int32(hm.CO2Level[i]),
					int32(hm.RadonLongTermAvg), int32(hm.RadonShortTermAvg),
					hm.Temperature[i], hm.RelativeAtmosphericPressure[i], hm.Humidity[i]),
			})
		}
	}
	return samples
}

func sensorToDevice(s airthings.Sensor) *device.Device {
	modelName := "Wave Plus"
	m := s.CurrentMeasurement()
	vocLevel := int32(m.VOCLevel)
	co2Level := int32(m.CO2Level)
	radonLTAvg := int32(m.RadonLongTermAvg)
	radonSTAvg := int32(m.RadonShortTermAvg)
	return &device.Device{
		Id:           fmt.Sprintf("%d", s.SerialNumber()),
		ModelId:      "Wave Plus",
		Manufacturer: "Airthings",
		ModelName:    &modelName,
		LastSeen:     timestamppb.Now(),
		Address: &device.Device_Address{
			Address:     s.Address(),
			IsReachable: true,
		},
		Details: &device.Device_Sensor{
			Sensor: &device.Sensor{
				Metadata: &device.Sensor_Metadata{
					OnBattery: true,
					History: &device.Sensor_Metadata_History{
						MaxWindow:      durationpb.New(historyMaxWindow),
						SampleInterval: durationpb.New(historySampleInterval),
					},
				},
				AirQuality: &trait.AirQuality{
					State: &trait.AirQuality_State{
						VolatileOrganicCompoundsPpb: &vocLevel,
						Co2Ppm:                      &co2Level,
						RadonBqM3:                   &radonLTAvg,
						RadonShortTermBqM3:          &radonSTAvg,
					},
				},
				AirProperties: &trait.AirProperties{
					State: &trait.AirProperties_State{
						TemperatureC:       m.Temperature,
						PressureHpa:        m.RelativeAtmosphericPressure,
						HumidityPercentage: m.Humidity,
					},
				},
				Battery: &trait.Battery{
					Attributes: &trait.Battery_Attributes{},
					State: &trait.Battery_State{
						CapacityRemainingPct: int32(s.BatteryLevel()),
					},
				},
			},
		},
	}
}

// AirthingsBridge acts as the handler for Bridge requests for the Airthings sensors registered.
type AirthingsBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	scanner     sensorFinder
	refreshLock sync.Mutex
	ids         []int

	// retryDelay is the pause between retry attempts.
	retryDelay time.Duration
	// scanTimeout and readTimeout bound one sensor's scan and read in Refresh; zero is unbounded.
	scanTimeout time.Duration
	readTimeout time.Duration
}

// NewAirthingsBridge creates a new bridge to a collection Airthings sensors
func NewAirthingsBridge(logger *zap.Logger, svc *bridge.Service, btAdapter *bluetooth.Adapter, sensorIDs []int) *AirthingsBridge {
	btMac, err := btAdapter.Address()
	if err != nil {
		logger.Fatal("unable to get address from bluetooth adapter", zap.Error(err))
	}

	b := &api2.Bridge{
		Id:           viper.GetString("bridge.id"),
		IsReachable:  true,
		ModelId:      "ATS1",
		Manufacturer: "Faltung Networks",
		Config: &api2.Bridge_Config{
			Name:        viper.GetString("bridge.name"),
			Description: viper.GetString("bridge.description"),
			Address: &api2.Address{
				Bluetooth: &api2.Address_Bluetooth{
					Address: btMac.MAC.String(),
				},
			},
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}

	ab := &AirthingsBridge{
		logger:  logger,
		svc:     svc,
		scanner: airthings.NewScanner(btAdapter),
		ids:     sensorIDs,
		b:       b,

		retryDelay:  historyRetryDelay,
		scanTimeout: scanTimeout,
		readTimeout: readTimeout,
	}

	return ab
}

// ProcessCommand takes a given command request and attempts to execute it.
// We only worry about processing valid commands for the given device traits.
func (ab *AirthingsBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	ab.logger.Error("received unsupported command - shouldn't happen")
	return nil, bridge.ErrUnsupportedCommand
}

// SetBridgeConfig takes the supplied config params and saves them for future reference.
func (ab *AirthingsBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	ab.b.Config.Name = config.Name
	ab.b.Config.Description = config.Description

	viper.Set("bridge.name", config.Name)
	viper.Set("bridge.description", config.Description)
	viper.WriteConfig()

	return nil
}

// ProcessCommandAsync is present to conform to the bridge.Handler interface. This bridge has no
// device traits eligible for asynchronous commands, so it always returns ErrAsyncCommandsNotSupported.
func (ab *AirthingsBridge) ProcessCommandAsync(ctx context.Context, cmd *command.Command) error {
	return bridge.ErrAsyncCommandsNotSupported
}

// Refresh is present to conform to the bridge.Handler interface. In this implementation it queries
// the charger API and returns the current state of the charger.
func (ab *AirthingsBridge) Refresh(ctx context.Context) error {
	ab.refreshLock.Lock()
	defer ab.refreshLock.Unlock()

	for _, id := range ab.ids {
		// A sensor that can't be read is skipped, not fatal: the others still get refreshed.
		sensor, err := ab.refreshSensor(ctx, id)
		if err != nil {
			ab.logger.Error("unable to refresh sensor", zap.Int("sensor_id", id), zap.Error(err))
			continue
		}

		ab.svc.UpdateDevice(sensorToDevice(sensor))
	}
	return nil
}

// refreshSensor finds and reads one sensor, retrying a failed connect or read (but not a timeout)
// up to refreshAttempts times. The returned sensor has already been disconnected. The caller must
// hold refreshLock.
func (ab *AirthingsBridge) refreshSensor(ctx context.Context, id int) (airthings.Sensor, error) {
	var lastErr error
	for attempt := 1; attempt <= refreshAttempts; attempt++ {
		sensor, err := ab.readSensor(ctx, id)
		if err == nil {
			return sensor, nil
		}
		lastErr = err

		if attempt == refreshAttempts || ctx.Err() != nil || !retryable(err) {
			break
		}
		ab.logger.Warn("sensor refresh failed, retrying",
			zap.Int("sensor_id", id), zap.Int("attempt", attempt),
			zap.Int("attempts", refreshAttempts), zap.Error(err))

		select {
		case <-time.After(ab.retryDelay):
		case <-ctx.Done():
		}
	}
	return nil, lastErr
}

// readSensor makes one attempt to find a sensor and read its current values, always releasing the
// connection afterwards. The scan and the read each have their own bound so a sensor that is out of
// range, or stalls mid-read, can't hold up the rest of the refresh.
func (ab *AirthingsBridge) readSensor(ctx context.Context, id int) (airthings.Sensor, error) {
	ab.logger.Debug("scanning for sensor", zap.Int("sensor_id", id))

	scanCtx, cancelScan := withTimeout(ctx, ab.scanTimeout)
	sensor, err := ab.scanner.FindSensor(scanCtx, id)
	cancelScan()
	if err != nil {
		return nil, fmt.Errorf("unable to find sensor: %w", err)
	}

	// Disconnect on every path: a connection left open after a failed refresh holds the one
	// adapter and makes the next session (live or history) fail to connect.
	readCtx, cancelRead := withTimeout(ctx, ab.readTimeout)
	defer cancelRead()
	err = sensor.Refresh(readCtx)
	sensor.Disconnect()
	if err != nil {
		return nil, fmt.Errorf("unable to read sensor: %w", err)
	}
	return sensor, nil
}

// GetHistory implements bridge.HistoryHandler by downloading the sensor's on-board log over BLE.
//
// The sensor can only be asked for "the last N hours" - there is no way to ask for an older,
// bounded window - so a since further back than historyMaxWindow is clamped to it, and anything
// older is unrecoverable. One extra hour is requested beyond the window to cover the block still
// being filled, otherwise a request exactly 24h wide would silently lose the oldest hour; callers
// are expected to run more often than historyMaxWindow and rely on samples being idempotent.
//
// It holds refreshLock for the whole BLE session so it can't overlap Refresh on the one adapter.
//
// A failed download (the sensor not found, or the connection dropping mid-transfer) is retried up
// to historyAttempts times, each with its own historyTimeout. Only if every attempt fails does the
// caller see Unavailable, so a single flaky connect doesn't leave a hole in the backfilled history.
// An attempt that times out is not retried, since it has already spent its whole window.
func (ab *AirthingsBridge) GetHistory(ctx context.Context, deviceID string, since, until time.Time) ([]*api2.HistorySample, error) {
	serial := 0
	for _, id := range ab.ids {
		if strconv.Itoa(id) == deviceID {
			serial = id
			break
		}
	}
	if serial == 0 {
		return nil, bridge.ErrDeviceNotFound
	}

	window := time.Since(since)
	if since.IsZero() || window > historyMaxWindow {
		if !since.IsZero() {
			ab.logger.Warn("requested history is further back than the sensor can be asked for; clamping",
				zap.String("device_id", deviceID),
				zap.Time("since", since),
				zap.Duration("max_window", historyMaxWindow))
		}
		window = historyMaxWindow
	}
	hours := int(math.Ceil(window.Hours())) + 1

	ab.refreshLock.Lock()
	defer ab.refreshLock.Unlock()

	var lastErr error
	for attempt := 1; attempt <= historyAttempts; attempt++ {
		samples, err := ab.downloadHistory(ctx, deviceID, serial, hours)
		if err == nil {
			return samples, nil
		}
		lastErr = err

		if attempt == historyAttempts || ctx.Err() != nil || !retryable(err) {
			break
		}
		ab.logger.Warn("sensor history download failed, retrying",
			zap.String("device_id", deviceID), zap.Int("attempt", attempt),
			zap.Int("attempts", historyAttempts), zap.Error(err))

		select {
		case <-time.After(ab.retryDelay):
		case <-ctx.Done():
		}
	}

	ab.logger.Error("unable to download sensor history",
		zap.String("device_id", deviceID), zap.Int("hours", hours), zap.Error(lastErr))
	return nil, status.Error(codes.Unavailable, lastErr.Error())
}

// downloadHistory makes one attempt to find the sensor and read its on-board log, always releasing
// the connection afterwards. The caller must hold refreshLock.
func (ab *AirthingsBridge) downloadHistory(ctx context.Context, deviceID string, serial, hours int) ([]*api2.HistorySample, error) {
	ctx, cancel := context.WithTimeout(ctx, historyTimeout)
	defer cancel()

	sensor, err := ab.scanner.FindSensor(ctx, serial)
	if err != nil {
		return nil, fmt.Errorf("unable to find sensor: %w", err)
	}
	defer sensor.Disconnect()

	if err := sensor.RefreshHistory(ctx, hours); err != nil {
		return nil, fmt.Errorf("unable to download history: %w", err)
	}

	return historyToSamples(deviceID, sensor.HistoricalMeasurements()), nil
}

// Run begins the process of polling the sensor and reporting back the state.
func (ab *AirthingsBridge) Run(ctx context.Context) {
	ab.Refresh(ctx)

	refreshTimer := time.NewTicker(time.Second * time.Duration(viper.GetInt("bridge.refresh_interval")))
	ab.logger.Info("beginning refresh loop", zap.Int("refresh_interval", viper.GetInt("bridge.refresh_interval")))
	for {
		select {
		case <-refreshTimer.C:
			if err := ab.Refresh(ctx); err != nil {
				ab.logger.Error("unable to refresh sensors",
					zap.Error(err))
				continue
			}
			ab.logger.Debug("refreshed")
		case <-ctx.Done():
			ab.logger.Info("run context cancelled")
			return
		}
	}
}
