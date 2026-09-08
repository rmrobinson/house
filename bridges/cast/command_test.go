package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/bridges/lib/castctrl"
)

// fakeSession implements castSession for pure-logic tests of dispatchCommand
// and setVolumeAbsolute, with no live device or TLS connection involved.
type fakeSession struct {
	status castctrl.Status

	playCalled, pauseCalled, stopCalled bool
	seekCalls                           []float64
	volumeCalls                         []float64
	muteCalls                           []bool
	launchCalls                         []string

	err error // returned by every method, if set
}

func (f *fakeSession) Run(ctx context.Context) {}

func (f *fakeSession) Status() castctrl.Status { return f.status }

func (f *fakeSession) Play(ctx context.Context) error {
	f.playCalled = true
	return f.err
}

func (f *fakeSession) Pause(ctx context.Context) error {
	f.pauseCalled = true
	return f.err
}

func (f *fakeSession) StopMedia(ctx context.Context) error {
	f.stopCalled = true
	return f.err
}

func (f *fakeSession) SeekAbsolute(ctx context.Context, positionS float64) error {
	f.seekCalls = append(f.seekCalls, positionS)
	return f.err
}

func (f *fakeSession) SetVolumeLevel(ctx context.Context, level float64) error {
	f.volumeCalls = append(f.volumeCalls, level)
	if f.err != nil {
		return f.err
	}
	// Reflect the write back into status, the way a real device's echoed
	// RECEIVER_STATUS would — setVolumeAbsolute's step-synthesis loop reads
	// Status() fresh before computing each step's direction/target.
	f.status.Volume.Level = level
	return nil
}

func (f *fakeSession) SetMuted(ctx context.Context, muted bool) error {
	f.muteCalls = append(f.muteCalls, muted)
	return f.err
}

func (f *fakeSession) LaunchApp(ctx context.Context, appID string) error {
	f.launchCalls = append(f.launchCalls, appID)
	return f.err
}

func TestDispatchCommand_Playback(t *testing.T) {
	tests := []struct {
		name   string
		action command.Playback_Action
		check  func(t *testing.T, f *fakeSession)
	}{
		{"play", command.Playback_ACTION_PLAY, func(t *testing.T, f *fakeSession) { assert.True(t, f.playCalled) }},
		{"pause", command.Playback_ACTION_PAUSE, func(t *testing.T, f *fakeSession) { assert.True(t, f.pauseCalled) }},
		{"stop", command.Playback_ACTION_STOP, func(t *testing.T, f *fakeSession) { assert.True(t, f.stopCalled) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeSession{}
			cd := &castDevice{session: f}
			err := dispatchCommand(context.Background(), cd, &command.Command{
				Details: &command.Command_Playback{Playback: &command.Playback{Action: tt.action}},
			})
			require.NoError(t, err)
			tt.check(t, f)
		})
	}
}

func TestDispatchCommand_SeekAbsolute(t *testing.T) {
	f := &fakeSession{}
	cd := &castDevice{session: f}
	err := dispatchCommand(context.Background(), cd, &command.Command{
		Details: &command.Command_SeekAbsolute{SeekAbsolute: &command.SeekAbsolute{PositionS: 42}},
	})
	require.NoError(t, err)
	assert.Equal(t, []float64{42}, f.seekCalls)
}

func TestDispatchCommand_SeekRelative_ClampedToDuration(t *testing.T) {
	f := &fakeSession{status: castctrl.Status{
		Media: castctrl.MediaStatus{
			CurrentTime: 190,
			Media:       &castctrl.MediaInformation{Duration: 200},
		},
	}}
	cd := &castDevice{session: f}
	err := dispatchCommand(context.Background(), cd, &command.Command{
		Details: &command.Command_SeekRelative{SeekRelative: &command.SeekRelative{DeltaS: 30}}, // would overshoot
	})
	require.NoError(t, err)
	require.Len(t, f.seekCalls, 1)
	assert.Equal(t, 200.0, f.seekCalls[0])
}

func TestDispatchCommand_SeekRelative_ClampedToZero(t *testing.T) {
	f := &fakeSession{status: castctrl.Status{
		Media: castctrl.MediaStatus{CurrentTime: 5, Media: &castctrl.MediaInformation{Duration: 200}},
	}}
	cd := &castDevice{session: f}
	err := dispatchCommand(context.Background(), cd, &command.Command{
		Details: &command.Command_SeekRelative{SeekRelative: &command.SeekRelative{DeltaS: -30}},
	})
	require.NoError(t, err)
	require.Len(t, f.seekCalls, 1)
	assert.Equal(t, 0.0, f.seekCalls[0])
}

