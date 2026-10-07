package policy

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func waitForChange(t *testing.T, changes <-chan bool, want bool) {
	t.Helper()
	select {
	case got := <-changes:
		assert.Equal(t, want, got)
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for onChange(%v)", want)
	}
}

func assertNoChange(t *testing.T, changes <-chan bool) {
	t.Helper()
	select {
	case got := <-changes:
		t.Fatalf("unexpected onChange(%v)", got)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestPollingConditionFiresOnlyOnTransition(t *testing.T) {
	var value atomic.Bool
	changes := make(chan bool, 8)

	cond := NewPollingCondition(value.Load, 10*time.Millisecond)

	ctx := t.Context()

	cond.Start(ctx, func(v bool) { changes <- v })

	// No transition yet: baseline eval on Start must not fire onChange.
	assertNoChange(t, changes)

	value.Store(true)
	waitForChange(t, changes, true)
	assert.True(t, cond.Evaluate())

	// Repeated identical ticks must not re-fire.
	assertNoChange(t, changes)

	value.Store(false)
	waitForChange(t, changes, false)
}

func TestHysteresisPollingConditionBandBehaviour(t *testing.T) {
	var value atomic.Value
	value.Store(20.0)
	read := func() float64 { return value.Load().(float64) }

	changes := make(chan bool, 8)
	cond := NewHysteresisPollingCondition(read, 24.0, 22.0, 10*time.Millisecond)

	ctx := t.Context()
	cond.Start(ctx, func(v bool) { changes <- v })

	// Baseline seed (20 < high) must not fire onChange, and must be false.
	assertNoChange(t, changes)
	assert.False(t, cond.Evaluate())

	// Inside the band (22-24) from below: no transition.
	value.Store(23.0)
	assertNoChange(t, changes)
	assert.False(t, cond.Evaluate())

	// Crosses the high threshold: becomes true.
	value.Store(24.0)
	waitForChange(t, changes, true)
	assert.True(t, cond.Evaluate())

	// Drops back into the band: must not flip false yet.
	value.Store(23.0)
	assertNoChange(t, changes)
	assert.True(t, cond.Evaluate())

	// Crosses the low threshold: becomes false.
	value.Store(22.0)
	waitForChange(t, changes, false)
	assert.False(t, cond.Evaluate())
}

func TestHysteresisPredicateConditionBandBehaviour(t *testing.T) {
	bus := NewBus()
	var value atomic.Value
	value.Store(20.0)
	read := func() float64 { return value.Load().(float64) }

	cond := NewHysteresisPredicateCondition(bus, "topic", read, 24.0, 22.0, false)

	changes := make(chan bool, 8)
	ctx := t.Context()
	cond.Start(ctx, func(v bool) { changes <- v })

	// Baseline seed (20 < high) must not fire onChange, and must be false.
	assertNoChange(t, changes)
	assert.False(t, cond.Evaluate())

	// An event while still inside the band must not fire.
	value.Store(23.0)
	bus.Publish(Event{Topic: "topic"})
	assertNoChange(t, changes)
	assert.False(t, cond.Evaluate())

	// Crosses the high threshold: becomes true.
	value.Store(24.0)
	bus.Publish(Event{Topic: "topic"})
	waitForChange(t, changes, true)
	assert.True(t, cond.Evaluate())

	// An unrelated topic must not trigger a re-evaluation.
	value.Store(10.0)
	bus.Publish(Event{Topic: "other-topic"})
	assertNoChange(t, changes)
	assert.True(t, cond.Evaluate())

	// Drops to the low threshold: becomes false.
	value.Store(22.0)
	bus.Publish(Event{Topic: "topic"})
	waitForChange(t, changes, false)
	assert.False(t, cond.Evaluate())
}

// TestHysteresisPredicateConditionFalling covers the mirror-image shape
// (see hysteresisNext's falling parameter): true once the value falls
// to/below Low, false again once it recovers to/above High.
func TestHysteresisPredicateConditionFalling(t *testing.T) {
	bus := NewBus()
	var value atomic.Value
	value.Store(0.0) // well above Low

	cond := NewHysteresisPredicateCondition(bus, "topic", func() float64 { return value.Load().(float64) }, -14.0, -15.0, true)

	changes := make(chan bool, 8)
	ctx := t.Context()
	cond.Start(ctx, func(v bool) { changes <- v })

	assertNoChange(t, changes)
	assert.False(t, cond.Evaluate())

	// Staying above the recovery threshold must not fire.
	value.Store(-10.0)
	bus.Publish(Event{Topic: "topic"})
	assertNoChange(t, changes)
	assert.False(t, cond.Evaluate())

	// Falls to/below Low: alarm.
	value.Store(-16.0)
	bus.Publish(Event{Topic: "topic"})
	waitForChange(t, changes, true)
	assert.True(t, cond.Evaluate())

	// Recovers to/above High: clears.
	value.Store(-14.0)
	bus.Publish(Event{Topic: "topic"})
	waitForChange(t, changes, false)
	assert.False(t, cond.Evaluate())
}

func TestEventConditionPulseWithNilPredicates(t *testing.T) {
	bus := NewBus()
	cond := NewEventCondition(bus, "sensor.motion.front", nil, nil)

	changes := make(chan bool, 8)
	ctx := t.Context()
	cond.Start(ctx, func(v bool) { changes <- v })

	bus.Publish(Event{Topic: "sensor.motion.front", Payload: nil})

	waitForChange(t, changes, true)
	waitForChange(t, changes, false)
	assert.False(t, cond.Evaluate())
}

func TestEventConditionLevelWithPredicates(t *testing.T) {
	bus := NewBus()
	trueFn := func(ev Event) bool { return ev.Payload == "vacation" }
	falseFn := func(ev Event) bool { return ev.Payload != "vacation" }
	cond := NewEventCondition(bus, "house.mode", trueFn, falseFn)

	changes := make(chan bool, 8)
	ctx := t.Context()
	cond.Start(ctx, func(v bool) { changes <- v })

	bus.Publish(Event{Topic: "house.mode", Payload: "home"})
	assertNoChange(t, changes)
	assert.False(t, cond.Evaluate())

	bus.Publish(Event{Topic: "house.mode", Payload: "vacation"})
	waitForChange(t, changes, true)
	assert.True(t, cond.Evaluate())

	// A non-matching event while true must not flip it back.
	bus.Publish(Event{Topic: "house.mode", Payload: "vacation"})
	assertNoChange(t, changes)

	bus.Publish(Event{Topic: "house.mode", Payload: "home"})
	waitForChange(t, changes, false)
	assert.False(t, cond.Evaluate())
}

func TestEventConditionUnsubscribesOnCancel(t *testing.T) {
	bus := NewBus()
	cond := NewEventCondition(bus, "topic", nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cond.Start(ctx, func(bool) {})
	cancel()

	require.Eventually(t, func() bool {
		bus.mu.Lock()
		defer bus.mu.Unlock()
		return len(bus.subs["topic"]) == 0
	}, time.Second, 10*time.Millisecond, "subscriber was not removed after ctx cancellation")
}

func TestPredicateConditionFiresOnlyOnTransition(t *testing.T) {
	bus := NewBus()
	var value atomic.Bool
	cond := NewPredicateCondition(bus, "topic", value.Load)

	changes := make(chan bool, 8)
	ctx := t.Context()
	cond.Start(ctx, func(v bool) { changes <- v })

	// Baseline eval on Start must not fire onChange.
	assertNoChange(t, changes)
	assert.False(t, cond.Evaluate())

	// An event that doesn't change fn's result must not fire either.
	bus.Publish(Event{Topic: "topic"})
	assertNoChange(t, changes)

	value.Store(true)
	bus.Publish(Event{Topic: "topic"})
	waitForChange(t, changes, true)
	assert.True(t, cond.Evaluate())

	// An unrelated topic must not trigger a re-evaluation.
	value.Store(false)
	bus.Publish(Event{Topic: "other-topic"})
	assertNoChange(t, changes)
	assert.True(t, cond.Evaluate())

	bus.Publish(Event{Topic: "topic"})
	waitForChange(t, changes, false)
}

func TestIdleConditionFiresAfterDurationThenResetsOnEvent(t *testing.T) {
	bus := NewBus()
	cond := NewIdleCondition(bus, "topic", 30*time.Millisecond)

	changes := make(chan bool, 8)
	ctx := t.Context()
	cond.Start(ctx, func(v bool) { changes <- v })

	// Starts false; must not fire before the duration elapses.
	assert.False(t, cond.Evaluate())
	select {
	case <-changes:
		t.Fatal("fired before the idle duration elapsed")
	case <-time.After(10 * time.Millisecond):
	}

	waitForChange(t, changes, true)
	assert.True(t, cond.Evaluate())

	// A new event resets it to false and rearms the timer.
	bus.Publish(Event{Topic: "topic"})
	waitForChange(t, changes, false)
	assert.False(t, cond.Evaluate())

	// Idle again for the full duration: fires true a second time.
	waitForChange(t, changes, true)
}

func TestScheduleConditionFiresAtComputedInstantThenPulses(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 1, 1, 8, 59, 59, 900_000_000, loc) // 100ms before 09:00

	cond := NewScheduleCondition(loc, 9, 0)
	cond.now = func() time.Time { return now } // read once, at Start; never advances

	changes := make(chan bool, 8)
	cond.Start(t.Context(), func(v bool) { changes <- v })

	waitForChange(t, changes, true)
	waitForChange(t, changes, false)
	assert.False(t, cond.Evaluate(), "must pulse back to false, not stay true")
}

func TestScheduleConditionRestrictsToWeekdays(t *testing.T) {
	loc := time.UTC
	// 2026-01-01 is a Thursday.
	base := time.Date(2026, 1, 1, 8, 0, 0, 0, loc)
	// Only fire on Saturday (2026-01-03).
	cond := NewScheduleCondition(loc, 9, 0, time.Saturday)

	next := cond.next(base)
	assert.Equal(t, time.Saturday, next.Weekday())
	assert.Equal(t, 2026, next.Year())
	assert.Equal(t, time.January, next.Month())
	assert.Equal(t, 3, next.Day())
}

func TestSunEventConditionFiresAtComputedInstantThenPulses(t *testing.T) {
	const lat, lon = 43.7, -79.4 // Toronto-ish
	loc, err := time.LoadLocation("America/Toronto")
	require.NoError(t, err)

	day := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	_, sunset, solved, _ := sunTimesUTC(day, lat, lon)
	require.True(t, solved)

	now := sunset.Add(-100 * time.Millisecond)
	// loc must be the location the coordinates actually sit in, not an
	// arbitrary reporting zone: sunTimesUTC's "day" is a UTC-clock label
	// that, for a longitude west of Greenwich, rolls the evening's sunset
	// into the next UTC calendar day (see day/sunset above) - next() derives
	// "today" from from.In(loc), so a loc that doesn't match the
	// coordinates' own civil day (e.g. plain UTC here) would bucket this
	// instant into the wrong day and search a whole day too far ahead.
	cond := NewSunEventCondition(loc, true, 0, func() (float64, float64, bool) { return lat, lon, true })
	cond.now = func() time.Time { return now }

	changes := make(chan bool, 8)
	cond.Start(t.Context(), func(v bool) { changes <- v })

	waitForChange(t, changes, true)
	waitForChange(t, changes, false)
	assert.False(t, cond.Evaluate(), "must pulse back to false, not stay true")
}

func TestSunEventConditionNextAppliesOffset(t *testing.T) {
	const lat, lon = 43.7, -79.4
	day := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	_, sunset, solved, _ := sunTimesUTC(day, lat, lon)
	require.True(t, solved)

	cond := NewSunEventCondition(time.UTC, true, -30*time.Minute, func() (float64, float64, bool) { return lat, lon, true })
	next, fire := cond.next(day)
	assert.WithinDuration(t, sunset.Add(-30*time.Minute), next, time.Second)
	assert.True(t, fire)
}

func TestSunEventConditionRetriesWhenLocationUnavailable(t *testing.T) {
	cond := NewSunEventCondition(time.UTC, false, 0, func() (float64, float64, bool) { return 0, 0, false })
	from := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	next, fire := cond.next(from)
	assert.Equal(t, from.Add(cond.retryInterval), next)
	assert.False(t, fire)
}

// TestSunEventConditionDoesNotPulseOnLocationRetryTick guards the actual bug found live on
// 2026-10-01: a policyd deployed with no --house-addr/--building-id (so locate always returns
// ok=false) made SunEventCondition fire for real once an hour, every hour, instead of staying
// silent until a location became available - because Start used to pulse onChange on every timer
// wake-up unconditionally, including retryInterval retry ticks. retryInterval is shrunk here, on
// this cond alone, so the test can observe several retry ticks elapse without waiting a real hour -
// it's a field rather than a package global specifically so this doesn't race Start's background
// goroutine, which reads it on every reschedule for as long as it runs (including past this test's
// own return, since nothing joins that goroutine).
func TestSunEventConditionDoesNotPulseOnLocationRetryTick(t *testing.T) {
	cond := NewSunEventCondition(time.UTC, false, 0, func() (float64, float64, bool) { return 0, 0, false })
	cond.retryInterval = 10 * time.Millisecond

	changes := make(chan bool, 8)
	cond.Start(t.Context(), func(v bool) { changes <- v })

	select {
	case v := <-changes:
		t.Fatalf("must not pulse on a location-unavailable retry tick, got %v", v)
	case <-time.After(100 * time.Millisecond): // several retry ticks at 10ms each
	}
	assert.False(t, cond.Evaluate())
}

func TestSunWindowConditionStateReflectsSunriseSunset(t *testing.T) {
	const lat, lon = 43.7, -79.4
	loc, err := time.LoadLocation("America/Toronto")
	require.NoError(t, err)

	day := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC) // sunTimesUTC only reads Y/M/D
	sunrise, sunset, solved, _ := sunTimesUTC(day, lat, lon)
	require.True(t, solved)

	// See TestSunEventConditionFiresAtComputedInstantThenPulses for why loc
	// must match the coordinates' own civil day, not plain UTC. For the same
	// reason, "midnight" for state() must be midnight in loc, not UTC
	// midnight - which is still the evening of June 20 in Toronto.
	midnightLocal := time.Date(2026, 6, 21, 0, 0, 0, 0, loc)

	cond := NewSunWindowCondition(loc, func() (float64, float64, bool) { return lat, lon, true })

	upAtMidnight, nextFromMidnight := cond.state(midnightLocal)
	assert.False(t, upAtMidnight)
	assert.WithinDuration(t, sunrise, nextFromMidnight, time.Second)

	upAtNoon, nextFromNoon := cond.state(sunrise.Add(time.Hour))
	assert.True(t, upAtNoon)
	assert.WithinDuration(t, sunset, nextFromNoon, time.Second)

	upAfterSunset, nextAfterSunset := cond.state(sunset.Add(time.Hour))
	assert.False(t, upAfterSunset)
	tomorrowSunrise, _, tomorrowSolved, _ := sunTimesUTC(day.AddDate(0, 0, 1), lat, lon)
	require.True(t, tomorrowSolved)
	assert.WithinDuration(t, tomorrowSunrise, nextAfterSunset, time.Second)
}

func TestSunWindowConditionPolarDayIsAlwaysUp(t *testing.T) {
	const lat, lon = 78.0, 15.0 // well inside the Arctic Circle
	day := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)

	cond := NewSunWindowCondition(time.UTC, func() (float64, float64, bool) { return lat, lon, true })
	up, next := cond.state(day)
	assert.True(t, up)
	assert.Equal(t, day.Add(24*time.Hour), next)
}

