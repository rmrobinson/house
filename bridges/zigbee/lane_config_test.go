package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rmrobinson/house/service/bridge"
)

// A command queued behind one that is merely slow to time out must still get its turn. If the
// lane's queue-wait bound were shorter than the write timeout, every command that arrived while a
// device was timing out would be dropped as stale no matter how recent it was.
func TestLaneQueueWaitOutlastsOneWriteTimeout(t *testing.T) {
	assert.Greater(t, bridge.DefaultCommandQueueWait, writeTimeout,
		"a command must survive waiting out one in-flight command's full write timeout")
}

// A command that does not supersede the one in flight (here a brightness change behind an Off)
// waits for it to time out and is then sent, rather than being dropped as stale.
func TestCommandQueuedBehindATimingOutCommandStillGoesOut(t *testing.T) {
	shrinkWriteTimeout(t, 150*time.Millisecond)
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false))

	first := make(chan error, 1)
	go func() {
		_, err := nc.applyCommand(context.Background(), offCommand("zigbee-bulb"))
		first <- err
	}()
	waitForSet(t, fc, 1)

	second := make(chan error, 1)
	go func() {
		_, err := nc.applyCommand(context.Background(), levelCommand("zigbee-bulb", 40))
		second <- err
	}()

	assert.ErrorIs(t, <-first, bridge.ErrCommandTimeout)
	waitForSet(t, fc, 2) // the queued command was sent once the first gave up its slot

	fc.deliver("zigbee2mqtt/lamp1", []byte(`{"state":"ON","brightness":102}`)) // 40% of 254
	select {
	case err := <-second:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("the queued command never completed")
	}
}
