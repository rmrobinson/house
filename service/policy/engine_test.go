package policy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
)

// registerManualTrigger registers a condition type named name backed by a
// manualCondition, and returns it so the test can flip it directly instead
// of waiting on a real event source.
func registerManualTrigger(t *testing.T, r *ConditionRegistry, name string) *manualCondition {
	t.Helper()
	m := &manualCondition{}
	RegisterConditionType(r, name, func(_ struct{}) Condition { return m })
	return m
}

func newTestEngine(t *testing.T, home HomeAPI, opts ...EngineOption) (*Engine, *ConditionRegistry) {
	t.Helper()
	r := NewConditionRegistry()
	e := NewEngine(home, r, zaptest.NewLogger(t), opts...)
	t.Cleanup(e.Close)
	return e, r
}

func TestEngineRegisterRunsScriptOnConditionTrue(t *testing.T) {
	home := newFakeHomeAPI()
	e, r := newTestEngine(t, home)
	trigger := registerManualTrigger(t, r, "trigger")

	require.NoError(t, e.Register(&Policy{
		ID:            "test.light-on",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        `home.setLight("light.kitchen", true)`,
	}))

	trigger.set(true)

	require.Eventually(t, func() bool {
		return home.getLight("light.kitchen")
	}, time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		return len(e.LogsForPolicy("test.light-on")) == 1
	}, time.Second, 10*time.Millisecond)

	logs := e.LogsForPolicy("test.light-on")
	assert.Equal(t, StatusSuccess, logs[0].Status)
	assert.Empty(t, logs[0].Error)
}

func TestEngineRegisterRejectsInvalidScript(t *testing.T) {
	e, r := newTestEngine(t, newFakeHomeAPI())
	registerManualTrigger(t, r, "trigger")

	err := e.Register(&Policy{
		ID:            "test.bad-script",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        `this is not lua (`,
	})
	assert.Error(t, err)
}

func TestEngineRegisterRejectsMissingFields(t *testing.T) {
	e, r := newTestEngine(t, newFakeHomeAPI())
	trigger := Use(registerAlwaysTrueType(t, r), struct{}{})

	assert.Error(t, e.Register(&Policy{ConditionExpr: trigger, Script: "x=1"}))
	assert.Error(t, e.Register(&Policy{ID: "p", Script: "x=1"}))
	assert.Error(t, e.Register(&Policy{ID: "p", ConditionExpr: trigger}))
}

func registerAlwaysTrueType(t *testing.T, r *ConditionRegistry) string {
	t.Helper()
	name := "always-true"
	RegisterConditionType(r, name, func(_ struct{}) Condition {
		return NewPollingCondition(func() bool { return true }, time.Hour)
	})
	return name
}

func TestEngineScriptLevelErrorIsStatusFailure(t *testing.T) {
	home := newFakeHomeAPI()
	e, r := newTestEngine(t, home)
	trigger := registerManualTrigger(t, r, "trigger")

	require.NoError(t, e.Register(&Policy{
		ID:            "test.script-error",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        `error("boom")`,
	}))

	trigger.set(true)

	require.Eventually(t, func() bool {
		return len(e.LogsForPolicy("test.script-error")) == 1
	}, time.Second, 10*time.Millisecond)

	logs := e.LogsForPolicy("test.script-error")
	assert.Equal(t, StatusFailure, logs[0].Status)
	assert.NotEmpty(t, logs[0].Error)
}

func TestEngineBindingErrorIsStatusError(t *testing.T) {
	home := newFakeHomeAPI()
	home.sensorErr = assert.AnError
	e, r := newTestEngine(t, home)
	trigger := registerManualTrigger(t, r, "trigger")

	require.NoError(t, e.Register(&Policy{
		ID:            "test.binding-error",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        `home.getSensor("temp.kitchen")`,
	}))

	trigger.set(true)

	require.Eventually(t, func() bool {
		return len(e.LogsForPolicy("test.binding-error")) == 1
	}, time.Second, 10*time.Millisecond)

	logs := e.LogsForPolicy("test.binding-error")
	assert.Equal(t, StatusError, logs[0].Status)
	assert.NotEmpty(t, logs[0].Error)
}

