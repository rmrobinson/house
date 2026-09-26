package policy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegisterBuiltinConditionTypesRegistersAllFour(t *testing.T) {
	e, r := newTestEngine(t, newFakeHomeAPI())
	RegisterBuiltinConditionTypes(e)

	assert.Equal(t, []string{"attribute.equals", "attribute.threshold", "event.idle-for", "schedule.daily"}, r.TypeNames())
}

func TestBuiltinConditionType_AttributeThreshold(t *testing.T) {
	home := newFakeHomeAPI()
	require.NoError(t, home.SetAttribute("sensor-1", "temp", 15.0)) // below Low
	e, _ := newTestEngine(t, home)
	RegisterBuiltinConditionTypes(e)

	require.NoError(t, e.Register(&Policy{
		ID: "test.hot",
		ConditionExpr: Use("attribute.threshold", AttributeThresholdParams{
			DeviceID: "sensor-1", Key: "temp", High: 25, Low: 20, IntervalSeconds: 1,
		}),
		Script: `home.notify("hot", nil)`,
	}))

	assert.Equal(t, 0, home.notifyCount())

	require.NoError(t, home.SetAttribute("sensor-1", "temp", 30.0)) // crosses High
	require.Eventually(t, func() bool {
		return home.notifyCount() == 1
	}, 3*time.Second, 50*time.Millisecond)
}

func TestBuiltinConditionType_AttributeEquals(t *testing.T) {
	home := newFakeHomeAPI()
	require.NoError(t, home.SetAttribute("door-1", "state", "closed"))
	e, _ := newTestEngine(t, home)
	RegisterBuiltinConditionTypes(e)

	require.NoError(t, e.Register(&Policy{
		ID: "test.door-open",
		ConditionExpr: Use("attribute.equals", AttributeEqualsParams{
			DeviceID: "door-1", Key: "state", Value: "open",
		}),
		Script: `home.notify("door-open", nil)`,
	}))

	assert.Equal(t, 0, home.notifyCount())

	// Simulates bridgehome.applyDeviceUpdate's generic per-device signal.
	require.NoError(t, home.SetAttribute("door-1", "state", "open"))
	e.Bus().Publish(Event{Topic: "device.updated.door-1"})

	require.Eventually(t, func() bool {
		return home.notifyCount() == 1
	}, time.Second, 10*time.Millisecond)
}

// TestBuiltinConditionType_EventIdleFor is the "no motion for N minutes"
// case from the condition catalog plan, with a short duration standing in
// for a real 30 minutes.
func TestBuiltinConditionType_EventIdleFor(t *testing.T) {
	home := newFakeHomeAPI()
	e, _ := newTestEngine(t, home)
	RegisterBuiltinConditionTypes(e)

	require.NoError(t, e.Register(&Policy{
		ID: "test.idle",
		ConditionExpr: Use("event.idle-for", EventIdleForParams{
			Topic: "motion.detected", DurationSeconds: 1,
		}),
		Script: `home.notify("idle", nil)`,
	}))

	// A recent event must reset the idle timer.
	e.Bus().Publish(Event{Topic: "motion.detected"})
	assert.Equal(t, 0, home.notifyCount())

	require.Eventually(t, func() bool {
		return home.notifyCount() == 1
	}, 3*time.Second, 50*time.Millisecond)
}

// TestBuiltinConditionType_ScheduleDaily only proves registration/params
// plumbing (build succeeds, TZ resolves, the right Condition type comes
// back) without waiting for a real fire - ScheduleCondition's own Start/
// pulse/reschedule timing is covered directly in condition_test.go, where
// "now" is injectable.
func TestBuiltinConditionType_ScheduleDaily(t *testing.T) {
	e, r := newTestEngine(t, newFakeHomeAPI())
	RegisterBuiltinConditionTypes(e)

	cond, err := r.build("schedule.daily", ScheduleDailyParams{
		Hour: 9, Minute: 0, Weekdays: []int{int(time.Monday)}, TZ: "America/Toronto",
	})
	require.NoError(t, err)
	assert.False(t, cond.Evaluate())

	_, ok := cond.(*ScheduleCondition)
	assert.True(t, ok)
}

func TestBuiltinConditionType_ScheduleDailyInvalidTZFallsBackToLocal(t *testing.T) {
	e, r := newTestEngine(t, newFakeHomeAPI())
	RegisterBuiltinConditionTypes(e)

	cond, err := r.build("schedule.daily", ScheduleDailyParams{Hour: 9, Minute: 0, TZ: "Not/A/Zone"})
	require.NoError(t, err)
	assert.NotNil(t, cond)
}
