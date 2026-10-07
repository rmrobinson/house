package policy

import (
	_ "embed"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	lua "github.com/yuin/gopher-lua"
)

// Test-only embeds, same rationale as battery_report_test.go: these are user
// policies registered through adminui/SavePolicy, never compiled into
// policyd itself.
//
//go:embed scripts/ups_on_battery.lua
var upsOnBatteryScript string

//go:embed scripts/ups_low_battery.lua
var upsLowBatteryScript string

//go:embed scripts/water_detected.lua
var waterDetectedScript string

func addUPS(home *fakeHomeAPI, e *Engine, id, name, room string, discharging bool, pct, mins int64) {
	home.setDeviceName(id, name)
	if room != "" {
		home.setDeviceRoom(id, room)
	}
	setUPS(home, id, discharging, pct, mins)
	e.UpdateDeviceState(id, "ups", nil)
}

func setUPS(home *fakeHomeAPI, id string, discharging bool, pct, mins int64) {
	_ = home.SetState(id, "battery", struct{}{})
	_ = home.SetState(id, "battery.state.discharging", discharging)
	_ = home.SetState(id, "battery.state.capacity_remaining_pct", pct)
	_ = home.SetState(id, "battery.state.capacity_remaining_mins", mins)
}

func addWaterSensor(home *fakeHomeAPI, e *Engine, id, name string, wet bool) {
	home.setDeviceName(id, name)
	_ = home.SetState(id, "water", struct{}{})
	_ = home.SetState(id, "water.is_active", wet)
	e.UpdateDeviceState(id, "sensor", nil)
}

