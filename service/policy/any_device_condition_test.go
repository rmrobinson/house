package policy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	lua "github.com/yuin/gopher-lua"
)

// These tests cover the "devices.any-match" condition and the Lua `trigger`
// table with inline scripts. Real user policies built on them live in the
// house-config repo (policies/), not here.

// notifiedEvents returns the event names passed to home.notify, in order.
func notifiedEvents(h *fakeHomeAPI) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	events := make([]string, 0, len(h.notifications))
	for _, n := range h.notifications {
		events = append(events, n.event)
	}
	return events
}

// joinedScript reports every device that just started matching as "joined:<id>".
const joinedScript = `
	for _, id in ipairs(trigger.device_ids) do
		home.notify("joined:" .. id, nil)
	end`

func setBatteryState(home *fakeHomeAPI, id string, discharging bool, pct int64) {
	_ = home.SetState(id, "battery.state.discharging", discharging)
	_ = home.SetState(id, "battery.state.capacity_remaining_pct", pct)
}

func addWaterSensor(home *fakeHomeAPI, e *Engine, id, name string, wet bool) {
	home.setDeviceName(id, name)
	_ = home.SetState(id, "water", struct{}{})
	_ = home.SetState(id, "water.is_active", wet)
	e.UpdateDeviceState(id, "sensor", nil)
}

func registerAnyDevicePolicy(t *testing.T, e *Engine, id, script string, p AnyDeviceParams) {
	t.Helper()
	require.NoError(t, e.Register(&Policy{
		ID:               id,
		ConditionExpr:    Use("devices.any-match", p),
		Script:           script,
		OnConditionFalse: Complete,
	}))
}

// TestAnyDeviceConditionTriggersOnlyForDevicesThatNewlyMatch: one policy covers
// every device of the kind; a device already matching isn't repeated, and a
// second one joining while the first still matches triggers again, naming
// only the new one (a plain level condition would swallow that).
func TestAnyDeviceConditionTriggersOnlyForDevicesThatNewlyMatch(t *testing.T) {
	home := newFakeHomeAPI()
	e, _ := newTestEngine(t, home)
	RegisterBuiltinConditionTypes(e)

	for _, id := range []string{"ups-a", "ups-b"} {
		setBatteryState(home, id, false, 100)
		e.UpdateDeviceState(id, "ups", nil)
	}
	// A device of another kind with the same keys must never match.
	setBatteryState(home, "other", true, 100)
	e.UpdateDeviceState("other", "generic", nil)

	registerAnyDevicePolicy(t, e, "p", joinedScript, AnyDeviceParams{
		Kind:    "ups",
		Clauses: []DeviceClause{{Key: "battery.state.discharging", Op: "eq", Value: true}},
	})

	setBatteryState(home, "ups-a", true, 90)
	e.UpdateDeviceState("ups-a", "ups", nil)
	require.Eventually(t, func() bool { return home.notifyCount() == 1 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"joined:ups-a"}, notifiedEvents(home))

	// Same device updating again: no repeat.
	e.UpdateDeviceState("ups-a", "ups", nil)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1, home.notifyCount())

	setBatteryState(home, "ups-b", true, 80)
	e.UpdateDeviceState("ups-b", "ups", nil)
	require.Eventually(t, func() bool { return home.notifyCount() == 2 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"joined:ups-a", "joined:ups-b"}, notifiedEvents(home))
}

// TestAnyDeviceConditionRequiresEveryClause: a device matches only when all
// clauses hold.
func TestAnyDeviceConditionRequiresEveryClause(t *testing.T) {
	home := newFakeHomeAPI()
	e, _ := newTestEngine(t, home)
	RegisterBuiltinConditionTypes(e)

	setBatteryState(home, "ups-a", false, 15) // low, but on mains
	e.UpdateDeviceState("ups-a", "ups", nil)
	setBatteryState(home, "ups-b", false, 100)
	e.UpdateDeviceState("ups-b", "ups", nil)

	registerAnyDevicePolicy(t, e, "p", joinedScript, AnyDeviceParams{Kind: "ups", Clauses: []DeviceClause{
		{Key: "battery.state.discharging", Op: "eq", Value: true},
		{Key: "battery.state.capacity_remaining_pct", Op: "lte", Value: 20},
	}})

	// On battery but healthy: no trigger.
	setBatteryState(home, "ups-b", true, 60)
	e.UpdateDeviceState("ups-b", "ups", nil)
	time.Sleep(100 * time.Millisecond)
	assert.Zero(t, home.notifyCount())

	// The low one goes on battery: only it triggers.
	setBatteryState(home, "ups-a", true, 15)
	e.UpdateDeviceState("ups-a", "ups", nil)
	require.Eventually(t, func() bool { return home.notifyCount() == 1 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"joined:ups-a"}, notifiedEvents(home))
}

