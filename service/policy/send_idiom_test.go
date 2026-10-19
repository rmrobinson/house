package policy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The idiom a multi-light policy uses: build every light's steps and send them all. home.send
// raises on its own if any failed. One policy handles any number of lights, and a stuck one can't
// hold up the rest.
func TestSendIdiomBrightenEveryLightEvenWhenOneIsStuck(t *testing.T) {
	g := newGatedHome("slow")

	done := make(chan error, 1)
	go func() {
		done <- runScriptForTest(t, g, `
			local lights = {"slow", "fast"}
			local steps = {}
			for _, id in ipairs(lights) do
				if not home.getLight(id) then
					steps[#steps + 1] = {id = id, op = "setLight", on = true}
				end
				steps[#steps + 1] = {id = id, op = "setState", key = "brightness_relative", value = {change_percent = 10}}
			end
			home.send(steps)
		`)
	}()

	// The fast light is switched on and brightened in full while the slow one is still stuck on
	// its first step. Wait for both facts: the slow device's goroutine may start after the fast
	// one has already finished, so "stuck" is only true once its first call has been recorded.
	require.Eventually(t, func() bool {
		return len(g.callsFor("fast")) == 2
	}, time.Second, time.Millisecond, "the fast light's steps were held up by the slow one")
	require.Eventually(t, func() bool {
		return len(g.callsFor("slow")) == 1
	}, time.Second, time.Millisecond, "the slow light never started its first step")
	assert.Equal(t, []string{"setLight"}, g.callsFor("slow"), "the slow light must not get past its first step")

	close(g.gate)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("the script never finished")
	}
	assert.Equal(t, []string{"setLight", "setState:brightness_relative"}, g.callsFor("slow"))
}
