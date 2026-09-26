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

// hysteresisNext computes a hysteresis band's next value for a fresh
// reading v given the band's current state, shared by
// HysteresisPollingCondition and HysteresisPredicateCondition so both apply
// exactly the same logic.
//
// falling selects which edge is the "alarm" edge: false (the default shape)
// flips false→true once v rises to/above high and back true→false once v
// falls to/below low - e.g. "temperature is above 25". true is the mirror
// image, for "falls below" facts (e.g. "temperature is below −15", "battery
// is below 5%"): it flips false→true once v falls to/below low, and back
// true→false once v recovers to/above high.
func hysteresisNext(current bool, v, high, low float64, falling bool) bool {
	if falling {
		switch {
		case !current && v <= low:
			return true
		case current && v >= high:
			return false
		default:
			return current
		}
	}
	switch {
	case !current && v >= high:
		return true
	case current && v <= low:
		return false
	default:
		return current
	}
}

// HysteresisPollingCondition wraps a func() float64 that is evaluated on a
// fixed interval, applying a hysteresis band (see hysteresisNext) so a value
// oscillating near a single threshold doesn't rapidly toggle the condition.
type HysteresisPollingCondition struct {
	fn            func() float64
	thresholdHigh float64
	thresholdLow  float64
	interval      time.Duration
	falling       bool

	mu    sync.Mutex
	value bool
}

// NewHysteresisPollingCondition creates a HysteresisPollingCondition that
// evaluates fn every interval against the [thresholdLow, thresholdHigh]
// band, rising-edge shaped (see hysteresisNext).
func NewHysteresisPollingCondition(fn func() float64, thresholdHigh, thresholdLow float64, interval time.Duration) *HysteresisPollingCondition {
	return NewDirectionalHysteresisPollingCondition(fn, thresholdHigh, thresholdLow, interval, false)
}

// NewDirectionalHysteresisPollingCondition is NewHysteresisPollingCondition
// with control over which edge is the alarm edge - see hysteresisNext's
// falling parameter.
func NewDirectionalHysteresisPollingCondition(fn func() float64, thresholdHigh, thresholdLow float64, interval time.Duration, falling bool) *HysteresisPollingCondition {
	return &HysteresisPollingCondition{
		fn:            fn,
		thresholdHigh: thresholdHigh,
		thresholdLow:  thresholdLow,
		interval:      interval,
		falling:       falling,
	}
}

func (h *HysteresisPollingCondition) Evaluate() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.value
}

func (h *HysteresisPollingCondition) next(current bool, v float64) bool {
	return hysteresisNext(current, v, h.thresholdHigh, h.thresholdLow, h.falling)
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

// HysteresisPredicateCondition is HysteresisPollingCondition's band logic
// (see hysteresisNext) driven by a Bus signal instead of a ticker - the same
// relationship PredicateCondition has to PollingCondition. Useful for a
// numeric fact that should react to a push-driven signal (e.g. bridgehome's
// per-device "device.updated.<id>" event) rather than be polled on a timer.
type HysteresisPredicateCondition struct {
	bus           *Bus
	topic         string
	fn            func() float64
	thresholdHigh float64
	thresholdLow  float64
	falling       bool

	mu    sync.Mutex
	value bool
}

// NewHysteresisPredicateCondition creates a HysteresisPredicateCondition
// that re-evaluates fn against the [thresholdLow, thresholdHigh] band
// whenever an event is published to topic on bus. See hysteresisNext's
// falling parameter.
func NewHysteresisPredicateCondition(bus *Bus, topic string, fn func() float64, thresholdHigh, thresholdLow float64, falling bool) *HysteresisPredicateCondition {
	return &HysteresisPredicateCondition{
		bus:           bus,
		topic:         topic,
		fn:            fn,
		thresholdHigh: thresholdHigh,
		thresholdLow:  thresholdLow,
		falling:       falling,
	}
}

func (h *HysteresisPredicateCondition) Evaluate() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.value
}

func (h *HysteresisPredicateCondition) next(current bool, v float64) bool {
	return hysteresisNext(current, v, h.thresholdHigh, h.thresholdLow, h.falling)
}

