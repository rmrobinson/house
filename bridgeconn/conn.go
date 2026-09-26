// Package bridgeconn provides a reconnecting BridgeService client: dial,
// stream StreamUpdates, and retry with jittered exponential backoff on drop.
// It's the pattern every long-lived BridgeService consumer in this repo
// needs (service/bridge/facade's per-upstream connection,
// service/policy/bridgehome's Adapter) - Conn holds exactly that mechanics,
// leaving what to do with each Update and what "connected" means to the
// caller.
package bridgeconn

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/backoffutil"
	"github.com/rmrobinson/house/grpcutil"
)

const (
	MinBackoff = time.Second
	MaxBackoff = 30 * time.Second
)

// Conn manages one long-lived BridgeService connection: dialing, streaming
// updates, and reconnecting with jittered exponential backoff whenever it
// drops. The zero value is not usable; construct with New.
type Conn struct {
	addr   string
	logger *zap.Logger

	// backoff is nanoseconds, reset to MinBackoff once the connection has
	// actually delivered a message (see connectOnce) so a brief blip after a
	// long stable connection doesn't pay for backoff accumulated by earlier,
	// unrelated failures.
	backoff atomic.Int64

	mu     sync.Mutex
	client api2.BridgeServiceClient
}

// New creates a Conn that will dial addr once Run is called.
func New(logger *zap.Logger, addr string) *Conn {
	c := &Conn{logger: logger, addr: addr}
	c.backoff.Store(int64(MinBackoff))
	return c
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
	for ctx.Err() == nil {
		if err := c.connectOnce(ctx, onUpdate, onDrop); err != nil && ctx.Err() == nil {
			c.logger.Warn("bridge connection ended, will retry", zap.String("addr", c.addr), zap.Error(err))
		}

		backoff := time.Duration(c.backoff.Load())

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoffutil.Jitter(backoff)):
		}

		c.backoff.Store(int64(min(backoff*2, MaxBackoff)))
	}
}

func (c *Conn) connectOnce(ctx context.Context, onUpdate func(*api2.Update), onDrop func()) error {
	conn, err := grpcutil.DialInsecure(c.addr)
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
			c.backoff.Store(int64(MinBackoff))
			first = false
		}

		onUpdate(update)
	}
}
