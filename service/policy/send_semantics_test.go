package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// home.send must raise on a failed command like every other home.* call, so a script that ignores
// the outcome can't log a success while its commands failed.
func TestSendRaisesOnFailureButOnlyAfterTryingEveryDevice(t *testing.T) {
	g := newGatedHome("")
	g.fail["a"] = errors.New("device did not confirm command in time")
	g.setDeviceName("a", "Left Nightstand Light")

	err := runScriptForTest(t, g, `
		home.send({
			{id = "a", op = "setLight", on = true},
			{id = "b", op = "setLight", on = true},
		})
	`)
	require.Error(t, err)

	bindingErr := asBindingError(err)
	require.NotNil(t, bindingErr, "a failed command is a binding error, like a failed home.setLight")
	assert.Contains(t, bindingErr.Error(), "home.send")
	assert.Contains(t, bindingErr.Error(), "Left Nightstand Light")
	assert.Contains(t, bindingErr.Error(), "did not confirm")

	on, _ := g.GetLight("b")
	assert.True(t, on, "the healthy device must still have been commanded before the script raised")
}

func TestSendDoesNotRaiseWhenEverythingSucceeds(t *testing.T) {
	g := newGatedHome("")
	require.NoError(t, runScriptForTest(t, g, `home.send({{id = "a", op = "setLight", on = true}})`))
}

// A command a newer one superseded is not a failure: the newer command governs the device, and
// raising would log an error for every button press that was simply followed by another.
func TestSupersededCommandIsNotAFailure(t *testing.T) {
	g := newGatedHome("")
	g.fail["a"] = status.Error(codes.Aborted, "command superseded by a newer command to the same device")

	// send does not raise...
	require.NoError(t, runScriptForTest(t, g, `
		home.send({
			{id = "a", op = "setLight", on = true},
			{id = "a", op = "setState", key = "brightness_relative", value = {change_percent = 10}},
			{id = "b", op = "setLight", on = true},
		})
	`))
	assert.Equal(t, []string{"setLight"}, g.callsFor("a"), "what followed a superseded step is moot, so it is not sent")

	// ...and trySend says what happened.
	require.NoError(t, runScriptForTest(t, g, `
		local results, failures = home.trySend({
			{id = "a", op = "setLight", on = true},
			{id = "a", op = "setState", key = "x", value = 1},
			{id = "b", op = "setLight", on = true},
		})
		assert(results[1].ok == false and results[1].superseded == true, "step 1 should read as superseded")
		assert(results[2].skipped == true, "step 2 should be skipped")
		assert(results[3].ok == true)
		assert(failures == nil, "a superseded command must not count as a failure: " .. tostring(failures))
	`))
}

// Once the script's context is done (it timed out or was interrupted) no further step may start.
func TestSendStartsNoStepsOnceTheScriptIsCancelled(t *testing.T) {
	g := newGatedHome("")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	steps := []sendStep{
		{deviceID: "a", op: "setLight", on: true},
		{deviceID: "a", op: "setLight", on: false},
		{deviceID: "b", op: "setLight", on: true},
	}
	results := runSendSteps(ctx, g, steps)

	for i, r := range results {
		assert.True(t, r.Skipped, "step %d must be skipped", i+1)
		assert.False(t, r.failed(), "a cancelled script's skipped steps are not failures")
	}
	assert.Empty(t, g.calls, "nothing may be sent after the script was cancelled")
}

// api is called on goroutines of ours, where an unrecovered panic would take down the process.
type panickyHome struct{ *fakeHomeAPI }

func (panickyHome) SetLight(string, bool) error { panic("boom") }

func TestSendTurnsAPanicInACommandIntoThatStepsError(t *testing.T) {
	steps := []sendStep{
		{deviceID: "a", op: "setLight", on: true},
		{deviceID: "a", op: "setLight", on: false},
	}
	results := runSendSteps(context.Background(), panickyHome{newFakeHomeAPI()}, steps)

	require.Len(t, results, 2)
	assert.True(t, results[0].failed())
	assert.Contains(t, results[0].Err.Error(), "panicked")
	assert.True(t, results[1].Skipped, "the step after a failure is skipped")
}

func TestTrySendNeverRaisesForAFailedCommand(t *testing.T) {
	g := newGatedHome("")
	g.fail["a"] = errors.New("nope")
	require.NoError(t, runScriptForTest(t, g, `
		local results, failures = home.trySend({{id = "a", op = "setLight", on = true}})
		assert(results[1].ok == false and string.find(results[1].error, "nope"))
		assert(string.find(failures, "nope"))
	`))
}

func TestSendAndTrySendBothRaiseForAMalformedTable(t *testing.T) {
	for _, fn := range []string{"send", "trySend"} {
		err := runScriptForTest(t, newGatedHome(""), `home.`+fn+`({{op = "setLight", on = true}})`)
		require.Error(t, err, fn)
		assert.Nil(t, asBindingError(err), "%s: a malformed call is the script's own mistake", fn)
	}
}