// TestSunWindowConditionStartFiresOnSunriseTransition drives "now" off real
// elapsed wall-clock time (rather than a single fixed instant, as
// TestScheduleConditionFiresAtComputedInstantThenPulses does): unlike
// ScheduleCondition's pulse, SunWindowCondition re-reads "now" on every
// wake to decide its new level value, so a fixed clock would recompute the
// exact same pre-sunrise answer forever and never observe the transition.
func TestSunWindowConditionStartFiresOnSunriseTransition(t *testing.T) {
	const lat, lon = 43.7, -79.4
	day := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	sunrise, _, solved, _ := sunTimesUTC(day, lat, lon)
	require.True(t, solved)

	base := sunrise.Add(-100 * time.Millisecond)
	wallStart := time.Now()
	cond := NewSunWindowCondition(time.UTC, func() (float64, float64, bool) { return lat, lon, true })
	cond.now = func() time.Time { return base.Add(time.Since(wallStart)) }

	changes := make(chan bool, 8)
	cond.Start(t.Context(), func(v bool) { changes <- v })

	assert.False(t, cond.Evaluate())
	waitForChange(t, changes, true)
	assert.True(t, cond.Evaluate())
}

func TestDateRangeConditionMatchesNonWrappingRange(t *testing.T) {
	cond := NewDateRangeCondition(time.UTC, 12, 1, 12, 31) // December
	assert.True(t, cond.matches(time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC)))
	assert.False(t, cond.matches(time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)))
	assert.False(t, cond.matches(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)))
}

