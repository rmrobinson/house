package homekitctrl

import (
	"bufio"
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeAccessory is a minimal in-process stand-in for a paired HAP accessory: it plays the
// accessory side of pair-verify (M1-M4) against a real Controller, then serves
// GET/PUT /characteristics over the resulting encrypted session. It exists so Controller's
// pair-verify and characteristic read/write logic can be tested end-to-end without real
// hardware - every step of the protocol it implements was validated against a real ecobee first
// (see bridges/ecobee/docs/ecobee-hap-dump.txt), this just replays the accessory's half of it.
type fakeAccessory struct {
	t         *testing.T
	ltsk      ed25519.PrivateKey
	ltpk      ed25519.PublicKey
	pairingID string
	listener  net.Listener

	controllerPairingID string
	controllerPublicKey ed25519.PublicKey

	mu          sync.Mutex
	chars       map[CharID]json.RawMessage
	subscribed  map[CharID]bool // characteristics a PUT .../characteristics with "ev":true armed
	failWriteOf *CharID         // if set, WriteCharacteristics for this id reports a HAP failure status
	hangReads   bool            // if set, GET /characteristics accepts the request but never responds
	replyDelay  time.Duration   // if set, handleRead sleeps this long after reading the request but before replying
	activeConn  net.Conn        // the current encrypted connection, if any - set by serveCharacteristics

	// writeMu serializes writes to activeConn: net.Conn permits one concurrent reader and one
	// concurrent writer, but not two concurrent writers, and pushEvent (called from a test
	// goroutine) can otherwise race with this connection's own handleRead/handleWrite goroutine
	// replying at the same time.
	writeMu sync.Mutex
}

func newFakeAccessory(t *testing.T, controllerPairingID string, controllerPublicKey ed25519.PublicKey) *fakeAccessory {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() })

	fa := &fakeAccessory{
		t:                   t,
		ltsk:                priv,
		ltpk:                pub,
		pairingID:           "AA:BB:CC:DD:EE:FF",
		listener:            l,
		controllerPairingID: controllerPairingID,
		controllerPublicKey: controllerPublicKey,
		chars:               map[CharID]json.RawMessage{},
		subscribed:          map[CharID]bool{},
	}
	go fa.serve()
	return fa
}

func (fa *fakeAccessory) addr() string {
	return fa.listener.Addr().String()
}

func (fa *fakeAccessory) accessoryRecord() *AccessoryRecord {
	return &AccessoryRecord{Name: "fake", PairingID: fa.pairingID, PublicKey: fa.ltpk}
}

func (fa *fakeAccessory) setCharacteristic(id CharID, value any) {
	b, err := json.Marshal(value)
	require.NoError(fa.t, err)

	fa.mu.Lock()
	defer fa.mu.Unlock()
	fa.chars[id] = b
}

func (fa *fakeAccessory) serve() {
	for {
		conn, err := fa.listener.Accept()
		if err != nil {
			return
		}
		go fa.handleConn(conn)
	}
}

