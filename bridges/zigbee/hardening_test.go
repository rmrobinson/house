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
// bridge.CommandLane (used by builtDevice).

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

// waitForSet blocks until n /set messages have been published to lamp1: i.e. until that many
// commands are genuinely in flight, which a fixed sleep only approximates.
func waitForSet(t *testing.T, fc *fakeMQTTClient, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return countPublished(fc, "zigbee2mqtt/lamp1/set") >= n },
		2*time.Second, time.Millisecond, "command never reached the device")
}

func levelCommand(id string, pct int32) *command.Command {
	return &command.Command{DeviceId: id, Details: &command.Command_BrightnessAbsolute{BrightnessAbsolute: &command.BrightnessAbsolute{BrightnessPercent: pct}}}
}

// publishedPayloads returns the payloads published to topic, in order.
func publishedPayloads(fc *fakeMQTTClient, topic string) []string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	var out []string
	for _, p := range fc.published {
		if p.topic == topic {
			out = append(out, string(p.payload))
		}
	}
	return out
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
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightWithButtons())

	cmdDone := make(chan error, 1)
	go func() {
		_, err := nc.applyCommand(context.Background(), offCommand("zigbee-bulb"))
		cmdDone <- err
	}()
	waitForSet(t, fc, 1) // the command is now waiting for an echo that never comes

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
// published to the device. (The second command is a brightness change, which - unlike another
// Off - does not supersede the Off in flight, so it genuinely has to wait its turn.)
func TestQueuedCommandWithExpiredContextIsNotPublished(t *testing.T) {
	shrinkWriteTimeout(t, 400*time.Millisecond)
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false))

	first := make(chan error, 1)
	go func() {
		_, err := nc.applyCommand(context.Background(), offCommand("zigbee-bulb"))
		first <- err
	}()
	waitForSet(t, fc, 1) // first is in flight, holding the device's command slot

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err := nc.applyCommand(ctx, levelCommand("zigbee-bulb", 50))
	assert.ErrorIs(t, err, context.DeadlineExceeded, "the queued caller gives up at its own deadline")

	assert.ErrorIs(t, <-first, bridge.ErrCommandTimeout)
	assert.Equal(t, 1, countPublished(fc, "zigbee2mqtt/lamp1/set"), "only the in-flight command may reach the device; the abandoned one must not")
}

// A newer brightness command must not wait out the write timeout of one that never got confirmed:
// the stuck one is abandoned the moment the newer one arrives.
func TestNewerBrightnessCommandAbandonsStuckOneImmediately(t *testing.T) {
	shrinkWriteTimeout(t, 5*time.Second) // far longer than the test: waiting it out would fail
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false))

	stuck := make(chan error, 1)
	go func() {
		_, err := nc.applyCommand(context.Background(), levelCommand("zigbee-bulb", 30))
		stuck <- err
	}()
	waitForSet(t, fc, 1)

	newer := make(chan error, 1)
	go func() {
		_, err := nc.applyCommand(context.Background(), levelCommand("zigbee-bulb", 60))
		newer <- err
	}()

	select {
	case err := <-stuck:
		assert.ErrorIs(t, err, bridge.ErrCommandSuperseded)
	case <-time.After(time.Second):
		t.Fatal("the stuck command kept waiting for its echo after a newer command superseded it")
	}
	waitForSet(t, fc, 2)

	// The device finally confirms the newer one.
	fc.deliver("zigbee2mqtt/lamp1", []byte(`{"state":"ON","brightness":152}`)) // 60% of 254
	select {
	case err := <-newer:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("the newer command never completed")
	}
}

// The reported failure: brightness changes sent to a slow bulb must not be delivered after the Off
// that followed them. Each press supersedes the one before it - so nothing waits out a timeout -
// and the Off is the final word: it goes out last, and nothing older follows it.
func TestOffIsTheLastThingSentToABulbThatNeverConfirms(t *testing.T) {
	shrinkWriteTimeout(t, 5*time.Second) // waiting any of these out would fail the test
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false))

	results := make(chan error, 4)
	run := func(cmd *command.Command) {
		go func() {
			_, err := nc.applyCommand(context.Background(), cmd)
			results <- err
		}()
	}

	// Presses arrive one after another, each while the bulb is still not confirming the last.
	// Each is admitted (published) before the next is sent, so "newer" is unambiguous.
	run(levelCommand("zigbee-bulb", 30))
	waitForSet(t, fc, 1)
	run(levelCommand("zigbee-bulb", 40))
	waitForSet(t, fc, 2)
	run(levelCommand("zigbee-bulb", 50))
	waitForSet(t, fc, 3)
	run(offCommand("zigbee-bulb"))
	waitForSet(t, fc, 4)

	payloads := publishedPayloads(fc, "zigbee2mqtt/lamp1/set")
	require.Len(t, payloads, 4, "%v", payloads)
	assert.JSONEq(t, `{"state":"OFF"}`, payloads[3], "Off must be the last thing sent to the device")

	// The three brightness callers are released promptly, as superseded - none waited out the 5s.
	for i := 0; i < 3; i++ {
		select {
		case err := <-results:
			assert.ErrorIs(t, err, bridge.ErrCommandSuperseded)
		case <-time.After(time.Second):
			t.Fatal("a superseded caller was still waiting")
		}
	}

	fc.deliver("zigbee2mqtt/lamp1", []byte(`{"state":"OFF","brightness":76}`))
	select {
	case err := <-results:
		assert.NoError(t, err, "the Off itself completes normally")
	case <-time.After(time.Second):
		t.Fatal("the Off never completed")
	}

	// Nothing was sent after the Off.
	time.Sleep(50 * time.Millisecond)
	assert.Len(t, publishedPayloads(fc, "zigbee2mqtt/lamp1/set"), 4)
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