func TestDateRangeConditionMatchesSingleDay(t *testing.T) {
	cond := NewDateRangeCondition(time.UTC, 12, 25, 12, 25) // Christmas, every year
	assert.True(t, cond.matches(time.Date(2026, 12, 25, 23, 59, 0, 0, time.UTC)))
	assert.False(t, cond.matches(time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC)))
	assert.False(t, cond.matches(time.Date(2026, 12, 26, 0, 0, 0, 0, time.UTC)))
}

func TestDateRangeConditionWrapsAcrossNewYear(t *testing.T) {
	cond := NewDateRangeCondition(time.UTC, 12, 20, 1, 5) // Dec 20 - Jan 5
	assert.True(t, cond.matches(time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC)))
	assert.True(t, cond.matches(time.Date(2027, 1, 3, 0, 0, 0, 0, time.UTC)))
	assert.False(t, cond.matches(time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)))
}

// TestDateRangeConditionStartFiresAtLocalMidnight uses the same
// elapsed-wall-clock "now" as TestSunWindowConditionStartFiresOnSunriseTransition,
// for the same reason: the fire branch re-reads "now" to decide the new
// value.
func TestDateRangeConditionStartFiresAtLocalMidnight(t *testing.T) {
	base := time.Date(2026, 12, 24, 23, 59, 59, 900_000_000, time.UTC)
	wallStart := time.Now()
	cond := NewDateRangeCondition(time.UTC, 12, 25, 12, 25)
	cond.now = func() time.Time { return base.Add(time.Since(wallStart)) }

	changes := make(chan bool, 8)
	cond.Start(t.Context(), func(v bool) { changes <- v })

	assert.False(t, cond.Evaluate())
	waitForChange(t, changes, true)
	assert.True(t, cond.Evaluate())
}

