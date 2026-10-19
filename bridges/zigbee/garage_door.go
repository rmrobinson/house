package main

import (
	"context"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
)

const (
	garageContactProperty = "garage_door_contact"
	garageTriggerProperty = "trigger"

	// garagePulseSettle is how long after a pulse OPEN/CLOSE refuse to pulse again unless the
	// contact has reported. Until the contact changes the cached position is unchanged, so a
	// double-tap or a retried policy would otherwise pulse twice and stop (or reverse) the door.
	garagePulseSettle = 10 * time.Second
)

// garageDoorBuilder builds a house GarageDoor device from a Tuya/LoraTap GDC311ZBQ1-style opener,
// recognised by its "garage_door_contact" binary expose (read-only: the wireless sensor's
// closed-position contact) alongside its "trigger" binary expose (writable: a momentary pulse to
// the door's motor, the same as pressing the wall button).
//
// The hardware has no open/close/stop commands and no way to tell "opening" or "closing" from
// "open": the contact only knows whether the door is fully closed. OpenClose.Position is
// therefore CLOSED or OPEN (anything that isn't closed), and command.OpenClose is translated
// into a pulse only when the door isn't already at the target - so an OPEN/CLOSE command is
// idempotent, which a bare toggle never would be. The remaining hazard is a command arriving
// while the door is mid-travel: CLOSE while it is opening reads as "open", so it pulses, which
// stops the door (or reverses it, depending on the opener's firmware). Nothing in the exposes
// can tell this bridge that is happening.
//
// The expose documents which raw value means closed (value_on: false, value_off: true on a real
// GDC311ZBQ1 - "closed" is the logical off state, as with Sensor's "contact"), so that is read
// from the expose rather than hardcoded.
//
// A contact value matching neither closedValue nor openValue (a null, an unexpected type) leaves
// the position as it was: guessing "open" for a closed door would make the viewer offer CLOSE,
// which would then open it.
type garageDoorBuilder struct {
	// closedValue and openValue are the raw "garage_door_contact" values meaning the door is
	// closed / not closed.
	closedValue, openValue any
	// canTrigger is false for a variant which only reports the contact.
	canTrigger bool
	// lastPulse is the unix-nano time of the last pulse sent, or 0 once the contact has reported
	// since. A pointer so the copies of this value type that classify hands around share it.
	lastPulse *atomic.Int64
}

func newGarageDoorBuilder(exposes []expose) (garageDoorBuilder, bool) {
	contact, ok := findByProperty(exposes, garageContactProperty)
	if !ok {
		return garageDoorBuilder{}, false
	}
	_, canTrigger := findByProperty(exposes, garageTriggerProperty)
	closed := decodeAny(contact.ValueOff, true)
	open := decodeAny(contact.ValueOn, nil)
	if b, isBool := closed.(bool); open == nil && isBool {
		open = !b
	}
	return garageDoorBuilder{
		closedValue: closed,
		openValue:   open,
		canTrigger:  canTrigger,
		lastPulse:   new(atomic.Int64),
	}, true
}

func (gb garageDoorBuilder) build(bd bridgeDevice) (*device.Device, error) {
	return &device.Device{
		Manufacturer:     bd.Definition.Vendor,
		ModelId:          bd.Definition.Model,
		ModelDescription: &bd.Definition.Description,
		Config:           &device.Device_Config{Name: bd.FriendlyName},
		Details: &device.Device_GarageDoor{GarageDoor: &device.GarageDoor{
			OpenClose: &trait.OpenClose{
				Attributes: &trait.OpenClose_Attributes{CanControl: gb.canTrigger},
				State:      &trait.OpenClose_State{},
			},
		}},
	}, nil
}

func (gb garageDoorBuilder) applyState(d *device.Device, state map[string]any) {
	v, ok := state[garageContactProperty]
	st := d.GetGarageDoor().GetOpenClose().GetState()
	if !ok || st == nil {
		return
	}
	switch v.(type) {
	case bool, string, float64:
	default:
		return // comparing interfaces holding a map or slice would panic, and none is a position
	}
	switch v {
	case gb.closedValue:
		st.Position = trait.OpenClose_POSITION_CLOSED
	case gb.openValue:
		st.Position = trait.OpenClose_POSITION_OPEN
	default:
		return
	}
	gb.lastPulse.Store(0) // the door has told us where it is; no pulse is still pending
}

func (gb garageDoorBuilder) applyCommand(ctx context.Context, m *mqttConn, friendlyName string, d *device.Device, cmd *command.Command) error {
	oc := cmd.GetOpenClose()
	if oc == nil {
		return bridge.ErrUnsupportedCommand
	}

	var want trait.OpenClose_Position
	switch oc.GetAction() {
	case command.OpenClose_ACTION_OPEN:
		want = trait.OpenClose_POSITION_OPEN
	case command.OpenClose_ACTION_CLOSE:
		want = trait.OpenClose_POSITION_CLOSED
	case command.OpenClose_ACTION_TRIGGER:
		if !gb.canTrigger {
			return bridge.ErrUnsupportedCommand
		}
		// Deliberately ignores the reported position and any recent pulse: this is the escape
		// hatch for when the contact is silent or wrong, so consulting it would defeat the point.
		return gb.pulse(ctx, m, friendlyName)
	default:
		return status.Error(codes.InvalidArgument, "open_close action must be OPEN, CLOSE or TRIGGER")
	}
	if !gb.canTrigger {
		return bridge.ErrUnsupportedCommand
	}

	switch d.GetGarageDoor().GetOpenClose().GetState().GetPosition() {
	case want:
		return nil // already there; pulsing would move it the wrong way
	case trait.OpenClose_POSITION_UNSPECIFIED:
		// A pulse is a toggle, so without knowing which way it would go it could do the opposite
		// of what was asked. ACTION_TRIGGER is the explicit way around this.
		return status.Error(codes.FailedPrecondition, "garage door position is not known yet; use TRIGGER to press the opener's button regardless")
	}
	if last := gb.lastPulse.Load(); last != 0 && time.Since(time.Unix(0, last)) < garagePulseSettle {
		return status.Error(codes.FailedPrecondition, "garage door was just triggered and has not reported a new position yet")
	}
	return gb.pulse(ctx, m, friendlyName)
}

// pulse presses the opener's button once.
func (gb garageDoorBuilder) pulse(ctx context.Context, m *mqttConn, friendlyName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	gb.lastPulse.Store(time.Now().UnixNano())
	// Published, not WriteState: "trigger" is a momentary pulse, and nothing guarantees the device
	// echoes it back. The resulting movement shows up as the contact changing, which the normal
	// state stream applies - so State.Position is deliberately not updated optimistically here.
	if err := m.publish(m.stateTopic(friendlyName)+"/set", map[string]any{garageTriggerProperty: true}); err != nil {
		gb.lastPulse.Store(0) // nothing was sent, so there is nothing to wait for
		return err
	}
	return nil
}
