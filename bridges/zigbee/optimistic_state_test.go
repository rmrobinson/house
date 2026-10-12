package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
)

// published reads a device the way the rest of the house does - through the bridge's own API - so
// these tests see exactly what a policy or the UI would, not the bridge's internal copy (which is
// locked for the whole of an in-flight command).
func published(t *testing.T, nc *networkConn, id string) *device.Device {
	t.Helper()
	d, err := nc.svc.API().GetDevice(context.Background(), &api2.GetDeviceRequest{Id: id})
	require.NoError(t, err)
	return d
}

func lightIsOn(t *testing.T, nc *networkConn, id string) bool {
	t.Helper()
	return published(t, nc, id).GetLight().GetOnOff().GetState().GetIsOn()
}

func lightLevel(t *testing.T, nc *networkConn, id string) int32 {
	t.Helper()
	return published(t, nc, id).GetLight().GetBrightness().GetState().GetLevel()
}

func relativeCommand(id string, delta int32) *command.Command {
	return &command.Command{DeviceId: id, Details: &command.Command_BrightnessRelative{BrightnessRelative: &command.BrightnessRelative{ChangePercent: delta}}}
}

func settle(t *testing.T, nc *networkConn, fc *fakeMQTTClient, state string) {
	t.Helper()
	fc.deliver("zigbee2mqtt/lamp1", []byte(state))
}

// Captured live: three taps on a pair of lamps that were stalling all sent "on", because each tap
// decided from the state the house still believed (off) while the first command was in flight for
// ~14 seconds. The commanded state must be visible the moment the command is accepted.
func TestCommandedStateIsPublishedBeforeTheDeviceConfirms(t *testing.T) {
	shrinkWriteTimeout(t, 5*time.Second) // the device never answers in this test
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false))
	settle(t, nc, fc, `{"state":"OFF","brightness":127}`)
	require.False(t, lightIsOn(t, nc, "zigbee-bulb"))

	done := make(chan error, 1)
	go func() {
		_, err := nc.applyCommand(context.Background(), onCommand("zigbee-bulb"))
		done <- err
	}()
	waitForSet(t, fc, 1) // sent, and the lamp has not answered

	assert.True(t, lightIsOn(t, nc, "zigbee-bulb"), "the house must already see the lamp as on")

	settle(t, nc, fc, `{"state":"ON","brightness":127}`)
	require.NoError(t, <-done)
}

// The scenario itself: a toggle policy reads the published state, then commands the opposite.
func TestASecondTapTogglesFromTheCommandedStateNotTheStaleOne(t *testing.T) {
	shrinkWriteTimeout(t, 5*time.Second)
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false))
	settle(t, nc, fc, `{"state":"OFF","brightness":127}`)

	toggle := func() <-chan error {
		ch := make(chan error, 1)
		on := lightIsOn(t, nc, "zigbee-bulb") // what a toggle policy does: read, then flip
		cmd := onCommand("zigbee-bulb")
		if on {
			cmd = offCommand("zigbee-bulb")
		}
		go func() {
			_, err := nc.applyCommand(context.Background(), cmd)
			ch <- err
		}()
		return ch
	}

	first := toggle()
	waitForSet(t, fc, 1) // first tap sent; the lamp stalls
	second := toggle()   // second tap, while the first is still unconfirmed
	waitForSet(t, fc, 2)

	payloads := publishedPayloads(fc, "zigbee2mqtt/lamp1/set")
	require.Len(t, payloads, 2)
	assert.JSONEq(t, `{"state":"ON"}`, payloads[0])
	assert.JSONEq(t, `{"state":"OFF"}`, payloads[1], "the second tap must turn it off, not send On again")

	assert.ErrorIs(t, <-first, bridge.ErrCommandSuperseded)
	settle(t, nc, fc, `{"state":"OFF","brightness":127}`)
	assert.NoError(t, <-second)
}

// Two +10% steps sent while the first is unconfirmed are +20% in total, as the person pressing
// expects - not two steps both computed from the same stale level.
func TestRelativeStepsAddUpWhileTheDeviceIsSilent(t *testing.T) {
	shrinkWriteTimeout(t, 5*time.Second)
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false))
	settle(t, nc, fc, `{"state":"ON","brightness":127}`) // 50%
	require.Equal(t, int32(50), lightLevel(t, nc, "zigbee-bulb"))

	results := make(chan error, 2)
	run := func() {
		go func() {
			_, err := nc.applyCommand(context.Background(), relativeCommand("zigbee-bulb", 10))
			results <- err
		}()
	}
	run()
	waitForSet(t, fc, 1)
	assert.Equal(t, int32(60), lightLevel(t, nc, "zigbee-bulb"), "the first step is visible at once")
	run()
	waitForSet(t, fc, 2)
	assert.Equal(t, int32(70), lightLevel(t, nc, "zigbee-bulb"), "the second adds to the first")

	payloads := publishedPayloads(fc, "zigbee2mqtt/lamp1/set")
	require.Len(t, payloads, 2)
	assert.JSONEq(t, fmt.Sprintf(`{"brightness":%d}`, percentToBrightness(60, 254)), payloads[0])
	assert.JSONEq(t, fmt.Sprintf(`{"brightness":%d}`, percentToBrightness(70, 254)), payloads[1],
		"the second command must be 70%, not 60% again")
	<-results // the first is superseded
	settle(t, nc, fc, fmt.Sprintf(`{"state":"ON","brightness":%d}`, percentToBrightness(70, 254)))
	<-results
}