func (h *HysteresisPredicateCondition) Start(ctx context.Context, onChange func(bool)) {
	h.mu.Lock()
	h.value = h.next(false, h.fn())
	h.mu.Unlock()

	ch := h.bus.Subscribe(h.topic)

	go func() {
		defer h.bus.Unsubscribe(h.topic, ch)

		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-ch:
				if !ok {
					return
				}

				h.mu.Lock()
				cur := h.next(h.value, h.fn())
				changed := cur != h.value
				h.value = cur
				h.mu.Unlock()

				if changed {
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

// PredicateCondition wraps a func() bool that is re-evaluated once
// immediately on Start (a baseline, without treating that first read as a
// transition - same convention as PollingCondition) and again every time an
// event is published to a Bus topic, rather than on a fixed interval.
// onChange fires only when a re-evaluation's result differs from the
// previous one. This is PollingCondition's shape driven by a signal instead
// of a ticker - for a "level" fact (unlike EventCondition's pulse/predicate
// shape) that should react to some other event rather than be polled on a
// timer.
type PredicateCondition struct {
	bus   *Bus
	topic string
	fn    func() bool

	mu    sync.Mutex
	value bool
}

// NewPredicateCondition creates a PredicateCondition that re-evaluates fn
// whenever an event is published to topic on bus.
func NewPredicateCondition(bus *Bus, topic string, fn func() bool) *PredicateCondition {
	return &PredicateCondition{bus: bus, topic: topic, fn: fn}
}

func (p *PredicateCondition) Evaluate() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.value
}

func (p *PredicateCondition) Start(ctx context.Context, onChange func(bool)) {
	p.mu.Lock()
	p.value = p.fn()
	p.mu.Unlock()

	ch := p.bus.Subscribe(p.topic)

	go func() {
		defer p.bus.Unsubscribe(p.topic, ch)

		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-ch:
				if !ok {
					return
				}

				cur := p.fn()

				p.mu.Lock()
				changed := cur != p.value
				p.value = cur
				p.mu.Unlock()

				if changed {
					onChange(cur)
				}
			}
		}
	}()
}

// IdleCondition is true once no event has been published to a Bus topic for
// at least duration, and flips back to false - rearming the timer - the
// instant a new event arrives. It needs no trueFn/falseFn the way
// EventCondition does: the passage of time itself, uninterrupted by an
// event, is what flips it true. Useful for "no motion for 30 minutes" or
// "device hasn't reported in 5 minutes" style facts.
type IdleCondition struct {
	bus      *Bus
	topic    string
	duration time.Duration

	mu    sync.Mutex
	value bool
}

// NewIdleCondition creates an IdleCondition that becomes true once duration
// passes with no event published to topic on bus.
func NewIdleCondition(bus *Bus, topic string, duration time.Duration) *IdleCondition {
	return &IdleCondition{bus: bus, topic: topic, duration: duration}
}

func (i *IdleCondition) Evaluate() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.value
}

func (i *IdleCondition) Start(ctx context.Context, onChange func(bool)) {
	ch := i.bus.Subscribe(i.topic)
	timer := time.NewTimer(i.duration)

	go func() {
		defer i.bus.Unsubscribe(i.topic, ch)
		defer timer.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				i.mu.Lock()
				changed := !i.value
				i.value = true
				i.mu.Unlock()
				if changed {
					onChange(true)
				}
			case _, ok := <-ch:
				if !ok {
					return
				}

				// Reset requires a stopped-or-drained timer: Stop can race
				// with an already-fired timer whose value nobody has read
				// yet, so drain it non-blockingly before rearming.
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(i.duration)

				i.mu.Lock()
				changed := i.value
				i.value = false
				i.mu.Unlock()
				if changed {
					onChange(false)
				}
			}
		}
	}()
}

// ScheduleCondition fires once at a specific wall-clock time each day, or
// only on specific weekdays if any are given, pulsing onChange(true) then
// immediately onChange(false) - mirroring EventCondition's
// pulse-when-falseFn-nil convention, since "fire at 9am" is a momentary
// event, not a state that stays true - then reschedules for the next
// matching occurrence.
type ScheduleCondition struct {
	loc      *time.Location
	hour     int
	minute   int
	weekdays map[time.Weekday]bool // nil/empty: every day

	now func() time.Time // overridable in tests

	mu    sync.Mutex
	value bool
}

// NewScheduleCondition creates a ScheduleCondition that fires at
// hour:minute, loc's wall clock, every day, or only on the given weekdays if
// any are provided.
func NewScheduleCondition(loc *time.Location, hour, minute int, weekdays ...time.Weekday) *ScheduleCondition {
	s := &ScheduleCondition{loc: loc, hour: hour, minute: minute, now: time.Now}
	if len(weekdays) > 0 {
		s.weekdays = make(map[time.Weekday]bool, len(weekdays))
		for _, wd := range weekdays {
			s.weekdays[wd] = true
		}
	}
	return s
}

func (s *ScheduleCondition) Evaluate() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value
}

func (s *ScheduleCondition) matchesDay(t time.Time) bool {
	if len(s.weekdays) == 0 {
		return true
	}
	return s.weekdays[t.Weekday()]
}

// next returns the next instant strictly after from that is hour:minute on a
// matching weekday.
func (s *ScheduleCondition) next(from time.Time) time.Time {
	from = from.In(s.loc)
	candidate := time.Date(from.Year(), from.Month(), from.Day(), s.hour, s.minute, 0, 0, s.loc)
	if !candidate.After(from) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	for !s.matchesDay(candidate) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate
}

func (s *ScheduleCondition) Start(ctx context.Context, onChange func(bool)) {
	go func() {
		for {
			now := s.now()
			timer := time.NewTimer(s.next(now).Sub(now))

			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				s.mu.Lock()
				s.value = true
				s.mu.Unlock()
				onChange(true)

				s.mu.Lock()
				s.value = false
				s.mu.Unlock()
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
