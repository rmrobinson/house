package main

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/backoffutil"
)

// policyHub maintains a single upstream PolicyService.StreamEvents
// subscription and fans PolicyEvents out to every subscribed SSE client
// (see handleSSE) - the policy-engine analogue of deviceHub (hub.go), same
// "one upstream stream backs every open browser tab" reasoning.
type policyHub struct {
	logger *zap.Logger
	policy api2.PolicyServiceClient

	mu   sync.Mutex
	subs map[chan *api2.PolicyEvent]struct{}
}

func newPolicyHub(logger *zap.Logger, policy api2.PolicyServiceClient) *policyHub {
	return &policyHub{
		logger: logger,
		policy: policy,
		subs:   make(map[chan *api2.PolicyEvent]struct{}),
	}
}

// run maintains the upstream subscription until ctx is cancelled,
// reconnecting with jittered backoff on drop - see deviceHub.run.
func (h *policyHub) run(ctx context.Context) {
	backoff := hubReconnectMinDelay
	for ctx.Err() == nil {
		if err := h.streamOnce(ctx); err != nil && ctx.Err() == nil {
			h.logger.Warn("policy event stream ended, reconnecting", zap.Error(err))
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoffutil.Jitter(backoff)):
		}
		backoff = min(backoff*2, hubReconnectMaxDelay)
	}
}

func (h *policyHub) streamOnce(ctx context.Context) error {
	stream, err := h.policy.StreamEvents(ctx, &api2.StreamEventsRequest{})
	if err != nil {
		return err
	}

	for {
		ev, err := stream.Recv()
		if err != nil {
			return err
		}
		h.broadcast(ev)
	}
}

func (h *policyHub) broadcast(ev *api2.PolicyEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
			// Slow subscriber - drop rather than block every other
			// subscriber on it, same tradeoff as deviceHub.broadcast.
		}
	}
}

// subscribe registers a new subscriber, returning its event channel and an
// unsubscribe func the caller must call exactly once when done.
func (h *policyHub) subscribe() (<-chan *api2.PolicyEvent, func()) {
	ch := make(chan *api2.PolicyEvent, 16)

	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}