// A relative step is computed from the level BEFORE it is updated; reading it after would apply the
// delta twice. One step on an idle lamp must be exactly one step.
func TestOneRelativeStepIsAppliedExactlyOnce(t *testing.T) {
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false))
	settle(t, nc, fc, `{"state":"ON","brightness":127}`) // 50%
	fc.respond("zigbee2mqtt/lamp1/set", "zigbee2mqtt/lamp1",
		[]byte(fmt.Sprintf(`{"state":"ON","brightness":%d}`, percentToBrightness(60, 254))))

	_, err := nc.applyCommand(context.Background(), relativeCommand("zigbee-bulb", 10))
	require.NoError(t, err)

	payloads := publishedPayloads(fc, "zigbee2mqtt/lamp1/set")
	require.Len(t, payloads, 1)
	assert.JSONEq(t, fmt.Sprintf(`{"brightness":%d}`, percentToBrightness(60, 254)), payloads[0])
	assert.Equal(t, int32(60), lightLevel(t, nc, "zigbee-bulb"))
}

// A command that was sent but not confirmed may still take effect, so the commanded state stands
// until the device's own report says otherwise.
func TestAnUnconfirmedCommandKeepsItsCommandedState(t *testing.T) {
	shrinkWriteTimeout(t, 100*time.Millisecond)
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false))
	settle(t, nc, fc, `{"state":"OFF","brightness":127}`)

	_, err := nc.applyCommand(context.Background(), onCommand("zigbee-bulb"))
	require.ErrorIs(t, err, bridge.ErrCommandTimeout)
	assert.True(t, lightIsOn(t, nc, "zigbee-bulb"), "a timeout must not undo the commanded state")

	// ...and the device's own report corrects it, as any report does.
	settle(t, nc, fc, `{"state":"OFF","brightness":127}`)
	assert.False(t, lightIsOn(t, nc, "zigbee-bulb"))
}

// If the command never left this process there is nothing in flight to wait for: the assumed
// state is wrong, and must be put back - and the correction published, not just kept locally.
func TestACommandThatNeverLeftIsRolledBack(t *testing.T) {
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false))
	settle(t, nc, fc, `{"state":"OFF","brightness":127}`)
	nc.queryWG.Wait() // the startup state query for the new device must be done before the broker "fails"
	fc.setPublishErr(errors.New("broker unavailable"))

	_, err := nc.applyCommand(context.Background(), onCommand("zigbee-bulb"))
	require.Error(t, err)
	assert.False(t, lightIsOn(t, nc, "zigbee-bulb"), "nothing was sent, so the lamp must not read as on")
	assert.Empty(t, publishedPayloads(fc, "zigbee2mqtt/lamp1/set"))

	// And the bridge is not left wedged: with the broker back, the same command works.
	fc.setPublishErr(nil)
	fc.respond("zigbee2mqtt/lamp1/set", "zigbee2mqtt/lamp1", []byte(`{"state":"ON","brightness":127}`))
	_, err = nc.applyCommand(context.Background(), onCommand("zigbee-bulb"))
	require.NoError(t, err)
	assert.True(t, lightIsOn(t, nc, "zigbee-bulb"))
}

// A plain switch or plug (the generic on/off device) behaves the same.
func TestPlugCommandedStateIsPublishedBeforeConfirmation(t *testing.T) {
	shrinkWriteTimeout(t, 5*time.Second)
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(switchDevice(false)) // zigbee-abc, topic zigbee2mqtt/plug1

	done := make(chan error, 1)
	go func() {
		_, err := nc.applyCommand(context.Background(), onCommand("zigbee-abc"))
		done <- err
	}()
	require.Eventually(t, func() bool { return countPublished(fc, "zigbee2mqtt/plug1/set") == 1 },
		2*time.Second, time.Millisecond)

	assert.True(t, published(t, nc, "zigbee-abc").GetGeneric().GetOnOff().GetState().GetIsOn())

	fc.deliver("zigbee2mqtt/plug1", []byte(`{"state":"ON"}`))
	require.NoError(t, <-done)
}
