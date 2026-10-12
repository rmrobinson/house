package policy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The idiom a multi-light policy uses: build every light's steps, send them all, raise if any
// failed. One policy handles any number of lights, and a stuck one can't hold up the rest.
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
			local _, failures = home.send(steps)
			if failures then error(failures) end
		`)
	}()

	// The fast light is switched on and brightened in full while the slow one is still stuck on
	// its first step.
	require.Eventually(t, func() bool {
		return len(g.callsFor("fast")) == 2
	}, time.Second, time.Millisecond, "the fast light's steps were held up by the slow one")
	assert.Equal(t, []string{"setLight"}, g.callsFor("slow"), "the slow light is stuck on its first step")

	close(g.gate)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("the script never finished")
	}
	assert.Equal(t, []string{"setLight", "setState:brightness_relative"}, g.callsFor("slow"))
}
