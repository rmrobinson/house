package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
)

// stillImageHandler is a fakeHandler that also implements StillImageHandler.
type stillImageHandler struct {
	fakeHandler

	resp *api2.GetStillImageResponse
	err  error
	got  *api2.GetStillImageRequest
}

func (h *stillImageHandler) GetStillImage(ctx context.Context, req *api2.GetStillImageRequest) (*api2.GetStillImageResponse, error) {
	h.got = req
	return h.resp, h.err
}

func newStillImageTestService(t *testing.T, h Handler) *Service {
	svc := NewService(zaptest.NewLogger(t))
	svc.RegisterHandler(h, &api2.Bridge{Id: "test-bridge"})
	svc.UpdateDevice(&device.Device{
		Id: "cam",
		Details: &device.Device_Camera{Camera: &device.Camera{
			StillImage: &trait.StillImage{Attributes: &trait.StillImage_Attributes{ContentTypes: []string{"image/jpeg"}}},
		}},
	})
	svc.UpdateDevice(lightDevice("d1"))
	return svc
}

func TestGetStillImage_UnsupportedHandlerIsUnimplemented(t *testing.T) {
	svc := newStillImageTestService(t, &fakeHandler{})

	_, err := svc.API().GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "cam"})
	assert.ErrorIs(t, err, ErrStillImageNotSupported)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

func TestGetStillImage_UnknownDeviceAndMissingTrait(t *testing.T) {
	svc := newStillImageTestService(t, &stillImageHandler{})

	_, err := svc.API().GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "nope"})
	assert.ErrorIs(t, err, ErrDeviceNotFound)

	_, err = svc.API().GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "d1"})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	_, err = svc.API().GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "cam", MaxHeight: -1})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestGetStillImage_PassesThroughAndWrapsErrors(t *testing.T) {
	h := &stillImageHandler{resp: &api2.GetStillImageResponse{Data: []byte("jpeg"), ContentType: "image/jpeg"}}
	svc := newStillImageTestService(t, h)

	resp, err := svc.API().GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "cam", MaxHeight: 720})
	require.NoError(t, err)
	assert.Equal(t, []byte("jpeg"), resp.GetData())
	assert.Equal(t, int32(720), h.got.GetMaxHeight())

	h.resp, h.err = nil, errors.New("boom")
	_, err = svc.API().GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "cam"})
	assert.Equal(t, codes.Internal, status.Code(err))

	h.err = status.Error(codes.Unavailable, "no picture")
	_, err = svc.API().GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "cam"})
	assert.Equal(t, codes.Unavailable, status.Code(err))
}
