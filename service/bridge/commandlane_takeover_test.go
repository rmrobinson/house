package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A caller that JOINED a pending command must not inherit the owner's cancellation: the owner's
// deadline passing says nothing about the joiner's request, nothing was ever sent for the joiner,
// and the joiner's own context is fine.
func TestLane_JoinerIsToldWhenTheOwnersCallerGivesUp(t *testing.T) {
	var l CommandLane
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	owner := expectResult(t, acquireAsync(&l, ownerCtx, powerCmd(true)), time.Second)
	require.NoError(t, owner.err)

	joined := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
	require.NotNil(t, joined.flight)
	outcome := waitOutcome(joined.flight, context.Background())

	// The owner's request is cancelled while its command is in flight; the bridge ends it with the
	// context error and releases the lease with it.
	cancelOwner()
	owner.lease.Release(owner.ctx.Err())

	err := expectOutcome(t, outcome, time.Second)
	assert.ErrorIs(t, err, ErrOwnerGaveUp, "the joiner must be told to take over, not handed the owner's cancellation")
	assert.NotErrorIs(t, err, context.Canceled)
}

func TestLane_JoinerIsToldWhenTheOwnerExpiresWhileStillQueued(t *testing.T) {
	var l CommandLane
	busy := expectResult(t, acquireAsync(&l, context.Background(), toggleCmd("led", true)), time.Second)
	require.NoError(t, busy.err)

	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	owner := acquireAsync(&l, ownerCtx, powerCmd(true)) // queued behind the Toggle
	waitForQueued(t, &l, 2)

	joined := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
	require.NotNil(t, joined.flight)
	outcome := waitOutcome(joined.flight, context.Background())

	cancelOwner() // the owner's caller gives up before its command was ever sent
	assert.ErrorIs(t, expectResult(t, owner, time.Second).err, context.Canceled)
	assert.ErrorIs(t, expectOutcome(t, outcome, time.Second), ErrOwnerGaveUp)

	busy.release()
}

// What the device does is the joiner's business: a device timeout, being superseded and going stale
// are shared as they are, and are not mistaken for the owner giving up.
func TestLane_DeviceOutcomesAreNotMistakenForTheOwnerGivingUp(t *testing.T) {
	for _, outcome := range []error{ErrCommandTimeout, ErrCommandSuperseded, ErrCommandStale, nil} {
		var l CommandLane
		owner := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
		require.NoError(t, owner.err)
		joined := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
		require.NotNil(t, joined.flight)
		ch := waitOutcome(joined.flight, context.Background())

		owner.lease.Release(outcome)
		got := expectOutcome(t, ch, time.Second)
		if outcome == nil {
			assert.NoError(t, got)
		} else {
			assert.ErrorIs(t, got, outcome)
		}
		assert.NotErrorIs(t, got, ErrOwnerGaveUp)
	}
}

// The three commands the lane coalesces must stay spelled the way Command.details spells them: the
// allow-list is by field name, so a rename would silently stop coalescing without failing anything.
func TestIdempotentCommandsAreNamedAsTheCommandProtoNamesThem(t *testing.T) {
	assert.True(t, idempotent(powerCmd(true)), "on_off")
	assert.True(t, idempotent(levelCmd(50)), "brightness_absolute")
	assert.True(t, idempotent(colourCmd()), "colour")
	assert.False(t, idempotent(relativeCmd(10)), "brightness_relative steps from the current level")
	assert.False(t, idempotent(toggleCmd("led", true)), "a Toggle is a partial update")
	assert.False(t, idempotent(muteCmd()))
}
