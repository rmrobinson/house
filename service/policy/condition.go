package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
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

	value atomic.Bool
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
	return h.value.Load()
}

func (h *HysteresisPollingCondition) next(current bool, v float64) bool {
	return hysteresisNext(current, v, h.thresholdHigh, h.thresholdLow, h.falling)
}

func (h *HysteresisPollingCondition) Start(ctx context.Context, onChange func(bool)) {
	last := h.next(false, h.fn())
	h.value.Store(last)

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
					h.value.Store(cur)
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

	value atomic.Bool
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
	return h.value.Load()
}

func (h *HysteresisPredicateCondition) next(current bool, v float64) bool {
	return hysteresisNext(current, v, h.thresholdHigh, h.thresholdLow, h.falling)
}

func (h *HysteresisPredicateCondition) Start(ctx context.Context, onChange func(bool)) {
	h.value.Store(h.next(false, h.fn()))

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

				cur := h.next(h.value.Load(), h.fn())
				changed := cur != h.value.Load()
				h.value.Store(cur)

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

	value atomic.Bool
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
	return e.value.Load()
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

				if !e.value.Load() {
					if e.trueFn != nil && !e.trueFn(ev) {
						continue
					}
					e.value.Store(true)
					onChange(true)

					if e.falseFn == nil {
						e.value.Store(false)
						onChange(false)
					}
					continue
				}

				// Currently true: falseFn is guaranteed non-nil here, since
				// the nil case above always resets to false immediately.
				if !e.falseFn(ev) {
					continue
				}
				e.value.Store(false)
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

	value atomic.Bool
}

// NewPredicateCondition creates a PredicateCondition that re-evaluates fn
// whenever an event is published to topic on bus.
func NewPredicateCondition(bus *Bus, topic string, fn func() bool) *PredicateCondition {
	return &PredicateCondition{bus: bus, topic: topic, fn: fn}
}

func (p *PredicateCondition) Evaluate() bool {
	return p.value.Load()
}

func (p *PredicateCondition) Start(ctx context.Context, onChange func(bool)) {
	p.value.Store(p.fn())

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
				changed := cur != p.value.Load()
				p.value.Store(cur)

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

	value atomic.Bool
}

// NewIdleCondition creates an IdleCondition that becomes true once duration
// passes with no event published to topic on bus.
func NewIdleCondition(bus *Bus, topic string, duration time.Duration) *IdleCondition {
	return &IdleCondition{bus: bus, topic: topic, duration: duration}
}

func (i *IdleCondition) Evaluate() bool {
	return i.value.Load()
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
				changed := !i.value.Load()
				i.value.Store(true)
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

				changed := i.value.Load()
				i.value.Store(false)
				if changed {
					onChange(false)
				}
			}
		}
	}()
}

// HeldForCondition is true once child has been continuously true for at
// least duration, and resets to false - cancelling any pending countdown -
// the instant child goes false. It's IdleCondition's mirror image, but keyed
// off a child Condition's own true/false transitions instead of a bus
// topic's mere presence of events: IdleCondition answers "nothing has
// happened for a while" (silence); this answers "something has stayed true
// for a while" (a held level) - the shape a "if this light is still on
// after 5 minutes, turn it off" policy needs, which idle-for's
// silence-based reasoning doesn't fit (a light staying on need not publish
// any further events at all).
type HeldForCondition struct {
	child    Condition
	duration time.Duration

	mu    sync.Mutex
	timer *time.Timer
	value bool
}

// NewHeldForCondition creates a HeldForCondition that becomes true once
// child has held true continuously for duration.
func NewHeldForCondition(child Condition, duration time.Duration) *HeldForCondition {
	return &HeldForCondition{child: child, duration: duration}
}

func (h *HeldForCondition) Evaluate() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.value
}

// fire applies v as the current value if it's a real transition, calling
// onChange exactly once for it. Called both from the countdown's own timer
// (child has been true for the full duration) and directly, from apply
// (child went false).
func (h *HeldForCondition) fire(onChange func(bool), v bool) {
	h.mu.Lock()
	changed := v != h.value
	h.value = v
	h.mu.Unlock()
	if changed {
		onChange(v)
	}
}

// apply reacts to child's current value: true arms a duration countdown (a
// further true while one is already pending is a no-op - the countdown
// doesn't restart, since child never stopped being true); false cancels any
// pending countdown and, if this condition had already become true, fires
// it straight back to false.
func (h *HeldForCondition) apply(onChange func(bool), childValue bool) {
	h.mu.Lock()
	if childValue {
		if h.timer != nil {
			h.mu.Unlock()
			return
		}
		h.timer = time.AfterFunc(h.duration, func() { h.fire(onChange, true) })
		h.mu.Unlock()
		return
	}

	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	h.mu.Unlock()
	h.fire(onChange, false)
}

