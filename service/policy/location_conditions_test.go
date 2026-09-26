package policy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestRegisterLocationConditionTypesRegistersAllThree(t *testing.T) {
	e, r := newTestEngine(t, newFakeHomeAPI())
	RegisterLocationConditionTypes(e)

	assert.Equal(t, []string{"schedule.date-range", "schedule.daylight", "schedule.sun-event"}, r.TypeNames())
}

// TestConditionType_ScheduleDateRange proves registration/params plumbing
// (build succeeds, TZ resolves, the right Condition type comes back)
// without waiting for a real midnight rollover - DateRangeCondition's own
// Start/matches/reschedule timing is covered directly in condition_test.go.
func TestConditionType_ScheduleDateRange(t *testing.T) {
	e, r := newTestEngine(t, newFakeHomeAPI())
	RegisterLocationConditionTypes(e)

	cond, err := r.build("schedule.date-range", DateRangeParams{
		StartMonth: 12, StartDay: 25, EndMonth: 12, EndDay: 25, TZ: "America/Toronto",
	})
	require.NoError(t, err)

	dr, ok := cond.(*DateRangeCondition)
	require.True(t, ok)
	assert.Equal(t, monthDay{12, 25}, dr.start)
	assert.Equal(t, monthDay{12, 25}, dr.end)
}

func TestConditionType_ScheduleDateRangeUsesHouseTimezoneWhenTZEmpty(t *testing.T) {
	home := newFakeHomeAPI()
	require.NoError(t, home.SetHouseState("location.timezone", "America/Toronto"))
	e, r := newTestEngine(t, home)
	RegisterLocationConditionTypes(e)

	cond, err := r.build("schedule.date-range", DateRangeParams{StartMonth: 12, StartDay: 1, EndMonth: 12, EndDay: 31})
	require.NoError(t, err)

	dr, ok := cond.(*DateRangeCondition)
	require.True(t, ok)
	want, err := time.LoadLocation("America/Toronto")
	require.NoError(t, err)
	assert.Equal(t, want, dr.loc)
}

func TestConditionType_ScheduleDateRangeNormalizesOutOfRangeMonthDay(t *testing.T) {
	core, recorded := observer.New(zap.WarnLevel)
	e, r := newTestEngine(t, newFakeHomeAPI())
	e.logger = zap.New(core)
	RegisterLocationConditionTypes(e)

	cond, err := r.build("schedule.date-range", DateRangeParams{StartMonth: 13, StartDay: 32, EndMonth: 12, EndDay: 25})
	require.NoError(t, err)

	dr, ok := cond.(*DateRangeCondition)
	require.True(t, ok)
	assert.Equal(t, 1, dr.start.month)
	assert.Equal(t, 1, dr.start.day)

	entries := recorded.All()
	require.Len(t, entries, 1)
	assert.Contains(t, entries[0].Message, "out of range")
}

// TestConditionType_ScheduleDateRangeEvaluatesCurrentMembership proves the
// registered condition type builds and computes real membership right now,
// without waiting for an actual local-midnight transition (DateRangeCondition
// only calls onChange on that edge - see its Start's doc comment - so a
// range that's already true when Register runs, like the always-on one
// here, never fires a script; that's exercised directly against
// DateRangeCondition.Start in condition_test.go instead of through a full
// policy).
func TestConditionType_ScheduleDateRangeEvaluatesCurrentMembership(t *testing.T) {
	e, r := newTestEngine(t, newFakeHomeAPI())
	RegisterLocationConditionTypes(e)

	cond, err := r.build("schedule.date-range", DateRangeParams{
		StartMonth: 1, StartDay: 1, EndMonth: 12, EndDay: 31, // always on
	})
	require.NoError(t, err)

	cond.Start(t.Context(), func(bool) {})
	require.Eventually(t, cond.Evaluate, time.Second, time.Millisecond)
}

func TestConditionType_ScheduleSunEventDefaultsToSunriseOnInvalidEvent(t *testing.T) {
	core, recorded := observer.New(zap.WarnLevel)
	e, r := newTestEngine(t, newFakeHomeAPI())
	e.logger = zap.New(core)
	RegisterLocationConditionTypes(e)

	cond, err := r.build("schedule.sun-event", SunEventParams{Event: "high-noon"})
	require.NoError(t, err)

	se, ok := cond.(*SunEventCondition)
	require.True(t, ok)
	assert.False(t, se.sunset)

	entries := recorded.All()
	require.Len(t, entries, 1)
	assert.Contains(t, entries[0].Message, "defaulting to sunrise")
}

func TestConditionType_ScheduleDaylightBuilds(t *testing.T) {
	e, r := newTestEngine(t, newFakeHomeAPI())
	RegisterLocationConditionTypes(e)

	cond, err := r.build("schedule.daylight", SunWindowParams{TZ: "UTC"})
	require.NoError(t, err)
	_, ok := cond.(*SunWindowCondition)
	assert.True(t, ok)
}

func TestResolveTZPrefersOverrideThenHouseStateThenLocal(t *testing.T) {
	home := newFakeHomeAPI()
	logger := zap.NewNop()

	// Neither set: falls back to local.
	assert.Equal(t, time.Local, resolveTZ(home, "", logger))

	// House state set, no override: uses house state.
	require.NoError(t, home.SetHouseState("location.timezone", "America/Toronto"))
	want, err := time.LoadLocation("America/Toronto")
	require.NoError(t, err)
	assert.Equal(t, want, resolveTZ(home, "", logger))

	// Override takes priority over house state.
	want2, err := time.LoadLocation("UTC")
	require.NoError(t, err)
	assert.Equal(t, want2, resolveTZ(home, "UTC", logger))
}

func TestResolveTZInvalidFallsBackToLocal(t *testing.T) {
	core, recorded := observer.New(zap.WarnLevel)
	logger := zap.New(core)

	assert.Equal(t, time.Local, resolveTZ(newFakeHomeAPI(), "Not/A/Zone", logger))
	entries := recorded.All()
	require.Len(t, entries, 1)
	assert.Contains(t, entries[0].Message, "invalid timezone")
}

// TestLocationReaderHoldsLastKnownValueOnFailedRead mirrors
// attribute.threshold's own "hold the last value" test for a failed
// GetAttribute: once a locationReader has a good reading, a subsequent
// failure (here, GetHouseState answering with a non-numeric value) must not
// zero out the sun calculation in flight.
func TestLocationReaderHoldsLastKnownValueOnFailedRead(t *testing.T) {
	home := newFakeHomeAPI()
	require.NoError(t, home.SetHouseState("location.latitude", 43.7))
	require.NoError(t, home.SetHouseState("location.longitude", -79.4))

	r := newLocationReader(home, zap.NewNop())

	lat, lon, ok := r.read()
	require.True(t, ok)
	assert.Equal(t, 43.7, lat)
	assert.Equal(t, -79.4, lon)

	// Corrupt the reading: a non-numeric value fails toFloat64.
	require.NoError(t, home.SetHouseState("location.latitude", "not a number"))

	lat, lon, ok = r.read()
	require.True(t, ok, "must still report ok, holding the last good reading")
	assert.Equal(t, 43.7, lat)
	assert.Equal(t, -79.4, lon)
}

func TestLocationReaderReportsNotOkWithNoPriorReading(t *testing.T) {
	home := newFakeHomeAPI() // no location.* keys set
	r := newLocationReader(home, zap.NewNop())

	_, _, ok := r.read()
	assert.False(t, ok)
}
