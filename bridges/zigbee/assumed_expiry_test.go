package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rmrobinson/house/service/bridge"
)

// stopAssumedStateExpiries cancels every device's pending revert and waits for any that already
// fired, so no timer outlives the test that started it.
func (nc *networkConn) stopAssumedStateExpiries() {
	nc.mu.Lock()
	var devs []*builtDevice
	for _, bd := range nc.devices {
		devs = append(devs, bd)
	}
	nc.mu.Unlock()
	for _, bd := range devs {
		bd.mu.Lock()
		nc.cancelAssumedExpiry(bd)
		bd.mu.Unlock()
	}
	nc.expiryWG.Wait()
}

func shrinkGrace(t *testing.T, d time.Duration) {
	t.Helper()
	orig := unconfirmedStateGrace
	unconfirmedStateGrace = d
	t.Cleanup(func() { unconfirmedStateGrace = orig })
}

func newLampOff(t *testing.T) (*networkConn, *fakeMQTTClient) {
	t.Helper()
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false))
	settle(t, nc, fc, `{"state":"OFF","brightness":127}`)
	require.False(t, lightIsOn(t, nc, "zigbee-bulb"))
	return nc, fc
}

// A command to a lamp that has dropped off the network times out - the caller is told it failed -
// and no report will ever arrive. The house must not go on showing the commanded state forever.
func TestAnAssumedStateThatIsNeverConfirmedExpires(t *testing.T) {
	shrinkWriteTimeout(t, 40*time.Millisecond)
	shrinkGrace(t, 150*time.Millisecond)
	nc, _ := newLampOff(t)

	_, err := nc.applyCommand(context.Background(), onCommand("zigbee-bulb"))
	require.ErrorIs(t, err, bridge.ErrCommandTimeout)
	assert.True(t, lightIsOn(t, nc, "zigbee-bulb"), "the commanded state stands while the lamp may still be catching up")

	require.Eventually(t, func() bool { return !lightIsOn(t, nc, "zigbee-bulb") },
		2*time.Second, 10*time.Millisecond, "an unconfirmed commanded state must not stand forever")
}

// A slow lamp that does answer (just late) must keep its commanded state until its own report.
func TestALateReportInsideTheGraceWindowReplacesTheAssumptionAndIsNotRevertedOver(t *testing.T) {
	shrinkWriteTimeout(t, 40*time.Millisecond)
	shrinkGrace(t, 250*time.Millisecond)
	nc, fc := newLampOff(t)

	_, err := nc.applyCommand(context.Background(), onCommand("zigbee-bulb"))
	require.ErrorIs(t, err, bridge.ErrCommandTimeout)

	settle(t, nc, fc, `{"state":"ON","brightness":127}`) // the lamp finally reports
	time.Sleep(450 * time.Millisecond)                   // well past the grace window
	assert.True(t, lightIsOn(t, nc, "zigbee-bulb"), "the lamp's own report must stand; the expiry must not undo it")
}

// A report that carries no on/off or brightness (just link quality) confirms nothing.
func TestAReportOfOnlyLinkQualityDoesNotCancelTheExpiry(t *testing.T) {
	shrinkWriteTimeout(t, 40*time.Millisecond)
	shrinkGrace(t, 150*time.Millisecond)
	nc, fc := newLampOff(t)

	_, err := nc.applyCommand(context.Background(), onCommand("zigbee-bulb"))
	require.ErrorIs(t, err, bridge.ErrCommandTimeout)
	settle(t, nc, fc, `{"linkquality":120}`)

	require.Eventually(t, func() bool { return !lightIsOn(t, nc, "zigbee-bulb") },
		2*time.Second, 10*time.Millisecond)
}

// Several unconfirmed commands in a row revert to what the lamp last REPORTED, not to one of the
// intermediate guesses.
func TestAChainOfUnconfirmedCommandsRevertsToTheLastReportedState(t *testing.T) {
	shrinkWriteTimeout(t, 40*time.Millisecond)
	shrinkGrace(t, 150*time.Millisecond)
	nc, _ := newLampOff(t)

	_, err := nc.applyCommand(context.Background(), onCommand("zigbee-bulb"))
	require.ErrorIs(t, err, bridge.ErrCommandTimeout)
	_, err = nc.applyCommand(context.Background(), levelCommand("zigbee-bulb", 30))
	require.ErrorIs(t, err, bridge.ErrCommandTimeout)
	require.Equal(t, int32(30), lightLevel(t, nc, "zigbee-bulb"))

	require.Eventually(t, func() bool { return !lightIsOn(t, nc, "zigbee-bulb") },
		2*time.Second, 10*time.Millisecond)
	assert.Equal(t, int32(50), lightLevel(t, nc, "zigbee-bulb"), "back to the reported 127/254, not the guessed 30%")
}

// A newer command takes over the assumption; the older one's pending revert must not fire on top.
func TestANewerCommandCancelsThePendingRevert(t *testing.T) {
	shrinkWriteTimeout(t, 40*time.Millisecond)
	shrinkGrace(t, 200*time.Millisecond)
	nc, fc := newLampOff(t)

	_, err := nc.applyCommand(context.Background(), onCommand("zigbee-bulb"))
	require.ErrorIs(t, err, bridge.ErrCommandTimeout)

	// Before that revert is due, a second command succeeds: the lamp confirms it.
	fc.respond("zigbee2mqtt/lamp1/set", "zigbee2mqtt/lamp1", []byte(`{"state":"ON","brightness":127}`))
	_, err = nc.applyCommand(context.Background(), onCommand("zigbee-bulb"))
	require.NoError(t, err)

	time.Sleep(450 * time.Millisecond)
	assert.True(t, lightIsOn(t, nc, "zigbee-bulb"), "the first command's expiry must not undo a confirmed one")
}

// An unconfirmed command whose state matches what was already reported has nothing to revert.
func TestNothingToRevertWhenTheAssumptionEqualsTheLastReport(t *testing.T) {
	shrinkWriteTimeout(t, 40*time.Millisecond)
	shrinkGrace(t, 80*time.Millisecond)
	nc, _ := newLampOff(t)

	_, err := nc.applyCommand(context.Background(), offCommand("zigbee-bulb")) // it is already off
	require.ErrorIs(t, err, bridge.ErrCommandTimeout)
	time.Sleep(250 * time.Millisecond)
	assert.False(t, lightIsOn(t, nc, "zigbee-bulb"))
}
