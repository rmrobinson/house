package homekitctrl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
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
type Controller struct {
	mu   sync.Mutex
	conn *encryptedConn
	host string
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

	encConn, err := newEncryptedConn(conn, result.controllerToAccessoryKey, result.accessoryToControllerKey)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("init encrypted session with %s: %w", host, err)
	}

	return &Controller{conn: encConn, host: host}, nil
}

// Close releases the underlying connection. The Controller is unusable afterwards - callers
// needing to keep talking to the accessory should Connect again.
//
// Deliberately does not take mu: do() can be blocked inside a Read/Write for up to
// defaultOperationTimeout (or ctx's own deadline) at the time Close is called, and closing the
// underlying net.Conn is exactly what's supposed to unblock that pending call (a Read/Write on a
// closed net.Conn returns promptly with an error, per net.Conn's documented behavior) - requiring
// mu here would make Close wait for the very call it's meant to interrupt, deadlocking forever.
func (c *Controller) Close() error {
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

// do writes a single HAP JSON request directly to the encrypted connection and returns the
// response body and status code. Requests are serialized (mu) since pair-verify'd HAP sessions
// don't support pipelining - only one request may be in flight at a time. The whole exchange is
// bounded by ctx (or defaultOperationTimeout, whichever applies) via withDeadline, since this
// accessory is documented to hang rather than error promptly - see Close's doc comment for why
// that timeout, not this mutex, is what makes recovery from a hang possible at all.
func (c *Controller) do(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var respBody []byte
	var statusCode int

	err := withDeadline(ctx, c.conn, func() error {
		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}

		req, err := http.NewRequest(method, "http://"+c.host+path, bodyReader)
		if err != nil {
			return err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/hap+json")
			req.ContentLength = int64(len(body))
		}

		if err := req.Write(c.conn); err != nil {
			return fmt.Errorf("write request: %w", err)
		}

		resp, err := http.ReadResponse(bufio.NewReader(c.conn), req)
		if err != nil {
			return fmt.Errorf("read response: %w", err)
		}
		defer resp.Body.Close()

		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("read response body: %w", err)
		}
		respBody = b
		statusCode = resp.StatusCode
		return nil
	})

	return respBody, statusCode, err
}
