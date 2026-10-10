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
// a bridge restart (count resets to zero, last_action empty) doesn't fire.
type ButtonActionCondition struct {
	e        *Engine
	deviceID string
	actions  map[string]bool // nil: any action

	mu    sync.Mutex
	last  int64
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
		b.last = n
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
	prev := b.last
	b.last = n
	b.mu.Unlock()
	if n == prev {
		return
	}

	// A drop in the count is the bridge having restarted, not a press.
	if n < prev {
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
