package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/service/bridge"
)

// Tests for behaviour under a slow or unresponsive device (one at the edge of range, whose
// commands are acknowledged late or not at all). See mqttConn.dispatch and
// builtDevice.acquireCommand.

func shrinkWriteTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := writeTimeout
	writeTimeout = d
	t.Cleanup(func() { writeTimeout = orig })
}

// lightWithButtons is a dimmable light that also reports an "action" (an Inovelli-style paddle).
func lightWithButtons() bridgeDevice {
	bd := lightDevice(true, false, false)
	bd.Definition.Exposes = append(bd.Definition.Exposes, actionExpose("up_single", "up_double"))
	return bd
}

func offCommand(id string) *command.Command {
	return &command.Command{DeviceId: id, Details: &command.Command_OnOff{OnOff: &command.OnOff{On: false}}}
}

// A command stuck waiting on one device must not stop other devices' messages being handled -
// previously the MQTT receive goroutine blocked on the stuck device's lock and every message on
// the network queued behind it.
func TestDispatch_StuckDeviceDoesNotBlockOtherDevices(t *testing.T) {
	nc, _ := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false)) // zigbee-bulb, topic zigbee2mqtt/lamp1
	nc.buildDevice(switchDevice(false))             // zigbee-abc, topic zigbee2mqtt/plug1

	nc.mu.Lock()
	stuck := nc.devices["zigbee-bulb"]
	other := nc.devices["zigbee-abc"]
	nc.mu.Unlock()

	// Simulate a command holding the bulb's lock while it waits for an echo that is slow to come.
	stuck.mu.Lock()
	released := false
	defer func() {
		if !released {
			stuck.mu.Unlock()
		}
	}()

	returned := make(chan struct{})
	go func() {
		nc.mqtt.dispatch("zigbee2mqtt/lamp1", []byte(`{"state":"ON"}`))
		nc.mqtt.dispatch("zigbee2mqtt/plug1", []byte(`{"state":"ON"}`))
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch blocked on a device lock held by an in-flight command")
	}

	require.Eventually(t, func() bool {
		other.mu.Lock()
		defer other.mu.Unlock()
		return other.device.GetGeneric().GetOnOff().GetState().GetIsOn()
	}, 2*time.Second, 5*time.Millisecond, "the other device's message must be applied while the bulb is stuck")

	// The stuck device's own message is applied, in order, once its lock frees.
	stuck.mu.Unlock()
	released = true
	nc.mqtt.quiesce()
	stuck.mu.Lock()
	defer stuck.mu.Unlock()
	assert.True(t, stuck.device.GetLight().GetOnOff().GetState().GetIsOn())
}

// A button event arriving while a command is in flight on the same device (topic) used to be
// consumed by the command's waiter and never applied - silently losing a press.
func TestActionDuringInFlightCommandIsNotLost(t *testing.T) {
	shrinkWriteTimeout(t, 300*time.Millisecond)
	nc, _ := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightWithButtons())

	cmdDone := make(chan error, 1)
	go func() {
		_, err := nc.applyCommand(context.Background(), offCommand("zigbee-bulb"))
		cmdDone <- err
	}()
	time.Sleep(50 * time.Millisecond) // the command is now waiting for an echo that never comes

	nc.mqtt.dispatch("zigbee2mqtt/lamp1", []byte(`{"action":"up_double","state":"ON"}`))

	assert.ErrorIs(t, <-cmdDone, bridge.ErrCommandTimeout)
	nc.mqtt.quiesce()

	nc.mu.Lock()
	bd := nc.devices["zigbee-bulb"]
	nc.mu.Unlock()
	bd.mu.Lock()
	defer bd.mu.Unlock()
	b := bd.device.GetLight().GetButtons()
	require.NotNil(t, b)
	assert.Equal(t, int64(1), b.State.EventCount)
	assert.Equal(t, "up_double", b.State.LastAction)
}

// A command whose caller has already given up while queued behind a slow one must never be
// published to the device.
func TestQueuedCommandWithExpiredContextIsNotPublished(t *testing.T) {
	shrinkWriteTimeout(t, 400*time.Millisecond)
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false))

	first := make(chan error, 1)
	go func() {
		_, err := nc.applyCommand(context.Background(), offCommand("zigbee-bulb"))
		first <- err
	}()
	time.Sleep(50 * time.Millisecond) // first is in flight, holding the device's command slot

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err := nc.applyCommand(ctx, offCommand("zigbee-bulb"))
	assert.ErrorIs(t, err, context.DeadlineExceeded, "the queued caller gives up at its own deadline")

	assert.ErrorIs(t, <-first, bridge.ErrCommandTimeout)

	sets := 0
	for _, topic := range fc.publishedTopics() {
		if topic == "zigbee2mqtt/lamp1/set" {
			sets++
		}
	}
	assert.Equal(t, 1, sets, "only the in-flight command may reach the device; the abandoned one must not")
}

// A late echo (after the caller already saw a timeout) still converges the cached state.
func TestLateEchoAfterTimeoutStillAppliesState(t *testing.T) {
	shrinkWriteTimeout(t, 100*time.Millisecond)
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false))
	fc.deliver("zigbee2mqtt/lamp1", []byte(`{"state":"ON","brightness":200}`))

	_, err := nc.applyCommand(context.Background(), offCommand("zigbee-bulb"))
	assert.ErrorIs(t, err, bridge.ErrCommandTimeout)

	// zigbee2mqtt gets there eventually.
	fc.deliver("zigbee2mqtt/lamp1", []byte(`{"state":"OFF","brightness":200}`))

	nc.mu.Lock()
	bd := nc.devices["zigbee-bulb"]
	nc.mu.Unlock()
	bd.mu.Lock()
	defer bd.mu.Unlock()
	assert.False(t, bd.device.GetLight().GetOnOff().GetState().GetIsOn())
}

func TestWriteState_RefusesToPublishWithExpiredContext(t *testing.T) {
	mc, fc := newTestMQTTConn(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := mc.WriteState(ctx, "lamp1", map[string]any{"state": "ON"})
	assert.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, fc.publishedTopics(), "zigbee2mqtt/lamp1/set")
}