func (fa *fakeAccessory) handleConn(conn net.Conn) {
	defer conn.Close()

	br := bufio.NewReader(conn)

	// M1
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	m1Body, err := io.ReadAll(req.Body)
	if err != nil {
		return
	}
	m1 := decodeTLV8(m1Body)
	clientEphemeralPubBytes := m1[tlvTagPublicKey]

	clientEphemeralPub, err := ecdh.X25519().NewPublicKey(clientEphemeralPubBytes)
	if err != nil {
		return
	}
	accEphemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return
	}
	accEphemeralPub := accEphemeral.PublicKey().Bytes()

	sharedSecret, err := accEphemeral.ECDH(clientEphemeralPub)
	if err != nil {
		return
	}
	sessionKey, err := hkdf.Key(sha512.New, sharedSecret, []byte("Pair-Verify-Encrypt-Salt"), "Pair-Verify-Encrypt-Info", 32)
	if err != nil {
		return
	}

	signedMaterial := concat(accEphemeralPub, []byte(fa.pairingID), clientEphemeralPubBytes)
	sig := ed25519.Sign(fa.ltsk, signedMaterial)

	inner := encodeTLV8(
		tlv8Item{tag: tlvTagIdentifier, value: []byte(fa.pairingID)},
		tlv8Item{tag: tlvTagSignature, value: sig},
	)
	sealed, err := chachaSeal(sessionKey, "PV-Msg02", inner)
	if err != nil {
		return
	}
	m2 := encodeTLV8(
		tlv8Item{tag: tlvTagState, value: []byte{2}},
		tlv8Item{tag: tlvTagPublicKey, value: accEphemeralPub},
		tlv8Item{tag: tlvTagEncryptedData, value: sealed},
	)
	if err := writeTLV8Response(conn, m2); err != nil {
		return
	}

	// M3
	req2, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	m3Body, err := io.ReadAll(req2.Body)
	if err != nil {
		return
	}
	m3 := decodeTLV8(m3Body)

	plaintext, err := chachaOpen(sessionKey, "PV-Msg03", m3[tlvTagEncryptedData])
	if err != nil {
		return
	}
	inner2 := decodeTLV8(plaintext)
	clientPairingID := inner2[tlvTagIdentifier]
	clientSig := inner2[tlvTagSignature]

	clientSignedMaterial := concat(clientEphemeralPubBytes, clientPairingID, accEphemeralPub)
	if !ed25519.Verify(fa.controllerPublicKey, clientSignedMaterial, clientSig) {
		m4 := encodeTLV8(tlv8Item{tag: tlvTagState, value: []byte{4}}, tlv8Item{tag: tlvTagError, value: []byte{2}})
		writeTLV8Response(conn, m4)
		return
	}

	m4 := encodeTLV8(tlv8Item{tag: tlvTagState, value: []byte{4}})
	if err := writeTLV8Response(conn, m4); err != nil {
		return
	}

	controllerToAccessoryKey, err := hkdf.Key(sha512.New, sharedSecret, []byte("Control-Salt"), "Control-Write-Encryption-Key", 32)
	if err != nil {
		return
	}
	accessoryToControllerKey, err := hkdf.Key(sha512.New, sharedSecret, []byte("Control-Salt"), "Control-Read-Encryption-Key", 32)
	if err != nil {
		return
	}

	// Directions are swapped relative to Controller's own newEncryptedConn call: the accessory
	// encrypts what it writes with accessoryToControllerKey and decrypts what it reads with
	// controllerToAccessoryKey.
	encConn, err := newEncryptedConn(conn, accessoryToControllerKey, controllerToAccessoryKey)
	if err != nil {
		return
	}
	fa.serveCharacteristics(encConn)
}

func (fa *fakeAccessory) serveCharacteristics(conn net.Conn) {
	fa.mu.Lock()
	fa.activeConn = conn
	fa.mu.Unlock()

	br := bufio.NewReader(conn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}

		switch {
		case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/characteristics"):
			fa.handleRead(conn, req)
		case req.Method == http.MethodPut && req.URL.Path == "/characteristics":
			fa.handleWrite(conn, req)
		default:
			fa.writeResponse(conn, http.StatusNotFound, []byte(`{}`))
		}
	}
}

