package bridge

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
)

// fakeHandler is a Handler whose command-processing behaviour is supplied per-test.
type fakeHandler struct {
	processCommandFn      func(ctx context.Context, cmd *command.Command) (*device.Device, error)
	processCommandAsyncFn func(ctx context.Context, cmd *command.Command) error
}

func (f *fakeHandler) SetBridgeConfig(ctx context.Context, config Config) error { return nil }

func (f *fakeHandler) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	return f.processCommandFn(ctx, cmd)
}

func (f *fakeHandler) Refresh(ctx context.Context) error { return nil }

func (f *fakeHandler) ProcessCommandAsync(ctx context.Context, cmd *command.Command) error {
	return f.processCommandAsyncFn(ctx, cmd)
}

func mediaPlayerDevice(id string) *device.Device {
	return &device.Device{
		Id: id,
		Details: &device.Device_MediaPlayer{
			MediaPlayer: &device.MediaPlayer{
				Volume: &trait.Volume{},
				Media:  &trait.Media{},
			},
		},
	}
}

func televisionDevice(id string) *device.Device {
	return &device.Device{
		Id: id,
		Details: &device.Device_Television{
			Television: &device.Television{
				OnOff:  &trait.OnOff{},
				Volume: &trait.Volume{},
				Media:  &trait.Media{},
			},
		},
	}
}

func lightDevice(id string) *device.Device {
	return &device.Device{
		Id: id,
		Details: &device.Device_Light{
			Light: &device.Light{
				OnOff: &trait.OnOff{},
			},
		},
	}
}

func newTestService(t *testing.T, h *fakeHandler) *Service {
	svc := NewService(zaptest.NewLogger(t))
	svc.RegisterHandler(h, &api2.Bridge{Id: "test-bridge"})
	return svc
}

func TestDeviceSupportsCommand(t *testing.T) {
	tests := []struct {
		name string
		d    *device.Device
		cmd  *command.Command
		want bool
	}{
		{
			name: "media player volume absolute supported",
			d:    mediaPlayerDevice("d1"),
			cmd:  &command.Command{DeviceId: "d1", Details: &command.Command_VolumeAbsolute{VolumeAbsolute: &command.VolumeAbsolute{Level: 10}}},
			want: true,
		},
		{
			name: "media player playback supported",
			d:    mediaPlayerDevice("d1"),
			cmd:  &command.Command{DeviceId: "d1", Details: &command.Command_Playback{Playback: &command.Playback{Action: command.Playback_ACTION_PAUSE}}},
			want: true,
		},
		{
			name: "media player onoff not supported",
			d:    mediaPlayerDevice("d1"),
			cmd:  &command.Command{DeviceId: "d1", Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}}},
			want: false,
		},
		{
			name: "light onoff supported",
			d:    lightDevice("d1"),
			cmd:  &command.Command{DeviceId: "d1", Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}}},
			want: true,
		},
		{
			name: "light volume not supported",
			d:    lightDevice("d1"),
			cmd:  &command.Command{DeviceId: "d1", Details: &command.Command_VolumeAbsolute{VolumeAbsolute: &command.VolumeAbsolute{Level: 10}}},
			want: false,
		},
		{
			name: "television volume absolute supported",
			d:    televisionDevice("d1"),
			cmd:  &command.Command{DeviceId: "d1", Details: &command.Command_VolumeAbsolute{VolumeAbsolute: &command.VolumeAbsolute{Level: 10}}},
			want: true,
		},
		{
			name: "television playback supported",
			d:    televisionDevice("d1"),
			cmd:  &command.Command{DeviceId: "d1", Details: &command.Command_Playback{Playback: &command.Playback{Action: command.Playback_ACTION_PAUSE}}},
			want: true,
		},
		{
			name: "television onoff supported",
			d:    televisionDevice("d1"),
			cmd:  &command.Command{DeviceId: "d1", Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}}},
			want: true,
		},
		{
			name: "television onoff not supported when field unset",
			d: &device.Device{
				Id: "d1",
				Details: &device.Device_Television{
					Television: &device.Television{Volume: &trait.Volume{}},
				},
			},
			cmd:  &command.Command{DeviceId: "d1", Details: &command.Command_OnOff{OnOff: &command.OnOff{On: true}}},
			want: false,
		},
		{
			name: "television app launch supported",
			d: &device.Device{
				Id: "d1",
				Details: &device.Device_Television{
					Television: &device.Television{App: &trait.App{}},
				},
			},
			cmd:  &command.Command{DeviceId: "d1", Details: &command.Command_AppLaunch{AppLaunch: &command.AppLaunch{ApplicationId: "233637DE"}}},
			want: true,
		},
		{
			name: "media player app launch supported",
			d: &device.Device{
				Id: "d1",
				Details: &device.Device_MediaPlayer{
					MediaPlayer: &device.MediaPlayer{App: &trait.App{}},
				},
			},
			cmd:  &command.Command{DeviceId: "d1", Details: &command.Command_AppLaunch{AppLaunch: &command.AppLaunch{ApplicationId: "233637DE"}}},
			want: true,
		},
		{
			name: "television app launch not supported when field unset",
			d:    televisionDevice("d1"),
			cmd:  &command.Command{DeviceId: "d1", Details: &command.Command_AppLaunch{AppLaunch: &command.AppLaunch{ApplicationId: "233637DE"}}},
			want: false,
		},
		{
			name: "light scene app launch supported",
			d: &device.Device{
				Id: "d1",
				Details: &device.Device_Light{
					Light: &device.Light{OnOff: &trait.OnOff{}, Scene: &trait.App{}},
				},
			},
			cmd:  &command.Command{DeviceId: "d1", Details: &command.Command_AppLaunch{AppLaunch: &command.AppLaunch{ApplicationId: "Nemo"}}},
			want: true,
		},
		{
			name: "light app launch not supported when scene field unset",
			d:    lightDevice("d1"),
			cmd:  &command.Command{DeviceId: "d1", Details: &command.Command_AppLaunch{AppLaunch: &command.AppLaunch{ApplicationId: "Nemo"}}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, deviceSupportsCommand(tt.d, tt.cmd))
		})
	}
}

