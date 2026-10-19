package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitOutcome runs flight.Wait on its own goroutine so a test can check whether it has returned.
func waitOutcome(f *Flight, ctx context.Context) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- f.Wait(ctx) }()
	return ch
}

func expectOutcome(t *testing.T, ch <-chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(within):
		t.Fatal("the joined caller never got an outcome")
		return nil
	}
}

// A slow device is already behind. Sending it the same On a second time only lengthens its queue
// (and gives an old copy another chance to land after a newer command), so an identical command
// joins the one pending instead.
func TestLane_IdenticalCommandJoinsInsteadOfBeingSentAgain(t *testing.T) {
	var l CommandLane
	first := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
	require.NoError(t, first.err)
	require.NotNil(t, first.lease)

	second := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
	require.NoError(t, second.err)
	assert.Nil(t, second.lease, "an identical command must not be granted a lease to send")
	require.NotNil(t, second.flight, "it must join the pending one instead")

	outcome := waitOutcome(second.flight, context.Background())
	select {
	case <-outcome:
		t.Fatal("the joined caller must wait for the command it joined")
	case <-time.After(30 * time.Millisecond):
	}
	assert.NoError(t, first.ctx.Err(), "joining must not disturb the command in flight")

	first.lease.Release(nil)
	assert.NoError(t, expectOutcome(t, outcome, time.Second), "the joiner shares a successful outcome")
}

func TestLane_JoinedCallersShareTheFailureToo(t *testing.T) {
	var l CommandLane
	first := expectResult(t, acquireAsync(&l, context.Background(), levelCmd(40)), time.Second)
	require.NoError(t, first.err)

	joined := expectResult(t, acquireAsync(&l, context.Background(), levelCmd(40)), time.Second)
	require.NotNil(t, joined.flight)
	outcome := waitOutcome(joined.flight, context.Background())

	first.lease.Release(ErrCommandTimeout)
	assert.ErrorIs(t, expectOutcome(t, outcome, time.Second), ErrCommandTimeout)
}

// Nine presses of Brighten against a bulb that has gone quiet used to be nine On commands for it
// to flush. One goes out; the rest wait on it.
func TestLane_ManyIdenticalCommandsSendOnce(t *testing.T) {
	var l CommandLane
	first := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
	require.NoError(t, first.err)

	var outcomes []<-chan error
	for i := 0; i < 8; i++ {
		r := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
		require.NoError(t, r.err)
		require.Nil(t, r.lease, "repeat %d must not be sent", i+1)
		outcomes = append(outcomes, waitOutcome(r.flight, context.Background()))
	}
	waitForQueued(t, &l, 1) // still just the one command in the lane

	first.lease.Release(nil)
	for i, o := range outcomes {
		assert.NoError(t, expectOutcome(t, o, time.Second), "joiner %d", i+1)
	}
}

// Different values are different requests: the newer replaces the older, as before.
func TestLane_DifferentValueIsNotJoined(t *testing.T) {
	var l CommandLane
	first := expectResult(t, acquireAsync(&l, context.Background(), levelCmd(30)), time.Second)
	require.NoError(t, first.err)

	second := acquireAsync(&l, context.Background(), levelCmd(60))
	waitForQueued(t, &l, 2)
	require.Eventually(t, func() bool { return first.ctx.Err() != nil }, time.Second, time.Millisecond,
		"a different brightness must supersede the one in flight, not join it")

	first.lease.Release(ErrCommandSuperseded)
	r := expectResult(t, second, time.Second)
	require.NoError(t, r.err)
	require.NotNil(t, r.lease, "the newer, different command is sent")
	r.release()
}

// A relative step isn't "the same" the second time: it would be applied again.
func TestLane_RelativeBrightnessIsNeverJoined(t *testing.T) {
	var l CommandLane
	first := expectResult(t, acquireAsync(&l, context.Background(), relativeCmd(10)), time.Second)
	require.NoError(t, first.err)

	second := acquireAsync(&l, context.Background(), relativeCmd(10))
	waitForQueued(t, &l, 2)
	require.Eventually(t, func() bool { return first.ctx.Err() != nil }, time.Second, time.Millisecond,
		"an identical relative step supersedes the first like any brightness command; it must not join it")

	first.lease.Release(ErrCommandSuperseded)
	r := expectResult(t, second, time.Second)
	require.NoError(t, r.err)
	assert.NotNil(t, r.lease)
	r.release()
}

