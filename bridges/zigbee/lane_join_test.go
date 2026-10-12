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

func onCommand(id string) *command.Command {
	return &command.Command{DeviceId: id, Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}}}
}

// Captured live: against a bulb that had gone quiet, nine Brighten presses sent nine identical On
// commands in six seconds, all of which the bulb then had to flush at once - and any of which could
// land after a later Off. Now one goes out and the other eight wait on it.
func TestRepeatedIdenticalCommandsToASlowBulbAreSentOnce(t *testing.T) {
	shrinkWriteTimeout(t, 5*time.Second) // far longer than the test: waiting it out would fail
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false))

	const callers = 9
	results := make(chan error, callers)
	run := func() {
		go func() {
			_, err := nc.applyCommand(context.Background(), onCommand("zigbee-bulb"))
			results <- err
		}()
	}

	run()
	waitForSet(t, fc, 1) // the first is on the wire and unconfirmed: the bulb has gone quiet
	for i := 1; i < callers; i++ {
		run()
	}
	time.Sleep(100 * time.Millisecond) // give the repeats every chance to (wrongly) be sent
	assert.Equal(t, 1, countPublished(fc, "zigbee2mqtt/lamp1/set"), "repeats of a pending command must not be sent")

	// The bulb finally confirms: every caller, the joiners included, is told it worked.
	fc.deliver("zigbee2mqtt/lamp1", []byte(`{"state":"ON","brightness":200}`))
	for i := 0; i < callers; i++ {
		select {
		case err := <-results:
			assert.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatalf("caller %d never got an outcome", i+1)
		}
	}
	assert.Equal(t, 1, countPublished(fc, "zigbee2mqtt/lamp1/set"))
}

// If the one command that was sent is never confirmed, everyone who joined it shares that.
func TestJoinedCallersShareATimeout(t *testing.T) {
	shrinkWriteTimeout(t, 150*time.Millisecond)
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false))

	results := make(chan error, 4)
	for i := 0; i < 4; i++ {
		if i == 1 {
			waitForSet(t, fc, 1)
		}
		go func() {
			_, err := nc.applyCommand(context.Background(), onCommand("zigbee-bulb"))
			results <- err
		}()
	}

	for i := 0; i < 4; i++ {
		select {
		case err := <-results:
			assert.ErrorIs(t, err, bridge.ErrCommandTimeout)
		case <-time.After(2 * time.Second):
			t.Fatal("a joined caller never got the timeout")
		}
	}
	assert.Equal(t, 1, countPublished(fc, "zigbee2mqtt/lamp1/set"))
}

// A different request is not a repeat: Off after On is sent (it supersedes), as ever.
func TestADifferentCommandIsNotMistakenForARepeat(t *testing.T) {
	shrinkWriteTimeout(t, 5*time.Second)
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false))

	on := make(chan error, 1)
	go func() {
		_, err := nc.applyCommand(context.Background(), onCommand("zigbee-bulb"))
		on <- err
	}()
	waitForSet(t, fc, 1)

	off := make(chan error, 1)
	go func() {
		_, err := nc.applyCommand(context.Background(), offCommand("zigbee-bulb"))
		off <- err
	}()
	waitForSet(t, fc, 2)

	select {
	case err := <-on:
		assert.ErrorIs(t, err, bridge.ErrCommandSuperseded)
	case <-time.After(time.Second):
		t.Fatal("the superseded On kept waiting")
	}
	payloads := publishedPayloads(fc, "zigbee2mqtt/lamp1/set")
	require.Len(t, payloads, 2)
	assert.JSONEq(t, `{"state":"OFF"}`, payloads[1])

	fc.deliver("zigbee2mqtt/lamp1", []byte(`{"state":"OFF","brightness":200}`))
	assert.NoError(t, <-off)
}