func (h *HeldForCondition) Start(ctx context.Context, onChange func(bool)) {
	h.child.Start(ctx, func(childValue bool) { h.apply(onChange, childValue) })

	// Pick up a child that's already true when Start runs (e.g. a light
	// already on when the engine boots): Start doesn't fire onChange for a
	// child's own baseline value (see e.g. PollingCondition's doc comment),
	// so without this the countdown would never begin until the child's
	// next transition - which, for an already-true fact, might never come.
	h.apply(onChange, h.child.Evaluate())

	// time.AfterFunc's timer runs independently of ctx, unlike every other
	// Condition here (each of which blocks on ctx.Done() in its own
	// goroutine): stop a still-pending countdown on cancellation so it
	// doesn't leak past this policy's teardown.
	context.AfterFunc(ctx, func() {
		h.mu.Lock()
		if h.timer != nil {
			h.timer.Stop()
			h.timer = nil
		}
		h.mu.Unlock()
	})
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

	value atomic.Bool
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
	return s.value.Load()
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
				s.value.Store(true)
				onChange(true)

				s.value.Store(false)
				onChange(false)
			}
		}
	}()
}

// sunLocationRetryInterval is how long SunEventCondition/SunWindowCondition
// wait before checking again when locate reports the observer's location
// isn't available yet - a fixed backoff rather than the "hold the last
// value" convention numeric readers use, since there's no prior sun
// computation to fall back on the first time this happens.
var sunLocationRetryInterval = time.Hour

// SunEventCondition fires once each day at the computed sunrise or sunset
// for a location supplied by locate (degrees; ok=false defers to
// sunLocationRetryInterval), offset by offset, pulsing onChange(true) then
// onChange(false) exactly like ScheduleCondition - "at sunset" is a
// momentary event, not a state that stays true. locate is called fresh on
// every reschedule, so a location that becomes available (or changes) after
// Start is picked up without a restart.
type SunEventCondition struct {
	locate func() (lat, lon float64, ok bool)
	loc    *time.Location
	sunset bool // true selects sunset, false selects sunrise
	offset time.Duration

	now func() time.Time // overridable in tests

	value atomic.Bool
}

// NewSunEventCondition creates a SunEventCondition. sunset selects sunset
// over sunrise; offset shifts the trigger from the exact instant (negative
// fires earlier, positive later).
func NewSunEventCondition(loc *time.Location, sunset bool, offset time.Duration, locate func() (lat, lon float64, ok bool)) *SunEventCondition {
	return &SunEventCondition{loc: loc, sunset: sunset, offset: offset, locate: locate, now: time.Now}
}

func (s *SunEventCondition) Evaluate() bool {
	return s.value.Load()
}

// next returns the next instant strictly after from at which Start's timer
// should wake up, and whether that instant is a genuine sunrise/sunset
// event Start should pulse onChange for (fire=true): the next day (starting
// with from's own calendar day) whose computed sunrise/sunset+offset falls
// after from. fire is false when from's wake-up is only a
// sunLocationRetryInterval check-back - the location isn't available, or no
// solution turned up within a year (deep polar latitudes) - so Start knows
// to silently reschedule rather than treat the retry tick itself as a sun
// event.
func (s *SunEventCondition) next(from time.Time) (next time.Time, fire bool) {
	lat, lon, ok := s.locate()
	if !ok {
		return from.Add(sunLocationRetryInterval), false
	}

	local := from.In(s.loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)

	for range 366 {
		sunrise, sunset, solved, _ := sunTimesUTC(day, lat, lon)
		if solved {
			target := sunrise
			if s.sunset {
				target = sunset
			}
			target = target.Add(s.offset)
			if target.After(from) {
				return target, true
			}
		}
		day = day.AddDate(0, 0, 1)
	}
	return from.Add(sunLocationRetryInterval), false
}

func (s *SunEventCondition) Start(ctx context.Context, onChange func(bool)) {
	go func() {
		for {
			now := s.now()
			target, fire := s.next(now)
			timer := time.NewTimer(target.Sub(now))

			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				// A retry tick (fire=false, from an unavailable location or an
				// unsolvable polar day) is not itself a sunrise/sunset event - only
				// loop back to reschedule, without pulsing onChange. Previously this
				// pulsed unconditionally on every timer wake-up, so a location that
				// never resolved (e.g. policyd started with no --house-addr/
				// --building-id) made the condition fire for real, every
				// sunLocationRetryInterval, around the clock.
				if !fire {
					continue
				}
				s.value.Store(true)
				onChange(true)

				s.value.Store(false)
				onChange(false)
			}
		}
	}()
}

// SunWindowCondition is true while the current time is between today's
// sunrise and sunset for a location supplied by locate, recomputing at each
// transition (or retrying after sunLocationRetryInterval if the location
// isn't available) so it stays correct as sunrise/sunset drift day to day.
// Wrap with Not for "is it dark"/"is it night".
type SunWindowCondition struct {
	locate func() (lat, lon float64, ok bool)
	loc    *time.Location

	now func() time.Time // overridable in tests

	value atomic.Bool
}

