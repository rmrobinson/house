package castctrl

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/protobuf/proto"
)

// fakeDevice is a minimal hand-rolled server speaking CastMessage framing
// over plain TCP (no TLS — Conn's framing/heartbeat/correlation logic doesn't
// care about the transport's security properties, only conn_test's dial hook
// does, and it points straight at this listener instead of dialTLS).
type fakeDevice struct {
	t        *testing.T
	listener net.Listener

	mu       sync.Mutex
	conn     net.Conn
	received []*CastMessage

	// onMessage, if set, is called for every message received from the
	// client, from the fake device's accept goroutine.
	onMessage func(msg *CastMessage, reply func(*CastMessage))
}

func newFakeDevice(t *testing.T) *fakeDevice {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	fd := &fakeDevice{t: t, listener: l}
	go fd.acceptLoop()
	t.Cleanup(func() { l.Close() })
	return fd
}

func (fd *fakeDevice) addr() string {
	return fd.listener.Addr().String()
}

func (fd *fakeDevice) acceptLoop() {
	for {
		conn, err := fd.listener.Accept()
		if err != nil {
			return
		}

		fd.mu.Lock()
		fd.conn = conn
		fd.mu.Unlock()

		go fd.readLoop(conn)
	}
}

func (fd *fakeDevice) readLoop(conn net.Conn) {
	for {
		msg, err := readCastMessage(conn)
		if err != nil {
			return
		}

		fd.mu.Lock()
		fd.received = append(fd.received, msg)
		onMessage := fd.onMessage
		fd.mu.Unlock()

		if onMessage != nil {
			onMessage(msg, func(reply *CastMessage) {
				fd.send(reply)
			})
		}
	}
}

func (fd *fakeDevice) send(msg *CastMessage) error {
	fd.mu.Lock()
	conn := fd.conn
	fd.mu.Unlock()
	if conn == nil {
		return net.ErrClosed
	}
	return writeCastMessage(conn, msg)
}

func (fd *fakeDevice) receivedCount() int {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return len(fd.received)
}

func (fd *fakeDevice) hasConn() bool {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return fd.conn != nil
}

