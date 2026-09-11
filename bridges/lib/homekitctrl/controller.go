package homekitctrl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// CharID identifies a single characteristic on a paired accessory's connection: an accessory ID
// (aid) and characteristic ID (iid) pair. A controller's connection can span multiple aids - a
// thermostat's own aid plus any remote sensors paired alongside it under the same connection.
type CharID struct {
	AccessoryID      uint64
	CharacteristicID uint64
}

// CharacteristicValue is one characteristic's value as returned by a read.
type CharacteristicValue struct {
	AccessoryID      uint64          `json:"aid"`
	CharacteristicID uint64          `json:"iid"`
	Value            json.RawMessage `json:"value,omitempty"`
	Status           *int            `json:"status,omitempty"`
}

// CharacteristicWrite is one characteristic to write.
type CharacteristicWrite struct {
	AccessoryID      uint64
	CharacteristicID uint64
	Value            any
}

// Controller is a single authenticated session with a paired HAP accessory. It owns one
// persistent TCP connection - HAP doesn't support request pipelining over pair-verify'd
// sessions, so every operation is serialized through mu rather than allowing concurrent use.
//
// A single background goroutine (runReader, see events.go) owns every Read on conn for the
// Controller's whole lifetime, demuxing each frame as either the response to whatever request is
// currently pending (delivered via pending) or an unsolicited EVENT/1.0 push (dispatched to
// onEvent). do() only ever writes to conn, never reads from it directly - net.Conn's contract
// allows one concurrent reader and one concurrent writer safely, and encryptedConn's Read/Write
// touch disjoint fields (readAEAD/readCounter/readBuf vs writeAEAD/writeCounter - see session.go),
// so no additional lock around conn itself is needed for this to be safe.
type Controller struct {
	mu   sync.Mutex
	conn *encryptedConn
	host string

	// pending is the current do() call's response channel, if any - at most one is ever in
	// flight since do() holds mu for its entire duration. runReader delivers a non-EVENT frame
	// here; do() clears it back to nil when it returns, whether by response, timeout, or Close.
	pending atomic.Pointer[chan *hapMessage]

	// evMu guards onEvent/onDisconnect independently of mu, since they're read from the
	// runReader goroutine while do() may be holding mu for an unrelated in-flight request.
	evMu         sync.Mutex
	onEvent      func([]CharacteristicValue)
	onDisconnect func(error)

	closeOnce sync.Once
	closed    chan struct{}
}

// Connect dials host ("ip:port") and performs pair-verify against it, authenticating the
// accessory against accessory.PublicKey and proving identity has controller's own key. host is
// expected to come from a fresh Discover/DeviceByID call rather than a cached address - see the
// package doc for why.
func Connect(ctx context.Context, host string, controller *ControllerIdentity, accessory *AccessoryRecord) (*Controller, error) {
	// KeepAlive matters specifically for this accessory: without it, a peer that vanishes
	// without sending FIN/RST (a silent network drop, which ecobee's HAP server is documented to
	// do) leaves a half-open connection with no OS-level signal that anything is wrong, on top of
	// whatever request-level deadline withDeadline applies.
	d := net.Dialer{KeepAlive: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", host, err)
	}

	result, err := pairVerify(ctx, conn, host, controller, accessory)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("pair-verify against %s: %w", host, err)
	}

	// pairVerify's postTLV8 calls leave conn's deadline set (withDeadline sets it but never
	// clears it - see timeout.go), which do() no longer refreshes per-call now that it waits on
	// runReader via a channel instead of reading the conn directly. Left uncleared, that stale
	// deadline would eventually expire and permanently fail every future Read on this connection,
	// well after pair-verify itself has nothing to do with the failure.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("clear pair-verify deadline: %w", err)
	}

	encConn, err := newEncryptedConn(conn, result.controllerToAccessoryKey, result.accessoryToControllerKey)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("init encrypted session with %s: %w", host, err)
	}

	c := &Controller{conn: encConn, host: host, closed: make(chan struct{})}
	// Started unconditionally, not lazily on first Subscribe call: do() depends on runReader to
	// deliver every response, whether or not this Controller is ever subscribed to anything.
	go c.runReader()
	return c, nil
}

// Close releases the underlying connection. The Controller is unusable afterwards - callers
// needing to keep talking to the accessory should Connect again.
//
// Deliberately does not take mu: do() can be blocked waiting on a response for up to
// defaultOperationTimeout (or ctx's own deadline) at the time Close is called, and closing the
// underlying net.Conn is exactly what's supposed to unblock both that pending call and the
// background reader goroutine (a Read/Write on a closed net.Conn returns promptly with an error,
// per net.Conn's documented behavior) - requiring mu here would make Close wait for the very call
// it's meant to interrupt, deadlocking forever.
func (c *Controller) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.conn.Close()
}

