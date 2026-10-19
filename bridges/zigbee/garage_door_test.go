package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
)

// garageDoorDevice mirrors the exposes of a real LoraTap GDC311ZBQ1 (read from a live
// zigbee2mqtt): a writable "trigger" and a read-only "garage_door_contact" whose value_on is
// false - i.e. true means closed.
func garageDoorDevice(withTrigger bool) bridgeDevice {
	exposes := []expose{
		{Type: "binary", Name: "garage_door_contact", Property: "garage_door_contact",
			ValueOn: json.RawMessage(`false`), ValueOff: json.RawMessage(`true`)},
		{Type: "numeric", Name: "linkquality", Property: "linkquality"},
	}
	if withTrigger {
		exposes = append([]expose{{Type: "binary", Name: "trigger", Property: "trigger",
			ValueOn: json.RawMessage(`true`), ValueOff: json.RawMessage(`false`)}}, exposes...)
	}
	return bridgeDevice{
		IEEEAddress:  "0xa4c1383804c99208",
		FriendlyName: "garage_door",
		Type:         "Router",
		Supported:    true,
		Definition: &deviceDefiniton{Model: "GDC311ZBQ1", Vendor: "Tuya",
			Description: "LoraTap garage door opener with wireless sensor", Exposes: exposes},
	}
}

func mustGarageDoorBuilder(t *testing.T, bd bridgeDevice) garageDoorBuilder {
	t.Helper()
	gb, ok := newGarageDoorBuilder(bd.Definition.Exposes)
	require.True(t, ok)
	return gb
}

func openCloseCmd(a command.OpenClose_Action) *command.Command {
	return &command.Command{Details: &command.Command_OpenClose{OpenClose: &command.OpenClose{Action: a}}}
}

func TestClassify_GarageDoor(t *testing.T) {
	b, ok := (&networkConn{}).classify(garageDoorDevice(true))
	require.True(t, ok)
	assert.IsType(t, garageDoorBuilder{}, b)
}

func TestGarageDoorBuilder_BuildAndApplyState(t *testing.T) {
	bd := garageDoorDevice(true)
	gb := mustGarageDoorBuilder(t, bd)
	d, err := gb.build(bd)
	require.NoError(t, err)

	assert.Equal(t, "garage_door", d.Config.Name)
	oc := d.GetGarageDoor().OpenClose
	assert.True(t, oc.Attributes.CanControl)
	assert.Equal(t, trait.OpenClose_POSITION_UNSPECIFIED, oc.State.Position)

	gb.applyState(d, map[string]any{"linkquality": 40.0})
	assert.Equal(t, trait.OpenClose_POSITION_UNSPECIFIED, oc.State.Position, "a report without the contact must not set a position")

	gb.applyState(d, map[string]any{"garage_door_contact": true})
	assert.Equal(t, trait.OpenClose_POSITION_CLOSED, oc.State.Position)
	gb.applyState(d, map[string]any{"garage_door_contact": false})
	assert.Equal(t, trait.OpenClose_POSITION_OPEN, oc.State.Position)
}

// An unrecognised contact value must not be read as "open": the viewer would then offer CLOSE,
// which pulses the opener and opens a door that is actually shut.
func TestGarageDoorBuilder_UnrecognisedContactValueLeavesPosition(t *testing.T) {
	bd := garageDoorDevice(true)
	gb := mustGarageDoorBuilder(t, bd)
	d, err := gb.build(bd)
	require.NoError(t, err)
	oc := d.GetGarageDoor().OpenClose

	for _, bad := range []any{nil, "banana", map[string]any{"x": 1}, []any{true}} {
		gb.applyState(d, map[string]any{"garage_door_contact": bad})
		assert.Equal(t, trait.OpenClose_POSITION_UNSPECIFIED, oc.State.Position, "%#v", bad)
	}

	gb.applyState(d, map[string]any{"garage_door_contact": true})
	gb.applyState(d, map[string]any{"garage_door_contact": nil})
	assert.Equal(t, trait.OpenClose_POSITION_CLOSED, oc.State.Position, "a bad report must keep the last known position")
}

