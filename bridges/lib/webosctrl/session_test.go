package webosctrl

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// newTestSession wires a Session to fd using fd.dial instead of Session's
// normal dialTV, mirroring conn_test.go's approach.
func newTestSession(t *testing.T, fd *fakeDevice, hasTuner bool, getClientKey func() string, onPaired func(string)) (*Session, chan Status) {
	changes := make(chan Status, 16)
	s := &Session{
		logger:       zaptest.NewLogger(t),
		getClientKey: getClientKey,
		onPaired:     onPaired,
		onChange:     func(st Status) { changes <- st },
		hasTuner:     hasTuner,
	}
	s.conn = newConn(zaptest.NewLogger(t), fd.addr(), fd.dial, s.handleStateChange)
	return s, changes
}

// awaitStatus reads from changes until pred is satisfied or the test times
// out.
func awaitStatus(t *testing.T, changes chan Status, pred func(Status) bool) Status {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case st := <-changes:
			if pred(st) {
				return st
			}
		case <-deadline:
			t.Fatal("timed out waiting for expected status")
		}
	}
}

func TestSession_Register_FirstPairing(t *testing.T) {
	fd := newFakeDevice(t)

	var pairedKey string
	fd.onEnvelope = func(e requestEnvelope, reply func(any)) {
		if e.Type != "register" {
			return
		}
		// First pairing: no client-key yet in the payload.
		fd.send(envelope{Type: "registered", ID: e.ID, Payload: mustMarshal(t, registeredPayload{ClientKey: "new-key-123"})})
	}

	s, changes := newTestSession(t, fd, false, func() string { return "" }, func(key string) { pairedKey = key })
	go s.Run(t.Context())

	awaitStatus(t, changes, func(st Status) bool { return st.Paired })

	assert.Equal(t, "new-key-123", pairedKey)
	assert.True(t, s.Status().Paired)
}

func TestSession_Register_ReconnectSkipsPromptWithExistingKey(t *testing.T) {
	fd := newFakeDevice(t)

	var sawClientKey string
	fd.onEnvelope = func(e requestEnvelope, reply func(any)) {
		if e.Type != "register" {
			return
		}
		payload, ok := e.Payload.(map[string]any)
		require.True(t, ok)
		if ck, ok := payload["client-key"].(string); ok {
			sawClientKey = ck
		}
		fd.send(envelope{Type: "registered", ID: e.ID, Payload: mustMarshal(t, registeredPayload{ClientKey: "existing-key"})})
	}

	onPairedCalls := 0
	s, changes := newTestSession(t, fd, false, func() string { return "existing-key" }, func(string) { onPairedCalls++ })
	go s.Run(t.Context())

	awaitStatus(t, changes, func(st Status) bool { return st.Paired })

	assert.Equal(t, "existing-key", sawClientKey)
	// The TV echoed back the same key it was given - onPaired shouldn't fire
	// for a key that hasn't actually changed.
	assert.Equal(t, 0, onPairedCalls)
}

func TestSession_HandleStateChange_DisconnectClearsReachableAndPaired(t *testing.T) {
	fd := newFakeDevice(t)
	fd.onEnvelope = func(e requestEnvelope, reply func(any)) {
		if e.Type == "register" {
			fd.send(envelope{Type: "registered", ID: e.ID, Payload: mustMarshal(t, registeredPayload{ClientKey: "k"})})
		}
	}

	s, changes := newTestSession(t, fd, false, func() string { return "" }, func(string) {})
	go s.Run(t.Context())

	awaitStatus(t, changes, func(st Status) bool { return st.Paired })

	fd.closeConn()

	st := awaitStatus(t, changes, func(st Status) bool { return !st.Reachable && !st.Paired })
	// EverPaired must survive the disconnect - it's what lets normalize()
	// keep reporting last-known trait state for a device that's merely gone
	// unreachable, rather than wiping it back to "never seen" (see the plan
	// doc's state-mapping table and normalize.go's doc comment).
	assert.True(t, st.EverPaired)
}

func TestSession_Request_ReturnValueFalseIsRejected(t *testing.T) {
	fd := newFakeDevice(t)
	fd.onEnvelope = func(e requestEnvelope, reply func(any)) {
		switch e.Type {
		case "register":
			fd.send(envelope{Type: "registered", ID: e.ID, Payload: mustMarshal(t, registeredPayload{ClientKey: "k"})})
		case "request":
			reply(returnValuePayload{ReturnValue: false, ErrorText: "not allowed"})
		}
	}

	s, changes := newTestSession(t, fd, false, func() string { return "" }, func(string) {})
	go s.Run(t.Context())
	awaitStatus(t, changes, func(st Status) bool { return st.Paired })

	err := s.SetVolumeAbsolute(context.Background(), 50)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRequestRejected)
}

func TestSession_Request_BeforePairingIsRejected(t *testing.T) {
	fd := newFakeDevice(t)
	// No onEnvelope handler at all - register never completes, so Paired
	// stays false.

	s, _ := newTestSession(t, fd, false, func() string { return "" }, func(string) {})

	err := s.SetVolumeAbsolute(context.Background(), 50)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPairingRequired)
}