// TestAnyDeviceConditionIgnoresDevicesLackingTheAttribute: a sensor with no
// water trait fails the clause (GetState errors) and never matches.
func TestAnyDeviceConditionIgnoresDevicesLackingTheAttribute(t *testing.T) {
	home := newFakeHomeAPI()
	e, _ := newTestEngine(t, home)
	RegisterBuiltinConditionTypes(e)

	addWaterSensor(home, e, "w-1", "Sump", false)
	home.setDeviceName("motion", "Hall Motion")
	e.UpdateDeviceState("motion", "sensor", nil)

	registerAnyDevicePolicy(t, e, "p", joinedScript, AnyDeviceParams{
		Kind: "sensor", Clauses: []DeviceClause{{Key: "water.is_active", Op: "eq", Value: true}},
	})

	e.UpdateDeviceState("motion", "sensor", nil)
	_ = home.SetState("w-1", "water.is_active", true)
	e.UpdateDeviceState("w-1", "sensor", nil)
	require.Eventually(t, func() bool { return home.notifyCount() == 1 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"joined:w-1"}, notifiedEvents(home))
}

func TestAnyDeviceConditionClearsWhenNoDeviceMatches(t *testing.T) {
	home := newFakeHomeAPI()
	e, _ := newTestEngine(t, home)
	RegisterBuiltinConditionTypes(e)

	addWaterSensor(home, e, "w-1", "Sensor", false)
	registerAnyDevicePolicy(t, e, "p", `home.notify("wet", nil)`, AnyDeviceParams{
		Kind: "sensor", Clauses: []DeviceClause{{Key: "water.is_active", Op: "eq", Value: true}},
	})

	_ = home.SetState("w-1", "water.is_active", true)
	e.UpdateDeviceState("w-1", "sensor", nil)
	require.Eventually(t, func() bool { return home.notifyCount() == 1 }, time.Second, 10*time.Millisecond)

	// Dries out, then wets again: a fresh transition, so a second trigger.
	_ = home.SetState("w-1", "water.is_active", false)
	e.UpdateDeviceState("w-1", "sensor", nil)
	// The condition re-reads state when it processes an event, so give it
	// time to observe the dry state before wetting again.
	time.Sleep(100 * time.Millisecond)
	_ = home.SetState("w-1", "water.is_active", true)
	e.UpdateDeviceState("w-1", "sensor", nil)
	require.Eventually(t, func() bool { return home.notifyCount() == 2 }, time.Second, 10*time.Millisecond)
}

func TestTriggerTableEmptyWithoutContext(t *testing.T) {
	require.NoError(t, runScriptForTestWithTrigger(t, nil, `
		assert(#trigger.device_ids == 0)
		assert(trigger.device_id == nil)
	`))
	require.NoError(t, runScriptForTestWithTrigger(t, []string{"a", "b"}, `
		assert(#trigger.device_ids == 2)
		assert(trigger.device_id == "a")
	`))
}

func runScriptForTestWithTrigger(t *testing.T, ids []string, script string) error {
	t.Helper()
	L := lua.NewState()
	defer L.Close()
	registerTriggerTable(L, ids)
	return L.DoString(script)
}

// TestAnyDeviceConditionReportsSimultaneousJoiners: two devices matching in
// the same re-evaluation produce one trigger carrying both ids.
func TestAnyDeviceConditionReportsSimultaneousJoiners(t *testing.T) {
	home := newFakeHomeAPI()
	e, _ := newTestEngine(t, home)
	RegisterBuiltinConditionTypes(e)

	addWaterSensor(home, e, "w-1", "One", false)
	addWaterSensor(home, e, "w-2", "Two", false)
	registerAnyDevicePolicy(t, e, "p", `
		assert(#trigger.device_ids == 2 and trigger.device_ids[1] == "w-1" and trigger.device_ids[2] == "w-2")
		home.notify("both", nil)`, AnyDeviceParams{
		Kind: "sensor", Clauses: []DeviceClause{{Key: "water.is_active", Op: "eq", Value: true}},
	})

	_ = home.SetState("w-1", "water.is_active", true)
	_ = home.SetState("w-2", "water.is_active", true)
	e.UpdateDeviceState("w-1", "sensor", nil)

	require.Eventually(t, func() bool { return home.notifyCount() == 1 }, time.Second, 10*time.Millisecond)
}