// A repeat OPEN/CLOSE before the contact has reported would pulse the door a second time and stop
// or reverse it; the contact reporting (or TRIGGER) is the way past.
func TestGarageDoorBuilder_RepeatCommandHeldUntilContactReports(t *testing.T) {
	bd := garageDoorDevice(true)
	gb := mustGarageDoorBuilder(t, bd)
	mc, fc := newTestMQTTConn(t)
	d, err := gb.build(bd)
	require.NoError(t, err)
	gb.applyState(d, map[string]any{"garage_door_contact": true}) // closed

	pulses := func() int {
		n := 0
		for _, p := range fc.published {
			if p.topic == "zigbee2mqtt/garage_door/set" {
				n++
			}
		}
		return n
	}
	ctx := context.Background()

	require.NoError(t, gb.applyCommand(ctx, mc, "garage_door", d, openCloseCmd(command.OpenClose_ACTION_OPEN)))
	err = gb.applyCommand(ctx, mc, "garage_door", d, openCloseCmd(command.OpenClose_ACTION_OPEN))
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Equal(t, 1, pulses(), "a double-tap must not pulse twice")

	// TRIGGER is explicit, so it is never held back.
	require.NoError(t, gb.applyCommand(ctx, mc, "garage_door", d, openCloseCmd(command.OpenClose_ACTION_TRIGGER)))
	assert.Equal(t, 2, pulses())

	// Once the contact reports, the hold is lifted.
	gb.applyState(d, map[string]any{"garage_door_contact": false}) // now open
	require.NoError(t, gb.applyCommand(ctx, mc, "garage_door", d, openCloseCmd(command.OpenClose_ACTION_CLOSE)))
	assert.Equal(t, 3, pulses())
}

func TestGarageDoorBuilder_InvalidActionBeatsCapabilityCheck(t *testing.T) {
	bd := garageDoorDevice(false) // contact only
	gb := mustGarageDoorBuilder(t, bd)
	d, err := gb.build(bd)
	require.NoError(t, err)
	mc, _ := newTestMQTTConn(t)

	err = gb.applyCommand(context.Background(), mc, "garage_door", d, openCloseCmd(command.OpenClose_ACTION_UNSPECIFIED))
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestGarageDoorBuilder_ContactOnly_NotControllable(t *testing.T) {
	bd := garageDoorDevice(false)
	gb := mustGarageDoorBuilder(t, bd)
	d, err := gb.build(bd)
	require.NoError(t, err)
	assert.False(t, d.GetGarageDoor().OpenClose.Attributes.CanControl)

	mc, _ := newTestMQTTConn(t)
	for _, a := range []command.OpenClose_Action{command.OpenClose_ACTION_OPEN, command.OpenClose_ACTION_TRIGGER} {
		err = gb.applyCommand(context.Background(), mc, "garage_door", d, openCloseCmd(a))
		assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand, a.String())
	}
}

func TestGarageDoorBuilder_Command(t *testing.T) {
	bd := garageDoorDevice(true)
	gb := mustGarageDoorBuilder(t, bd)

	tests := []struct {
		name      string
		contact   any // raw garage_door_contact; nil = never reported
		action    command.OpenClose_Action
		wantPulse bool
		wantCode  codes.Code
	}{
		{"open a closed door pulses", true, command.OpenClose_ACTION_OPEN, true, codes.OK},
		{"close an open door pulses", false, command.OpenClose_ACTION_CLOSE, true, codes.OK},
		{"close a closed door is a no-op", true, command.OpenClose_ACTION_CLOSE, false, codes.OK},
		{"open an open door is a no-op", false, command.OpenClose_ACTION_OPEN, false, codes.OK},
		{"unknown position refuses", nil, command.OpenClose_ACTION_CLOSE, false, codes.FailedPrecondition},
		{"unspecified action refuses", true, command.OpenClose_ACTION_UNSPECIFIED, false, codes.InvalidArgument},
		{"trigger pulses with unknown position", nil, command.OpenClose_ACTION_TRIGGER, true, codes.OK},
		{"trigger pulses when closed", true, command.OpenClose_ACTION_TRIGGER, true, codes.OK},
		{"trigger pulses when open", false, command.OpenClose_ACTION_TRIGGER, true, codes.OK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mc, fc := newTestMQTTConn(t)
			d, err := gb.build(bd)
			require.NoError(t, err)
			if tc.contact != nil {
				gb.applyState(d, map[string]any{"garage_door_contact": tc.contact})
			}
			before := d.GetGarageDoor().OpenClose.State.Position

			err = gb.applyCommand(context.Background(), mc, "garage_door", d, openCloseCmd(tc.action))
			assert.Equal(t, tc.wantCode, status.Code(err), "err: %v", err)
			// The contact, not the command, is the source of truth for position.
			assert.Equal(t, before, d.GetGarageDoor().OpenClose.State.Position)

			var pulses int
			for _, p := range fc.published {
				if p.topic == "zigbee2mqtt/garage_door/set" {
					pulses++
					assert.JSONEq(t, `{"trigger":true}`, string(p.payload))
				}
			}
			if tc.wantPulse {
				assert.Equal(t, 1, pulses)
			} else {
				assert.Zero(t, pulses)
			}
		})
	}
}

func TestGarageDoorBuilder_RejectsOtherCommands(t *testing.T) {
	bd := garageDoorDevice(true)
	gb := mustGarageDoorBuilder(t, bd)
	d, err := gb.build(bd)
	require.NoError(t, err)
	mc, _ := newTestMQTTConn(t)

	err = gb.applyCommand(context.Background(), mc, "garage_door", d, &command.Command{
		Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
	})
	assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
}