// manualCondition lets a test flip a boolean directly, for exercising
// composite conditions without going through a bus or ticker.
type manualCondition struct {
	mu       sync.Mutex
	value    bool
	onChange func(bool)
}

func (m *manualCondition) Evaluate() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.value
}

func (m *manualCondition) Start(ctx context.Context, onChange func(bool)) {
	m.mu.Lock()
	m.onChange = onChange
	m.mu.Unlock()
}

func (m *manualCondition) set(v bool) {
	m.mu.Lock()
	m.value = v
	cb := m.onChange
	m.mu.Unlock()
	if cb != nil {
		cb(v)
	}
}

func TestAndCondition(t *testing.T) {
	a := &manualCondition{}
	b := &manualCondition{}
	cond := And(a, b)

	changes := make(chan bool, 8)
	ctx := t.Context()
	cond.Start(ctx, func(v bool) { changes <- v })

	assert.False(t, cond.Evaluate())

	a.set(true)
	assertNoChange(t, changes) // still false: b is false
	assert.False(t, cond.Evaluate())

	b.set(true)
	waitForChange(t, changes, true)
	assert.True(t, cond.Evaluate())

	a.set(false)
	waitForChange(t, changes, false)
}

func TestOrCondition(t *testing.T) {
	a := &manualCondition{}
	b := &manualCondition{}
	cond := Or(a, b)

	changes := make(chan bool, 8)
	ctx := t.Context()
	cond.Start(ctx, func(v bool) { changes <- v })

	a.set(true)
	waitForChange(t, changes, true)

	b.set(true)
	assertNoChange(t, changes) // already true

	a.set(false)
	assertNoChange(t, changes) // b still true

	b.set(false)
	waitForChange(t, changes, false)
}

