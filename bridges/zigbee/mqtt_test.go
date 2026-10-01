package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/rmrobinson/house/service/bridge"
)

func newTestMQTTConn(t *testing.T) (*mqttConn, *fakeMQTTClient) {
	t.Helper()
	logger := zaptest.NewLogger(t)
	fc := newFakeMQTTClient()
	mc := &mqttConn{logger: logger, cfg: mqttConfig{BaseTopic: "zigbee2mqtt"}, client: fc, waiters: make(map[string][]chan []byte)}
	mc.handleConnect(fc)
	return mc, fc
}

func TestStateMatches(t *testing.T) {
	raw := []byte(`{"state":"ON","brightness":150,"linkquality":60}`)
	assert.True(t, stateMatches(raw, map[string]any{"state": "ON"}))
	assert.True(t, stateMatches(raw, map[string]any{"state": "ON", "brightness": float64(150)}))
	assert.False(t, stateMatches(raw, map[string]any{"state": "OFF"}))
	assert.False(t, stateMatches(raw, map[string]any{"missing_key": "x"}))

	rawColour := []byte(`{"state":"ON","color":{"hue":200,"saturation":50}}`)
	assert.True(t, stateMatches(rawColour, map[string]any{"color": map[string]any{"hue": 200, "saturation": 50}}))
	assert.False(t, stateMatches(rawColour, map[string]any{"color": map[string]any{"hue": 201, "saturation": 50}}))
}

func TestWriteState_Success(t *testing.T) {
	mc, fc := newTestMQTTConn(t)

	fc.respond("zigbee2mqtt/lamp1/set", "zigbee2mqtt/lamp1", []byte(`{"state":"ON","brightness":150}`))

	err := mc.WriteState(context.Background(), "lamp1", map[string]any{"state": "ON"})
	require.NoError(t, err)
	assert.Contains(t, fc.publishedTopics(), "zigbee2mqtt/lamp1/set")
}

func TestWriteState_NoMatchingEchoTimesOut(t *testing.T) {
	origTimeout := writeTimeout
	writeTimeout = 20 * time.Millisecond
	defer func() { writeTimeout = origTimeout }()

	mc, fc := newTestMQTTConn(t)

	// The only message the device ever reports doesn't reflect what was asked for - whether
	// because it rejected the command, or because this is just an unrelated report and the real
	// echo never comes. WriteState can't tell those apart from this topic alone (see its doc
	// comment), so both surface the same way an unreachable device already does: a timeout, not
	// an immediate "did not accept" error.
	fc.respond("zigbee2mqtt/lamp1/set", "zigbee2mqtt/lamp1", []byte(`{"state":"OFF"}`))

	err := mc.WriteState(context.Background(), "lamp1", map[string]any{"state": "ON"})
	assert.ErrorIs(t, err, bridge.ErrCommandTimeout)
}

// TestWriteState_IgnoresStaleReportBeforeRealEcho guards the actual bug found live against a
// Jasco 43080 in-wall porch dimmer on 2026-10-01: an unrelated state report raced the real /set
// echo and was mistaken for a rejection, failing the write even though the device would have
// confirmed the real echo moments later.
func TestWriteState_IgnoresStaleReportBeforeRealEcho(t *testing.T) {
	origTimeout := writeTimeout
	writeTimeout = time.Second
	defer func() { writeTimeout = origTimeout }()

	mc, fc := newTestMQTTConn(t)

	errCh := make(chan error, 1)
	go func() {
		errCh <- mc.WriteState(context.Background(), "lamp1", map[string]any{"state": "OFF"})
	}()

	// Give WriteState time to register its waiter before either message is delivered.
	time.Sleep(20 * time.Millisecond)

	// An unrelated report - e.g. a periodic attribute report from the device - races the real
	// echo and must not be mistaken for a rejection.
	fc.deliver("zigbee2mqtt/lamp1", []byte(`{"state":"ON","linkquality":80}`))

	time.Sleep(20 * time.Millisecond)

	// The real echo, confirming the write.
	fc.deliver("zigbee2mqtt/lamp1", []byte(`{"state":"OFF"}`))

	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("WriteState did not return")
	}
}

func TestWriteState_Timeout(t *testing.T) {
	origTimeout := writeTimeout
	writeTimeout = 20 * time.Millisecond
	defer func() { writeTimeout = origTimeout }()

	mc, _ := newTestMQTTConn(t)

	err := mc.WriteState(context.Background(), "lamp1", map[string]any{"state": "ON"})
	assert.ErrorIs(t, err, bridge.ErrCommandTimeout)
}

func TestDecodeAvailability(t *testing.T) {
	online, ok := decodeAvailability([]byte(`{"state":"online"}`))
	require.True(t, ok)
	assert.True(t, online)

	offline, ok := decodeAvailability([]byte(`{"state":"offline"}`))
	require.True(t, ok)
	assert.False(t, offline)

	legacyOnline, ok := decodeAvailability([]byte(`online`))
	require.True(t, ok)
	assert.True(t, legacyOnline)

	legacyOffline, ok := decodeAvailability([]byte(`"offline"`))
	require.True(t, ok)
	assert.False(t, legacyOffline)

	_, ok = decodeAvailability([]byte(``))
	assert.False(t, ok)
}
