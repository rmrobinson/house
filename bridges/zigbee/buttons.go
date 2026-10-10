package main

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/rmrobinson/house/api/trait"
)

// findActionExpose returns a device's top-level "action" enum expose - zigbee2mqtt's convention
// for a button, paddle or tap event (a Hue dimmer's "on_press", an Inovelli's "up_double"). Unlike
// findByProperty it matches Type "enum", which is how zigbee2mqtt types "action".
func findActionExpose(exposes []expose) (expose, bool) {
	for _, e := range exposes {
		if e.Property == "action" && e.Endpoint == "" && e.Type == "enum" {
			return e, true
		}
	}
	return expose{}, false
}

// newButtonsTrait returns a Buttons trait listing the device's reportable actions if it exposes an
// "action" enum, else nil. Shared by lightBuilder and sensorBuilder so both decode events
// identically.
func newButtonsTrait(exposes []expose) *trait.Buttons {
	e, ok := findActionExpose(exposes)
	if !ok {
		return nil
	}
	return &trait.Buttons{
		Attributes: &trait.Buttons_Attributes{Actions: append([]string(nil), e.Values...)},
		State:      &trait.Buttons_State{},
	}
}

// applyButtonsState records an action event from a (possibly partial) state message onto b. A nil b
// (device has no buttons) is a no-op.
//
// zigbee2mqtt publishes "action" only on the message for the event itself, and follows it with a
// message carrying an empty-string "action" to reset it - so an empty or absent value is not an
// event and is ignored. Every non-empty one increments event_count, even if identical to the last,
// so two double-taps in a row remain two distinguishable state changes (see trait.Buttons).
func applyButtonsState(b *trait.Buttons, state map[string]any, now time.Time) {
	if b == nil {
		return
	}
	action, ok := state["action"].(string)
	if !ok || action == "" {
		return
	}
	b.State.LastAction = action
	b.State.EventCount++
	b.State.LastActionTime = timestamppb.New(now)
}
