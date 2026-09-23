package webosctrl

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/rmrobinson/house/backoffutil"
)

const (
	reconnectMinDelay = 1 * time.Second
	reconnectMaxDelay = 60 * time.Second

	// heartbeatInterval is how often a WS ping is sent on an otherwise-idle
	// connection. Mirrors bridges/lib/castctrl's heartbeat cadence.
	heartbeatInterval = 5 * time.Second
	// heartbeatMissedLimit consecutive missed PONGs (10s at the interval
	// above) before the connection is declared dead. Without this, a TV that
	// vanishes without sending a WS close frame (e.g. losing power mid
	// system/turnOff rather than closing the socket first - confirmed live
	// against a real webOS 5.5 set) leaves ReadMessage blocked indefinitely,
	// so Reachable never flips false and a subsequent SetOnOff(true) skips
	// Wake-on-LAN because the stale state says it doesn't need to.
	heartbeatMissedLimit = 2
	// pingWriteTimeout bounds a single ping control frame write.
	pingWriteTimeout = 1 * time.Second
)

var (
	errNotConnected     = errors.New("webosctrl: not connected")
	errConnectionLost   = errors.New("webosctrl: connection lost")
	errHeartbeatTimeout = errors.New("webosctrl: heartbeat timeout")
)

// dialFunc opens the underlying websocket transport to host. Overridable in
// tests so Conn's request/response/subscription plumbing can be exercised
// against an in-process fake server instead of a real TV.
type dialFunc func(ctx context.Context, host string) (*websocket.Conn, error)

// dialTV dials wss://<host>:3001 (webOS 4.x+/2018+, mandatory on 2023+
// firmware) and falls back to ws://<host>:3000 (pre-2018 sets) if that
// fails. No webOS TV is known to meaningfully support both at once, so this
// tries the modern port first rather than probing both in parallel.
//
// TLS verification is disabled: webOS TVs present a certificate from LG's
// own private CA, which doesn't chain to any public root, and LG doesn't
// publish a way to verify it. Every known community client (the only prior
// art here, since LG doesn't document this protocol) disables verification
// for exactly this reason. This is a real tradeoff - it trusts any TLS cert
// from anything claiming to be the TV's IP - stated explicitly here rather
// than silently inherited.
func dialTV(ctx context.Context, host string) (*websocket.Conn, error) {
	dialer := &websocket.Dialer{
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		HandshakeTimeout: 10 * time.Second,
	}

	wssURL := fmt.Sprintf("wss://%s:3001", host)
	conn, _, err := dialer.DialContext(ctx, wssURL, nil)
	if err == nil {
		return conn, nil
	}

	wsURL := fmt.Sprintf("ws://%s:3000", host)
	conn, _, err2 := dialer.DialContext(ctx, wsURL, nil)
	if err2 != nil {
		return nil, fmt.Errorf("wss dial failed (%w), ws fallback failed (%w)", err, err2)
	}
	return conn, nil
}

// Conn owns a single websocket connection to one webOS TV. It understands
// only the {type,id,uri,payload} JSON envelope and request/response
// correlation by id - it has no notion of the register handshake or ssap://
// semantics beyond routing decoded envelopes to a caller-supplied handler
// or a pending waiter. Session (session.go) is built on top of it, mirroring
// castctrl's Conn/Session split.
//
// Conn.Run owns reconnection: dial failures and connection loss are retried
// with jittered exponential backoff until the context passed to Run is done.
type Conn struct {
	logger *zap.Logger
	host   string
	dial   dialFunc

	// onStateChange is called with true once the transport is up, false
	// when it's lost or was never established. Subscriptions do not survive
	// a reconnect (the TV has no memory of a prior socket's subscribe
	// calls) - the caller is expected to re-subscribe on every true.
	onStateChange func(connected bool)

	mu     sync.Mutex
	ws     *websocket.Conn
	doneCh chan struct{}

	writeMu sync.Mutex

	reqID atomic.Uint64

	pendingMu sync.Mutex
	pending   map[string]chan envelope

	subMu sync.Mutex
	subs  map[string]func(envelope)

	backoff atomic.Int64 // nanoseconds; reset to reconnectMinDelay on every successful dial

	missedPongs atomic.Int32
}

// NewConn creates a Conn for the TV at host (a bare IP or hostname, no
// port). The connection isn't established until Run is called.
func NewConn(logger *zap.Logger, host string, onStateChange func(connected bool)) *Conn {
	return newConn(logger, host, dialTV, onStateChange)
}

func newConn(logger *zap.Logger, host string, dial dialFunc, onStateChange func(connected bool)) *Conn {
	c := &Conn{
		logger:        logger,
		host:          host,
		dial:          dial,
		onStateChange: onStateChange,
		pending:       make(map[string]chan envelope),
		subs:          make(map[string]func(envelope)),
	}
	c.backoff.Store(int64(reconnectMinDelay))
	return c
}

// Run dials and maintains the connection, reconnecting with jittered
// exponential backoff (1s to 60s, reset on every successful dial) on dial
// failure or connection loss, until ctx is done. It blocks until then - call
// it from its own goroutine.
func (c *Conn) Run(ctx context.Context) {
	for ctx.Err() == nil {
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}

		backoff := time.Duration(c.backoff.Load())
		c.logger.Warn("webos connection lost, reconnecting",
			zap.String("host", c.host), zap.Error(err), zap.Duration("backoff", backoff))
		c.onStateChange(false)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoffutil.Jitter(backoff)):
		}

		c.backoff.Store(int64(min(backoff*2, reconnectMaxDelay)))
	}
}