func TestExecuteCommandAsync_DeviceNotFound(t *testing.T) {
	h := &fakeHandler{}
	svc := newTestService(t, h)

	resp, err := svc.API().ExecuteCommandAsync(context.Background(), &command.Command{DeviceId: "missing"})
	assert.Nil(t, resp)
	assert.ErrorIs(t, err, ErrDeviceNotFound)
}

func TestExecuteCommandAsync_EmptyIDRejected(t *testing.T) {
	h := &fakeHandler{}
	svc := newTestService(t, h)
	svc.UpdateDevice(mediaPlayerDevice("d1"))

	resp, err := svc.API().ExecuteCommandAsync(context.Background(), &command.Command{
		DeviceId: "d1",
		Details:  &command.Command_Playback{Playback: &command.Playback{Action: command.Playback_ACTION_PAUSE}},
	})
	assert.Nil(t, resp)
	assert.ErrorIs(t, err, ErrCommandIDRequired)
}

func TestExecuteCommandAsync_UnsupportedCommand(t *testing.T) {
	h := &fakeHandler{}
	svc := newTestService(t, h)
	svc.UpdateDevice(lightDevice("d1"))

	resp, err := svc.API().ExecuteCommandAsync(context.Background(), &command.Command{
		DeviceId: "d1",
		Details:  &command.Command_VolumeAbsolute{VolumeAbsolute: &command.VolumeAbsolute{Level: 10}},
	})
	assert.Nil(t, resp)
	assert.ErrorIs(t, err, ErrCommandNotSupported)
}

