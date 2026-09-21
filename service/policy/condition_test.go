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
