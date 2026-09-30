package policy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func staticConditionType(r *ConditionRegistry, name string, value bool) {
	RegisterConditionType(r, name, func(_ struct{}) Condition {
		return NewPollingCondition(func() bool { return value }, time.Hour)
	})
}

func TestConditionExprAndOrNot(t *testing.T) {
	r := NewConditionRegistry()
	staticConditionType(r, "true-cond", true)
	staticConditionType(r, "false-cond", false)

	and, err := ExprAnd(Use("true-cond", struct{}{}), Use("false-cond", struct{}{})).build(r)
	require.NoError(t, err)
	assert.False(t, and.Evaluate())

	or, err := ExprOr(Use("true-cond", struct{}{}), Use("false-cond", struct{}{})).build(r)
	require.NoError(t, err)
	assert.True(t, or.Evaluate())

	not, err := ExprNot(Use("true-cond", struct{}{})).build(r)
	require.NoError(t, err)
	assert.False(t, not.Evaluate())
}

func TestConditionExprBuildErrorPropagates(t *testing.T) {
	r := NewConditionRegistry()
	staticConditionType(r, "true-cond", true)

	_, err := ExprAnd(Use("true-cond", struct{}{}), Use("unregistered", struct{}{})).build(r)
	assert.Error(t, err)

	_, err = ExprOr(Use("unregistered", struct{}{})).build(r)
	assert.Error(t, err)

	_, err = ExprNot(Use("unregistered", struct{}{})).build(r)
	assert.Error(t, err)
}

func TestConditionExprJSONRoundTrip(t *testing.T) {
	type houseModeParams struct {
		Mode string
	}
	type motionParams struct {
		SensorID string
	}

	r := NewConditionRegistry()
	var gotMode string
	var gotSensors []string
	RegisterConditionType(r, "house-mode-is", func(p houseModeParams) Condition {
		gotMode = p.Mode
		return NewPollingCondition(func() bool { return true }, time.Hour)
	})
	RegisterConditionType(r, "motion-detected", func(p motionParams) Condition {
		gotSensors = append(gotSensors, p.SensorID)
		return NewPollingCondition(func() bool { return false }, time.Hour)
	})

	expr := ExprAnd(
		Use("house-mode-is", houseModeParams{Mode: "vacation"}),
		ExprOr(
			Use("motion-detected", motionParams{SensorID: "front-door"}),
			Use("motion-detected", motionParams{SensorID: "garage"}),
		),
	)

	data, err := MarshalConditionExpr(expr)
	require.NoError(t, err)

	restored, err := UnmarshalConditionExpr(data, r)
	require.NoError(t, err)

	_, err = restored.build(r)
	require.NoError(t, err)

	assert.Equal(t, "vacation", gotMode)
	assert.ElementsMatch(t, []string{"front-door", "garage"}, gotSensors)
}

func TestUnmarshalConditionExprErrors(t *testing.T) {
	r := NewConditionRegistry()
	staticConditionType(r, "true-cond", true)

	_, err := UnmarshalConditionExpr([]byte(`{"type": "unknown"}`), r)
	assert.Error(t, err)

	_, err = UnmarshalConditionExpr([]byte(`{"type": "not", "children": []}`), r)
	assert.Error(t, err)

	_, err = UnmarshalConditionExpr([]byte(`{"type": "held-for", "children": []}`), r)
	assert.Error(t, err)

	_, err = UnmarshalConditionExpr([]byte(`{"type": "use", "name": "does-not-exist"}`), r)
	assert.Error(t, err)

	_, err = UnmarshalConditionExpr([]byte(`not json`), r)
	assert.Error(t, err)
}

func TestExprHeldForBuildsAHeldForCondition(t *testing.T) {
	r := NewConditionRegistry()
	staticConditionType(r, "true-cond", true)

	cond, err := ExprHeldFor(Use("true-cond", struct{}{}), 5*time.Minute).build(r)
	require.NoError(t, err)

	hf, ok := cond.(*HeldForCondition)
	require.True(t, ok)
	assert.Equal(t, 5*time.Minute, hf.duration)
}

func TestExprHeldForBuildErrorPropagates(t *testing.T) {
	r := NewConditionRegistry()
	_, err := ExprHeldFor(Use("unregistered", struct{}{}), time.Minute).build(r)
	assert.Error(t, err)
}

// TestExprHeldForJSONRoundTrip covers persistence: MarshalConditionExpr then
// UnmarshalConditionExpr must preserve both the wrapped expression and the
// duration.
func TestExprHeldForJSONRoundTrip(t *testing.T) {
	type onOffParams struct {
		DeviceID string
	}

	r := NewConditionRegistry()
	var gotDeviceID string
	RegisterConditionType(r, "is-on", func(p onOffParams) Condition {
		gotDeviceID = p.DeviceID
		return NewPollingCondition(func() bool { return true }, time.Hour)
	})

	expr := ExprHeldFor(Use("is-on", onOffParams{DeviceID: "light.kitchen"}), 5*time.Minute)

	data, err := MarshalConditionExpr(expr)
	require.NoError(t, err)

	restored, err := UnmarshalConditionExpr(data, r)
	require.NoError(t, err)

	cond, err := restored.build(r)
	require.NoError(t, err)

	assert.Equal(t, "light.kitchen", gotDeviceID)
	hf, ok := cond.(*HeldForCondition)
	require.True(t, ok)
	assert.Equal(t, 5*time.Minute, hf.duration)
}

func TestConditionExprNestedComposite(t *testing.T) {
	r := NewConditionRegistry()
	staticConditionType(r, "true-cond", true)
	staticConditionType(r, "false-cond", false)

	// true AND (false OR true)
	expr := ExprAnd(
		Use("true-cond", struct{}{}),
		ExprOr(
			Use("false-cond", struct{}{}),
			Use("true-cond", struct{}{}),
		),
	)

	cond, err := expr.build(r)
	require.NoError(t, err)
	assert.True(t, cond.Evaluate())
}