func TestEngineExecutionTimeout(t *testing.T) {
	home := newFakeHomeAPI()
	e, r := newTestEngine(t, home, WithExecutionTimeout(50*time.Millisecond))
	trigger := registerManualTrigger(t, r, "trigger")

	require.NoError(t, e.Register(&Policy{
		ID:            "test.runaway",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        `while true do end`,
	}))

	trigger.set(true)

	require.Eventually(t, func() bool {
		return len(e.LogsForPolicy("test.runaway")) == 1
	}, 2*time.Second, 10*time.Millisecond)

	logs := e.LogsForPolicy("test.runaway")
	assert.Equal(t, StatusError, logs[0].Status)
}

func TestEngineUnregisterInterruptsRunningScript(t *testing.T) {
	home := newFakeHomeAPI()
	e, r := newTestEngine(t, home, WithExecutionTimeout(10*time.Second))
	trigger := registerManualTrigger(t, r, "trigger")

	require.NoError(t, e.Register(&Policy{
		ID:               "test.interruptible",
		ConditionExpr:    Use("trigger", struct{}{}),
		Script:           `while true do end`,
		OnConditionFalse: Interrupt,
	}))

	trigger.set(true)
	time.Sleep(20 * time.Millisecond) // let the script actually start

	require.NoError(t, e.Unregister("test.interruptible"))

	require.Eventually(t, func() bool {
		return len(e.LogsForPolicy("test.interruptible")) == 1
	}, time.Second, 10*time.Millisecond)

	logs := e.LogsForPolicy("test.interruptible")
	assert.Equal(t, StatusError, logs[0].Status)
}

