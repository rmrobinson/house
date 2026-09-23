package main

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/backoffutil"
)

const (
	hubReconnectMinDelay = time.Second
	hubReconnectMaxDelay = 30 * time.Second
)

// deviceHub maintains a single upstream BridgeService.StreamUpdates
// subscription and fans DeviceUpdate events out to every subscribed SSE
// client (see handleSSE), so N open browser tabs cost one upstream stream -
// and one initial-state replay, which no subscriber even needs, since a
// DeviceUpdate only ever swaps a device_info cell a page already rendered
// from its own GetDevice/ListDevices call - instead of N.
type deviceHub struct {
	logger *zap.Logger
	bridge api2.BridgeServiceClient

	mu   sync.Mutex
	subs map[chan *api2.Update]struct{}
}

func newDeviceHub(logger *zap.Logger, bridge api2.BridgeServiceClient) *deviceHub {
	return &deviceHub{
		logger: logger,
		bridge: bridge,
		subs:   make(map[chan *api2.Update]struct{}),
	}
}

// run maintains the upstream subscription until ctx is cancelled,
// reconnecting with jittered backoff on drop - existing SSE clients simply
// stop seeing updates while a reconnect is in flight, rather than having
// their own HTTP connection torn down. Call it from its own goroutine.
func (h *deviceHub) run(ctx context.Context) {
	backoff := hubReconnectMinDelay
	for ctx.Err() == nil {
		if err := h.streamOnce(ctx); err != nil && ctx.Err() == nil {
			h.logger.Warn("bridge update stream ended, reconnecting", zap.Error(err))
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoffutil.Jitter(backoff)):
		}
		backoff = min(backoff*2, hubReconnectMaxDelay)
	}
}

func (h *deviceHub) streamOnce(ctx context.Context) error {
	stream, err := h.bridge.StreamUpdates(ctx, &api2.StreamUpdatesRequest{})
	if err != nil {
		return err
	}

	for {
		update, err := stream.Recv()
		if err != nil {
			return err
		}
		h.broadcast(update)
	}
}

func (h *deviceHub) broadcast(update *api2.Update) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for ch := range h.subs {
		select {
		case ch <- update:
		default:
			// Slow subscriber - drop rather than block every other
			// subscriber on it. The device_info cell it missed just stays
			// stale until the next update for that device arrives.
		}
	}
}

// subscribe registers a new subscriber, returning its update channel and an
// unsubscribe func the caller must call exactly once when done (e.g. via
// defer) to stop receiving updates and release the channel.
func (h *deviceHub) subscribe() (<-chan *api2.Update, func()) {
	ch := make(chan *api2.Update, 16)

	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}
