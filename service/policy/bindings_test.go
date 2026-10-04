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
	return runScriptForTestWithDevices(t, home, nil, script)
}

// runScriptForTestWithDevices is runScriptForTest with control over
// home.findDevices' backing data, for tests that exercise it directly.
func runScriptForTestWithDevices(t *testing.T, home HomeAPI, devicesOfKind func(kind string) []string, script string) error {
	t.Helper()

	L := lua.NewState()
	defer L.Close()

	if devicesOfKind == nil {
		devicesOfKind = func(string) []string { return nil }
	}
	registerHomeTable(L, home, devicesOfKind)

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

func TestBindingsStateRoundTrip(t *testing.T) {
	home := newFakeHomeAPI()

	require.NoError(t, runScriptForTest(t, home, `
		home.setState("thermostat.hall", "mode", "heat")
		assert(home.getState("thermostat.hall", "mode") == "heat")
	`))

	v, err := home.GetState("thermostat.hall", "mode")
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

func TestBindingsGetDeviceName(t *testing.T) {
	home := newFakeHomeAPI()
	home.setDeviceName("light.kitchen", "Kitchen Light")

	require.NoError(t, runScriptForTest(t, home, `
		assert(home.getDeviceName("light.kitchen") == "Kitchen Light")
		assert(home.getDeviceName("light.unnamed") == "light.unnamed")
	`))
}

func TestBindingsGetDeviceRoom(t *testing.T) {
	home := newFakeHomeAPI()
	home.setDeviceRoom("light.kitchen", "Kitchen")

	require.NoError(t, runScriptForTest(t, home, `
		assert(home.getDeviceRoom("light.kitchen") == "Kitchen")
		assert(home.getDeviceRoom("light.unlinked") == "")
	`))
}

func TestBindingsHasState(t *testing.T) {
	home := newFakeHomeAPI()
	require.NoError(t, home.SetState("sensor.hall", "battery", true))

	require.NoError(t, runScriptForTest(t, home, `
		assert(home.hasState("sensor.hall", "battery") == true)
		assert(home.hasState("sensor.hall", "no_such_key") == false)
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

func TestBindingsGetStateConvertsNumericAndTimeTypes(t *testing.T) {
	home := newFakeHomeAPI()
	home.attributes["thermostat.hall"] = map[string]any{
		"code":      int32(7),
		"timestamp": time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}

	require.NoError(t, runScriptForTest(t, home, `
		assert(home.getState("thermostat.hall", "code") > 5)
		assert(home.getState("thermostat.hall", "timestamp") == "2026-01-02T03:04:05Z")
	`))
}

func TestBindingsGetStateUnconvertibleTypeRaisesCatchableError(t *testing.T) {
	home := newFakeHomeAPI()
	home.attributes["thermostat.hall"] = map[string]any{
		"callback": func() {},
	}

	err := runScriptForTest(t, home, `
		local ok, err = pcall(function() return home.getState("thermostat.hall", "callback") end)
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

func TestBindingsFindDevicesReturnsCacheBackedList(t *testing.T) {
	home := newFakeHomeAPI()
	devicesOfKind := func(kind string) []string {
		if kind == "light" {
			return []string{"light.kitchen", "light.porch"}
		}
		return nil
	}

	require.NoError(t, runScriptForTestWithDevices(t, home, devicesOfKind, `
		local lights = home.findDevices("light")
		assert(#lights == 2)
		assert(lights[1] == "light.kitchen")
		assert(lights[2] == "light.porch")

		local sensors = home.findDevices("sensor")
		assert(#sensors == 0)
	`))
}

func TestLocationHomeAPIAnswersLocationKeysAndDelegatesOthers(t *testing.T) {
	inner := newFakeHomeAPI()
	require.NoError(t, inner.SetHouseState("occupancy", "away"))

	home := NewLocationHomeAPI(inner, 43.7, -79.4, "America/Toronto")

	lat, err := home.GetHouseState("location.latitude")
	require.NoError(t, err)
	assert.Equal(t, 43.7, lat)

	lon, err := home.GetHouseState("location.longitude")
	require.NoError(t, err)
	assert.Equal(t, -79.4, lon)

	tz, err := home.GetHouseState("location.timezone")
	require.NoError(t, err)
	assert.Equal(t, "America/Toronto", tz)

	// Any other key falls through to the wrapped HomeAPI unchanged.
	occupancy, err := home.GetHouseState("occupancy")
	require.NoError(t, err)
	assert.Equal(t, "away", occupancy)
}

// TestLocationHomeAPIEmptyTZFallsThroughToWrapped covers an empty tz
// argument: "location.timezone" isn't answered locally, so whatever the
// wrapped HomeAPI has for that key (nothing, for fakeHomeAPI) comes back
// instead - not an empty string masquerading as a real answer.
func TestLocationHomeAPIEmptyTZFallsThroughToWrapped(t *testing.T) {
	inner := newFakeHomeAPI()
	home := NewLocationHomeAPI(inner, 43.7, -79.4, "")

	tz, err := home.GetHouseState("location.timezone")
	require.NoError(t, err)
	assert.Nil(t, tz)
}