func TestEngineRegisterReplacesExistingPolicy(t *testing.T) {
	home := newFakeHomeAPI()
	e, r := newTestEngine(t, home, WithExecutionTimeout(10*time.Second))
	trigger := registerManualTrigger(t, r, "trigger")

	require.NoError(t, e.Register(&Policy{
		ID:               "test.replaceable",
		ConditionExpr:    Use("trigger", struct{}{}),
		Script:           `while true do end`,
		OnConditionFalse: Interrupt,
	}))
	trigger.set(true)
	time.Sleep(20 * time.Millisecond)

	// Re-register the same ID with a different, fast script and a new
	// trigger. The old running "while true" instance must be interrupted
	// because the OLD policy was configured with Interrupt.
	newTrigger := registerManualTrigger(t, r, "new-trigger")
	require.NoError(t, e.Register(&Policy{
		ID:            "test.replaceable",
		ConditionExpr: Use("new-trigger", struct{}{}),
		Script:        `home.setLight("light.replaced", true)`,
	}))

	require.Eventually(t, func() bool {
		logs := e.LogsForPolicy("test.replaceable")
		for _, l := range logs {
			if l.Status == StatusError {
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond, "old running instance was not interrupted on replace")

	newTrigger.set(true)
	require.Eventually(t, func() bool {
		return home.getLight("light.replaced")
	}, time.Second, 10*time.Millisecond)
}

func TestEngineLogsAcrossPolicies(t *testing.T) {
	home := newFakeHomeAPI()
	e, r := newTestEngine(t, home)
	a := registerManualTrigger(t, r, "trigger-a")
	b := registerManualTrigger(t, r, "trigger-b")

	require.NoError(t, e.Register(&Policy{ID: "policy.a", ConditionExpr: Use("trigger-a", struct{}{}), Script: "x=1"}))
	require.NoError(t, e.Register(&Policy{ID: "policy.b", ConditionExpr: Use("trigger-b", struct{}{}), Script: "x=1"}))

	a.set(true)
	b.set(true)

	require.Eventually(t, func() bool {
		return len(e.Logs()) == 2
	}, time.Second, 10*time.Millisecond)

	assert.Len(t, e.LogsForPolicy("policy.a"), 1)
	assert.Len(t, e.LogsForPolicy("policy.b"), 1)
}

func TestEngineCloseDoesNotInterruptCompleteScripts(t *testing.T) {
	home := newFakeHomeAPI()
	e, r := newTestEngine(t, home, WithExecutionTimeout(10*time.Second))
	trigger := registerManualTrigger(t, r, "trigger")

	require.NoError(t, e.Register(&Policy{
		ID:               "test.complete-survives-close",
		ConditionExpr:    Use("trigger", struct{}{}),
		Script:           `local x = 0; for i = 1, 500000 do x = x + 1 end; home.setLight("light.done", true)`,
		OnConditionFalse: Complete,
	}))

	trigger.set(true)
	time.Sleep(5 * time.Millisecond) // let the script actually start, but not finish

	e.Close()

	require.Eventually(t, func() bool {
		return len(e.LogsForPolicy("test.complete-survives-close")) == 1
	}, 5*time.Second, 10*time.Millisecond)

	logs := e.LogsForPolicy("test.complete-survives-close")
	assert.Equal(t, StatusSuccess, logs[0].Status, "a Complete policy's running script must survive engine Close")
	assert.True(t, home.getLight("light.done"))
}

func TestEngineCloseInterruptsRunningInterruptScripts(t *testing.T) {
	home := newFakeHomeAPI()
	e, r := newTestEngine(t, home, WithExecutionTimeout(10*time.Second))
	trigger := registerManualTrigger(t, r, "trigger")

	require.NoError(t, e.Register(&Policy{
		ID:               "test.interrupt-on-close",
		ConditionExpr:    Use("trigger", struct{}{}),
		Script:           `while true do end`,
		OnConditionFalse: Interrupt,
	}))

	trigger.set(true)
	time.Sleep(20 * time.Millisecond)

	e.Close()

	require.Eventually(t, func() bool {
		return len(e.LogsForPolicy("test.interrupt-on-close")) == 1
	}, time.Second, 10*time.Millisecond)

	logs := e.LogsForPolicy("test.interrupt-on-close")
	assert.Equal(t, StatusError, logs[0].Status)
}

func TestEngineRegisterCopiesPolicyNotAliasesIt(t *testing.T) {
	home := newFakeHomeAPI()
	e, r := newTestEngine(t, home)
	trigger := registerManualTrigger(t, r, "trigger")

	p := &Policy{
		ID:            "test.no-alias",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        `home.setLight("light.original", true)`,
	}
	require.NoError(t, e.Register(p))

	// Mutate the caller's Policy after Register: this must not affect what
	// the engine actually runs.
	p.Script = `home.setLight("light.mutated", true)`

	trigger.set(true)

	require.Eventually(t, func() bool {
		return home.getLight("light.original")
	}, time.Second, 10*time.Millisecond)
	assert.False(t, home.getLight("light.mutated"))
}

func TestEngineRegisterSurvivesCallerMutatingIDAfterward(t *testing.T) {
	home := newFakeHomeAPI()
	e, r := newTestEngine(t, home)
	trigger := registerManualTrigger(t, r, "trigger")

	p := &Policy{
		ID:            "test.id-mutated",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        `home.setLight("light.x", true)`,
	}
	require.NoError(t, e.Register(p))

	// A caller reusing or cleaning up their own *Policy after Register
	// returns must not affect what's already registered — including a
	// condition transition that fires well after this mutation.
	p.ID = "mutated-away"

	trigger.set(true)

	require.Eventually(t, func() bool {
		return home.getLight("light.x")
	}, time.Second, 10*time.Millisecond)

	logs := e.LogsForPolicy("test.id-mutated")
	require.Len(t, logs, 1)
	assert.Equal(t, StatusSuccess, logs[0].Status)
}

func TestEngineUnregisterConditionType(t *testing.T) {
	home := newFakeHomeAPI()
	e, r := newTestEngine(t, home)
	trigger := registerManualTrigger(t, r, "trigger")

	require.NoError(t, e.Register(&Policy{
		ID:            "test.uses-trigger",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        "x=1",
	}))

	e.UnregisterConditionType("trigger")

	// The type is gone: a fresh Register referencing it must now fail...
	assert.Error(t, e.Register(&Policy{
		ID:            "test.uses-trigger-2",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        "x=1",
	}))

	// ...but the already-running policy that referenced it is unaffected.
	trigger.set(true)
	require.Eventually(t, func() bool {
		return len(e.LogsForPolicy("test.uses-trigger")) == 1
	}, time.Second, 10*time.Millisecond)
}

func TestEngineUnregisterConditionTypeWarnsWhenReferenced(t *testing.T) {
	home := newFakeHomeAPI()
	core, recorded := observer.New(zap.WarnLevel)
	e, r := newTestEngine(t, home)
	e.logger = zap.New(core)
	registerManualTrigger(t, r, "trigger")

	require.NoError(t, e.Register(&Policy{
		ID:            "test.uses-trigger",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        "x=1",
	}))

	e.UnregisterConditionType("trigger")

	entries := recorded.All()
	require.Len(t, entries, 1)
	assert.Contains(t, entries[0].Message, "condition type unregistered")
}

func TestEnginePoliciesAndPolicySnapshot(t *testing.T) {
	e, r := newTestEngine(t, newFakeHomeAPI())
	trigger := registerManualTrigger(t, r, "trigger")

	_, ok := e.Policy("test.snapshot")
	assert.False(t, ok)
	assert.Empty(t, e.Policies())

	require.NoError(t, e.Register(&Policy{
		ID:               "test.snapshot",
		ConditionExpr:    Use("trigger", struct{}{}),
		Script:           "x=1",
		OnConditionFalse: Complete,
	}))

	info, ok := e.Policy("test.snapshot")
	require.True(t, ok)
	assert.Equal(t, "test.snapshot", info.ID)
	assert.Equal(t, "x=1", info.Script)
	assert.Equal(t, Complete, info.OnConditionFalse)
	assert.False(t, info.Active)

	all := e.Policies()
	require.Len(t, all, 1)
	assert.Equal(t, "test.snapshot", all[0].ID)

	trigger.set(true)
	require.Eventually(t, func() bool {
		info, _ := e.Policy("test.snapshot")
		return info.Active
	}, time.Second, 10*time.Millisecond)

	require.NoError(t, e.Unregister("test.snapshot"))
	_, ok = e.Policy("test.snapshot")
	assert.False(t, ok)
}

func TestEngineSimulate(t *testing.T) {
	e, r := newTestEngine(t, newFakeHomeAPI())
	trigger := registerManualTrigger(t, r, "trigger")

	_, ok := e.Simulate("test.sim")
	assert.False(t, ok)

	require.NoError(t, e.Register(&Policy{
		ID:            "test.sim",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        "x=1",
	}))

	result, ok := e.Simulate("test.sim")
	require.True(t, ok)
	assert.False(t, result)

	trigger.set(true)
	require.Eventually(t, func() bool {
		result, _ := e.Simulate("test.sim")
		return result
	}, time.Second, 10*time.Millisecond)
}

func TestEngineUpdateDeviceStateFeedsCache(t *testing.T) {
	e, _ := newTestEngine(t, newFakeHomeAPI())

	_, ok := e.GetLastKnown("sensor.temp")
	assert.False(t, ok)

	e.UpdateDeviceState("sensor.temp", 21.5)

	v, ok := e.GetLastKnown("sensor.temp")
	require.True(t, ok)
	assert.Equal(t, 21.5, v)
}
