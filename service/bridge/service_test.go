package bridge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
)

func TestCompleteCommand_Success(t *testing.T) {
	h := &fakeHandler{}
	svc := newTestService(t, h)

	before := mediaPlayerDevice("d1")
	svc.UpdateDevice(before)

	sink := svc.updates.NewSink()
	defer sink.Close()

	after := mediaPlayerDevice("d1")
	after.Version = "v2"

	svc.CompleteCommand(&command.Command{DeviceId: "d1", Id: "cmd1"}, nil, after)

	// CHANGED (from the device update) must arrive before the terminal CommandUpdate,
	// so an in-order consumer of the stream sees state before completion.
	msg := <-sink.Messages()
	deviceUpdate := msg.(*api2.Update)
	require.Equal(t, api2.Update_CHANGED, deviceUpdate.Action)
	assert.Equal(t, "v2", deviceUpdate.GetDeviceUpdate().GetDevice().GetVersion())

	msg = <-sink.Messages()
	commandUpdate := msg.(*api2.Update)
	require.Equal(t, api2.Update_EXECUTED, commandUpdate.Action)
	cu := commandUpdate.GetCommandUpdate()
	require.NotNil(t, cu)
	assert.Equal(t, "cmd1", cu.CommandId)
	assert.Equal(t, "d1", cu.DeviceId)
	assert.Equal(t, int32(codes.OK), cu.Result.Code)
	assert.Equal(t, "v2", cu.ResultingVersion)

	assert.Equal(t, "v2", svc.getDevice("d1").GetVersion())
}

func TestCompleteCommand_Failure(t *testing.T) {
	h := &fakeHandler{}
	svc := newTestService(t, h)
	svc.UpdateDevice(mediaPlayerDevice("d1"))

	sink := svc.updates.NewSink()
	defer sink.Close()

	svc.CompleteCommand(&command.Command{DeviceId: "d1", Id: "cmd1"}, status.Error(codes.DeadlineExceeded, "timed out"), nil)

	// No device update on failure — only the terminal CommandUpdate.
	msg := <-sink.Messages()
	update := msg.(*api2.Update)
	require.Equal(t, api2.Update_EXECUTED, update.Action)
	cu := update.GetCommandUpdate()
	require.NotNil(t, cu)
	assert.Equal(t, int32(codes.DeadlineExceeded), cu.Result.Code)
	assert.Empty(t, cu.ResultingVersion)
}
