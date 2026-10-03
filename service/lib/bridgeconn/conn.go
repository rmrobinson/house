// Package bridgeconn provides a reconnecting BridgeService client: dial,
// stream StreamUpdates, and retry with jittered exponential backoff on drop.
// It's the pattern every long-lived BridgeService consumer in this repo
// needs (service/bridge/facade's per-upstream connection,
// service/policy/bridgehome's Adapter) - Conn holds exactly that mechanics,
// leaving what to do with each Update and what "connected" means to the
// caller.
//
// Backoff and Retry hold the reconnect-with-jittered-exponential-backoff
// mechanics on their own, independent of BridgeService, so a second
// long-lived streaming RPC client elsewhere in the repo (e.g.
// service/policy/housestate's Adapter, for HouseService.StreamHouseUpdates)
// can reuse the same behavior instead of hand-copying it.
package bridgeconn

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/backoffutil"
	"github.com/rmrobinson/house/service/lib/grpcutil"
)

const (
	MinBackoff = time.Second
	MaxBackoff = 30 * time.Second
)

// Backoff tracks jittered exponential backoff state across reconnect
// attempts for one long-lived streaming RPC client. The zero value is not
// usable; construct with NewBackoff.
type Backoff struct {
	// d is nanoseconds, reset to MinBackoff once the connection has actually
	// delivered a message (see Reset) so a brief blip after a long stable
	// connection doesn't pay for backoff accumulated by earlier, unrelated
	// failures.
	d atomic.Int64
}

// NewBackoff creates a Backoff starting at MinBackoff.
func NewBackoff() *Backoff {
	b := &Backoff{}
	b.d.Store(int64(MinBackoff))
	return b
}

// Current returns the backoff duration the next Wait call will sleep for,
// before jitter and growth.
func (b *Backoff) Current() time.Duration {
	return time.Duration(b.d.Load())
}

// Set seeds the backoff duration directly. Exposed for tests that need to
// simulate backoff already grown by prior failures.
func (b *Backoff) Set(d time.Duration) {
	b.d.Store(int64(d))
}

// Reset collapses the backoff back to MinBackoff. Call once a connection has
// actually delivered data, confirming it's live.
func (b *Backoff) Reset() {
	b.d.Store(int64(MinBackoff))
}

// Wait blocks for the current backoff duration (jittered) or until ctx is
// done, whichever comes first, then grows the backoff toward MaxBackoff for
// the next call. Returns ctx.Err() if ctx is why it returned.
func (b *Backoff) Wait(ctx context.Context) error {
	d := b.Current()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(backoffutil.Jitter(d)):
	}
	b.d.Store(int64(min(d*2, MaxBackoff)))
	return nil
}

// Retry calls connectOnce repeatedly until ctx is done, waiting with
// jittered exponential backoff (via backoff) between attempts whenever
// connectOnce returns an error. onError, if non-nil, is called with that
// error each time (but not when it's just ctx being cancelled), so callers
// can log it with whatever fields identify the specific connection (address,
// building ID, ...). connectOnce is handed backoff and should call its
// Reset once its connection has actually delivered data, confirming it's
// live - the same reasoning Conn.connectOnce documents for resetting there
// rather than right after dialing.
func Retry(ctx context.Context, backoff *Backoff, onError func(error), connectOnce func(ctx context.Context, backoff *Backoff) error) {
	for ctx.Err() == nil {
		if err := connectOnce(ctx, backoff); err != nil && ctx.Err() == nil && onError != nil {
			onError(err)
		}
		if backoff.Wait(ctx) != nil {
			return
		}
	}
}

// Conn manages one long-lived BridgeService connection: dialing, streaming
// updates, and reconnecting with jittered exponential backoff whenever it
// drops. The zero value is not usable; construct with New.
type Conn struct {
	addr    string
	logger  *zap.Logger
	tlsCfg  *grpcutil.ClientTLSConfig
	backoff *Backoff

	mu     sync.Mutex
	client api2.BridgeServiceClient
}

// New creates a Conn that will dial addr once Run is called. tlsCfg, if
// non-nil, is used for the connection (see grpcutil.Dial); nil means
// plaintext gRPC.
func New(logger *zap.Logger, addr string, tlsCfg *grpcutil.ClientTLSConfig) *Conn {
	return &Conn{logger: logger, addr: addr, tlsCfg: tlsCfg, backoff: NewBackoff()}
}

// Client returns the client for the current connection, or nil if Run has
// never connected or the connection is currently down.
func (c *Conn) Client() api2.BridgeServiceClient {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client
}

// Run connects to addr and streams updates until ctx is cancelled,
// reconnecting with jittered exponential backoff whenever the connection
// drops. onUpdate is called synchronously for every Update received. onDrop,
// if non-nil, is called each time the connection is lost - including when
// ctx is cancelled while connected - after Client() has already started
// returning nil for it. Run blocks until ctx is done.
func (c *Conn) Run(ctx context.Context, onUpdate func(*api2.Update), onDrop func()) {
	Retry(ctx, c.backoff, func(err error) {
		c.logger.Warn("bridge connection ended, will retry", zap.String("addr", c.addr), zap.Error(err))
	}, func(ctx context.Context, backoff *Backoff) error {
		return c.connectOnce(ctx, backoff, onUpdate, onDrop)
	})
}

func (c *Conn) connectOnce(ctx context.Context, backoff *Backoff, onUpdate func(*api2.Update), onDrop func()) error {
	conn, err := grpcutil.Dial(c.addr, c.tlsCfg)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	client := api2.NewBridgeServiceClient(conn)
	c.mu.Lock()
	c.client = client
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.client = nil
		c.mu.Unlock()
		if onDrop != nil {
			onDrop()
		}
	}()

	stream, err := client.StreamUpdates(ctx, &api2.StreamUpdatesRequest{})
	if err != nil {
		return fmt.Errorf("stream updates: %w", err)
	}

	first := true
	for {
		update, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}

		if first {
			// The connection is only confirmed live once a message has
			// actually been received from it - neither a successful Dial
			// (grpcutil.DialInsecure dials lazily and essentially never
			// fails synchronously) nor a successful StreamUpdates call
			// (which can still return a stream that errors on the first
			// Recv) proves the upstream is reachable. Resetting backoff
			// here, rather than right after Dial, means a persistently
			// failing upstream actually backs off toward MaxBackoff instead
			// of retrying at the minimum forever.
			backoff.Reset()
			first = false
		}

		onUpdate(update)
	}
}
