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

func TestWriteState_RejectedEcho(t *testing.T) {
	mc, fc := newTestMQTTConn(t)

	// The device reports a state that doesn't reflect what was asked for - e.g. it rejected the
	// command, or reported something unrelated first.
	fc.respond("zigbee2mqtt/lamp1/set", "zigbee2mqtt/lamp1", []byte(`{"state":"OFF"}`))

	err := mc.WriteState(context.Background(), "lamp1", map[string]any{"state": "ON"})
	assert.Error(t, err)
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
