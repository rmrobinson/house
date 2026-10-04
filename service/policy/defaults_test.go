package policy

import (
	_ "embed"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sysOccupancyScript/sysPowerRestoreScript are test-only: the engine ships
// no default policies (see RegisterSystemConditionTypes's doc comment) -
// these are the same example scripts kept at scripts/*.lua for an operator
// to paste into adminui, embedded here only so they're exercised by these
// tests.
//
//go:embed scripts/sys_occupancy.lua
var sysOccupancyScript string

//go:embed scripts/sys_power_restore.lua
var sysPowerRestoreScript string

func TestRegisterSystemConditionTypesRegistersAllThree(t *testing.T) {
	e, r := newTestEngine(t, newFakeHomeAPI())
	RegisterSystemConditionTypes(e)

	assert.Equal(t, []string{"sys.any-motion-detected", "sys.occupied", "sys.power-restored"}, r.TypeNames())
}

func TestRegisterSystemConditionTypesTwiceOnSameEnginePanics(t *testing.T) {
	e, _ := newTestEngine(t, newFakeHomeAPI())
	RegisterSystemConditionTypes(e)

	assert.Panics(t, func() {
		RegisterSystemConditionTypes(e)
	})
}

// TestUserPolicyOnOccupancyScript proves sysOccupancyScript still behaves
// correctly as an ordinary user policy built on the "sys.occupied"/
// "sys.any-motion-detected" condition types - no different from any policy
// a user enters through adminui.
func TestUserPolicyOnOccupancyScript(t *testing.T) {
	home := newFakeHomeAPI()
	e, _ := newTestEngine(t, home)
	RegisterSystemConditionTypes(e)

	require.NoError(t, e.Register(&Policy{
		ID:            "user.occupancy",
		ConditionExpr: ExprOr(Use("sys.occupied", struct{}{}), Use("sys.any-motion-detected", struct{}{})),
		Script:        sysOccupancyScript,
	}))

	require.NoError(t, home.SetHouseState("occupied", true))
	e.Bus().Publish(Event{Topic: HouseStateChangedTopic})

	require.Eventually(t, func() bool {
		return home.getHouseState("mode") == "home"
	}, time.Second, 10*time.Millisecond)
}

// TestUserPolicyOnPowerRestoreScript covers sysPowerRestoreScript's loop
// over every enumerated "light" device (see home.findDevices in
// bindings.go): two lights in different prior states, both simulated as
// having powered back on by themselves (a common firmware default), plus a
// non-light device that findDevices("light") must exclude.
func TestUserPolicyOnPowerRestoreScript(t *testing.T) {
	home := newFakeHomeAPI()
	e, _ := newTestEngine(t, home)
	RegisterSystemConditionTypes(e)

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

	require.NoError(t, e.Register(&Policy{
		ID:            "user.power-restore",
		ConditionExpr: Use("sys.power-restored", struct{}{}),
		Script:        sysPowerRestoreScript,
	}))

	require.NoError(t, home.SetLight("light.living_room", true))
	require.NoError(t, home.SetLight("light.porch", true))

	e.Bus().Publish(Event{Topic: "power.restored"})

	require.Eventually(t, func() bool {
		return home.getLight("light.living_room") && !home.getLight("light.porch")
	}, time.Second, 10*time.Millisecond)
}