// NewSunWindowCondition creates a SunWindowCondition for a location supplied
// by locate.
func NewSunWindowCondition(loc *time.Location, locate func() (lat, lon float64, ok bool)) *SunWindowCondition {
	return &SunWindowCondition{loc: loc, locate: locate, now: time.Now}
}

func (w *SunWindowCondition) Evaluate() bool {
	return w.value.Load()
}

// state reports whether the sun is up at t, and the next instant (strictly
// after t) at which that answer could change.
func (w *SunWindowCondition) state(t time.Time) (up bool, next time.Time) {
	lat, lon, ok := w.locate()
	if !ok {
		return false, t.Add(sunLocationRetryInterval)
	}

	local := t.In(w.loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)

	sunrise, sunset, solved, alwaysUp := sunTimesUTC(day, lat, lon)
	if !solved {
		return alwaysUp, t.Add(24 * time.Hour)
	}
	if t.Before(sunrise) {
		return false, sunrise
	}
	if t.Before(sunset) {
		return true, sunset
	}

	// Past today's sunset: down until tomorrow's sunrise.
	tomorrow := day.AddDate(0, 0, 1)
	nextSunrise, _, tomorrowSolved, tomorrowAlwaysUp := sunTimesUTC(tomorrow, lat, lon)
	if !tomorrowSolved {
		return tomorrowAlwaysUp, t.Add(24 * time.Hour)
	}
	return false, nextSunrise
}

func (w *SunWindowCondition) Start(ctx context.Context, onChange func(bool)) {
	// Baseline established synchronously, before the goroutine below runs -
	// the same convention PollingCondition/PredicateCondition use - so a
	// caller that reads Evaluate() (or wraps this in HeldForCondition)
	// immediately after Start sees the real current answer, not a
	// zero-value placeholder waiting on a goroutine that hasn't run yet.
	now := w.now()
	up, next := w.state(now)
	w.value.Store(up)

	go func() {
		for {
			timer := time.NewTimer(next.Sub(now))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				now = w.now()
				up, next = w.state(now)

				changed := up != w.value.Load()
				w.value.Store(up)

				if changed {
					onChange(up)
				}
			}
		}
	}()
}

// monthDay is a year-independent (month, day) pair, ordered lexically by
// month then day - Go doesn't support ordering operators on array/struct
// types directly, hence the explicit helpers below.
type monthDay struct{ month, day int }

func monthDayLess(a, b monthDay) bool {
	if a.month != b.month {
		return a.month < b.month
	}
	return a.day < b.day
}

func monthDayLessEq(a, b monthDay) bool {
	return a == b || monthDayLess(a, b)
}

// nextLocalMidnight returns the next local midnight, in loc, strictly after
// from.
func nextLocalMidnight(from time.Time, loc *time.Location) time.Time {
	local := from.In(loc)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	if !midnight.After(local) {
		midnight = midnight.AddDate(0, 0, 1)
	}
	return midnight
}

// DateRangeCondition is true while today's (month, day), in loc's timezone,
// falls within [start, end] inclusive - year-independent, so it recurs every
// year. A range whose end sorts before its start wraps across New Year's
// (e.g. December 20 - January 5); a single-day range (start == end) is
// exactly "on this calendar date every year" (e.g. Christmas: {12, 25} to
// {12, 25}). It rechecks once at every local midnight - the one instant its
// membership can change - like ScheduleCondition but level instead of pulse.
type DateRangeCondition struct {
	loc        *time.Location
	start, end monthDay

	now func() time.Time // overridable in tests

	value atomic.Bool
}

// NewDateRangeCondition creates a DateRangeCondition for [startMonth,startDay]
// through [endMonth,endDay] inclusive, evaluated in loc.
func NewDateRangeCondition(loc *time.Location, startMonth, startDay, endMonth, endDay int) *DateRangeCondition {
	return &DateRangeCondition{
		loc:   loc,
		start: monthDay{startMonth, startDay},
		end:   monthDay{endMonth, endDay},
		now:   time.Now,
	}
}

func (d *DateRangeCondition) Evaluate() bool {
	return d.value.Load()
}

func (d *DateRangeCondition) matches(t time.Time) bool {
	local := t.In(d.loc)
	cur := monthDay{int(local.Month()), local.Day()}

	if !monthDayLess(d.end, d.start) { // start <= end: a normal, non-wrapping range
		return monthDayLessEq(d.start, cur) && monthDayLessEq(cur, d.end)
	}
	// end < start: wraps across New Year's.
	return monthDayLessEq(d.start, cur) || monthDayLessEq(cur, d.end)
}

func (d *DateRangeCondition) Start(ctx context.Context, onChange func(bool)) {
	// Baseline established synchronously - see SunWindowCondition.Start's
	// identical comment for why.
	now := d.now()
	d.value.Store(d.matches(now))

	go func() {
		for {
			next := nextLocalMidnight(now, d.loc)
			timer := time.NewTimer(next.Sub(now))

			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				now = d.now()
				newVal := d.matches(now)

				changed := newVal != d.value.Load()
				d.value.Store(newVal)

				if changed {
					onChange(newVal)
				}
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
