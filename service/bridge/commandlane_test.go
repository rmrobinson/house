package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rmrobinson/house/api/command"
)

func powerCmd(on bool) *command.Command {
	return &command.Command{DeviceId: "d", Details: &command.Command_OnOff{OnOff: &command.OnOff{On: on}}}
}

func levelCmd(pct int32) *command.Command {
	return &command.Command{DeviceId: "d", Details: &command.Command_BrightnessAbsolute{BrightnessAbsolute: &command.BrightnessAbsolute{BrightnessPercent: pct}}}
}

func relativeCmd(delta int32) *command.Command {
	return &command.Command{DeviceId: "d", Details: &command.Command_BrightnessRelative{BrightnessRelative: &command.BrightnessRelative{ChangePercent: delta}}}
}

func colourCmd() *command.Command {
	return &command.Command{DeviceId: "d", Details: &command.Command_Colour{Colour: &command.Colour{}}}
}

func muteCmd() *command.Command {
	return &command.Command{DeviceId: "d", Details: &command.Command_Mute{Mute: &command.Mute{}}}
}

func TestClassOf(t *testing.T) {
	assert.Equal(t, ClassPower, ClassOf(powerCmd(true)))
	assert.Equal(t, ClassLevel, ClassOf(levelCmd(50)))
	assert.Equal(t, ClassLevel, ClassOf(relativeCmd(10)), "absolute and relative brightness are one class")
	assert.Equal(t, ClassColour, ClassOf(colourCmd()))
	assert.Equal(t, CommandClass("mute"), ClassOf(muteCmd()), "anything else is its own class, by name")
	assert.Equal(t, CommandClass("unknown"), ClassOf(&command.Command{}))
}

func TestSupersedes(t *testing.T) {
	cases := []struct {
		newer, older CommandClass
		want         bool
	}{
		{ClassLevel, ClassLevel, true},
		{ClassPower, ClassPower, true},
		{"mute", "mute", true},
		{ClassPower, ClassLevel, true},
		{ClassPower, ClassColour, true},
		// Brightness changes don't make an on/off obsolete, nor do unrelated commands.
		{ClassLevel, ClassPower, false},
		{ClassColour, ClassPower, false},
		{ClassLevel, ClassColour, false},
		{"mute", ClassLevel, false},
		{ClassPower, "mute", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, Supersedes(c.newer, c.older), "%s over %s", c.newer, c.older)
	}
}

type acquired struct {
	ctx     context.Context
	release func()
	err     error
}

// acquireAsync starts Acquire on its own goroutine and returns the channel its result arrives on.
func acquireAsync(l *CommandLane, ctx context.Context, cmd *command.Command) <-chan acquired {
	ch := make(chan acquired, 1)
	go func() {
		c, rel, err := l.Acquire(ctx, cmd)
		ch <- acquired{c, rel, err}
	}()
	return ch
}

func expectResult(t *testing.T, ch <-chan acquired, within time.Duration) acquired {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(within):
		t.Fatal("Acquire did not return in time")
		return acquired{}
	}
}

func expectPending(t *testing.T, ch <-chan acquired) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("Acquire returned, but it should still be waiting its turn")
	case <-time.After(50 * time.Millisecond):
	}
}

// waitForQueued blocks until n commands are registered with the lane (in flight or waiting), so a
// test acts on a settled state instead of sleeping and hoping.
func waitForQueued(t *testing.T, l *CommandLane, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.entries) == n
	}, 2*time.Second, time.Millisecond)
}

func TestLane_OneCommandAtATimeInOrderWhenNothingSupersedes(t *testing.T) {
	var l CommandLane
	first := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
	require.NoError(t, first.err)

	// A brightness change doesn't supersede an on/off, so it waits for its turn.
	second := acquireAsync(&l, context.Background(), levelCmd(50))
	waitForQueued(t, &l, 2)
	expectPending(t, second)
	assert.NoError(t, first.ctx.Err(), "the in-flight command must be left alone")

	first.release()
	r := expectResult(t, second, time.Second)
	require.NoError(t, r.err)
	r.release()
}

