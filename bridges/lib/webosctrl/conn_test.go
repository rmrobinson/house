package webosctrl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// fakeDevice is a minimal in-process server speaking the {type,id,uri,
// payload} JSON envelope over a real websocket - Conn's correlation/
// subscription/reconnect logic doesn't care about TLS, only conn_test's dial
// hook does, and it points straight at this listener's plain-ws address
// instead of dialTV's wss/ws-fallback logic.
type fakeDevice struct {
	t      *testing.T
	server *httptest.Server

	mu   sync.Mutex
	conn *websocket.Conn

	// onEnvelope, if set, is called for every envelope received from the
	// client, from the fake device's read goroutine. reply sends a message
	// back on the same connection.
	onEnvelope func(e requestEnvelope, reply func(any))

	// ignorePings, if set, suppresses gorilla's default auto-pong behaviour -
	// simulating a device that's vanished without closing the socket (see
	// TestConn_DeadAfterMissedPongs).
	ignorePings bool
}

func newFakeDevice(t *testing.T) *fakeDevice {
	fd := &fakeDevice{t: t}

	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)

		if fd.ignorePings {
			c.SetPingHandler(func(string) error { return nil })
		}

		fd.mu.Lock()
		fd.conn = c
		fd.mu.Unlock()

		go fd.readLoop(c)
	})

	fd.server = httptest.NewServer(mux)
	t.Cleanup(fd.server.Close)
	return fd
}

// addr returns the bare host:port this fake device listens on, matching
// what Conn's host parameter expects.
func (fd *fakeDevice) addr() string {
	return fd.server.Listener.Addr().String()
}

// dial connects to fd exactly like dialTV would connect to a real TV, minus
// TLS and the wss/ws fallback - a plain ws:// dial straight at fd's address.
func (fd *fakeDevice) dial(ctx context.Context, host string) (*websocket.Conn, error) {
	u := "ws://" + host
	c, _, err := websocket.DefaultDialer.DialContext(ctx, u, nil)
	return c, err
}

func (fd *fakeDevice) readLoop(c *websocket.Conn) {
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}

		var e requestEnvelope
		if err := json.Unmarshal(data, &e); err != nil {
			continue
		}

		fd.mu.Lock()
		onEnvelope := fd.onEnvelope
		fd.mu.Unlock()

		if onEnvelope != nil {
			onEnvelope(e, func(payload any) { fd.reply(e.ID, payload) })
		}
	}
}

// reply sends a {"type":"response","id":id,"payload":payload} message.
func (fd *fakeDevice) reply(id string, payload any) {
	fd.send(envelope{Type: "response", ID: id, Payload: mustMarshal(fd.t, payload)})
}

func (fd *fakeDevice) send(e envelope) {
	data, err := json.Marshal(e)
	require.NoError(fd.t, err)

	fd.mu.Lock()
	c := fd.conn
	fd.mu.Unlock()
	require.NotNil(fd.t, c, "fake device has no active connection yet")

	require.NoError(fd.t, c.WriteMessage(websocket.TextMessage, data))
}

// closeConn drops the current server-side connection, simulating the device
// dropping the socket (e.g. going into full standby).
func (fd *fakeDevice) closeConn() {
	fd.mu.Lock()
	c := fd.conn
	fd.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}

func TestConn_SendRequest_CorrelatesResponse(t *testing.T) {
	fd := newFakeDevice(t)
	fd.onEnvelope = func(e requestEnvelope, reply func(any)) {
		if e.URI == "ssap://audio/getVolume" {
			reply(volumePayload{Volume: 42, Muted: true})
		}
	}

	c := newConn(zaptest.NewLogger(t), fd.addr(), fd.dial, func(bool) {})
	go c.Run(t.Context())

	require.Eventually(t, func() bool {
		_, connected := c.connectedDone()
		return connected
	}, time.Second, 10*time.Millisecond)

	resp, err := c.SendRequest(context.Background(), requestEnvelope{
		Type: "request", ID: c.NextRequestID(), URI: "ssap://audio/getVolume",
	})
	require.NoError(t, err)

	var p volumePayload
	require.NoError(t, json.Unmarshal(resp.Payload, &p))
	assert.EqualValues(t, 42, p.Volume)
	assert.True(t, p.Muted)
}

func TestConn_Subscribe_ReceivesRepeatedPushes(t *testing.T) {
	fd := newFakeDevice(t)

	c := newConn(zaptest.NewLogger(t), fd.addr(), fd.dial, func(bool) {})
	go c.Run(t.Context())

	require.Eventually(t, func() bool {
		_, connected := c.connectedDone()
		return connected
	}, time.Second, 10*time.Millisecond)

	received := make(chan volumePayload, 4)
	subID := c.NextRequestID()
	c.subscribe(subID, func(e envelope) {
		var p volumePayload
		if err := json.Unmarshal(e.Payload, &p); err == nil {
			received <- p
		}
	})

	require.NoError(t, c.send(requestEnvelope{Type: "subscribe", ID: subID, URI: "ssap://audio/getVolume"}))

	// The device pushes two updates under the same subscription id - a
	// one-shot pending waiter would only ever see the first.
	fd.send(envelope{Type: "response", ID: subID, Payload: mustMarshal(t, volumePayload{Volume: 10})})
	fd.send(envelope{Type: "response", ID: subID, Payload: mustMarshal(t, volumePayload{Volume: 20})})

	select {
	case p := <-received:
		assert.EqualValues(t, 10, p.Volume)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first push")
	}
	select {
	case p := <-received:
		assert.EqualValues(t, 20, p.Volume)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second push")
	}
}

func TestConn_Run_ReconnectsAfterConnectionLoss(t *testing.T) {
	fd := newFakeDevice(t)

	var stateChanges []bool
	var mu sync.Mutex
	c := newConn(zaptest.NewLogger(t), fd.addr(), fd.dial, func(connected bool) {
		mu.Lock()
		stateChanges = append(stateChanges, connected)
		mu.Unlock()
	})

	go c.Run(t.Context())

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(stateChanges) >= 1
	}, time.Second, 10*time.Millisecond)

	fd.closeConn()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		// true (initial connect), false (loss), true (reconnect)
		return len(stateChanges) >= 3
	}, 5*time.Second, 10*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []bool{true, false, true}, stateChanges)
}

// TestConn_DeadAfterMissedPongs covers the gap a real webOS 5.5 TV exposed
// live: it went into full standby mid system/turnOff without ever closing
// the WS socket, so readLoop's ReadMessage blocked forever and Reachable
// never flipped false. The heartbeat ping/pong is what's supposed to catch
// exactly this.
func TestConn_DeadAfterMissedPongs(t *testing.T) {
	fd := newFakeDevice(t)
	fd.ignorePings = true

	var stateChanges []bool
	var mu sync.Mutex
	c := newConn(zaptest.NewLogger(t), fd.addr(), fd.dial, func(connected bool) {
		mu.Lock()
		stateChanges = append(stateChanges, connected)
		mu.Unlock()
	})

	go c.Run(t.Context())

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(stateChanges) >= 1
	}, time.Second, 10*time.Millisecond)

	// Two missed PONGs takes 2*heartbeatInterval; wait a bit past that.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(stateChanges) >= 2
	}, 3*heartbeatInterval, 10*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []bool{true, false}, stateChanges)
}
