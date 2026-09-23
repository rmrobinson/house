package castctrl

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/rmrobinson/house/backoffutil"
)

const (
	heartbeatInterval = 5 * time.Second
	// heartbeatMissedLimit consecutive missed PONGs (10s at the interval above)
	// before the connection is considered dead.
	heartbeatMissedLimit = 2

	reconnectMinDelay = 1 * time.Second
	reconnectMaxDelay = 60 * time.Second
)

var (
	errNotConnected     = errors.New("castctrl: not connected")
	errConnectionLost   = errors.New("castctrl: connection lost")
	errHeartbeatTimeout = errors.New("castctrl: heartbeat timeout")
)

// dialFunc opens the underlying transport to addr. Overridable in tests so
// framing/heartbeat/correlation behaviour can be exercised over a plain TCP
// fake instead of standing up a real TLS device.
type dialFunc func(ctx context.Context, addr string) (net.Conn, error)

func dialTLS(ctx context.Context, addr string) (net.Conn, error) {
	// Cast devices present a self-signed certificate that's regenerated
	// periodically; there's no CA to verify against, so verification is
	// disabled by design (see B0 in the handoff doc), not an oversight.
	d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec
	return d.DialContext(ctx, "tcp", addr)
}

// Conn owns a single connection to one Cast device's control port. It
// understands only CastMessage framing, the bidirectional heartbeat, and
// request/response correlation by requestId — it has no notion of
// receiver/app sessions or namespace semantics beyond routing raw messages
// to a caller-supplied handler. Session (session.go) is built on top of it.
//
// Conn.Run owns reconnection: dial failures and connection loss are retried
// with exponential backoff until the context passed to Run is done.
type Conn struct {
	logger *zap.Logger
	addr   string
	dial   dialFunc

	// onMessage receives every inbound message that isn't the heartbeat
	// namespace and isn't a correlated response to a pending SendRequest.
	onMessage func(*CastMessage)
	// onStateChange is called with true once the transport is up (before any
	// Cast-level handshake) and false when it's lost.
	onStateChange func(connected bool)

	mu      sync.Mutex
	netConn net.Conn
	doneCh  chan struct{} // closed when the current connection is lost

	writeMu sync.Mutex // serializes writes; a torn write corrupts framing for every subsequent message

	reqID atomic.Uint32

	pendingMu sync.Mutex
	pending   map[uint32]chan *CastMessage

	missedPongs atomic.Int32
}

// NewConn creates a Conn for the given "host:port" address. The connection
// isn't established until Run is called.
func NewConn(logger *zap.Logger, addr string, onMessage func(*CastMessage), onStateChange func(connected bool)) *Conn {
	return newConn(logger, addr, dialTLS, onMessage, onStateChange)
}

func newConn(logger *zap.Logger, addr string, dial dialFunc, onMessage func(*CastMessage), onStateChange func(connected bool)) *Conn {
	return &Conn{
		logger:        logger,
		addr:          addr,
		dial:          dial,
		onMessage:     onMessage,
		onStateChange: onStateChange,
		pending:       make(map[uint32]chan *CastMessage),
	}
}

// Run dials and maintains the connection, reconnecting with jittered
// exponential backoff (1s to 60s) on dial failure or connection loss, until
// ctx is done. It blocks until then.
func (c *Conn) Run(ctx context.Context) {
	backoff := reconnectMinDelay

	for ctx.Err() == nil {
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}

		c.logger.Warn("cast connection lost, reconnecting",
			zap.String("addr", c.addr), zap.Error(err), zap.Duration("backoff", backoff))
		c.onStateChange(false)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoffutil.Jitter(backoff)):
		}

		backoff *= 2
		if backoff > reconnectMaxDelay {
			backoff = reconnectMaxDelay
		}
	}
}

// runOnce dials, serves one connection to completion, and returns the error
// that ended it. A successful dial always returns via a read or heartbeat
// failure (or ctx being done), never nil.
func (c *Conn) runOnce(ctx context.Context) error {
	netConn, err := c.dial(ctx, c.addr)
	if err != nil {
		return err
	}

	done := make(chan struct{})
	c.mu.Lock()
	c.netConn = netConn
	c.doneCh = done
	c.mu.Unlock()
	c.missedPongs.Store(0)

	defer func() {
		c.mu.Lock()
		c.netConn = nil
		c.mu.Unlock()
		netConn.Close()
		close(done)
		c.clearPending()
	}()

	c.onStateChange(true)

	hbCtx, cancelHB := context.WithCancel(ctx)
	defer cancelHB()

	errCh := make(chan error, 2)
	go func() { errCh <- c.readLoop(netConn) }()
	go func() { errCh <- c.heartbeatLoop(hbCtx) }()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (c *Conn) readLoop(netConn net.Conn) error {
	for {
		msg, err := readCastMessage(netConn)
		if err != nil {
			return err
		}
		c.dispatch(msg)
	}
}

func (c *Conn) heartbeatLoop(ctx context.Context) error {
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
			if err := c.sendJSON(NamespaceHeartbeat, SenderID, ReceiverID, &pingPayload{Type: TypePing}); err != nil {
				return err
			}
		}
	}
}

