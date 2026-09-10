package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/bridges/lib/webosctrl"
)

// fakeSession is a webosSession that records calls and returns configured
// results, letting dispatchCommand be tested without a live connection.
type fakeSession struct {
	status webosctrl.Status
	err    error

	lastVolume  int32
	lastMuted   bool
	lastApp     string
	lastClose   string
	lastChannel string
	lastDelta   int32
	turnedOn    bool
	turnedOff   bool
	poweredOff  bool

	// turnOffScreenErr, if set, overrides err for TurnOffScreen only - lets
	// tests exercise the PowerOff fallback without every other call failing.
	turnOffScreenErr error
}

func (f *fakeSession) Status() webosctrl.Status { return f.status }
func (f *fakeSession) TurnOnScreen(ctx context.Context) error {
	f.turnedOn = true
	return f.err
}
func (f *fakeSession) TurnOffScreen(ctx context.Context) error {
	f.turnedOff = true
	if f.turnOffScreenErr != nil {
		return f.turnOffScreenErr
	}
	return f.err
}
func (f *fakeSession) PowerOff(ctx context.Context) error {
	f.poweredOff = true
	return f.err
}
func (f *fakeSession) SetVolumeAbsolute(ctx context.Context, level int32) error {
	f.lastVolume = level
	return f.err
}
func (f *fakeSession) SetMuted(ctx context.Context, muted bool) error {
	f.lastMuted = muted
	return f.err
}
func (f *fakeSession) LaunchApp(ctx context.Context, appID string) error {
	f.lastApp = appID
	return f.err
}
func (f *fakeSession) CloseApp(ctx context.Context, appID string) error {
	f.lastClose = appID
	return f.err
}
func (f *fakeSession) SetChannel(ctx context.Context, channelNumber string) error {
	f.lastChannel = channelNumber
	return f.err
}
func (f *fakeSession) ChannelStep(ctx context.Context, delta int32) error {
	f.lastDelta = delta
	return f.err
}

func TestDispatchCommand_OnOff_TurnsOnDirectlyWhenReachable(t *testing.T) {
	fs := &fakeSession{status: webosctrl.Status{Reachable: true}}
	wd := &webosDevice{session: fs}

	err := dispatchCommand(context.Background(), wd, &command.Command{Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}}})
	require.NoError(t, err)
	assert.True(t, fs.turnedOn)
}

func TestDispatchCommand_OnOff_Off(t *testing.T) {
	fs := &fakeSession{status: webosctrl.Status{Reachable: true}}
	wd := &webosDevice{session: fs}

	err := dispatchCommand(context.Background(), wd, &command.Command{Details: &command.Command_OnOff{OnOff: &command.OnOff{On: false}}})
	require.NoError(t, err)
	assert.True(t, fs.turnedOff)
}

func TestDispatchCommand_OnOff_Off_FallsBackToPowerOffWhenScreenOffUnsupported(t *testing.T) {
	fs := &fakeSession{
		status:           webosctrl.Status{Reachable: true},
		turnOffScreenErr: fmt.Errorf("%w: 404 no such service or method", webosctrl.ErrRequestRejected),
	}
	wd := &webosDevice{session: fs}

	err := dispatchCommand(context.Background(), wd, &command.Command{Details: &command.Command_OnOff{OnOff: &command.OnOff{On: false}}})
	require.NoError(t, err)
	assert.True(t, fs.turnedOff)
	assert.True(t, fs.poweredOff)
}

func TestDispatchCommand_OnOff_UnreachableWithNoMACFails(t *testing.T) {
	fs := &fakeSession{status: webosctrl.Status{Reachable: false}}
	wd := &webosDevice{session: fs, cfg: deviceConfig{MAC: ""}}

	err := dispatchCommand(context.Background(), wd, &command.Command{Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}}})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.False(t, fs.turnedOn)
}

func TestDispatchCommand_VolumeAbsolute(t *testing.T) {
	fs := &fakeSession{status: webosctrl.Status{Reachable: true}}
	wd := &webosDevice{session: fs}

	err := dispatchCommand(context.Background(), wd, &command.Command{Details: &command.Command_VolumeAbsolute{VolumeAbsolute: &command.VolumeAbsolute{Level: 30}}})
	require.NoError(t, err)
	assert.EqualValues(t, 30, fs.lastVolume)
}

