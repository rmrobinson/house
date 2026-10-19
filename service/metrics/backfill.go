package metrics

import (
	"context"
	"errors"
	"io"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
)

const (
	// DefaultBackfillInterval is how often devices are asked for history.
	// It must stay under the device's max_window (Airthings: 24h) or samples
	// fall out of reach before they're collected; 12h leaves a missed run
	// still recoverable.
	DefaultBackfillInterval = 12 * time.Hour
	// DefaultBackfillInitialDelay lets the update stream populate the set of
	// history-capable devices before the first pass.
	DefaultBackfillInitialDelay = time.Minute
	// DefaultBackfillTimeout bounds one device's request. The bridge may need
	// a BLE connect + download, which takes tens of seconds.
	DefaultBackfillTimeout = 3 * time.Minute

	// fallbackWindow applies when a device advertises no max_window.
	fallbackWindow = 24 * time.Hour
)

// BackfillConfig configures a Backfiller.
type BackfillConfig struct {
	Interval     time.Duration
	InitialDelay time.Duration
	// Timeout bounds each device's GetDeviceHistory call.
	Timeout time.Duration
}

// Backfiller periodically asks bridges for the history of devices that
// record their own, writing what it gets as source=backfill. This is what
// lets a battery device be polled rarely: its gaps are filled in afterwards.
//
// Devices are fetched one at a time - the bridge typically holds a single BLE
// adapter - and one device failing never stops the others; it is retried on
// the next pass.
type Backfiller struct {
	logger     *zap.Logger
	cfg        BackfillConfig
	svc        *Service
	sink       Sink
	watermarks WatermarkStore
	client     func() api2.BridgeServiceClient
	now        func() time.Time
}

// NewBackfiller creates a Backfiller. client returns the current connection's
// BridgeServiceClient, or nil while it is down (bridgeconn.Conn.Client).
func NewBackfiller(logger *zap.Logger, cfg BackfillConfig, svc *Service, sink Sink, watermarks WatermarkStore, client func() api2.BridgeServiceClient) *Backfiller {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultBackfillInterval
	}
	if cfg.InitialDelay < 0 {
		cfg.InitialDelay = DefaultBackfillInitialDelay
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultBackfillTimeout
	}
	return &Backfiller{
		logger: logger, cfg: cfg, svc: svc, sink: sink, watermarks: watermarks, client: client, now: time.Now,
	}
}

// Run backfills after InitialDelay and then every Interval until ctx is done.
func (b *Backfiller) Run(ctx context.Context) {
	timer := time.NewTimer(b.cfg.InitialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		b.RunOnce(ctx)
		timer.Reset(b.cfg.Interval)
	}
}

// RunOnce makes one pass over every history-capable device, returning how
// many points it wrote.
func (b *Backfiller) RunOnce(ctx context.Context) (written int) {
	devices := b.svc.HistoryDevices()
	if len(devices) == 0 {
		b.logger.Debug("no devices advertise history, nothing to backfill")
		return 0
	}

	for _, h := range devices {
		if ctx.Err() != nil {
			return written
		}
		n, err := b.backfillDevice(ctx, h)
		written += n
		if err != nil {
			b.logger.Warn("backfill failed, will retry next pass",
				zap.Error(err), zap.String("device_id", h.DeviceID), zap.String("device_name", h.Name))
			continue
		}
		b.logger.Info("backfilled device history",
			zap.String("device_id", h.DeviceID), zap.String("device_name", h.Name), zap.Int("points", n))
	}
	return written
}

func (b *Backfiller) backfillDevice(ctx context.Context, h HistoryDevice) (int, error) {
	client := b.client()
	if client == nil {
		return 0, errors.New("not connected to house")
	}

	now := b.now()
	window := h.MaxWindow
	if window <= 0 {
		window = fallbackWindow
	}
	floor := now.Add(-window)

	// Resume from the newest backfilled sample. If Influx can't be read the
	// device is skipped rather than re-fetched over the full window: a BLE
	// session is the expensive thing here and overwriting is only idempotent,
	// not free.
	since := floor
	wm, ok, err := b.watermarks.LatestBackfill(ctx, h.DeviceID, floor)
	if err != nil {
		return 0, err
	}
	if ok && wm.After(since) {
		since = wm
	}

	// A sample can't be newer than one interval after the last; asking sooner
	// just costs a BLE session for nothing.
	if h.SampleInterval > 0 && now.Sub(since) < h.SampleInterval {
		return 0, nil
	}

	b.logger.Info("requesting device history",
		zap.String("device_id", h.DeviceID), zap.Time("since", since), zap.Bool("resumed_from_watermark", ok))

	reqCtx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()

	stream, err := client.GetDeviceHistory(reqCtx, &api2.GetDeviceHistoryRequest{
		DeviceId: h.DeviceID,
		Since:    timestamppb.New(since),
		Until:    timestamppb.New(now),
	})
	if err != nil {
		return 0, err
	}

	var written int
	for {
		sample, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return written, nil
		}
		if err != nil {
			if status.Code(err) == codes.Unimplemented {
				b.logger.Warn("device advertises history but its bridge does not implement it",
					zap.String("device_id", h.DeviceID))
			}
			return written, err
		}

		points := b.svc.HistoryPoints(h, sample)
		b.sink.Write(points...)
		written += len(points)
	}
}
