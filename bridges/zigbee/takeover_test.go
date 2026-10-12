package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Two callers ask for the same thing; the first one's request has a short deadline. When that
// deadline passes the first caller gets its timeout - but the second, whose own request is fine,
// must not inherit it. It takes over and gets the lamp switched itself.
func TestACallerWhoJoinedTakesOverWhenTheOwnersDeadlinePasses(t *testing.T) {
	shrinkWriteTimeout(t, 5*time.Second)
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false))
	settle(t, nc, fc, `{"state":"OFF","brightness":127}`)

	ownerCtx, cancelOwner := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancelOwner()
	ownerDone := make(chan error, 1)
	go func() {
		_, err := nc.applyCommand(ownerCtx, onCommand("zigbee-bulb"))
		ownerDone <- err
	}()
	waitForSet(t, fc, 1) // the owner's command is on the wire and unconfirmed

	joinerDone := make(chan error, 1)
	go func() {
		_, err := nc.applyCommand(context.Background(), onCommand("zigbee-bulb"))
		joinerDone <- err
	}()
	time.Sleep(20 * time.Millisecond) // the joiner has joined the owner's command (nothing new was sent)
	assert.Equal(t, 1, countPublished(fc, "zigbee2mqtt/lamp1/set"))

	// The owner's deadline passes...
	select {
	case err := <-ownerDone:
		assert.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("the owner never gave up")
	}

	// ...and the joiner, whose request is still live, sends the command itself.
	waitForSet(t, fc, 2)
	settle(t, nc, fc, `{"state":"ON","brightness":127}`)
	select {
	case err := <-joinerDone:
		assert.NoError(t, err, "the joiner must succeed, not fail with the owner's deadline")
	case <-time.After(2 * time.Second):
		t.Fatal("the joiner never got an outcome")
	}
}
