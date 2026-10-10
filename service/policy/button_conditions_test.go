package policy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pressButton(home *fakeHomeAPI, e *Engine, id, action string, count int64) {
	_ = home.SetState(id, buttonLastActionKey, action)
	_ = home.SetState(id, buttonEventCountKey, count)
	e.Bus().Publish(Event{Topic: "device.updated." + id})
}

func registerButtonPolicy(t *testing.T, e *Engine, id string, p ButtonActionParams) {
	t.Helper()
	require.NoError(t, e.Register(&Policy{
		ID:               id,
		ConditionExpr:    Use("button.action", p),
		Script:           `home.notify(trigger.device_id, nil)`,
		OnConditionFalse: Complete,
	}))
}

func TestButtonActionFiresPerMatchingEventIncludingRepeats(t *testing.T) {
	home := newFakeHomeAPI()
	_ = home.SetState("remote", buttonEventCountKey, int64(7)) // pre-existing press: baseline only
	_ = home.SetState("remote", buttonLastActionKey, "on_press")
	e, _ := newTestEngine(t, home)
	RegisterBuiltinConditionTypes(e)

	registerButtonPolicy(t, e, "on", ButtonActionParams{DeviceID: "remote", Actions: []string{"on_press"}})
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 0, home.notifyCount(), "the baseline must not count as an event")

	pressButton(home, e, "remote", "on_press", 8)
	require.Eventually(t, func() bool { return home.notifyCount() == 1 }, time.Second, 10*time.Millisecond)

	// An identical action again is a second event.
	pressButton(home, e, "remote", "on_press", 9)
	require.Eventually(t, func() bool { return home.notifyCount() == 2 }, time.Second, 10*time.Millisecond)
}

func TestButtonActionIgnoresOtherActionsAndUnrelatedUpdates(t *testing.T) {
	home := newFakeHomeAPI()
	_ = home.SetState("remote", buttonEventCountKey, int64(0))
	e, _ := newTestEngine(t, home)
	RegisterBuiltinConditionTypes(e)

	registerButtonPolicy(t, e, "up", ButtonActionParams{DeviceID: "remote", Actions: []string{"up_press"}})

	pressButton(home, e, "remote", "down_press", 1)        // different action
	e.Bus().Publish(Event{Topic: "device.updated.remote"}) // count unchanged
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, home.notifyCount())

	pressButton(home, e, "remote", "up_press", 2)
	require.Eventually(t, func() bool { return home.notifyCount() == 1 }, time.Second, 10*time.Millisecond)
}

func TestButtonActionFirstPressAfterUnseenRestartStillFires(t *testing.T) {
	home := newFakeHomeAPI()
	_ = home.SetState("remote", buttonEventCountKey, int64(5))
	e, _ := newTestEngine(t, home)
	RegisterBuiltinConditionTypes(e)

	registerButtonPolicy(t, e, "any", ButtonActionParams{DeviceID: "remote"})

	// Bridge restarted and policyd never saw the zeroed state: the count jumps 5 -> 1.
	pressButton(home, e, "remote", "on_press", 1)
	require.Eventually(t, func() bool { return home.notifyCount() == 1 }, time.Second, 10*time.Millisecond)
}

func TestButtonActionCountResetIsNotAnEvent(t *testing.T) {
	home := newFakeHomeAPI()
	_ = home.SetState("remote", buttonEventCountKey, int64(5))
	e, _ := newTestEngine(t, home)
	RegisterBuiltinConditionTypes(e)

	registerButtonPolicy(t, e, "any", ButtonActionParams{DeviceID: "remote"})

	// Bridge restarted: count back to zero, no action yet.
	pressButton(home, e, "remote", "", 0)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, home.notifyCount())

	// And presses after the restart are seen again.
	pressButton(home, e, "remote", "off_press", 1)
	require.Eventually(t, func() bool { return home.notifyCount() == 1 }, time.Second, 10*time.Millisecond)
}
