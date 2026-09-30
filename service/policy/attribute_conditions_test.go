package policy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestRegisterBuiltinConditionTypesRegistersAllFour(t *testing.T) {
	e, r := newTestEngine(t, newFakeHomeAPI())
	RegisterBuiltinConditionTypes(e)

	assert.Equal(t, []string{"attribute.equals", "attribute.threshold", "event.idle-for", "schedule.daily"}, r.TypeNames())
}

func TestBuiltinConditionType_AttributeThreshold(t *testing.T) {
	home := newFakeHomeAPI()
	require.NoError(t, home.SetState("sensor-1", "temp", 15.0)) // below Low
	e, _ := newTestEngine(t, home)
	RegisterBuiltinConditionTypes(e)

	require.NoError(t, e.Register(&Policy{
		ID: "test.hot",
		ConditionExpr: Use("attribute.threshold", AttributeThresholdParams{
			DeviceID: "sensor-1", Key: "temp", High: 25, Low: 20,
		}),
		Script: `home.notify("hot", nil)`,
	}))

	assert.Equal(t, 0, home.notifyCount())

	// attribute.threshold is event-driven (see condition.go's
	// HysteresisPredicateCondition), not polled: it only re-reads on the
	// device's own "device.updated.<id>" signal.
	require.NoError(t, home.SetState("sensor-1", "temp", 30.0)) // crosses High
	e.Bus().Publish(Event{Topic: "device.updated.sensor-1"})

	require.Eventually(t, func() bool {
		return home.notifyCount() == 1
	}, time.Second, 10*time.Millisecond)
}

// TestBuiltinConditionType_AttributeThresholdFalling exercises Falling: the
// "drops below a threshold" shape (a cold-weather or low-battery alarm)
// that's the mirror image of the default "rises above" shape.
func TestBuiltinConditionType_AttributeThresholdFalling(t *testing.T) {
	home := newFakeHomeAPI()
	require.NoError(t, home.SetState("sensor-1", "temp", 0.0)) // well above Low
	e, _ := newTestEngine(t, home)
	RegisterBuiltinConditionTypes(e)

	require.NoError(t, e.Register(&Policy{
		ID: "test.cold",
		ConditionExpr: Use("attribute.threshold", AttributeThresholdParams{
			DeviceID: "sensor-1", Key: "temp", High: -14, Low: -15, Falling: true,
		}),
		Script: `home.notify("cold", nil)`,
	}))

	assert.Equal(t, 0, home.notifyCount())

	// Staying above the recovery threshold must not fire.
	require.NoError(t, home.SetState("sensor-1", "temp", -10.0))
	e.Bus().Publish(Event{Topic: "device.updated.sensor-1"})
	assert.Equal(t, 0, home.notifyCount())

	// Falls to/below Low: this is the alarm edge when Falling is set.
	require.NoError(t, home.SetState("sensor-1", "temp", -16.0))
	e.Bus().Publish(Event{Topic: "device.updated.sensor-1"})
	require.Eventually(t, func() bool {
		return home.notifyCount() == 1
	}, time.Second, 10*time.Millisecond)
}

// TestBuiltinConditionType_AttributeThresholdSwapsInvertedHighLow covers the
// finding-D fix: High < Low (an easy authoring mistake) is corrected rather
// than left to silently oscillate every re-evaluation.
func TestBuiltinConditionType_AttributeThresholdSwapsInvertedHighLow(t *testing.T) {
	home := newFakeHomeAPI()
	require.NoError(t, home.SetState("sensor-1", "temp", 15.0))
	core, recorded := observer.New(zap.WarnLevel)
	e, _ := newTestEngine(t, home)
	e.logger = zap.New(core)
	RegisterBuiltinConditionTypes(e)

	require.NoError(t, e.Register(&Policy{
		ID: "test.swapped",
		ConditionExpr: Use("attribute.threshold", AttributeThresholdParams{
			DeviceID: "sensor-1", Key: "temp", High: 20, Low: 25, // backwards
		}),
		Script: `home.notify("hot", nil)`,
	}))

	entries := recorded.All()
	require.Len(t, entries, 1)
	assert.Contains(t, entries[0].Message, "Low > High")

	require.NoError(t, home.SetState("sensor-1", "temp", 30.0))
	e.Bus().Publish(Event{Topic: "device.updated.sensor-1"})
	require.Eventually(t, func() bool {
		return home.notifyCount() == 1
	}, time.Second, 10*time.Millisecond)
}

