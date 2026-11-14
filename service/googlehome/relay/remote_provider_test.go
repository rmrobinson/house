package relay

import (
	"testing"

	action "github.com/rmrobinson/google-smart-home-action-go"
	"github.com/stretchr/testify/assert"
)

func TestMergeSuccessGroups_Empty(t *testing.T) {
	_, _, ok := mergeSuccessGroups(nil)
	assert.False(t, ok)
}

func TestMergeSuccessGroups_SingleOrdinaryGroup(t *testing.T) {
	state := action.NewDeviceState(true)
	var groups []stateGroup
	groups = addSuccessGroup(groups, []string{"light1"}, state, false)

	deviceIDs, got, ok := mergeSuccessGroups(groups)
	assert.True(t, ok)
	assert.Equal(t, state, got)
	assert.ElementsMatch(t, []string{"light1"}, deviceIDs)
}

// Original behaviour, preserved: two ordinary devices with different
// states both still get reported as "updated" (one of them cosmetically
// stale until the next ReportState/QUERY), rather than silently dropping
// the losing one.
func TestMergeSuccessGroups_OrdinaryLoserStillRidesAlong(t *testing.T) {
	winner := action.NewDeviceState(true).RecordOnOff(true)
	loser := action.NewDeviceState(true).RecordOnOff(false)

	var groups []stateGroup
	groups = addSuccessGroup(groups, []string{"light1", "light2"}, winner, false)
	groups = addSuccessGroup(groups, []string{"light3"}, loser, false)

	deviceIDs, got, ok := mergeSuccessGroups(groups)
	assert.True(t, ok)
	assert.Equal(t, winner, got)
	assert.ElementsMatch(t, []string{"light1", "light2", "light3"}, deviceIDs)
}

// A losing camera group must never ride under an ordinary winner's
// state - that would report a foreign (non-camera) state for a
// GetCameraStream device.
func TestMergeSuccessGroups_LosingCameraExcludedUnderOrdinaryWinner(t *testing.T) {
	winner := action.NewDeviceState(true).RecordOnOff(true)
	cameraState := action.NewDeviceState(true)
	cameraState.State["cameraStreamAuthToken"] = "secret-for-camera1"

	var groups []stateGroup
	groups = addSuccessGroup(groups, []string{"light1", "light2"}, winner, false)
	groups = addSuccessGroup(groups, []string{"camera1"}, cameraState, true)

	deviceIDs, got, ok := mergeSuccessGroups(groups)
	assert.True(t, ok)
	assert.Equal(t, winner, got)
	assert.ElementsMatch(t, []string{"light1", "light2"}, deviceIDs)
	assert.NotContains(t, deviceIDs, "camera1")
}

// The regression this test guards: when the WINNING group is itself a
// camera session, every other (ordinary) device must be excluded too -
// not just protected in the other direction. Before the fix, only
// g.isCamera was checked, so an ordinary device would ride along under
// a winning camera's state, exposing that camera's session token
// (cameraStreamAuthToken/cameraStreamSignalingUrl) as the reported
// result for an unrelated device.
func TestMergeSuccessGroups_OrdinaryDeviceExcludedUnderCameraWinner(t *testing.T) {
	cameraState := action.NewDeviceState(true)
	cameraState.State["cameraStreamAuthToken"] = "secret-for-camera1"
	ordinary := action.NewDeviceState(true).RecordOnOff(true)

	var groups []stateGroup
	// The camera group is larger, so it wins the size-based tie-break.
	groups = addSuccessGroup(groups, []string{"camera1", "camera-also-device-2"}, cameraState, true)
	groups = addSuccessGroup(groups, []string{"light1"}, ordinary, false)

	deviceIDs, got, ok := mergeSuccessGroups(groups)
	assert.True(t, ok)
	assert.Equal(t, cameraState, got)
	assert.ElementsMatch(t, []string{"camera1", "camera-also-device-2"}, deviceIDs)
	assert.NotContains(t, deviceIDs, "light1")
}

// A second regression case: two DISTINCT camera sessions (different
// tokens) in the same batch - the losing one must not ride along under
// the winning one's state either, even though both are isCamera.
func TestMergeSuccessGroups_LosingCameraExcludedUnderDifferentCameraWinner(t *testing.T) {
	winnerState := action.NewDeviceState(true)
	winnerState.State["cameraStreamAuthToken"] = "secret-for-camera1"
	loserState := action.NewDeviceState(true)
	loserState.State["cameraStreamAuthToken"] = "secret-for-camera2"

	var groups []stateGroup
	groups = addSuccessGroup(groups, []string{"camera1", "camera1b"}, winnerState, true)
	groups = addSuccessGroup(groups, []string{"camera2"}, loserState, true)

	deviceIDs, got, ok := mergeSuccessGroups(groups)
	assert.True(t, ok)
	assert.Equal(t, winnerState, got)
	assert.ElementsMatch(t, []string{"camera1", "camera1b"}, deviceIDs)
	assert.NotContains(t, deviceIDs, "camera2")
}

func TestAddSuccessGroup_MergesSameKindEqualState(t *testing.T) {
	state := action.NewDeviceState(true)
	var groups []stateGroup
	groups = addSuccessGroup(groups, []string{"light1"}, state, false)
	groups = addSuccessGroup(groups, []string{"light2"}, state, false)

	assert.Len(t, groups, 1)
	assert.ElementsMatch(t, []string{"light1", "light2"}, groups[0].deviceIDs)
}

// Two camera sessions never merge into one group even if (hypothetically)
// their states were equal, since isCamera groups are kept distinct per
// device by construction (unique tokens) - but addSuccessGroup itself
// only guards kind equality, not uniqueness; this documents that a
// same-kind, equal-state pair DOES merge, which is fine for ordinary
// devices and relies on token uniqueness to never actually occur for
// camera ones (see remote_provider.go's doc comment on stateGroup).
func TestAddSuccessGroup_DoesNotMergeDifferentKinds(t *testing.T) {
	state := action.NewDeviceState(true)
	var groups []stateGroup
	groups = addSuccessGroup(groups, []string{"light1"}, state, false)
	groups = addSuccessGroup(groups, []string{"camera1"}, state, true)

	assert.Len(t, groups, 2)
}