func TestNotCondition(t *testing.T) {
	a := &manualCondition{}
	cond := Not(a)

	changes := make(chan bool, 8)
	ctx := t.Context()
	cond.Start(ctx, func(v bool) { changes <- v })

	assert.True(t, cond.Evaluate())

	a.set(true)
	waitForChange(t, changes, false)
	assert.False(t, cond.Evaluate())
}

func TestHeldForConditionFiresAfterDurationThenResetsOnChildFalse(t *testing.T) {
	child := &manualCondition{}
	cond := NewHeldForCondition(child, 30*time.Millisecond)

	changes := make(chan bool, 8)
	cond.Start(t.Context(), func(v bool) { changes <- v })

	assert.False(t, cond.Evaluate())

	child.set(true)
	// Must not fire before the duration elapses.
	assertNoChange(t, changes)
	assert.False(t, cond.Evaluate())

	waitForChange(t, changes, true)
	assert.True(t, cond.Evaluate())

	// Child going false resets immediately, regardless of how long it had
	// been held true.
	child.set(false)
	waitForChange(t, changes, false)
	assert.False(t, cond.Evaluate())
}

// TestHeldForConditionResetsBeforeDurationElapses covers the countdown being
// cancelled, not merely ignored: a child that goes false partway through the
// duration must not still fire true once the original duration would have
// elapsed.
func TestHeldForConditionResetsBeforeDurationElapses(t *testing.T) {
	child := &manualCondition{}
	cond := NewHeldForCondition(child, 30*time.Millisecond)

	changes := make(chan bool, 8)
	cond.Start(t.Context(), func(v bool) { changes <- v })

	child.set(true)
	time.Sleep(10 * time.Millisecond)
	child.set(false) // well before the 30ms duration elapses

	select {
	case v := <-changes:
		t.Fatalf("unexpected onChange(%v): countdown should have been cancelled", v)
	case <-time.After(50 * time.Millisecond):
	}
	assert.False(t, cond.Evaluate())
}