// TestExecuteCommandAsync_AcceptedAlwaysReturnsEmptyResponse covers both the handler's
// success path (dispatched, no synchronous error) and its fast-fail path (synchronous
// error). Per bridge.proto's contract, both must return an empty response with no RPC
// error once the device/command-type check passes — the fast-fail case instead reports
// its outcome as an immediate CommandUpdate.
func TestExecuteCommandAsync_AcceptedAlwaysReturnsEmptyResponse(t *testing.T) {
	t.Run("handler dispatches without a synchronous error", func(t *testing.T) {
		h := &fakeHandler{
			processCommandAsyncFn: func(ctx context.Context, cmd *command.Command) error {
				return nil
			},
		}
		svc := newTestService(t, h)
		svc.UpdateDevice(mediaPlayerDevice("d1"))

		sink := svc.updates.NewSink()
		defer sink.Close()

		resp, err := svc.API().ExecuteCommandAsync(context.Background(), &command.Command{
			DeviceId: "d1",
			Id:       "cmd1",
			Details:  &command.Command_Playback{Playback: &command.Playback{Action: command.Playback_ACTION_PAUSE}},
		})
		require.NoError(t, err)
		require.NotNil(t, resp)

		// Success is reported later by the handler calling CompleteCommand itself,
		// not synchronously by ExecuteCommandAsync — nothing should be on the stream yet.
		select {
		case msg := <-sink.Messages():
			t.Fatalf("unexpected message published before handler completion: %v", msg)
		default:
		}
	})

	t.Run("handler fast-fails synchronously", func(t *testing.T) {
		fastFailErr := status.Error(codes.FailedPrecondition, "stale version")
		h := &fakeHandler{
			processCommandAsyncFn: func(ctx context.Context, cmd *command.Command) error {
				return fastFailErr
			},
		}
		svc := newTestService(t, h)
		svc.UpdateDevice(mediaPlayerDevice("d1"))

		sink := svc.updates.NewSink()
		defer sink.Close()

		resp, err := svc.API().ExecuteCommandAsync(context.Background(), &command.Command{
			DeviceId: "d1",
			Id:       "cmd1",
			Details:  &command.Command_Playback{Playback: &command.Playback{Action: command.Playback_ACTION_PAUSE}},
		})
		require.NoError(t, err)
		require.NotNil(t, resp)

		msg := <-sink.Messages()
		update, ok := msg.(*api2.Update)
		require.True(t, ok)
		assert.Equal(t, api2.Update_EXECUTED, update.Action)
		cu := update.GetCommandUpdate()
		require.NotNil(t, cu)
		assert.Equal(t, "cmd1", cu.CommandId)
		assert.Equal(t, codes.FailedPrecondition, codes.Code(cu.Result.Code))
		assert.Empty(t, cu.ResultingVersion)
	})
}

func TestExecuteCommand_UnsupportedCommand(t *testing.T) {
	h := &fakeHandler{}
	svc := newTestService(t, h)
	svc.UpdateDevice(lightDevice("d1"))

	d, err := svc.API().ExecuteCommand(context.Background(), &command.Command{
		DeviceId: "d1",
		Details:  &command.Command_VolumeAbsolute{VolumeAbsolute: &command.VolumeAbsolute{Level: 10}},
	})
	assert.Nil(t, d)
	assert.ErrorIs(t, err, ErrCommandNotSupported)
}

func TestExecuteCommand_Supported(t *testing.T) {
	wantDevice := lightDevice("d1")
	h := &fakeHandler{
		processCommandFn: func(ctx context.Context, cmd *command.Command) (*device.Device, error) {
			return wantDevice, nil
		},
	}
	svc := newTestService(t, h)
	svc.UpdateDevice(lightDevice("d1"))

	d, err := svc.API().ExecuteCommand(context.Background(), &command.Command{
		DeviceId: "d1",
		Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
	})
	require.NoError(t, err)
	assert.Equal(t, "d1", d.GetId())
}

func TestExecuteCommand_StaleVersionRejectedBeforeDispatch(t *testing.T) {
	dispatched := false
	h := &fakeHandler{
		processCommandFn: func(ctx context.Context, cmd *command.Command) (*device.Device, error) {
			dispatched = true
			return lightDevice("d1"), nil
		},
	}
	svc := newTestService(t, h)
	current := lightDevice("d1")
	current.Version = "v2"
	svc.UpdateDevice(current)

	d, err := svc.API().ExecuteCommand(context.Background(), &command.Command{
		DeviceId: "d1",
		Version:  "v1", // stale
		Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: true}},
	})
	assert.Nil(t, d)
	assert.ErrorIs(t, err, ErrVersionMismatch)
	assert.False(t, dispatched, "handler must not be called when the version check fails")
}

func TestExecuteCommandAsync_StaleVersionReportedAsCommandUpdate(t *testing.T) {
	dispatched := false
	h := &fakeHandler{
		processCommandAsyncFn: func(ctx context.Context, cmd *command.Command) error {
			dispatched = true
			return nil
		},
	}
	svc := newTestService(t, h)
	current := mediaPlayerDevice("d1")
	current.Version = "v2"
	svc.UpdateDevice(current)

	sink := svc.updates.NewSink()
	defer sink.Close()

	resp, err := svc.API().ExecuteCommandAsync(context.Background(), &command.Command{
		DeviceId: "d1",
		Id:       "cmd1",
		Version:  "v1", // stale
		Details:  &command.Command_Playback{Playback: &command.Playback{Action: command.Playback_ACTION_PAUSE}},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.False(t, dispatched, "handler must not be called when the version check fails")

	msg := <-sink.Messages()
	update := msg.(*api2.Update)
	require.Equal(t, api2.Update_EXECUTED, update.Action)
	cu := update.GetCommandUpdate()
	require.NotNil(t, cu)
	assert.Equal(t, codes.FailedPrecondition, codes.Code(cu.Result.Code))
}
