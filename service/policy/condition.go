package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Condition is a boolean signal a Policy's condition expression is built
// from. Start begins evaluation and must call onChange on every true/false
// transition until ctx is cancelled; Evaluate returns the current value
// synchronously, without side effects, so composite conditions can
// re-evaluate themselves after a child changes.
type Condition interface {
	Start(ctx context.Context, onChange func(value bool))
	Evaluate() bool
}

// PollingCondition wraps a func() bool that is evaluated on a fixed
// interval. onChange fires only when a tick's result differs from the
// previous one. It evaluates once immediately on Start to establish a
// baseline, without treating that first read as a transition.
type PollingCondition struct {
	fn       func() bool
	interval time.Duration
}

// NewPollingCondition creates a PollingCondition that evaluates fn every
// interval.
func NewPollingCondition(fn func() bool, interval time.Duration) *PollingCondition {
	return &PollingCondition{fn: fn, interval: interval}
}

func (p *PollingCondition) Evaluate() bool {
	return p.fn()
}

func (p *PollingCondition) Start(ctx context.Context, onChange func(bool)) {
	last := p.fn()

	go func() {
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cur := p.fn()
				if cur != last {
					last = cur
					onChange(cur)
				}
			}
		}
	}()
}

// HysteresisPollingCondition wraps a func() float64 that is evaluated on a
// fixed interval, applying a hysteresis band so a value oscillating near a
// single threshold doesn't rapidly toggle the condition. It only flips
// false→true once the value reaches thresholdHigh, and only flips back
// true→false once the value drops to thresholdLow; readings strictly
// between the two leave the current state unchanged.
type HysteresisPollingCondition struct {
	fn            func() float64
	thresholdHigh float64
	thresholdLow  float64
	interval      time.Duration

	mu    sync.Mutex
	value bool
}

// NewHysteresisPollingCondition creates a HysteresisPollingCondition that
// evaluates fn every interval against the [thresholdLow, thresholdHigh]
// band.
func NewHysteresisPollingCondition(fn func() float64, thresholdHigh, thresholdLow float64, interval time.Duration) *HysteresisPollingCondition {
	return &HysteresisPollingCondition{
		fn:            fn,
		thresholdHigh: thresholdHigh,
		thresholdLow:  thresholdLow,
		interval:      interval,
	}
}

func (h *HysteresisPollingCondition) Evaluate() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.value
}

// next computes the band-adjusted value for a fresh reading v, given the
// condition's current state. It's shared by Start's baseline seed and its
// ticker loop so both apply exactly the same band logic.
func (h *HysteresisPollingCondition) next(current bool, v float64) bool {
	switch {
	case !current && v >= h.thresholdHigh:
		return true
	case current && v <= h.thresholdLow:
		return false
	default:
		return current
	}
}

func (h *HysteresisPollingCondition) Start(ctx context.Context, onChange func(bool)) {
	h.mu.Lock()
	h.value = h.next(false, h.fn())
	last := h.value
	h.mu.Unlock()

	go func() {
		ticker := time.NewTicker(h.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cur := h.next(last, h.fn())
				if cur != last {
					last = cur
					h.mu.Lock()
					h.value = cur
					h.mu.Unlock()
					onChange(cur)
				}
			}
		}
	}()
}

// EventCondition subscribes to a Bus topic and derives a boolean value from
// the events it receives.
//
// trueFn decides whether an event flips the condition from false to true;
// if nil, any event on the topic does. falseFn decides whether an event
// flips it back from true to false; if nil, the condition returns to false
// immediately after becoming true (a pulse), rather than waiting for a
// further event.
type EventCondition struct {
	bus     *Bus
	topic   string
	trueFn  func(Event) bool
	falseFn func(Event) bool

	mu    sync.Mutex
	value bool
}

// NewEventCondition creates an EventCondition subscribed to topic on bus.
func NewEventCondition(bus *Bus, topic string, trueFn, falseFn func(Event) bool) *EventCondition {
	return &EventCondition{
		bus:     bus,
		topic:   topic,
		trueFn:  trueFn,
		falseFn: falseFn,
	}
}

func (e *EventCondition) Evaluate() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.value
}

func (e *EventCondition) Start(ctx context.Context, onChange func(bool)) {
	ch := e.bus.Subscribe(e.topic)

	go func() {
		defer e.bus.Unsubscribe(e.topic, ch)

		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}

				e.mu.Lock()
				if !e.value {
					if e.trueFn != nil && !e.trueFn(ev) {
						e.mu.Unlock()
						continue
					}
					e.value = true
					e.mu.Unlock()
					onChange(true)

					if e.falseFn == nil {
						e.mu.Lock()
						e.value = false
						e.mu.Unlock()
						onChange(false)
					}
					continue
				}

				// Currently true: falseFn is guaranteed non-nil here, since
				// the nil case above always resets to false immediately.
				if !e.falseFn(ev) {
					e.mu.Unlock()
					continue
				}
				e.value = false
				e.mu.Unlock()
				onChange(false)
			}
		}
	}()
}

type compositeOp int

const (
	opAnd compositeOp = iota
	opOr
)

