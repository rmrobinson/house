package homekitctrl

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
)

// hapMessage is one framed message read off the encrypted connection - either the HTTP/1.1
// response to a request this Controller sent, or an unsolicited EVENT/1.0 push notifying of a
// characteristic change on a subscribed accessory. HAP frames both identically: a start line,
// MIME headers, and a Content-Length-delimited JSON body - only the protocol token on the start
// line differs, which is why this package parses both itself rather than reusing net/http's
// http.ReadResponse (which requires an "HTTP/" token and a matching *http.Request, neither of
// which fits an EVENT/1.0 push).
type hapMessage struct {
	proto      string
	statusCode int
	body       []byte
	err        error
}

// runReader owns the connection's only Read calls for its entire lifetime. This is required once
// do() stops reading inline (see controller.go): every incoming frame must be demuxed here as
// either the response to whatever request is currently pending, or an EVENT/1.0 push to dispatch
// to onEvent - a per-call bufio.Reader (the old design) would silently discard any bytes
// buffered-but-unread across calls, which is exactly how a push arriving between requests could
// previously have corrupted the next call's parse.
//
// Deliberately blocks indefinitely on read with no deadline of its own: Close() (which closes the
// underlying net.Conn, unblocking the pending Read with an error) is the only intended way to stop
// this goroutine. The 15s TCP KeepAlive set in Connect is what surfaces a silently-dead peer while
// otherwise idle between polls/pushes.
//
// defer Close() here (not just on do()'s own timeout path) matters for the case where this loop
// exits on its own - a decrypt failure or malformed frame, say - without the underlying conn
// necessarily having been closed by anything else. Without this, the reader would be gone but the
// socket still technically writable, so a future do() call's req.Write would succeed while nothing
// is left to ever read its response, hanging until that call's own timeout instead of failing
// immediately.
func (c *Controller) runReader() {
	defer c.Close()

	tp := textproto.NewReader(bufio.NewReader(c.conn))
	for {
		msg, err := readHAPMessage(tp)
		if err != nil {
			c.handleReaderExit(err)
			return
		}
		if msg.proto == "EVENT/1.0" {
			c.dispatchEvent(msg.body)
			continue
		}
		if chp := c.pending.Load(); chp != nil {
			select {
			case *chp <- msg:
			default:
				// do() already gave up (timeout/Close) - nothing to deliver to.
			}
		}
	}
}

// readHAPMessage parses one frame: a start line ("HTTP/1.1 200 OK" or "EVENT/1.0 200 OK"),
// MIME headers, and an optional Content-Length-delimited body.
func readHAPMessage(tp *textproto.Reader) (*hapMessage, error) {
	line, err := tp.ReadLine()
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("malformed start line %q", line)
	}
	statusCode, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("malformed status in %q: %w", line, err)
	}

	header, err := tp.ReadMIMEHeader()
	if err != nil {
		return nil, err
	}

	var body []byte
	if cl := header.Get("Content-Length"); cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil {
			return nil, fmt.Errorf("malformed content-length %q: %w", cl, err)
		}
		if n > 0 {
			body = make([]byte, n)
			if _, err := io.ReadFull(tp.R, body); err != nil {
				return nil, err
			}
		}
	}

	return &hapMessage{proto: parts[0], statusCode: statusCode, body: body}, nil
}

// dispatchEvent parses an EVENT/1.0 push's body and hands it to onEvent, if set. A malformed push
// is dropped rather than tearing down the connection over it - there's nothing productive to do
// with it besides keep reading.
func (c *Controller) dispatchEvent(body []byte) {
	var parsed struct {
		Characteristics []CharacteristicValue `json:"characteristics"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return
	}

	c.evMu.Lock()
	onEvent := c.onEvent
	c.evMu.Unlock()

	if onEvent != nil {
		onEvent(parsed.Characteristics)
	}
}

// handleReaderExit runs once, when runReader's loop ends because a read failed (EOF, decrypt
// error, or the connection being Closed). It unblocks any do() call still waiting on a response
// and notifies onDisconnect, if set, so a caller (e.g. ecobeeConn) can react by reconnecting.
func (c *Controller) handleReaderExit(err error) {
	if chp := c.pending.Load(); chp != nil {
		select {
		case *chp <- &hapMessage{err: err}:
		default:
		}
	}

	c.evMu.Lock()
	onDisconnect := c.onDisconnect
	c.evMu.Unlock()

	if onDisconnect != nil {
		onDisconnect(err)
	}
}

// Subscribe arms HAP event notifications ("ev":true) for each given characteristic and registers
// callbacks for pushed values and connection loss.
//
// onEvent is invoked synchronously from the background reader goroutine each time one or more
// characteristics change - it must not block or call back into this Controller
// (ReadCharacteristics/WriteCharacteristics/Subscribe from inside onEvent would deadlock, since
// do() would wait forever on the very goroutine that's calling it). onDisconnect fires at most
// once, when the reader detects the connection is no longer usable (EOF, decrypt error, or
// Close) - Controller has no notion of reconnecting; callers own that entirely.
//
// A per-item HAP failure (e.g. some watched characteristic doesn't support ev on this unit's
// firmware - see docs/ecobee-hap-dump.txt for which ones do) is tolerated, not treated as an
// overall failure: both 204 and 207 responses are treated as success, matching
// WriteCharacteristics' own tolerance of per-item failure.
func (c *Controller) Subscribe(ctx context.Context, ids []CharID, onEvent func([]CharacteristicValue), onDisconnect func(error)) error {
	if len(ids) == 0 {
		return nil
	}

	c.evMu.Lock()
	c.onEvent, c.onDisconnect = onEvent, onDisconnect
	c.evMu.Unlock()

	type wireSub struct {
		AccessoryID      uint64 `json:"aid"`
		CharacteristicID uint64 `json:"iid"`
		Events           bool   `json:"ev"`
	}
	payload := struct {
		Characteristics []wireSub `json:"characteristics"`
	}{}
	for _, id := range ids {
		payload.Characteristics = append(payload.Characteristics, wireSub{id.AccessoryID, id.CharacteristicID, true})
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("subscribe: marshal request: %w", err)
	}

	_, status, err := c.do(ctx, http.MethodPut, "/characteristics", body)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	if status != http.StatusNoContent && status != http.StatusMultiStatus {
		return fmt.Errorf("subscribe: unexpected status %d", status)
	}
	return nil
}