// ReadCharacteristics reads the current value of each identified characteristic in a single
// request. The returned slice is in the order the accessory chose to reply in, not necessarily
// the order requested - match on AccessoryID/CharacteristicID, not position.
func (c *Controller) ReadCharacteristics(ctx context.Context, ids []CharID) ([]CharacteristicValue, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	var q bytes.Buffer
	q.WriteString("/characteristics?id=")
	for i, id := range ids {
		if i > 0 {
			q.WriteByte(',')
		}
		fmt.Fprintf(&q, "%d.%d", id.AccessoryID, id.CharacteristicID)
	}

	body, status, err := c.do(ctx, http.MethodGet, q.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("read characteristics: %w", err)
	}
	// HAP uses 207 Multi-Status when one or more requested characteristics in the batch
	// individually failed; the per-item Status field in the parsed body reflects that.
	if status != http.StatusOK && status != http.StatusMultiStatus {
		return nil, fmt.Errorf("read characteristics: unexpected status %d: %s", status, body)
	}

	var parsed struct {
		Characteristics []CharacteristicValue `json:"characteristics"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("read characteristics: parse response: %w", err)
	}
	return parsed.Characteristics, nil
}

// WriteCharacteristics writes every characteristic in a single request. Returns an error naming
// the first characteristic that failed, if any - per the plan's write-reliability findings,
// callers should treat a successful write here as provisional and re-read to confirm the
// accessory actually applied it, not as proof on its own.
func (c *Controller) WriteCharacteristics(ctx context.Context, writes []CharacteristicWrite) error {
	if len(writes) == 0 {
		return nil
	}

	type wireWrite struct {
		AccessoryID      uint64 `json:"aid"`
		CharacteristicID uint64 `json:"iid"`
		Value            any    `json:"value"`
	}
	payload := struct {
		Characteristics []wireWrite `json:"characteristics"`
	}{}
	for _, w := range writes {
		payload.Characteristics = append(payload.Characteristics, wireWrite{w.AccessoryID, w.CharacteristicID, w.Value})
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("write characteristics: marshal request: %w", err)
	}

	respBody, status, err := c.do(ctx, http.MethodPut, "/characteristics", body)
	if err != nil {
		return fmt.Errorf("write characteristics: %w", err)
	}
	if status == http.StatusNoContent {
		return nil
	}
	if status == http.StatusMultiStatus {
		var parsed struct {
			Characteristics []struct {
				AccessoryID      uint64 `json:"aid"`
				CharacteristicID uint64 `json:"iid"`
				Status           int    `json:"status"`
			} `json:"characteristics"`
		}
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return fmt.Errorf("write characteristics: parse multi-status response: %w", err)
		}
		for _, item := range parsed.Characteristics {
			if item.Status != 0 {
				return fmt.Errorf("write characteristics: %d.%d failed with HAP status %d", item.AccessoryID, item.CharacteristicID, item.Status)
			}
		}
		return nil
	}
	return fmt.Errorf("write characteristics: unexpected status %d: %s", status, respBody)
}

// operationCtx bounds a single do() exchange when ctx carries no deadline of its own -
// defaultOperationTimeout (see timeout.go) is the fallback, for the same reason withDeadline
// originally needed it: this accessory is documented to hang rather than error promptly.
// Distinct from withDeadline (still used by verify.go's pre-Controller pair-verify handshake):
// once runReader owns every Read on conn, a per-call conn.SetDeadline would incorrectly bound the
// background reader's (or an unrelated concurrent call's) I/O too, since SetDeadline applies to
// the whole net.Conn, not just the call that set it.
func operationCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultOperationTimeout)
}

// do writes a single HAP JSON request to the encrypted connection and waits for runReader (see
// events.go) to deliver the matching response. Requests are serialized (mu) since pair-verify'd
// HAP sessions don't support pipelining - only one request may be in flight at a time, which is
// what lets a single pending slot (rather than a request-ID map) correctly correlate the response.
// pending is registered before the request is written, not after, so a fast reply can never race
// ahead of this call's readiness to receive it.
func (c *Controller) do(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	respCh := make(chan *hapMessage, 1)
	c.pending.Store(&respCh)
	defer c.pending.Store(nil)

	opCtx, cancel := operationCtx(ctx)
	defer cancel()

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequest(method, "http://"+c.host+path, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/hap+json")
		req.ContentLength = int64(len(body))
	}

	if err := req.Write(c.conn); err != nil {
		return nil, 0, fmt.Errorf("write request: %w", err)
	}

	select {
	case msg := <-respCh:
		if msg.err != nil {
			return nil, 0, fmt.Errorf("read response: %w", msg.err)
		}
		return msg.body, msg.statusCode, nil
	case <-opCtx.Done():
		// Giving up here doesn't mean the accessory won't still reply eventually - HAP has no
		// pipelining and no request ID to correlate a late reply against, so if this connection
		// stayed open, that stale reply could land on a completely unrelated future call's
		// pending slot and be misdelivered as its response. Closing removes that risk entirely:
		// this Controller becomes unusable (matching Close's own doc comment), forcing the caller
		// to reconnect from scratch rather than continuing to use a connection whose
		// request/response alignment can no longer be trusted.
		c.Close()
		return nil, 0, opCtx.Err()
	case <-c.closed:
		return nil, 0, net.ErrClosed
	}
}