func upsDischarging() DeviceClause {
	return DeviceClause{Key: "battery.state.discharging", Op: "eq", Value: true}
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

func TestUPSOnBatteryPolicyCoversAnyUPSAndNamesWhichOne(t *testing.T) {
	home := newFakeHomeAPI()
	notify := &fakeNotifyAPI{}
	e, _ := newTestEngine(t, home, WithNotifyAPI(notify))
	RegisterBuiltinConditionTypes(e)

	addUPS(home, e, "ups-a", "Office UPS", "Office", false, 100, 40)
	addUPS(home, e, "ups-b", "Rack UPS", "", false, 100, 30)
	// A non-UPS device with the same keys must never match.
	home.setDeviceName("other", "Not a UPS")
	_ = home.SetState("other", "battery.state.discharging", true)
	e.UpdateDeviceState("other", "generic", nil)

	registerAnyDevicePolicy(t, e, "ups-on-battery", upsOnBatteryScript,
		AnyDeviceParams{Kind: "ups", Clauses: []DeviceClause{upsDischarging()}})

	// First UPS drops: one email naming only it, with room and runtime.
	setUPS(home, "ups-a", true, 90, 35)
	e.UpdateDeviceState("ups-a", "ups", nil)
	require.Eventually(t, func() bool { return notify.callCount() == 1 }, time.Second, 10*time.Millisecond)
	c := notify.call(0)
	assert.Equal(t, []string{"r"}, c.recipientIDs)
	assert.Equal(t, "text/html", c.content)
	assert.Equal(t, "[POWER] Office UPS (Office) lost grid power", c.subject)
	assert.Contains(t, c.body, "90%")
	assert.Contains(t, c.body, "35 minutes")
	assert.NotContains(t, c.body, "Rack UPS")
	assert.NotContains(t, c.body, "Not a UPS")

	// Same UPS updating again: no repeat alert.
	e.UpdateDeviceState("ups-a", "ups", nil)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1, notify.callCount())

	// A second UPS joins while the first is still down: a new alert naming
	// only the new one - a plain level condition would have swallowed this.
	setUPS(home, "ups-b", true, 80, 20)
	e.UpdateDeviceState("ups-b", "ups", nil)
	require.Eventually(t, func() bool { return notify.callCount() == 2 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, "[POWER] Rack UPS lost grid power", notify.call(1).subject)
	assert.Contains(t, notify.call(1).body, "80%")
	assert.NotContains(t, notify.call(1).body, "Office UPS")
}

func TestUPSLowBatteryPolicyOnlyListsUPSesAtOrBelowThreshold(t *testing.T) {
	home := newFakeHomeAPI()
	notify := &fakeNotifyAPI{}
	e, _ := newTestEngine(t, home, WithNotifyAPI(notify))
	RegisterBuiltinConditionTypes(e)

	addUPS(home, e, "ups-a", "Office UPS", "", false, 15, 5) // low, but on mains
	addUPS(home, e, "ups-b", "Rack UPS", "", false, 100, 30)

	registerAnyDevicePolicy(t, e, "ups-low-battery", upsLowBatteryScript, AnyDeviceParams{Kind: "ups", Clauses: []DeviceClause{
		upsDischarging(),
		{Key: "battery.state.capacity_remaining_pct", Op: "lte", Value: 20},
	}})

	// On battery but healthy: no alert.
	setUPS(home, "ups-b", true, 60, 20)
	e.UpdateDeviceState("ups-b", "ups", nil)
	time.Sleep(100 * time.Millisecond)
	assert.Zero(t, notify.callCount())

	// The low UPS goes on battery: alert names it and not the healthy one.
	setUPS(home, "ups-a", true, 15, 5)
	e.UpdateDeviceState("ups-a", "ups", nil)
	require.Eventually(t, func() bool { return notify.callCount() == 1 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, "[POWER] Office UPS battery low", notify.call(0).subject)
	assert.Contains(t, notify.call(0).body, "15%")
	assert.NotContains(t, notify.call(0).body, "Rack UPS")
}

func TestWaterDetectedPolicyCoversAnySensorAndSkipsNonWaterSensors(t *testing.T) {
	home := newFakeHomeAPI()
	notify := &fakeNotifyAPI{}
	e, _ := newTestEngine(t, home, WithNotifyAPI(notify))
	RegisterBuiltinConditionTypes(e)

	addWaterSensor(home, e, "w-1", "Sump Pump Water Sensor", false)
	addWaterSensor(home, e, "w-2", "Under Sink Water Sensor", false)
	home.setDeviceName("motion", "Hall Motion") // a sensor with no water trait
	e.UpdateDeviceState("motion", "sensor", nil)

	registerAnyDevicePolicy(t, e, "water-detected", waterDetectedScript, AnyDeviceParams{
		Kind: "sensor", Clauses: []DeviceClause{{Key: "water.is_active", Op: "eq", Value: true}},
	})

	_ = home.SetState("w-1", "water.is_active", true)
	e.UpdateDeviceState("w-1", "sensor", nil)
	require.Eventually(t, func() bool { return notify.callCount() == 1 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, "[WATER] Water detected: Sump Pump Water Sensor", notify.call(0).subject)
	assert.NotContains(t, notify.call(0).body, "Under Sink")
	assert.NotContains(t, notify.call(0).body, "Hall Motion")

	_ = home.SetState("w-2", "water.is_active", true)
	e.UpdateDeviceState("w-2", "sensor", nil)
	require.Eventually(t, func() bool { return notify.callCount() == 2 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, "[WATER] Water detected: Under Sink Water Sensor", notify.call(1).subject)
}

// TestAnyDevicePoliciesSurviveGetDeviceRoomError: the room suffix is
// cosmetic and must never abort an alert (see battery_report_test.go).
func TestAnyDevicePoliciesSurviveGetDeviceRoomError(t *testing.T) {
	home := newFakeHomeAPI()
	home.setDeviceRoomErr(ErrNotImplemented)
	notify := &fakeNotifyAPI{}
	e, _ := newTestEngine(t, home, WithNotifyAPI(notify))
	RegisterBuiltinConditionTypes(e)

	addWaterSensor(home, e, "w-1", "Sump Pump Water Sensor", false)
	registerAnyDevicePolicy(t, e, "water-detected", waterDetectedScript, AnyDeviceParams{
		Kind: "sensor", Clauses: []DeviceClause{{Key: "water.is_active", Op: "eq", Value: true}},
	})

	_ = home.SetState("w-1", "water.is_active", true)
	e.UpdateDeviceState("w-1", "sensor", nil)
	require.Eventually(t, func() bool { return notify.callCount() == 1 }, time.Second, 10*time.Millisecond)
	assert.Contains(t, notify.call(0).body, "Sump Pump Water Sensor")
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