func TestDispatchCommand_Mute(t *testing.T) {
	f := &fakeSession{}
	cd := &castDevice{session: f}
	err := dispatchCommand(context.Background(), cd, &command.Command{
		Details: &command.Command_Mute{Mute: &command.Mute{IsMuted: true}},
	})
	require.NoError(t, err)
	assert.Equal(t, []bool{true}, f.muteCalls)
}

func TestDispatchCommand_AppLaunch(t *testing.T) {
	f := &fakeSession{}
	cd := &castDevice{session: f}
	err := dispatchCommand(context.Background(), cd, &command.Command{
		Details: &command.Command_AppLaunch{AppLaunch: &command.AppLaunch{AppId: "233637DE"}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"233637DE"}, f.launchCalls)
}

func TestDispatchCommand_VolumeAbsolute_Attenuation_SingleCall(t *testing.T) {
	f := &fakeSession{status: castctrl.Status{
		Volume: castctrl.ReceiverVolume{Level: 0.5, ControlType: "attenuation", StepInterval: 0.1},
	}}
	cd := &castDevice{session: f}
	err := dispatchCommand(context.Background(), cd, &command.Command{
		Details: &command.Command_VolumeAbsolute{VolumeAbsolute: &command.VolumeAbsolute{Level: 8}},
	})
	require.NoError(t, err)
	require.Len(t, f.volumeCalls, 1)
	assert.InDelta(t, 0.8, f.volumeCalls[0], 0.001)
}

func TestDispatchCommand_VolumeAbsolute_Master_StepSynthesis(t *testing.T) {
	// maximumLevel = round(1/0.02) = 50; current level = round(0.5*50) = 25.
	f := &fakeSession{status: castctrl.Status{
		Volume: castctrl.ReceiverVolume{Level: 0.5, ControlType: "master", StepInterval: 0.02},
	}}
	cd := &castDevice{session: f}
	err := dispatchCommand(context.Background(), cd, &command.Command{
		Details: &command.Command_VolumeAbsolute{VolumeAbsolute: &command.VolumeAbsolute{Level: 28}},
	})
	require.NoError(t, err)
	// Three single-step calls (25 -> 26 -> 27 -> 28), not one jump straight
	// to the target — that's the whole point of step-synthesis on "master".
	require.Len(t, f.volumeCalls, 3)
	assert.InDelta(t, 26.0/50.0, f.volumeCalls[0], 0.001)
	assert.InDelta(t, 27.0/50.0, f.volumeCalls[1], 0.001)
	assert.InDelta(t, 28.0/50.0, f.volumeCalls[2], 0.001)
}

func TestDispatchCommand_VolumeAbsolute_Master_StepSynthesis_Downward(t *testing.T) {
	f := &fakeSession{status: castctrl.Status{
		Volume: castctrl.ReceiverVolume{Level: 0.5, ControlType: "master", StepInterval: 0.02}, // level 25/50
	}}
	cd := &castDevice{session: f}
	err := dispatchCommand(context.Background(), cd, &command.Command{
		Details: &command.Command_VolumeAbsolute{VolumeAbsolute: &command.VolumeAbsolute{Level: 22}},
	})
	require.NoError(t, err)
	require.Len(t, f.volumeCalls, 3)
	assert.InDelta(t, 24.0/50.0, f.volumeCalls[0], 0.001)
	assert.InDelta(t, 23.0/50.0, f.volumeCalls[1], 0.001)
	assert.InDelta(t, 22.0/50.0, f.volumeCalls[2], 0.001)
}

func TestDispatchCommand_VolumeRelative(t *testing.T) {
	f := &fakeSession{status: castctrl.Status{
		Volume: castctrl.ReceiverVolume{Level: 0.5, ControlType: "attenuation", StepInterval: 0.1}, // level 5/10
	}}
	cd := &castDevice{session: f}
	err := dispatchCommand(context.Background(), cd, &command.Command{
		Details: &command.Command_VolumeRelative{VolumeRelative: &command.VolumeRelative{Delta: 2}},
	})
	require.NoError(t, err)
	require.Len(t, f.volumeCalls, 1)
	assert.InDelta(t, 0.7, f.volumeCalls[0], 0.001) // 5+2 = 7/10
}

func TestDispatchCommand_VolumeAbsolute_ClampedToRange(t *testing.T) {
	f := &fakeSession{status: castctrl.Status{
		Volume: castctrl.ReceiverVolume{Level: 0.1, ControlType: "attenuation", StepInterval: 0.1},
	}}
	cd := &castDevice{session: f}
	err := dispatchCommand(context.Background(), cd, &command.Command{
		Details: &command.Command_VolumeAbsolute{VolumeAbsolute: &command.VolumeAbsolute{Level: 999}},
	})
	require.NoError(t, err)
	require.Len(t, f.volumeCalls, 1)
	assert.InDelta(t, 1.0, f.volumeCalls[0], 0.001) // clamped to maximumLevel=10 -> 1.0
}

func TestTranslateErr(t *testing.T) {
	assert.Nil(t, translateErr(nil))

	assert.Equal(t, codes.DeadlineExceeded, status.Code(translateErr(context.DeadlineExceeded)))
	assert.Equal(t, codes.Canceled, status.Code(translateErr(context.Canceled)))
	assert.Equal(t, codes.Unavailable, status.Code(translateErr(castctrl.ErrNoActiveApp)))
}

func TestMaximumLevel(t *testing.T) {
	assert.Equal(t, int32(50), maximumLevel(0.02))
	assert.Equal(t, int32(10), maximumLevel(0.1))
	assert.Equal(t, int32(1), maximumLevel(0))
	assert.Equal(t, int32(1), maximumLevel(-1))
}

func TestCommandDeadline(t *testing.T) {
	t.Run("single round trip uses commandTimeout", func(t *testing.T) {
		cd := &castDevice{session: &fakeSession{status: castctrl.Status{
			Volume: castctrl.ReceiverVolume{Level: 0.5, ControlType: "attenuation", StepInterval: 0.1},
		}}}
		cmd := &command.Command{Details: &command.Command_Mute{Mute: &command.Mute{IsMuted: true}}}
		assert.Equal(t, commandTimeout, commandDeadline(cd, cmd))
	})

	t.Run("attenuation absolute volume is a single round trip regardless of distance", func(t *testing.T) {
		cd := &castDevice{session: &fakeSession{status: castctrl.Status{
			Volume: castctrl.ReceiverVolume{Level: 0, ControlType: "attenuation", StepInterval: 0.02},
		}}}
		cmd := &command.Command{Details: &command.Command_VolumeAbsolute{VolumeAbsolute: &command.VolumeAbsolute{Level: 50}}}
		assert.Equal(t, commandTimeout, commandDeadline(cd, cmd))
	})

	t.Run("master step-synthesis scales the deadline by step count", func(t *testing.T) {
		// maximumLevel = 50, current level = 0, target 50 -> 50 steps.
		cd := &castDevice{session: &fakeSession{status: castctrl.Status{
			Volume: castctrl.ReceiverVolume{Level: 0, ControlType: "master", StepInterval: 0.02},
		}}}
		cmd := &command.Command{Details: &command.Command_VolumeAbsolute{VolumeAbsolute: &command.VolumeAbsolute{Level: 50}}}
		assert.Equal(t, 50*perStepTimeout, commandDeadline(cd, cmd))
	})

	t.Run("master step-synthesis for a short jump still floors at commandTimeout", func(t *testing.T) {
		// 2 steps * perStepTimeout (2s) = 4s, less than commandTimeout (5s).
		cd := &castDevice{session: &fakeSession{status: castctrl.Status{
			Volume: castctrl.ReceiverVolume{Level: 0.5, ControlType: "master", StepInterval: 0.02},
		}}}
		cmd := &command.Command{Details: &command.Command_VolumeAbsolute{VolumeAbsolute: &command.VolumeAbsolute{Level: 27}}}
		assert.Equal(t, commandTimeout, commandDeadline(cd, cmd))
	})

	t.Run("master volume relative scales the same as absolute", func(t *testing.T) {
		cd := &castDevice{session: &fakeSession{status: castctrl.Status{
			Volume: castctrl.ReceiverVolume{Level: 0, ControlType: "master", StepInterval: 0.02},
		}}}
		cmd := &command.Command{Details: &command.Command_VolumeRelative{VolumeRelative: &command.VolumeRelative{Delta: 40}}}
		assert.Equal(t, 40*perStepTimeout, commandDeadline(cd, cmd))
	})
}

func TestClampLevel(t *testing.T) {
	assert.Equal(t, int32(0), clampLevel(-5, 10))
	assert.Equal(t, int32(10), clampLevel(15, 10))
	assert.Equal(t, int32(5), clampLevel(5, 10))
}