func (c *Conn) dispatch(msg *CastMessage) {
	if msg.GetNamespace() == NamespaceHeartbeat {
		c.handleHeartbeat(msg)
		return
	}

	if reqID := requestIDFromPayload(msg.GetPayloadUtf8()); reqID != 0 {
		c.pendingMu.Lock()
		ch, ok := c.pending[reqID]
		if ok {
			delete(c.pending, reqID)
		}
		c.pendingMu.Unlock()

		if ok {
			ch <- msg
			return
		}
	}

	if c.onMessage != nil {
		c.onMessage(msg)
	}
}

func (c *Conn) handleHeartbeat(msg *CastMessage) {
	var e envelope
	if err := json.Unmarshal([]byte(msg.GetPayloadUtf8()), &e); err != nil {
		c.logger.Warn("unable to decode heartbeat payload", zap.Error(err))
		return
	}

	switch e.Type {
	case TypePing:
		// Reply on the same endpoint pairing the ping arrived on, mirrored —
		// the device pings both the receiver connection and, once an app is
		// running, the app connection, and expects the reply addressed back
		// to it specifically.
		if err := c.sendJSON(NamespaceHeartbeat, msg.GetDestinationId(), msg.GetSourceId(), &pongPayload{Type: TypePong}); err != nil {
			c.logger.Warn("unable to reply to inbound ping", zap.Error(err))
		}
	case TypePong:
		c.missedPongs.Store(0)
	}
}

// NextRequestID allocates a monotonic, non-zero request ID for correlating a
// request with its response. 0 is reserved to mean "unsolicited" and is
// never returned.
func (c *Conn) NextRequestID() uint32 {
	for {
		if id := c.reqID.Add(1); id != 0 {
			return id
		}
	}
}

// Send marshals payload as JSON and writes it as a CastMessage. It does not
// wait for or correlate any response.
func (c *Conn) Send(namespace, sourceID, destinationID string, payload any) error {
	return c.sendJSON(namespace, sourceID, destinationID, payload)
}

// SendRequest sends payload (which must already carry requestID in its
// requestId field) and waits for either the correlated response, the
// connection being lost, or ctx being done — whichever happens first. The
// pending registration is always cleaned up before returning.
func (c *Conn) SendRequest(ctx context.Context, namespace, sourceID, destinationID string, requestID uint32, payload any) (*CastMessage, error) {
	c.mu.Lock()
	done := c.doneCh
	connected := c.netConn != nil
	c.mu.Unlock()
	if !connected {
		return nil, errNotConnected
	}

	ch, cancel := c.await(requestID)
	defer cancel()

	if err := c.sendJSON(namespace, sourceID, destinationID, payload); err != nil {
		return nil, err
	}

	select {
	case msg := <-ch:
		return msg, nil
	case <-done:
		return nil, errConnectionLost
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Conn) await(requestID uint32) (<-chan *CastMessage, func()) {
	ch := make(chan *CastMessage, 1)

	c.pendingMu.Lock()
	c.pending[requestID] = ch
	c.pendingMu.Unlock()

	return ch, func() {
		c.pendingMu.Lock()
		delete(c.pending, requestID)
		c.pendingMu.Unlock()
	}
}

func (c *Conn) clearPending() {
	c.pendingMu.Lock()
	c.pending = make(map[uint32]chan *CastMessage)
	c.pendingMu.Unlock()
}

func (c *Conn) sendJSON(namespace, sourceID, destinationID string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	msg := &CastMessage{
		ProtocolVersion: CastMessage_CASTV2_1_0.Enum(),
		SourceId:        proto.String(sourceID),
		DestinationId:   proto.String(destinationID),
		Namespace:       proto.String(namespace),
		PayloadType:     CastMessage_STRING.Enum(),
		PayloadUtf8:     proto.String(string(data)),
	}

	c.mu.Lock()
	netConn := c.netConn
	c.mu.Unlock()
	if netConn == nil {
		return errNotConnected
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeCastMessage(netConn, msg)
}

// Close drops the current connection, if any, causing Run's loop to treat it
// as lost and reconnect per its normal backoff. It does not stop Run —
// cancel the context passed to Run for that.
func (c *Conn) Close() {
	c.mu.Lock()
	netConn := c.netConn
	c.mu.Unlock()
	if netConn != nil {
		netConn.Close()
	}
}

func requestIDFromPayload(payload string) uint32 {
	var e envelope
	if err := json.Unmarshal([]byte(payload), &e); err != nil {
		return 0
	}
	return e.RequestID
}

func writeCastMessage(w io.Writer, msg *CastMessage) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}

	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func readCastMessage(r io.Reader) (*CastMessage, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}

	data := make([]byte, binary.BigEndian.Uint32(lenBuf[:]))
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}

	msg := &CastMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		return nil, err
	}
	return msg, nil
}