// runOnce dials, serves one connection to completion, and returns the error
// that ended it.
func (c *Conn) runOnce(ctx context.Context) error {
	ws, err := c.dial(ctx, c.host)
	if err != nil {
		return err
	}
	c.backoff.Store(int64(reconnectMinDelay))
	c.missedPongs.Store(0)
	ws.SetPongHandler(func(string) error {
		c.missedPongs.Store(0)
		return nil
	})

	done := make(chan struct{})
	c.mu.Lock()
	c.ws = ws
	c.doneCh = done
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.ws = nil
		c.mu.Unlock()
		ws.Close()
		close(done)
		c.clearPending()
	}()

	c.onStateChange(true)

	hbCtx, cancelHB := context.WithCancel(ctx)
	defer cancelHB()

	errCh := make(chan error, 2)
	go func() { errCh <- c.readLoop(ws) }()
	go func() { errCh <- c.heartbeatLoop(hbCtx, ws) }()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// heartbeatLoop sends a WS ping every heartbeatInterval and declares the
// connection dead after heartbeatMissedLimit consecutive pings go
// unanswered - see heartbeatMissedLimit's doc comment for why this matters.
// ws.SetPongHandler (registered in runOnce) resets the counter as pongs
// arrive via readLoop's ReadMessage calls.
func (c *Conn) heartbeatLoop(ctx context.Context, ws *websocket.Conn) error {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if c.missedPongs.Add(1) >= heartbeatMissedLimit {
				return errHeartbeatTimeout
			}
			c.writeMu.Lock()
			err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(pingWriteTimeout))
			c.writeMu.Unlock()
			if err != nil {
				return err
			}
		}
	}
}

func (c *Conn) readLoop(ws *websocket.Conn) error {
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}

		var e envelope
		if err := json.Unmarshal(data, &e); err != nil {
			c.logger.Warn("unable to decode inbound envelope", zap.Error(err))
			continue
		}
		c.dispatch(e)
	}
}

// dispatch routes an inbound envelope to whatever's interested: an active
// subscription (checked first and never removed, since a subscribed uri
// keeps pushing under the same id for the connection's lifetime), else a
// one-shot waiter registered via await.
func (c *Conn) dispatch(e envelope) {
	c.subMu.Lock()
	handler, ok := c.subs[e.ID]
	c.subMu.Unlock()
	if ok {
		handler(e)
		return
	}

	c.pendingMu.Lock()
	ch, ok := c.pending[e.ID]
	c.pendingMu.Unlock()
	if ok {
		ch <- e
	}
}

// NextRequestID allocates a request id unique for this Conn's lifetime.
func (c *Conn) NextRequestID() string {
	return fmt.Sprintf("req-%d", c.reqID.Add(1))
}

// await registers interest in future envelopes carrying id and returns a
// channel of them plus a cancel func to stop listening. Buffered so a
// multi-message exchange (e.g. the register handshake's intermediate
// "response" before its terminal "registered") can't be missed by a slow
// reader.
func (c *Conn) await(id string) (<-chan envelope, func()) {
	ch := make(chan envelope, 4)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	return ch, func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}
}

func (c *Conn) clearPending() {
	c.pendingMu.Lock()
	c.pending = make(map[string]chan envelope)
	c.pendingMu.Unlock()

	// Subscriptions don't survive a lost connection either - the caller
	// re-subscribes from scratch on the next successful connect.
	c.subMu.Lock()
	c.subs = make(map[string]func(envelope))
	c.subMu.Unlock()
}

// subscribe registers a persistent handler for every future envelope
// carrying id. Used for ssap:// subscribe requests, which push repeated
// updates under the original request's id rather than a one-shot response.
func (c *Conn) subscribe(id string, handler func(envelope)) {
	c.subMu.Lock()
	c.subs[id] = handler
	c.subMu.Unlock()
}

// send marshals req and writes it. It does not wait for or correlate any
// response.
func (c *Conn) send(req requestEnvelope) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}

	c.mu.Lock()
	ws := c.ws
	c.mu.Unlock()
	if ws == nil {
		return errNotConnected
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return ws.WriteMessage(websocket.TextMessage, data)
}

// connectedDone reports whether a connection is currently live and, if so,
// the channel that closes when it's lost.
func (c *Conn) connectedDone() (chan struct{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.doneCh, c.ws != nil
}

// SendRequest sends req and waits for the first envelope with a matching id,
// the connection being lost, or ctx being done - whichever happens first.
func (c *Conn) SendRequest(ctx context.Context, req requestEnvelope) (envelope, error) {
	done, connected := c.connectedDone()
	if !connected {
		return envelope{}, errNotConnected
	}

	ch, cancel := c.await(req.ID)
	defer cancel()

	if err := c.send(req); err != nil {
		return envelope{}, err
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-done:
		return envelope{}, errConnectionLost
	case <-ctx.Done():
		return envelope{}, ctx.Err()
	}
}

// Close drops the current connection, if any, causing Run's loop to treat it
// as lost and reconnect per its normal backoff. It does not stop Run - cancel
// the context passed to Run for that.
func (c *Conn) Close() {
	c.mu.Lock()
	ws := c.ws
	c.mu.Unlock()
	if ws != nil {
		ws.Close()
	}
}
