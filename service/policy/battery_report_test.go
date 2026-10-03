package policy

import (
	_ "embed"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// batteryReportScript and batteryReportPolicyID are test-only: unlike
// defaults.go's sys.* scripts, the battery-report policy is a user policy
// specific to this house's actual device inventory, registered entirely as
// Lua text through adminui's policy editor and persisted in the policy
// store - it has no business being embedded in policyd's own binary. This
// embed exists solely so scripts/battery_report.lua is exercised by these
// tests; production code never references it.
//
//go:embed scripts/battery_report.lua
var batteryReportScript string

const batteryReportPolicyID = "battery-report"

// TestBatteryReportPolicyRegistersWithScheduleDaily proves
// batteryReportScript registers cleanly against the production-shaped
// condition (schedule.daily, 07:00, a house timezone) - the same
// Engine.Register call adminui's policy editor makes once this is pasted
// in there.
func TestBatteryReportPolicyRegistersWithScheduleDaily(t *testing.T) {
	e, r := newTestEngine(t, newFakeHomeAPI())
	RegisterBuiltinConditionTypes(e)

	require.NoError(t, e.Register(&Policy{
		ID:            batteryReportPolicyID,
		ConditionExpr: Use("schedule.daily", ScheduleDailyParams{Hour: 7, Minute: 0, TZ: "America/Toronto"}),
		Script:        batteryReportScript,
	}))

	info, ok := e.Policy(batteryReportPolicyID)
	require.True(t, ok)
	assert.Equal(t, batteryReportScript, info.Script)
	_ = r
}

// TestBatteryReportPolicyExecutesAndNotifies runs batteryReportScript
// through the real engine pipeline (Register -> condition fires -> trigger
// -> runScript -> notify.send), standing in for schedule.daily with a
// manualCondition - ScheduleCondition's own timing is already covered in
// condition_test.go/attribute_conditions_test.go, so this test is about the
// script's behaviour, not the clock.
//
// Fixture: a sensor at 5% (action item), a generic device at 42%
// (informational), a UPS at 3% (must be excluded - UPS is never in the
// {"sensor","generic"} kind loop), and a sensor with no battery trait at
// all (must be skipped via home.hasState).
func TestBatteryReportPolicyExecutesAndNotifies(t *testing.T) {
	home := newFakeHomeAPI()
	notify := &fakeNotifyAPI{}
	e, r := newTestEngine(t, home, WithNotifyAPI(notify))
	trigger := registerManualTrigger(t, r, "trigger")

	e.UpdateDeviceState("sensor.low", "sensor", nil)
	require.NoError(t, home.SetState("sensor.low", "battery", struct{}{}))
	require.NoError(t, home.SetState("sensor.low", "battery.state.capacity_remaining_pct", int64(5)))
	home.setDeviceName("sensor.low", "Hallway Smoke Detector")

	e.UpdateDeviceState("generic.ok", "generic", nil)
	require.NoError(t, home.SetState("generic.ok", "battery", struct{}{}))
	require.NoError(t, home.SetState("generic.ok", "battery.state.capacity_remaining_pct", int64(42)))
	home.setDeviceName("generic.ok", "Garage Remote")

	e.UpdateDeviceState("ups.main", "ups", nil)
	require.NoError(t, home.SetState("ups.main", "battery", struct{}{}))
	require.NoError(t, home.SetState("ups.main", "battery.state.capacity_remaining_pct", int64(3)))
	home.setDeviceName("ups.main", "Office UPS")

	e.UpdateDeviceState("sensor.no-battery", "sensor", nil)
	home.setDeviceName("sensor.no-battery", "Front Door Contact")

	require.NoError(t, e.Register(&Policy{
		ID:            batteryReportPolicyID,
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        batteryReportScript,
	}))

	trigger.set(true)

	require.Eventually(t, func() bool {
		return len(notify.calls) == 1
	}, time.Second, 10*time.Millisecond)

	logs := e.LogsForPolicy(batteryReportPolicyID)
	require.Len(t, logs, 1)
	assert.Equal(t, StatusSuccess, logs[0].Status)
	assert.Empty(t, logs[0].Error)

	call := notify.calls[0]
	assert.Equal(t, []string{"r"}, call.recipientIDs)
	assert.Equal(t, "Battery report", call.subject)
	assert.Equal(t, "text/html", call.content)
	assert.Contains(t, call.body, "Hallway Smoke Detector: 5%")
	assert.Contains(t, call.body, "Garage Remote: 42%")
	assert.NotContains(t, call.body, "Office UPS", "UPS devices are excluded from the battery report")
	assert.NotContains(t, call.body, "Front Door Contact", "a device with no battery trait must be skipped")
}