// compositeCondition implements And/Or over child conditions: it starts
// every child and re-evaluates the full expression whenever any child
// changes, firing onChange only on an actual transition of its own value.
type compositeCondition struct {
	op       compositeOp
	children []Condition

	mu    sync.Mutex
	value bool
}

// And returns a Condition that is true only while every child is true.
func And(children ...Condition) Condition {
	return &compositeCondition{op: opAnd, children: children}
}

// Or returns a Condition that is true while any child is true.
func Or(children ...Condition) Condition {
	return &compositeCondition{op: opOr, children: children}
}

func (c *compositeCondition) Evaluate() bool {
	switch c.op {
	case opAnd:
		for _, child := range c.children {
			if !child.Evaluate() {
				return false
			}
		}
		return len(c.children) > 0
	case opOr:
		for _, child := range c.children {
			if child.Evaluate() {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func (c *compositeCondition) Start(ctx context.Context, onChange func(bool)) {
	c.mu.Lock()
	c.value = c.Evaluate()
	c.mu.Unlock()

	recompute := func() {
		next := c.Evaluate()

		c.mu.Lock()
		changed := next != c.value
		c.value = next
		c.mu.Unlock()

		if changed {
			onChange(next)
		}
	}

	for _, child := range c.children {
		child.Start(ctx, func(bool) { recompute() })
	}
}

// notCondition implements Not over a single child condition.
type notCondition struct {
	child Condition
}

// Not returns a Condition that is the logical negation of child.
func Not(child Condition) Condition {
	return &notCondition{child: child}
}

func (n *notCondition) Evaluate() bool {
	return !n.child.Evaluate()
}

func (n *notCondition) Start(ctx context.Context, onChange func(bool)) {
	n.child.Start(ctx, func(childValue bool) {
		onChange(!childValue)
	})
}

// ConditionFactory produces a Condition instance from a typed parameter
// struct P. Each call must return an independent Condition owned by the
// caller.
type ConditionFactory[P any] func(params P) Condition

// ConditionRegistry holds condition types registered once by name and
// referenced by policies via Use.
type ConditionRegistry struct {
	mu           sync.RWMutex
	types        map[string]func(params any) (Condition, error)
	unmarshalers map[string]func(data []byte) (any, error)
}

// NewConditionRegistry creates an empty ConditionRegistry.
func NewConditionRegistry() *ConditionRegistry {
	return &ConditionRegistry{
		types:        make(map[string]func(params any) (Condition, error)),
		unmarshalers: make(map[string]func(data []byte) (any, error)),
	}
}

// RegisterConditionType registers a named condition type backed by factory.
// It panics if name is already registered, since that indicates a
// programming error (two condition types colliding on the same name), not a
// runtime condition callers should need to handle.
func RegisterConditionType[P any](r *ConditionRegistry, name string, factory ConditionFactory[P]) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.types[name]; exists {
		panic(fmt.Sprintf("policy: condition type %q already registered", name))
	}

	r.types[name] = func(params any) (Condition, error) {
		p, ok := params.(P)
		if !ok {
			return nil, fmt.Errorf("condition type %q: expected params of type %T, got %T", name, *new(P), params)
		}
		return factory(p), nil
	}

	// Captured here, at registration time, so a persisted policy's JSON
	// "params" can be unmarshalled into the same concrete type P the
	// factory above expects — the registry is the only place that knows
	// what P is for a given name.
	r.unmarshalers[name] = func(data []byte) (any, error) {
		p := new(P)
		if len(data) > 0 {
			if err := json.Unmarshal(data, p); err != nil {
				return nil, fmt.Errorf("condition type %q: unmarshalling params: %w", name, err)
			}
		}
		return *p, nil
	}
}

// UnregisterConditionType removes name from the registry. It does not
// inspect or affect any Condition trees already built from it — those keep
// running exactly as before, since a built Condition holds no live
// reference back to the registry. A subsequent Register/replace that
// resolves to name will fail once it's gone. Engine.UnregisterConditionType
// is the caller-facing entry point that also warns when active policies
// still reference name.
func (r *ConditionRegistry) UnregisterConditionType(name string) {
	r.mu.Lock()
	delete(r.types, name)
	delete(r.unmarshalers, name)
	r.mu.Unlock()
}

func (r *ConditionRegistry) build(name string, params any) (Condition, error) {
	r.mu.RLock()
	factory, ok := r.types[name]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("condition type %q not registered", name)
	}
	return factory(params)
}

// TypeNames returns the name of every currently registered condition type,
// sorted. It's meant for introspection (e.g. an HTTP UI enumerating what a
// policy's condition expression can reference), not for building one.
func (r *ConditionRegistry) TypeNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.types))
	for name := range r.types {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// unmarshalParams deserialises data (a condition type's persisted "params"
// JSON) into the concrete parameter type that name's factory was registered
// with.
func (r *ConditionRegistry) unmarshalParams(name string, data []byte) (any, error) {
	r.mu.RLock()
	unmarshal, ok := r.unmarshalers[name]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("condition type %q not registered", name)
	}
	return unmarshal(data)
}