func dialTo(fd *fakeDevice) dialFunc {
	return func(ctx context.Context, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

func stringPayload(msg *CastMessage) string {
	return msg.GetPayloadUtf8()
}

func decodeType(t *testing.T, msg *CastMessage) string {
	var e envelope
	require.NoError(t, json.Unmarshal([]byte(stringPayload(msg)), &e))
	return e.Type
}

func TestConn_RepliesToInboundPing(t *testing.T) {
	fd := newFakeDevice(t)

	var pongReceived atomic.Bool
	fd.onMessage = func(msg *CastMessage, reply func(*CastMessage)) {
		if msg.GetNamespace() != NamespaceHeartbeat {
			return
		}
		if decodeType(t, msg) == TypePong {
			pongReceived.Store(true)
		}
	}

	c := newConn(zaptest.NewLogger(t), fd.addr(), dialTo(fd), nil, func(bool) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	require.Eventually(t, func() bool { return fd.receivedCount() > 0 || fd.hasConn() }, time.Second, 10*time.Millisecond)

	data, err := json.Marshal(&pingPayload{Type: TypePing})
	require.NoError(t, err)
	require.NoError(t, fd.send(&CastMessage{
		ProtocolVersion: CastMessage_CASTV2_1_0.Enum(),
		SourceId:        proto.String(ReceiverID),
		DestinationId:   proto.String(SenderID),
		Namespace:       proto.String(NamespaceHeartbeat),
		PayloadType:     CastMessage_STRING.Enum(),
		PayloadUtf8:     proto.String(string(data)),
	}))

	assert.Eventually(t, pongReceived.Load, time.Second, 10*time.Millisecond)
}

func TestConn_DeadAfterTwoMissedPongs(t *testing.T) {
	fd := newFakeDevice(t)
	// Never reply to pings — the fake device just swallows them.

	var connected atomic.Bool
	stateChanges := make(chan bool, 10)
	c := newConn(zaptest.NewLogger(t), fd.addr(), dialTo(fd), nil, func(up bool) {
		connected.Store(up)
		stateChanges <- up
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	require.Eventually(t, connected.Load, time.Second, 10*time.Millisecond, "expected initial connect")

	// Two missed PONGs takes 2*heartbeatInterval; wait a bit past that.
	select {
	case up := <-stateChanges: // consume the initial "true"
		require.True(t, up)
	case <-time.After(time.Second):
		t.Fatal("did not observe initial connect")
	}

	select {
	case up := <-stateChanges:
		assert.False(t, up, "expected connection to be reported dead after missed pongs")
	case <-time.After(3 * heartbeatInterval):
		t.Fatal("connection was not torn down after missed pongs")
	}
}

func TestConn_ReconnectBackoff(t *testing.T) {
	var attempts atomic.Int32
	dial := func(ctx context.Context, addr string) (net.Conn, error) {
		attempts.Add(1)
		return nil, net.ErrClosed
	}

	c := newConn(zaptest.NewLogger(t), "unused:0", dial, nil, func(bool) {})

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	c.Run(ctx)

	// Backoff starts at 1s (jittered +/-20%), so within ~2.5s we expect the
	// initial attempt plus one retry, not a tight loop of many attempts.
	n := attempts.Load()
	assert.GreaterOrEqual(t, n, int32(1))
	assert.LessOrEqual(t, n, int32(4))
}

func TestConn_SendRequest_CorrelatesResponse(t *testing.T) {
	fd := newFakeDevice(t)
	fd.onMessage = func(msg *CastMessage, reply func(*CastMessage)) {
		if msg.GetNamespace() != NamespaceReceiver {
			return
		}
		var e envelope
		require.NoError(t, json.Unmarshal([]byte(stringPayload(msg)), &e))

		respData, err := json.Marshal(&receiverStatusPayload{
			Type:      TypeReceiverStatus,
			RequestID: e.RequestID,
		})
		require.NoError(t, err)
		reply(&CastMessage{
			ProtocolVersion: CastMessage_CASTV2_1_0.Enum(),
			SourceId:        proto.String(ReceiverID),
			DestinationId:   proto.String(SenderID),
			Namespace:       proto.String(NamespaceReceiver),
			PayloadType:     CastMessage_STRING.Enum(),
			PayloadUtf8:     proto.String(string(respData)),
		})
	}

	c := newConn(zaptest.NewLogger(t), fd.addr(), dialTo(fd), nil, func(bool) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	require.Eventually(t, fd.hasConn, time.Second, 10*time.Millisecond)

	id := c.NextRequestID()
	reqCtx, reqCancel := context.WithTimeout(context.Background(), time.Second)
	defer reqCancel()

	resp, err := c.SendRequest(reqCtx, NamespaceReceiver, SenderID, ReceiverID, id, &getStatusPayload{Type: TypeGetStatus, RequestID: id})
	require.NoError(t, err)
	assert.Equal(t, TypeReceiverStatus, decodeType(t, resp))
}

func TestConn_SendRequest_TimesOutCleanly(t *testing.T) {
	fd := newFakeDevice(t)
	// No onMessage handler — the fake device never responds.

	c := newConn(zaptest.NewLogger(t), fd.addr(), dialTo(fd), nil, func(bool) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	require.Eventually(t, fd.hasConn, time.Second, 10*time.Millisecond)

	id := c.NextRequestID()
	reqCtx, reqCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer reqCancel()

	_, err := c.SendRequest(reqCtx, NamespaceReceiver, SenderID, ReceiverID, id, &getStatusPayload{Type: TypeGetStatus, RequestID: id})
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	// The pending registration must have been cleaned up, not leaked.
	c.pendingMu.Lock()
	_, stillPending := c.pending[id]
	c.pendingMu.Unlock()
	assert.False(t, stillPending)
}

func TestConn_ConcurrentWritesDontInterleave(t *testing.T) {
	fd := newFakeDevice(t)

	c := newConn(zaptest.NewLogger(t), fd.addr(), dialTo(fd), nil, func(bool) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	require.Eventually(t, fd.hasConn, time.Second, 10*time.Millisecond)

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = c.Send(NamespaceReceiver, SenderID, ReceiverID, &getStatusPayload{Type: TypeGetStatus, RequestID: uint32(i + 1)})
		}(i)
	}
	wg.Wait()

	require.Eventually(t, func() bool { return fd.receivedCount() >= n }, time.Second, 10*time.Millisecond)

	fd.mu.Lock()
	defer fd.mu.Unlock()
	seen := make(map[uint32]bool)
	for _, msg := range fd.received {
		var e envelope
		require.NoError(t, json.Unmarshal([]byte(msg.GetPayloadUtf8()), &e))
		assert.False(t, seen[e.RequestID], "duplicate/corrupted request id %d — frames interleaved", e.RequestID)
		seen[e.RequestID] = true
	}
}