// TestHeldForConditionRepeatedTrueDoesNotRestartCountdown covers apply's "a
// further true while one is already pending is a no-op" rule: a child that
// re-fires true (e.g. a level condition re-publishing the same value) must
// not push the countdown's deadline back out.
func TestHeldForConditionRepeatedTrueDoesNotRestartCountdown(t *testing.T) {
	child := &manualCondition{}
	cond := NewHeldForCondition(child, 30*time.Millisecond)

	changes := make(chan bool, 8)
	cond.Start(t.Context(), func(v bool) { changes <- v })

	child.set(true)
	time.Sleep(20 * time.Millisecond)
	child.set(true) // re-fire true; must not restart the 30ms countdown

	waitForChange(t, changes, true) // fires ~10ms later, not ~30ms after the re-fire
}

// TestHeldForConditionStartPicksUpAlreadyTrueChild covers a child that's
// already true when Start runs (e.g. a light already on when the engine
// boots): the countdown must begin immediately, not wait for a transition
// that may never come.
func TestHeldForConditionStartPicksUpAlreadyTrueChild(t *testing.T) {
	child := &manualCondition{value: true}
	cond := NewHeldForCondition(child, 20*time.Millisecond)

	changes := make(chan bool, 8)
	cond.Start(t.Context(), func(v bool) { changes <- v })

	waitForChange(t, changes, true)
}

// TestHeldForConditionStopsCountdownOnCancel covers the context.AfterFunc
// cleanup: cancelling ctx while a countdown is pending must stop the timer,
// not merely leave it to fire into a torn-down onChange later.
func TestHeldForConditionStopsCountdownOnCancel(t *testing.T) {
	child := &manualCondition{}
	cond := NewHeldForCondition(child, 20*time.Millisecond)

	ctx, cancel := context.WithCancel(t.Context())
	changes := make(chan bool, 8)
	cond.Start(ctx, func(v bool) { changes <- v })

	child.set(true)
	cancel()

	select {
	case v := <-changes:
		t.Fatalf("unexpected onChange(%v) after cancellation", v)
	case <-time.After(50 * time.Millisecond):
	}
	assert.False(t, cond.Evaluate())
}

func TestConditionRegistryRegisterAndBuild(t *testing.T) {
	type params struct{ Threshold float64 }

	r := NewConditionRegistry()
	RegisterConditionType(r, "over-threshold", func(p params) Condition {
		return NewPollingCondition(func() bool { return p.Threshold > 10 }, time.Hour)
	})

	cond, err := r.build("over-threshold", params{Threshold: 20})
	require.NoError(t, err)
	assert.True(t, cond.Evaluate())

	_, err = r.build("does-not-exist", params{})
	assert.Error(t, err)

	_, err = r.build("over-threshold", "wrong type")
	assert.Error(t, err)
}

func TestConditionRegistryTypeNames(t *testing.T) {
	r := NewConditionRegistry()
	assert.Empty(t, r.TypeNames())

	RegisterConditionType(r, "b-cond", func(_ struct{}) Condition {
		return NewPollingCondition(func() bool { return true }, time.Hour)
	})
	RegisterConditionType(r, "a-cond", func(_ struct{}) Condition {
		return NewPollingCondition(func() bool { return true }, time.Hour)
	})

	assert.Equal(t, []string{"a-cond", "b-cond"}, r.TypeNames())

	r.UnregisterConditionType("a-cond")
	assert.Equal(t, []string{"b-cond"}, r.TypeNames())
}

func TestRegisterConditionTypeDuplicatePanics(t *testing.T) {
	r := NewConditionRegistry()
	RegisterConditionType(r, "dup", func(_ struct{}) Condition {
		return NewPollingCondition(func() bool { return true }, time.Hour)
	})

	assert.Panics(t, func() {
		RegisterConditionType(r, "dup", func(_ struct{}) Condition {
			return NewPollingCondition(func() bool { return false }, time.Hour)
		})
	})
}
