package policy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	lua "github.com/yuin/gopher-lua"
)

func runScriptForTest(t *testing.T, home HomeAPI, script string) error {
	t.Helper()

	L := lua.NewState()
	defer L.Close()

	registerHomeTable(L, home)

	return L.DoString(script)
}

func TestBindingsLightRoundTrip(t *testing.T) {
	home := newFakeHomeAPI()

	require.NoError(t, runScriptForTest(t, home, `
		home.setLight("light.kitchen", true)
		assert(home.getLight("light.kitchen") == true)
	`))
	assert.True(t, home.getLight("light.kitchen"))
}

func TestBindingsAttributeRoundTrip(t *testing.T) {
	home := newFakeHomeAPI()

	require.NoError(t, runScriptForTest(t, home, `
		home.setAttribute("thermostat.hall", "mode", "heat")
		assert(home.getAttribute("thermostat.hall", "mode") == "heat")
	`))

	v, err := home.GetAttribute("thermostat.hall", "mode")
	require.NoError(t, err)
	assert.Equal(t, "heat", v)
}

func TestBindingsHouseStateRoundTrip(t *testing.T) {
	home := newFakeHomeAPI()

	require.NoError(t, runScriptForTest(t, home, `
		home.setHouseState("mode", "vacation")
		assert(home.getHouseState("mode") == "vacation")
	`))
	assert.Equal(t, "vacation", home.getHouseState("mode"))
}

func TestBindingsGetLastKnown(t *testing.T) {
	home := newFakeHomeAPI()
	home.setLastKnown("light.living_room", true)

	require.NoError(t, runScriptForTest(t, home, `
		assert(home.getLastKnown("light.living_room") == true)
	`))
}

func TestBindingsNotifyWithPayload(t *testing.T) {
	home := newFakeHomeAPI()

	require.NoError(t, runScriptForTest(t, home, `
		home.notify("security-alert", { zone = "front-door", severity = "high" })
	`))

	require.Equal(t, 1, home.notifyCount())
	got := home.notifications[0]
	assert.Equal(t, "security-alert", got.event)
	assert.Equal(t, "front-door", got.payload["zone"])
	assert.Equal(t, "high", got.payload["severity"])
}

func TestBindingsNotifyWithoutPayload(t *testing.T) {
	home := newFakeHomeAPI()

	require.NoError(t, runScriptForTest(t, home, `home.notify("ping")`))

	require.Equal(t, 1, home.notifyCount())
	assert.Empty(t, home.notifications[0].payload)
}

func TestBindingsErrorSurfacesToScript(t *testing.T) {
	home := newFakeHomeAPI()
	home.sensorErr = assert.AnError

	err := runScriptForTest(t, home, `
		local ok, err = pcall(function() return home.getSensor("temp.kitchen") end)
		assert(ok == false)
	`)
	assert.NoError(t, err, "script pcalls the failing binding, so DoString itself should succeed")
}

func TestBindingsUnrecoveredErrorIsIdentifiableAsBindingError(t *testing.T) {
	home := newFakeHomeAPI()
	home.sensorErr = assert.AnError

	err := runScriptForTest(t, home, `home.getSensor("temp.kitchen")`)
	require.Error(t, err)

	bindingErr := asBindingError(err)
	require.NotNil(t, bindingErr, "an unrecovered home.* failure must be identifiable as a binding error")
	assert.ErrorIs(t, bindingErr, assert.AnError)
}

func TestBindingsScriptErrorAfterRecoveredBindingErrorIsNotMisattributed(t *testing.T) {
	home := newFakeHomeAPI()
	home.sensorErr = assert.AnError

	// The script recovers from a failing binding call via pcall, then
	// raises its own, unrelated error. asBindingError must not attribute
	// that later error to the earlier, already-handled binding failure.
	err := runScriptForTest(t, home, `
		pcall(function() return home.getSensor("temp.kitchen") end)
		error("my own bug")
	`)
	require.Error(t, err)
	assert.Nil(t, asBindingError(err), "a script's own error() after a recovered binding failure must not read as a binding error")
	assert.Contains(t, err.Error(), "my own bug")
}

func TestBindingsGetAttributeConvertsNumericAndTimeTypes(t *testing.T) {
	home := newFakeHomeAPI()
	home.attributes["thermostat.hall"] = map[string]any{
		"code":      int32(7),
		"timestamp": time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}

	require.NoError(t, runScriptForTest(t, home, `
		assert(home.getAttribute("thermostat.hall", "code") > 5)
		assert(home.getAttribute("thermostat.hall", "timestamp") == "2026-01-02T03:04:05Z")
	`))
}

func TestBindingsGetAttributeUnconvertibleTypeRaisesCatchableError(t *testing.T) {
	home := newFakeHomeAPI()
	home.attributes["thermostat.hall"] = map[string]any{
		"callback": func() {},
	}

	err := runScriptForTest(t, home, `
		local ok, err = pcall(function() return home.getAttribute("thermostat.hall", "callback") end)
		assert(ok == false)
	`)
	assert.NoError(t, err, "script pcalls around the unconvertible value, so DoString itself should succeed")
}

func TestBindingsNotifySequenceTablePayloadStaysArray(t *testing.T) {
	home := newFakeHomeAPI()

	require.NoError(t, runScriptForTest(t, home, `
		home.notify("evt", { items = {1, 2, 3} })
	`))

	require.Equal(t, 1, home.notifyCount())
	items, ok := home.notifications[0].payload["items"].([]any)
	require.True(t, ok, "a Lua sequence table must convert to a Go slice, not a map")
	assert.Equal(t, []any{1.0, 2.0, 3.0}, items)
}