func TestLane_NewerCommandOfSameClassSupersedesQueuedAndInFlight(t *testing.T) {
	var l CommandLane
	inFlight := expectResult(t, acquireAsync(&l, context.Background(), levelCmd(30)), time.Second)
	require.NoError(t, inFlight.err)

	queued := acquireAsync(&l, context.Background(), levelCmd(60))
	waitForQueued(t, &l, 2)

	newest := acquireAsync(&l, context.Background(), levelCmd(90))

	// The queued one is turned away immediately, never to be sent...
	r := expectResult(t, queued, time.Second)
	assert.ErrorIs(t, r.err, ErrCommandSuperseded)

	// ...and the in-flight one is told to stop waiting, so the slot can free.
	require.Eventually(t, func() bool { return inFlight.ctx.Err() != nil }, time.Second, time.Millisecond)
	assert.ErrorIs(t, SupersededError(inFlight.ctx, inFlight.ctx.Err()), ErrCommandSuperseded)

	inFlight.release()
	got := expectResult(t, newest, time.Second)
	require.NoError(t, got.err, "the newest command must go out as soon as the abandoned one lets go")
	got.release()
}

// The case that started all this: brightness changes stuck behind a slow bulb must never be
// delivered after the Off that followed them.
func TestLane_PowerIsTheFinalWordOverPendingLevelAndColour(t *testing.T) {
	var l CommandLane
	inFlight := expectResult(t, acquireAsync(&l, context.Background(), relativeCmd(10)), time.Second)
	require.NoError(t, inFlight.err)

	queuedLevel := acquireAsync(&l, context.Background(), relativeCmd(10))
	queuedColour := acquireAsync(&l, context.Background(), colourCmd())
	waitForQueued(t, &l, 3)

	off := acquireAsync(&l, context.Background(), powerCmd(false))

	assert.ErrorIs(t, expectResult(t, queuedLevel, time.Second).err, ErrCommandSuperseded)
	assert.ErrorIs(t, expectResult(t, queuedColour, time.Second).err, ErrCommandSuperseded)
	require.Eventually(t, func() bool { return inFlight.ctx.Err() != nil }, time.Second, time.Millisecond)

	inFlight.release()
	r := expectResult(t, off, time.Second)
	require.NoError(t, r.err)
	r.release()
}

func TestLane_LevelDoesNotSupersedePower(t *testing.T) {
	var l CommandLane
	off := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(false)), time.Second)
	require.NoError(t, off.err)

	later := acquireAsync(&l, context.Background(), levelCmd(80))
	waitForQueued(t, &l, 2)

	assert.NoError(t, off.ctx.Err(), "a brightness command must not cancel an Off in flight")
	off.release()
	r := expectResult(t, later, time.Second)
	require.NoError(t, r.err)
	r.release()
}

func TestLane_CommandThatWaitsTooLongIsDroppedNotDeliveredLate(t *testing.T) {
	l := CommandLane{MaxWait: 40 * time.Millisecond}
	busy := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
	require.NoError(t, busy.err)
	defer busy.release()

	start := time.Now()
	r := expectResult(t, acquireAsync(&l, context.Background(), levelCmd(50)), time.Second)
	assert.ErrorIs(t, r.err, ErrCommandStale)
	assert.Less(t, time.Since(start), 500*time.Millisecond)
}

func TestLane_CallerContextEndingWhileWaitingDropsTheCommand(t *testing.T) {
	var l CommandLane
	busy := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
	require.NoError(t, busy.err)

	ctx, cancel := context.WithCancel(context.Background())
	waiting := acquireAsync(&l, ctx, levelCmd(50))
	waitForQueued(t, &l, 2)
	cancel()

	r := expectResult(t, waiting, time.Second)
	assert.ErrorIs(t, r.err, context.Canceled)

	// It left no trace: once the device frees up, the next command gets it straight away.
	busy.release()
	next := expectResult(t, acquireAsync(&l, context.Background(), levelCmd(10)), time.Second)
	require.NoError(t, next.err)
	next.release()
}

func TestLane_ReleaseLeavesNothingBehind(t *testing.T) {
	var l CommandLane
	for i := 0; i < 5; i++ {
		r := expectResult(t, acquireAsync(&l, context.Background(), levelCmd(int32(i))), time.Second)
		require.NoError(t, r.err)
		r.release()
	}
	waitForQueued(t, &l, 0)
}
