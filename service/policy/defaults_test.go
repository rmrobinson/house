package policy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadSystemPoliciesOccupancyOnMotion(t *testing.T) {
	home := newFakeHomeAPI()
	e, _ := newTestEngine(t, home)

	require.NoError(t, LoadSystemPolicies(e))

	e.Bus().Publish(Event{Topic: "motion.detected"})

	require.Eventually(t, func() bool {
		return home.getHouseState("occupancy") == "occupied"
	}, time.Second, 10*time.Millisecond)
}

// TestLoadSystemPoliciesPowerRestore covers sys.power-restore's loop over
// every enumerated "light" device (see home.findDevices in bindings.go):
// two lights in different prior states, both simulated as having powered
// back on by themselves (a common firmware default), plus a non-light
// device that findDevices("light") must exclude.
func TestLoadSystemPoliciesPowerRestore(t *testing.T) {
	home := newFakeHomeAPI()
	e, _ := newTestEngine(t, home)

	// "on_off.state.is_on", not "light.on_off.state.is_on": bridgehome.Adapter.GetState's real
	// dotted path already starts inside the device's populated trait message (Light, Sensor, ...)
	// - see its doc comment - so a leading "light." segment doesn't resolve against a real
	// backend. fakeHomeAPI stores whatever key string it's given and would pass this test either
	// way, which is exactly how this mismatch shipped undetected; matching the real convention
	// here is what keeps this test meaningful.
	e.UpdateDeviceState("light.living_room", "light", nil)
	require.NoError(t, home.SetState("light.living_room", "on_off.state.is_on", true))
	e.UpdateDeviceState("light.porch", "light", nil)
	require.NoError(t, home.SetState("light.porch", "on_off.state.is_on", false))
	e.UpdateDeviceState("sensor.hallway", "sensor", nil)

	require.NoError(t, LoadSystemPolicies(e))

	require.NoError(t, home.SetLight("light.living_room", true))
	require.NoError(t, home.SetLight("light.porch", true))

	e.Bus().Publish(Event{Topic: "power.restored"})

	require.Eventually(t, func() bool {
		return home.getLight("light.living_room") && !home.getLight("light.porch")
	}, time.Second, 10*time.Millisecond)
}

func TestLoadSystemPoliciesTwiceOnSameEnginePanics(t *testing.T) {
	e, _ := newTestEngine(t, newFakeHomeAPI())
	require.NoError(t, LoadSystemPolicies(e))

	assert.Panics(t, func() {
		_ = LoadSystemPolicies(e)
	})
}

func TestSystemPolicyOverride(t *testing.T) {
	home := newFakeHomeAPI()
	e, _ := newTestEngine(t, home)
	require.NoError(t, LoadSystemPolicies(e))

	// A user can silently replace a system policy ID.
	require.NoError(t, e.Register(&Policy{
		ID:            "sys.occupancy",
		ConditionExpr: Use("sys.any-motion-detected", struct{}{}),
		Script:        `home.setHouseState("occupancy", "override")`,
	}))

	e.Bus().Publish(Event{Topic: "motion.detected"})

	require.Eventually(t, func() bool {
		return home.getHouseState("occupancy") == "override"
	}, time.Second, 10*time.Millisecond)
}
