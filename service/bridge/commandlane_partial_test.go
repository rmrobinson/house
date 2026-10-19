package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rmrobinson/house/api/command"
)

func toggleCmd(setting string, on bool) *command.Command {
	return &command.Command{DeviceId: "d", Details: &command.Command_Toggle{Toggle: &command.Toggle{Settings: map[string]bool{setting: on}}}}
}

// A Toggle is a PARTIAL update ("only the keys present are changed", api/command/toggle.proto), so a
// newer Toggle does not make an older one obsolete: setting a purifier's child lock and then its
// LED are two independent commands, and dropping the first because the second arrived would lose
// it. Only commands that wholly replace the previous one - on/off, brightness, colour - are ever
// superseded.
func TestLane_PartialUpdatesAreNeverSuperseded(t *testing.T) {
	assert.False(t, Supersedes(ClassOf(toggleCmd("led", true)), ClassOf(toggleCmd("child_lock", true))),
		"one Toggle must not supersede another")
	assert.False(t, Supersedes(ClassPower, ClassOf(toggleCmd("led", true))),
		"an on/off must not supersede a Toggle")

	var l CommandLane
	inFlight := expectResult(t, acquireAsync(&l, context.Background(), toggleCmd("child_lock", true)), time.Second)
	require.NoError(t, inFlight.err)

	second := acquireAsync(&l, context.Background(), toggleCmd("led", false))
	waitForQueued(t, &l, 2)

	assert.NoError(t, inFlight.ctx.Err(), "the Toggle in flight must be left to finish")
	expectPending(t, second) // waits its turn rather than being dropped, and doesn't cancel the first

	inFlight.release()
	r := expectResult(t, second, time.Second)
	require.NoError(t, r.err, "the second Toggle must still be sent, after the first")
	r.release()
}

// Release is called from a defer in callers, so calling it twice must not deadlock or free a slot
// someone else holds.
func TestLane_ReleaseIsIdempotent(t *testing.T) {
	var l CommandLane
	a := expectResult(t, acquireAsync(&l, context.Background(), levelCmd(10)), time.Second)
	require.NoError(t, a.err)
	a.release()
	a.release() // must not block or panic

	b := expectResult(t, acquireAsync(&l, context.Background(), powerCmd(true)), time.Second)
	require.NoError(t, b.err)
	other := acquireAsync(&l, context.Background(), levelCmd(20))
	waitForQueued(t, &l, 2)

	a.release() // a stale third call must not free b's slot for other
	expectPending(t, other)
	b.release()
	r := expectResult(t, other, time.Second)
	require.NoError(t, r.err)
	r.release()
}