func (fa *fakeAccessory) handleRead(conn net.Conn, req *http.Request) {
	if fa.hangReads {
		// Simulates an accessory that accepts the TCP connection and the request but never
		// replies - the failure mode this package's timeout/Close handling exists for. Blocks
		// this connection's goroutine for the rest of the test process's life; acceptable since
		// tests using this exercise the client giving up (via deadline or Close), not this
		// goroutine ever finishing on its own.
		select {}
	}
	if fa.replyDelay > 0 {
		// Sleeps after the request has already been fully read (see serveCharacteristics'
		// http.ReadRequest above) but before replying - gives a test room to push an event that
		// arrives on the wire while this response is still pending, to exercise the demux under
		// interleaving.
		time.Sleep(fa.replyDelay)
	}

	idsParam := req.URL.Query().Get("id")

	type item struct {
		AccessoryID      uint64          `json:"aid"`
		CharacteristicID uint64          `json:"iid"`
		Value            json.RawMessage `json:"value,omitempty"`
	}
	var items []item

	fa.mu.Lock()
	for pair := range strings.SplitSeq(idsParam, ",") {
		parts := strings.SplitN(pair, ".", 2)
		if len(parts) != 2 {
			continue
		}
		aid, err1 := strconv.ParseUint(parts[0], 10, 64)
		iid, err2 := strconv.ParseUint(parts[1], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		items = append(items, item{AccessoryID: aid, CharacteristicID: iid, Value: fa.chars[CharID{aid, iid}]})
	}
	fa.mu.Unlock()

	body, _ := json.Marshal(struct {
		Characteristics []item `json:"characteristics"`
	}{items})
	fa.writeResponse(conn, http.StatusOK, body)
}

func (fa *fakeAccessory) handleWrite(conn net.Conn, req *http.Request) {
	var payload struct {
		Characteristics []struct {
			AccessoryID      uint64          `json:"aid"`
			CharacteristicID uint64          `json:"iid"`
			Value            json.RawMessage `json:"value"`
			Ev               *bool           `json:"ev"` // present on a Subscribe request instead of Value
		} `json:"characteristics"`
	}
	body, _ := io.ReadAll(req.Body)
	if err := json.Unmarshal(body, &payload); err != nil {
		fa.writeResponse(conn, http.StatusBadRequest, []byte(`{}`))
		return
	}

	fa.mu.Lock()
	failed := false
	type status struct {
		AccessoryID      uint64 `json:"aid"`
		CharacteristicID uint64 `json:"iid"`
		Status           int    `json:"status"`
	}
	var statuses []status
	for _, c := range payload.Characteristics {
		id := CharID{c.AccessoryID, c.CharacteristicID}
		if c.Ev != nil {
			fa.subscribed[id] = *c.Ev
			statuses = append(statuses, status{c.AccessoryID, c.CharacteristicID, 0})
			continue
		}
		if fa.failWriteOf != nil && *fa.failWriteOf == id {
			failed = true
			statuses = append(statuses, status{c.AccessoryID, c.CharacteristicID, -70402})
			continue
		}
		fa.chars[id] = c.Value
		statuses = append(statuses, status{c.AccessoryID, c.CharacteristicID, 0})
	}
	fa.mu.Unlock()

	if !failed {
		fa.writeResponse(conn, http.StatusNoContent, nil)
		return
	}
	body, _ = json.Marshal(struct {
		Characteristics []status `json:"characteristics"`
	}{statuses})
	fa.writeResponse(conn, http.StatusMultiStatus, body)
}

// writeResponse serializes writes to conn against pushEvent - see writeMu's doc comment.
func (fa *fakeAccessory) writeResponse(conn net.Conn, status int, body []byte) {
	fa.writeMu.Lock()
	defer fa.writeMu.Unlock()
	writeJSONResponse(conn, status, body)
}

func (fa *fakeAccessory) isSubscribed(id CharID) bool {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	return fa.subscribed[id]
}

// pushEvent writes a raw EVENT/1.0-framed push for id to the current connection, simulating an
// accessory-initiated notification arriving asynchronously - independent of whether id was ever
// actually subscribed to, since exercising Controller's demux doesn't require a real HAP server's
// gating logic. Requires a connection to already be established (i.e. call after Connect returns).
func (fa *fakeAccessory) pushEvent(id CharID, value any) {
	v, err := json.Marshal(value)
	require.NoError(fa.t, err)

	body, err := json.Marshal(struct {
		Characteristics []struct {
			AccessoryID      uint64          `json:"aid"`
			CharacteristicID uint64          `json:"iid"`
			Value            json.RawMessage `json:"value"`
		} `json:"characteristics"`
	}{Characteristics: []struct {
		AccessoryID      uint64          `json:"aid"`
		CharacteristicID uint64          `json:"iid"`
		Value            json.RawMessage `json:"value"`
	}{{id.AccessoryID, id.CharacteristicID, v}}})
	require.NoError(fa.t, err)

	fa.mu.Lock()
	conn := fa.activeConn
	fa.mu.Unlock()
	require.NotNil(fa.t, conn, "pushEvent called before a connection was established")

	frame := fmt.Sprintf("EVENT/1.0 200 OK\r\nContent-Type: application/hap+json\r\nContent-Length: %d\r\n\r\n", len(body))

	fa.writeMu.Lock()
	defer fa.writeMu.Unlock()
	_, err = conn.Write(append([]byte(frame), body...))
	require.NoError(fa.t, err)
}

func writeTLV8Response(conn net.Conn, body []byte) error {
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/pairing+tlv8"}},
		ContentLength: int64(len(body)),
		Body:          io.NopCloser(bytes.NewReader(body)),
	}
	return resp.Write(conn)
}

func writeJSONResponse(conn net.Conn, status int, body []byte) error {
	resp := &http.Response{
		StatusCode:    status,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/hap+json"}},
		ContentLength: int64(len(body)),
		Body:          io.NopCloser(bytes.NewReader(body)),
	}
	if len(body) == 0 {
		resp.ContentLength = 0
	}
	return resp.Write(conn)
}
