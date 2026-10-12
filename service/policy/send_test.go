package policy

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatedHome wraps fakeHomeAPI so a test can hold one device's commands open (a slow or out-of-range
// device), fail another's, and see the order commands started in.
type gatedHome struct {
	*fakeHomeAPI

	slowID string
	gate   chan struct{} // commands to slowID block until this is closed
	fail   map[string]error

	cmu   sync.Mutex
	calls []string
}

func newGatedHome(slowID string) *gatedHome {
	return &gatedHome{fakeHomeAPI: newFakeHomeAPI(), slowID: slowID, gate: make(chan struct{}), fail: map[string]error{}}
}

func (g *gatedHome) before(id, call string) error {
	g.cmu.Lock()
	g.calls = append(g.calls, id+":"+call)
	err := g.fail[id]
	g.cmu.Unlock()
	if err != nil {
		return err
	}
	if id == g.slowID {
		<-g.gate
	}
	return nil
}

func (g *gatedHome) SetLight(id string, on bool) error {
	if err := g.before(id, "setLight"); err != nil {
		return err
	}
	return g.fakeHomeAPI.SetLight(id, on)
}

func (g *gatedHome) SetState(id, key string, value any) error {
	if err := g.before(id, "setState:"+key); err != nil {
		return err
	}
	return g.fakeHomeAPI.SetState(id, key, value)
}

func (g *gatedHome) callsFor(id string) []string {
	g.cmu.Lock()
	defer g.cmu.Unlock()
	var out []string
	for _, c := range g.calls {
		if len(c) > len(id) && c[:len(id)+1] == id+":" {
			out = append(out, c[len(id)+1:])
		}
	}
	return out
}

// The point of home.send: a device that is slow to confirm must not hold up the others.
func TestSendRunsDifferentDevicesInParallel(t *testing.T) {
	g := newGatedHome("slow")

	done := make(chan error, 1)
	go func() {
		done <- runScriptForTest(t, g, `
			local results, failures = home.trySend({
				{id = "slow", op = "setLight", on = true},
				{id = "fast", op = "setLight", on = true},
			})
			assert(failures == nil, tostring(failures))
			assert(results[1].ok and results[2].ok)
		`)
	}()

	// The fast device is commanded while the slow one is still stuck...
	require.Eventually(t, func() bool {
		on, _ := g.GetLight("fast")
		return on
	}, time.Second, time.Millisecond, "the fast device was held up by the slow one")
	select {
	case <-done:
		t.Fatal("send returned before the slow device finished")
	default:
	}

	// ...and send waits for the slow one before returning.
	close(g.gate)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("send never returned")
	}
	on, _ := g.GetLight("slow")
	assert.True(t, on)
}

func TestSendKeepsEachDevicesStepsInOrder(t *testing.T) {
	g := newGatedHome("") // nothing blocks
	require.NoError(t, runScriptForTest(t, g, `
		local results = home.trySend({
			{id = "a", op = "setLight", on = true},
			{id = "b", op = "setLight", on = true},
			{id = "a", op = "setState", key = "brightness_relative", value = {change_percent = 10}},
			{id = "b", op = "setState", key = "brightness_relative", value = {change_percent = 10}},
		})
		for i = 1, 4 do assert(results[i].ok, "step " .. i) end
	`))
	assert.Equal(t, []string{"setLight", "setState:brightness_relative"}, g.callsFor("a"))
	assert.Equal(t, []string{"setLight", "setState:brightness_relative"}, g.callsFor("b"))
}

func TestSendStopsADevicesStepsAtItsFirstFailureButNotOthers(t *testing.T) {
	g := newGatedHome("")
	g.fail["a"] = errors.New("device did not confirm command in time")
	g.setDeviceName("a", "Left Nightstand Light")
	g.setDeviceName("b", "Right Nightstand Light")

	require.NoError(t, runScriptForTest(t, g, `
		local results, failures = home.trySend({
			{id = "a", op = "setLight", on = true},
			{id = "a", op = "setState", key = "brightness_relative", value = {change_percent = 10}},
			{id = "b", op = "setLight", on = true},
		})
		assert(results[1].ok == false and not results[1].skipped, "step 1 should have failed")
		assert(string.find(results[1].error, "did not confirm"), tostring(results[1].error))
		assert(results[2].ok == false and results[2].skipped == true, "step 2 should be skipped")
		assert(results[3].ok == true, "the other device must be unaffected")

		-- the summary names the device and the cause, once (the skipped step isn't a second failure)
		assert(string.find(failures, "Left Nightstand Light", 1, true), tostring(failures))
		assert(string.find(failures, "did not confirm", 1, true), tostring(failures))
		assert(not string.find(failures, "Right Nightstand Light", 1, true), tostring(failures))
		assert(not string.find(failures, "skipped", 1, true), tostring(failures))
	`))
	assert.Equal(t, []string{"setLight"}, g.callsFor("a"), "the step after a failure must never be attempted")
	on, _ := g.GetLight("b")
	assert.True(t, on)
}

func TestSendReportsNoFailuresWhenEverythingSucceeds(t *testing.T) {
	g := newGatedHome("")
	require.NoError(t, runScriptForTest(t, g, `
		local results, failures = home.trySend({{id = "a", op = "setLight", on = false}})
		assert(failures == nil, "failures should be nil")
		assert(#results == 1 and results[1].ok == true and results[1].error == nil)
	`))
}

func TestSendEmptyBatchIsFine(t *testing.T) {
	g := newGatedHome("")
	require.NoError(t, runScriptForTest(t, g, `
		local results, failures = home.trySend({})
		assert(#results == 0 and failures == nil)
	`))
}

// A malformed steps table is a bug in the script, so it raises - unlike a failed command.
func TestSendRejectsMalformedSteps(t *testing.T) {
	cases := map[string]string{
		"missing id":             `home.trySend({{op = "setLight", on = true}})`,
		"unknown op":             `home.trySend({{id = "a", op = "explode"}})`,
		"setLight without on":    `home.trySend({{id = "a", op = "setLight"}})`,
		"setLight non-boolean":   `home.trySend({{id = "a", op = "setLight", on = "yes"}})`,
		"setState without key":   `home.trySend({{id = "a", op = "setState", value = 1}})`,
		"setState without value": `home.trySend({{id = "a", op = "setState", key = "x"}})`,
		"step is not a table":    `home.trySend({"a"})`,
		"argument not a table":   `home.trySend("a")`,
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			g := newGatedHome("")
			err := runScriptForTest(t, g, script)
			require.Error(t, err)
			assert.Nil(t, asBindingError(err), "a malformed call is the script's own mistake, not a binding failure")
			assert.Empty(t, g.calls, "nothing may be sent if any step is malformed")
		})
	}
}