// On, Off, On: the second On must supersede the Off (the newest pending command it conflicts with),
// not join the older On behind it - or the user's last request would be lost.
func TestLane_DoesNotJoinAnOlderEqualCommandBehindADifferentOne(t *testing.T) {
	var l CommandLane
	on1 := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
	require.NoError(t, on1.err)

	off := acquireAsync(&l, context.Background(), powerCmd(false))
	waitForQueued(t, &l, 2)

	on2 := acquireAsync(&l, context.Background(), powerCmd(true))
	assert.ErrorIs(t, expectResult(t, off, time.Second).err, ErrCommandSuperseded, "the Off is superseded by the newer On")

	on1.lease.Release(ErrCommandSuperseded)
	r := expectResult(t, on2, time.Second)
	require.NoError(t, r.err)
	require.NotNil(t, r.lease, "the final On is sent itself; it did not join the first")
	r.release()
}

// A command that supersedes the one a caller joined moots the joiner's request too.
func TestLane_JoinerSeesSupersessionOfTheCommandItJoined(t *testing.T) {
	var l CommandLane
	on := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
	require.NoError(t, on.err)

	joined := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
	require.NotNil(t, joined.flight)
	outcome := waitOutcome(joined.flight, context.Background())

	off := acquireAsync(&l, context.Background(), powerCmd(false)) // supersedes the On both callers wanted
	require.Eventually(t, func() bool { return on.ctx.Err() != nil }, time.Second, time.Millisecond)

	on.lease.Release(SupersededError(on.ctx, on.ctx.Err()))
	assert.ErrorIs(t, expectOutcome(t, outcome, time.Second), ErrCommandSuperseded)

	r := expectResult(t, off, time.Second)
	require.NoError(t, r.err)
	r.release()
}

// A pending command that hasn't been sent yet can be joined too.
func TestLane_CanJoinACommandStillWaitingItsTurn(t *testing.T) {
	var l CommandLane
	busy := expectResult(t, acquireAsync(&l, context.Background(), toggleCmd("led", true)), time.Second)
	require.NoError(t, busy.err)

	waiting := acquireAsync(&l, context.Background(), powerCmd(true)) // queued behind the Toggle
	waitForQueued(t, &l, 2)

	repeat := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
	require.NoError(t, repeat.err)
	require.NotNil(t, repeat.flight, "an identical command to one still queued joins it")
	outcome := waitOutcome(repeat.flight, context.Background())

	busy.release()
	w := expectResult(t, waiting, time.Second)
	require.NoError(t, w.err)
	require.NotNil(t, w.lease)
	w.lease.Release(nil)
	assert.NoError(t, expectOutcome(t, outcome, time.Second))
}

// Giving up waiting on a joined command must not cancel it for anyone else.
func TestLane_JoinerGivingUpDoesNotAffectTheCommand(t *testing.T) {
	var l CommandLane
	first := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
	require.NoError(t, first.err)

	joined := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
	require.NotNil(t, joined.flight)

	ctx, cancel := context.WithCancel(context.Background())
	outcome := waitOutcome(joined.flight, ctx)
	cancel()
	assert.ErrorIs(t, expectOutcome(t, outcome, time.Second), context.Canceled)

	assert.NoError(t, first.ctx.Err(), "the command itself must be unaffected")
	first.lease.Release(nil)
}

func TestLane_AnUnrelatedCommandIsNotJoined(t *testing.T) {
	var l CommandLane
	on := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
	require.NoError(t, on.err)

	// An absolute brightness is not "the same" as an On, even though both are idempotent.
	lvl := acquireAsync(&l, context.Background(), levelCmd(50))
	waitForQueued(t, &l, 2)
	expectPending(t, lvl)

	on.release()
	r := expectResult(t, lvl, time.Second)
	require.NoError(t, r.err)
	assert.NotNil(t, r.lease)
	r.release()
}