func TestBuiltinConditionType_AttributeEquals(t *testing.T) {
	home := newFakeHomeAPI()
	require.NoError(t, home.SetState("door-1", "state", "closed"))
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
	require.NoError(t, home.SetState("door-1", "state", "open"))
	e.Bus().Publish(Event{Topic: "device.updated.door-1"})

	require.Eventually(t, func() bool {
		return home.notifyCount() == 1
	}, time.Second, 10*time.Millisecond)
}

// TestBuiltinConditionType_AttributeEqualsSurvivesPersistenceRoundTripForNumericValue
// is finding B's regression test: a numeric Value must still match after
// going through exactly what store.go's SavePolicy/LoadPersistedPolicies do
// (MarshalConditionExpr -> JSON -> UnmarshalConditionExpr), even though that
// round-trip turns the authored int64 into a JSON number that comes back as
// float64.
func TestBuiltinConditionType_AttributeEqualsSurvivesPersistenceRoundTripForNumericValue(t *testing.T) {
	home := newFakeHomeAPI()
	require.NoError(t, home.SetState("thermostat-1", "mode", int64(1))) // not yet HEAT
	e, r := newTestEngine(t, home)
	RegisterBuiltinConditionTypes(e)

	expr := Use("attribute.equals", AttributeEqualsParams{
		DeviceID: "thermostat-1", Key: "mode", Value: int64(2), // e.g. HEAT
	})

	data, err := MarshalConditionExpr(expr)
	require.NoError(t, err)
	reloaded, err := UnmarshalConditionExpr(data, r)
	require.NoError(t, err)

	require.NoError(t, e.Register(&Policy{
		ID:            "test.mode-heat",
		ConditionExpr: reloaded,
		Script:        `home.notify("heat", nil)`,
	}))

	assert.Equal(t, 0, home.notifyCount())

	require.NoError(t, home.SetState("thermostat-1", "mode", int64(2)))
	e.Bus().Publish(Event{Topic: "device.updated.thermostat-1"})
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

// TestBuiltinConditionType_EventIdleForDefaultsWhenDurationNotPositive covers
// finding D: a non-positive DurationSeconds is warned about and given a safe
// fallback instead of producing an already-fired/zero timer.
func TestBuiltinConditionType_EventIdleForDefaultsWhenDurationNotPositive(t *testing.T) {
	home := newFakeHomeAPI()
	core, recorded := observer.New(zap.WarnLevel)
	e, _ := newTestEngine(t, home)
	e.logger = zap.New(core)
	RegisterBuiltinConditionTypes(e)

	require.NoError(t, e.Register(&Policy{
		ID: "test.idle-default",
		ConditionExpr: Use("event.idle-for", EventIdleForParams{
			Topic: "motion.detected", DurationSeconds: 0,
		}),
		Script: `home.notify("idle", nil)`,
	}))

	entries := recorded.All()
	require.Len(t, entries, 1)
	assert.Contains(t, entries[0].Message, "DurationSeconds must be positive")

	// The fallback duration is ~1s; give it real headroom rather than racing it.
	require.Eventually(t, func() bool {
		return home.notifyCount() == 1
	}, 3*time.Second, 10*time.Millisecond)
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

// TestBuiltinConditionType_ScheduleDailyNormalizesOutOfRangeHourMinute covers
// finding D: an Hour/Minute outside their valid ranges is warned about and
// normalized instead of silently rolling into a different day via
// time.Date's own overflow handling.
func TestBuiltinConditionType_ScheduleDailyNormalizesOutOfRangeHourMinute(t *testing.T) {
	core, recorded := observer.New(zap.WarnLevel)
	e, r := newTestEngine(t, newFakeHomeAPI())
	e.logger = zap.New(core)
	RegisterBuiltinConditionTypes(e)

	cond, err := r.build("schedule.daily", ScheduleDailyParams{Hour: 25, Minute: 61})
	require.NoError(t, err)
	sc, ok := cond.(*ScheduleCondition)
	require.True(t, ok)
	assert.Equal(t, 1, sc.hour)
	assert.Equal(t, 1, sc.minute)

	entries := recorded.All()
	require.Len(t, entries, 1)
	assert.Contains(t, entries[0].Message, "out of range")
}

func TestToFloat64(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want float64
	}{
		{"float64", float64(1.5), 1.5},
		{"float32", float32(2.5), 2.5},
		{"int64", int64(3), 3},
		{"int32", int32(4), 4},
		{"int", int(5), 5},
		{"uint64", uint64(6), 6},
		{"uint32", uint32(7), 7},
		{"bool true", true, 1},
		{"bool false", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := toFloat64(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	_, err := toFloat64("not a number")
	assert.Error(t, err)
}
