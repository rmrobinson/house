package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fastInitialQueries(t *testing.T) {
	t.Helper()
	orig := initialQueryInterval
	initialQueryInterval = time.Millisecond
	t.Cleanup(func() { initialQueryInterval = orig })
}

func countPublished(fc *fakeMQTTClient, topic string) int {
	n := 0
	for _, p := range fc.publishedTopics() {
		if p == topic {
			n++
		}
	}
	return n
}

// zigbee2mqtt doesn't retain device state, so a bridge restart leaves an idle bulb at its zero
// value until it next changes. Asking each newly discovered mains device once fixes that.
func TestNewMainsDevicesAreQueriedOnceForTheirState(t *testing.T) {
	fastInitialQueries(t)
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})

	nc.buildDevice(lightDevice(true, false, false)) // lamp1
	nc.buildDevice(switchDevice(false))             // plug1

	require.Eventually(t, func() bool {
		return countPublished(fc, "zigbee2mqtt/lamp1/get") == 1 && countPublished(fc, "zigbee2mqtt/plug1/get") == 1
	}, 2*time.Second, 5*time.Millisecond)
}

// Battery sensors sleep; a /get to one costs battery and is dropped or deferred.
func TestSensorsAreNotQueried(t *testing.T) {
	fastInitialQueries(t)
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})

	nc.buildDevice(sensorDevice("temperature", "battery")) // sensor1
	nc.buildDevice(lightDevice(true, false, false))        // lamp1: a barrier, queried after sensor1

	require.Eventually(t, func() bool { return countPublished(fc, "zigbee2mqtt/lamp1/get") == 1 }, 2*time.Second, 5*time.Millisecond)
	assert.Zero(t, countPublished(fc, "zigbee2mqtt/sensor1/get"))
}

// bridge/devices is republished whenever ANY device pairs, is removed or is renamed; devices the
// bridge already knows must not be re-queried each time.
func TestAlreadyKnownDeviceIsNotQueriedAgain(t *testing.T) {
	fastInitialQueries(t)
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})

	nc.buildDevice(lightDevice(true, false, false))
	nc.buildDevice(lightDevice(true, false, false)) // same ieee: the rediscovery path
	nc.buildDevice(switchDevice(false))             // barrier

	require.Eventually(t, func() bool { return countPublished(fc, "zigbee2mqtt/plug1/get") == 1 }, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, 1, countPublished(fc, "zigbee2mqtt/lamp1/get"))
}

// The point of the query: the reply is applied, so the device no longer reads as an off, 0% bulb.
func TestQueryReplyPopulatesIdleDeviceState(t *testing.T) {
	fastInitialQueries(t)
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	fc.respond("zigbee2mqtt/lamp1/get", "zigbee2mqtt/lamp1", []byte(`{"state":"ON","brightness":200}`))

	nc.buildDevice(lightDevice(true, false, false))

	nc.mu.Lock()
	bd := nc.devices["zigbee-bulb"]
	nc.mu.Unlock()
	require.Eventually(t, func() bool {
		bd.mu.Lock()
		defer bd.mu.Unlock()
		l := bd.device.GetLight()
		return l.GetOnOff().GetState().GetIsOn() && l.GetBrightness().GetState().GetLevel() == 79 // 200/254
	}, 2*time.Second, 5*time.Millisecond)
}
