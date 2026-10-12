package policy

import (
	"context"
	"sync"

	"go.uber.org/zap"
)

// Keys of the api/trait Buttons state, as home.GetState addresses them (no leading
// "light."/"sensor." segment - see bridgehome.GetState).
const (
	buttonEventCountKey = "buttons.state.event_count"
	buttonLastActionKey = "buttons.state.last_action"
)

// ButtonActionParams parameterizes the "button.action" condition type: a momentary trigger that
// pulses once for every button event on DeviceID whose action is one of Actions (a Hue dimmer's
// "on_press", an Inovelli's "up_double", ...). An empty Actions matches any action.
type ButtonActionParams struct {
	DeviceID string
	Actions  []string
}

// ButtonActionCondition pulses onChange(true) then onChange(false) per matching button event,
// like ScheduleCondition - "the button was pressed" is an event, not a state that stays true.
//
// Events are detected by trait.Buttons' event_count changing, not by last_action changing, so two
// identical actions in a row (two double-taps) are still two pulses. The count is read fresh from
// home state on each "device.updated.<id>" signal; Start takes a baseline of the current count
// without treating it as an event, so a policy (re)registered after a press doesn't replay it, and
// a bridge restart (count resets to zero, last_action empty) doesn't fire. The same goes for a
// device policyd hasn't heard of yet when it starts: its first reading is the baseline.
type ButtonActionCondition struct {
	e        *Engine
	deviceID string
	actions  map[string]bool // nil: any action

	mu sync.Mutex
	// last is the most recent event_count seen; seen reports whether last is a real reading yet.
	// Until it is, no update can be called a new event: the first reading is whatever the device
	// already had.
	last  int64
	seen  bool
	ids   []string
	value bool
}

// NewButtonActionCondition creates a ButtonActionCondition for deviceID. See ButtonActionParams.
func NewButtonActionCondition(e *Engine, deviceID string, actions []string) *ButtonActionCondition {
	b := &ButtonActionCondition{e: e, deviceID: deviceID}
	if len(actions) > 0 {
		b.actions = make(map[string]bool, len(actions))
		for _, a := range actions {
			b.actions[a] = true
		}
	}
	return b
}

func (b *ButtonActionCondition) Evaluate() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.value
}

// TriggerDeviceIDs implements TriggerContextProvider.
func (b *ButtonActionCondition) TriggerDeviceIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.ids...)
}

func (b *ButtonActionCondition) count() (int64, bool) {
	v, err := b.e.home.GetState(b.deviceID, buttonEventCountKey)
	if err != nil {
		b.e.logger.Warn("button.action: GetState failed",
			zap.String("deviceId", b.deviceID), zap.Error(err))
		return 0, false
	}
	f, err := toFloat64(v)
	if err != nil {
		b.e.logger.Warn("button.action: event_count is not numeric",
			zap.String("deviceId", b.deviceID), zap.Error(err))
		return 0, false
	}
	return int64(f), true
}

func (b *ButtonActionCondition) Start(ctx context.Context, onChange func(bool)) {
	if n, ok := b.count(); ok {
		b.mu.Lock()
		b.last, b.seen = n, true
		b.mu.Unlock()
	}

	topic := "device.updated." + b.deviceID
	ch := b.e.bus.Subscribe(topic)

	go func() {
		defer b.e.bus.Unsubscribe(topic, ch)

		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-ch:
				if !ok {
					return
				}
				b.onUpdate(onChange)
			}
		}
	}()
}

func (b *ButtonActionCondition) onUpdate(onChange func(bool)) {
	n, ok := b.count()
	if !ok {
		return
	}

	b.mu.Lock()
	prev, seen := b.last, b.seen
	b.last, b.seen = n, true
	b.mu.Unlock()
	if !seen {
		// The baseline read in Start failed - policyd registers persisted policies before the
		// device cache is populated - so this is the first look at a device that may already have
		// a history. Taking it as an event would replay its last action on every restart.
		return
	}
	if n == prev {
		return
	}

	// A bridge restart zeroes the count (and last_action), which isn't a press. But a drop to a
	// nonzero count is: it means policyd missed the zeroed state and this is the first press
	// after the restart, so only a count of exactly zero is skipped, not every decrease.
	if n == 0 {
		return
	}

	action, err := b.e.home.GetState(b.deviceID, buttonLastActionKey)
	if err != nil {
		b.e.logger.Warn("button.action: GetState failed",
			zap.String("deviceId", b.deviceID), zap.Error(err))
		return
	}
	s, _ := action.(string)
	if b.actions != nil && !b.actions[s] {
		return
	}

	b.mu.Lock()
	b.ids = []string{b.deviceID}
	b.value = true
	b.mu.Unlock()
	onChange(true)

	b.mu.Lock()
	b.value = false
	b.mu.Unlock()
	onChange(false)
}
