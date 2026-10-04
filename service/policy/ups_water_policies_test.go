package policy

import (
	_ "embed"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test-only embeds, same rationale as battery_report_test.go: these are user
// policies registered through adminui/SavePolicy with DEVICE_ID replaced per
// device, never compiled into policyd itself.
//
//go:embed scripts/ups_on_battery.lua
var upsOnBatteryScript string

//go:embed scripts/ups_low_battery.lua
var upsLowBatteryScript string

//go:embed scripts/water_detected.lua
var waterDetectedScript string

const testUPSID = "ups-1"

func forDevice(script, id string) string {
	return strings.ReplaceAll(script, "DEVICE_ID", id)
}

func newUPSWaterHome() *fakeHomeAPI {
	home := newFakeHomeAPI()
	home.setDeviceName(testUPSID, "Office UPS")
	home.setDeviceRoom(testUPSID, "Office")
	_ = home.SetState(testUPSID, "battery.state.discharging", false)
	_ = home.SetState(testUPSID, "battery.state.capacity_remaining_pct", int64(100))
	_ = home.SetState(testUPSID, "battery.state.capacity_remaining_mins", int64(40))
	home.setDeviceName("water-1", "Sump Pump Water Sensor")
	_ = home.SetState("water-1", "water.is_active", false)
	return home
}

func upsOnBatteryExpr(id string) ConditionExpr {
	return Use("attribute.equals", AttributeEqualsParams{DeviceID: id, Key: "battery.state.discharging", Value: true})
}

func upsLowBatteryExpr(id string) ConditionExpr {
	return ExprAnd(
		upsOnBatteryExpr(id),
		Use("attribute.threshold", AttributeThresholdParams{
			DeviceID: id, Key: "battery.state.capacity_remaining_pct", High: 25, Low: 20, Falling: true,
		}),
	)
}

func waterDetectedExpr(id string) ConditionExpr {
	return Use("attribute.equals", AttributeEqualsParams{DeviceID: id, Key: "water.is_active", Value: true})
}

func registerUPSWaterPolicies(t *testing.T, e *Engine) {
	t.Helper()
	RegisterBuiltinConditionTypes(e)
	require.NoError(t, e.Register(&Policy{ID: "ups-on-battery", ConditionExpr: upsOnBatteryExpr(testUPSID), Script: forDevice(upsOnBatteryScript, testUPSID)}))
	require.NoError(t, e.Register(&Policy{ID: "ups-low-battery", ConditionExpr: upsLowBatteryExpr(testUPSID), Script: forDevice(upsLowBatteryScript, testUPSID)}))
	require.NoError(t, e.Register(&Policy{ID: "water-detected", ConditionExpr: waterDetectedExpr("water-1"), Script: forDevice(waterDetectedScript, "water-1")}))
}

func TestUPSOnBatteryPolicyNotifies(t *testing.T) {
	home := newUPSWaterHome()
	notify := &fakeNotifyAPI{}
	e, _ := newTestEngine(t, home, WithNotifyAPI(notify))
	registerUPSWaterPolicies(t, e)

	require.NoError(t, home.SetState(testUPSID, "battery.state.discharging", true))
	require.NoError(t, home.SetState(testUPSID, "battery.state.capacity_remaining_pct", int64(90)))
	e.Bus().Publish(Event{Topic: "device.updated." + testUPSID})

	require.Eventually(t, func() bool { return len(notify.calls) == 1 }, time.Second, 10*time.Millisecond)

	call := notify.calls[0]
	assert.Equal(t, []string{"r"}, call.recipientIDs)
	assert.Equal(t, "[POWER] Office UPS (Office) lost grid power", call.subject)
	assert.Equal(t, "text/html", call.content)
	assert.Contains(t, call.body, "90%")
	assert.Contains(t, call.body, "40 minutes")

	// Stays on battery: another device update must not re-alert.
	e.Bus().Publish(Event{Topic: "device.updated." + testUPSID})
	time.Sleep(100 * time.Millisecond)
	assert.Len(t, notify.calls, 1)
}

func TestUPSLowBatteryPolicyFiresOnlyWhileDischargingBelowThreshold(t *testing.T) {
	home := newUPSWaterHome()
	notify := &fakeNotifyAPI{}
	e, _ := newTestEngine(t, home, WithNotifyAPI(notify))
	registerUPSWaterPolicies(t, e)

	// Low charge but on mains (still charging): not an alert.
	require.NoError(t, home.SetState(testUPSID, "battery.state.capacity_remaining_pct", int64(15)))
	e.Bus().Publish(Event{Topic: "device.updated." + testUPSID})
	time.Sleep(100 * time.Millisecond)
	for _, c := range notify.calls {
		assert.NotContains(t, c.subject, "battery low")
	}

	// Goes on battery while already at 15%: now the low-battery alert fires.
	require.NoError(t, home.SetState(testUPSID, "battery.state.discharging", true))
	e.Bus().Publish(Event{Topic: "device.updated." + testUPSID})

	require.Eventually(t, func() bool {
		for _, c := range notify.calls {
			if c.subject == "[POWER] Office UPS (Office) battery low" {
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond)
}

func TestWaterDetectedPolicyNotifies(t *testing.T) {
	home := newUPSWaterHome()
	notify := &fakeNotifyAPI{}
	e, _ := newTestEngine(t, home, WithNotifyAPI(notify))
	registerUPSWaterPolicies(t, e)

	require.NoError(t, home.SetState("water-1", "water.is_active", true))
	e.Bus().Publish(Event{Topic: "device.updated.water-1"})

	require.Eventually(t, func() bool { return len(notify.calls) == 1 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"r"}, notify.calls[0].recipientIDs)
	assert.Equal(t, "[WATER] Water detected: Sump Pump Water Sensor", notify.calls[0].subject)
}

// TestUPSWaterPoliciesSurviveGetDeviceRoomError: the room suffix is cosmetic
// and must never abort an alert (see battery_report_test.go).
func TestUPSWaterPoliciesSurviveGetDeviceRoomError(t *testing.T) {
	home := newUPSWaterHome()
	home.setDeviceRoomErr(ErrNotImplemented)
	notify := &fakeNotifyAPI{}
	e, _ := newTestEngine(t, home, WithNotifyAPI(notify))
	registerUPSWaterPolicies(t, e)

	require.NoError(t, home.SetState("water-1", "water.is_active", true))
	e.Bus().Publish(Event{Topic: "device.updated.water-1"})

	require.Eventually(t, func() bool { return len(notify.calls) == 1 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, "[WATER] Water detected: Sump Pump Water Sensor", notify.calls[0].subject)
}
