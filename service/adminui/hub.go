package main

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/backoffutil"
)

const (
	hubReconnectMinDelay = time.Second
	hubReconnectMaxDelay = 30 * time.Second
)

// hub maintains a single upstream gRPC server-streaming subscription and
// fans its messages out to every subscribed SSE client (see handleSSE), so N
// open browser tabs cost one upstream stream instead of N - the shared
// mechanics behind deviceHub (BridgeService.StreamUpdates) and policyHub
// (PolicyService.StreamEvents), so a fix to the reconnect/backoff/broadcast
// logic only has to be made once, not once per stream type.
type hub[T any] struct {
	logger *zap.Logger
	// connect opens one attempt at the upstream stream, returning a Recv
	// func to pull messages from it - e.g. bridge.StreamUpdates(ctx, ...)
	// then that call's own stream.Recv, bound as a value.
	connect func(ctx context.Context) (func() (T, error), error)

	mu   sync.Mutex
	subs map[chan T]struct{}
}

func newHub[T any](logger *zap.Logger, connect func(ctx context.Context) (func() (T, error), error)) *hub[T] {
	return &hub[T]{
		logger:  logger,
		connect: connect,
		subs:    make(map[chan T]struct{}),
	}
}

// run maintains the upstream subscription until ctx is cancelled,
// reconnecting with jittered backoff on drop - existing SSE clients simply
// stop seeing updates while a reconnect is in flight, rather than having
// their own HTTP connection torn down. Call it from its own goroutine.
// logMsg labels the reconnect warning with which stream dropped (e.g.
// "bridge update stream ended, reconnecting").
func (h *hub[T]) run(ctx context.Context, logMsg string) {
	backoff := hubReconnectMinDelay
	for ctx.Err() == nil {
		if err := h.streamOnce(ctx); err != nil && ctx.Err() == nil {
			h.logger.Warn(logMsg, zap.Error(err))
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoffutil.Jitter(backoff)):
		}
		backoff = min(backoff*2, hubReconnectMaxDelay)
	}
}

func (h *hub[T]) streamOnce(ctx context.Context) error {
	recv, err := h.connect(ctx)
	if err != nil {
		return err
	}

	for {
		v, err := recv()
		if err != nil {
			return err
		}
		h.broadcast(v)
	}
}

func (h *hub[T]) broadcast(v T) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for ch := range h.subs {
		select {
		case ch <- v:
		default:
			// Slow subscriber - drop rather than block every other
			// subscriber on it. The cell/row it missed just stays stale
			// until the next message arrives.
		}
	}
}

// subscribe registers a new subscriber, returning its message channel and an
// unsubscribe func the caller must call exactly once when done (e.g. via
// defer) to stop receiving messages and release the channel.
func (h *hub[T]) subscribe() (<-chan T, func()) {
	// Sized for a whole reconnect burst (one update per device of a bridge
	// that just came back - see bridge.Source.SendMessage), not steady-state
	// traffic: an SSE tab has no way to resync a dropped message short of a
	// full page reload, so a burst must fit rather than be partly dropped.
	ch := make(chan T, 1024)

	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// deviceHub is a hub fanning out BridgeService.StreamUpdates, so a
// device_info cell a page already rendered from its own GetDevice/
// ListDevices call can be patched live - see handleSSE.
type deviceHub = hub[*api2.Update]

func newDeviceHub(logger *zap.Logger, bridge api2.BridgeServiceClient) *deviceHub {
	return newHub(logger, func(ctx context.Context) (func() (*api2.Update, error), error) {
		stream, err := bridge.StreamUpdates(ctx, &api2.StreamUpdatesRequest{})
		if err != nil {
			return nil, err
		}
		return stream.Recv, nil
	})
}