func TestDispatchCommand_VolumeRelative_ClampsToRange(t *testing.T) {
	cases := []struct {
		name    string
		current int32
		delta   int32
		want    int32
	}{
		{"clamps above max", 95, 20, 100},
		{"clamps below zero", 5, -20, 0},
		{"normal step", 50, 5, 55},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &fakeSession{status: webosctrl.Status{Reachable: true, Volume: c.current}}
			wd := &webosDevice{session: fs}

			err := dispatchCommand(context.Background(), wd, &command.Command{Details: &command.Command_VolumeRelative{VolumeRelative: &command.VolumeRelative{Delta: c.delta}}})
			require.NoError(t, err)
			assert.Equal(t, c.want, fs.lastVolume)
		})
	}
}

func TestDispatchCommand_Mute(t *testing.T) {
	fs := &fakeSession{status: webosctrl.Status{Reachable: true}}
	wd := &webosDevice{session: fs}

	err := dispatchCommand(context.Background(), wd, &command.Command{Details: &command.Command_Mute{Mute: &command.Mute{IsMuted: true}}})
	require.NoError(t, err)
	assert.True(t, fs.lastMuted)
}

func TestDispatchCommand_AppLaunchAndClose(t *testing.T) {
	fs := &fakeSession{status: webosctrl.Status{Reachable: true, ForegroundID: "netflix"}}
	wd := &webosDevice{session: fs}

	err := dispatchCommand(context.Background(), wd, &command.Command{Details: &command.Command_AppLaunch{AppLaunch: &command.AppLaunch{ApplicationId: "youtube.leanback.v4"}}})
	require.NoError(t, err)
	assert.Equal(t, "youtube.leanback.v4", fs.lastApp)

	err = dispatchCommand(context.Background(), wd, &command.Command{Details: &command.Command_AppClose{AppClose: &command.AppClose{}}})
	require.NoError(t, err)
	assert.Equal(t, "netflix", fs.lastClose)
}

func TestDispatchCommand_Channel(t *testing.T) {
	fs := &fakeSession{status: webosctrl.Status{Reachable: true}}
	wd := &webosDevice{session: fs}

	err := dispatchCommand(context.Background(), wd, &command.Command{Details: &command.Command_ChannelAbsolute{ChannelAbsolute: &command.ChannelAbsolute{Channel: "4-1"}}})
	require.NoError(t, err)
	assert.Equal(t, "4-1", fs.lastChannel)

	err = dispatchCommand(context.Background(), wd, &command.Command{Details: &command.Command_ChannelRelative{ChannelRelative: &command.ChannelRelative{Delta: -1}}})
	require.NoError(t, err)
	assert.EqualValues(t, -1, fs.lastDelta)
}

func TestDispatchCommand_UnrecognizedCommand(t *testing.T) {
	fs := &fakeSession{status: webosctrl.Status{Reachable: true}}
	wd := &webosDevice{session: fs}

	err := dispatchCommand(context.Background(), wd, &command.Command{})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestTranslateErr(t *testing.T) {
	assert.NoError(t, translateErr(nil))
	assert.Equal(t, codes.FailedPrecondition, status.Code(translateErr(webosctrl.ErrPairingRequired)))
	assert.Equal(t, codes.FailedPrecondition, status.Code(translateErr(webosctrl.ErrRequestRejected)))
	assert.Equal(t, codes.Internal, status.Code(translateErr(errors.New("boom"))))
}

// TestTranslateErr_WrapsRealShapeOfRequestRejected covers the actual shape
// session.go's request()/register() produce - ErrRequestRejected wrapped via
// fmt.Errorf("%w: %s", ...) to carry the device's error text - not the bare
// sentinel TestTranslateErr checks above. A plain switch/== comparison
// against the bare sentinel never matches this, silently misclassifying
// every real device rejection as codes.Internal instead of
// codes.FailedPrecondition.
func TestTranslateErr_WrapsRealShapeOfRequestRejected(t *testing.T) {
	wrapped := fmt.Errorf("%w: 404 no such service or method", webosctrl.ErrRequestRejected)
	assert.Equal(t, codes.FailedPrecondition, status.Code(translateErr(wrapped)))
}
