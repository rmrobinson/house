package bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
)

// historyHandler is a fakeHandler that also implements HistoryHandler.
type historyHandler struct {
	fakeHandler

	samples []*api2.HistorySample
	err     error

	gotDeviceID string
	gotSince    time.Time
	gotUntil    time.Time
}

func (h *historyHandler) GetHistory(ctx context.Context, deviceID string, since, until time.Time) ([]*api2.HistorySample, error) {
	h.gotDeviceID, h.gotSince, h.gotUntil = deviceID, since, until
	return h.samples, h.err
}

type fakeHistoryStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent []*api2.HistorySample
}

func (s *fakeHistoryStream) Context() context.Context { return s.ctx }

func (s *fakeHistoryStream) Send(m *api2.HistorySample) error {
	s.sent = append(s.sent, m)
	return nil
}

func sampleAt(t time.Time) *api2.HistorySample {
	return &api2.HistorySample{Time: timestamppb.New(t)}
}

func newHistoryTestService(t *testing.T, h Handler) *Service {
	svc := NewService(zaptest.NewLogger(t))
	svc.RegisterHandler(h, &api2.Bridge{Id: "test-bridge"})
	svc.UpdateDevice(lightDevice("d1"))
	return svc
}

func TestGetDeviceHistory_UnsupportedHandlerIsUnimplemented(t *testing.T) {
	svc := newHistoryTestService(t, &fakeHandler{})

	err := svc.API().GetDeviceHistory(&api2.GetDeviceHistoryRequest{DeviceId: "d1"}, &fakeHistoryStream{ctx: context.Background()})
	assert.ErrorIs(t, err, ErrHistoryNotSupported)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

func TestGetDeviceHistory_UnknownDevice(t *testing.T) {
	svc := newHistoryTestService(t, &historyHandler{})

	err := svc.API().GetDeviceHistory(&api2.GetDeviceHistoryRequest{DeviceId: "nope"}, &fakeHistoryStream{ctx: context.Background()})
	assert.ErrorIs(t, err, ErrDeviceNotFound)
}

func TestGetDeviceHistory_FiltersSortsAndDefaultsUntilToNow(t *testing.T) {
	base := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	h := &historyHandler{samples: []*api2.HistorySample{
		sampleAt(base.Add(30 * time.Minute)), // out of order on purpose
		sampleAt(base),                       // == since, exclusive so dropped
		sampleAt(base.Add(10 * time.Minute)),
		sampleAt(time.Now().Add(time.Hour)), // in the future, past the default until
		{},                                  // no time at all
	}}
	svc := newHistoryTestService(t, h)

	stream := &fakeHistoryStream{ctx: context.Background()}
	err := svc.API().GetDeviceHistory(&api2.GetDeviceHistoryRequest{
		DeviceId: "d1",
		Since:    timestamppb.New(base),
	}, stream)
	require.NoError(t, err)

	assert.Equal(t, "d1", h.gotDeviceID)
	assert.True(t, h.gotSince.Equal(base))
	assert.WithinDuration(t, time.Now(), h.gotUntil, 5*time.Second)

	require.Len(t, stream.sent, 2)
	assert.True(t, stream.sent[0].GetTime().AsTime().Equal(base.Add(10*time.Minute)))
	assert.True(t, stream.sent[1].GetTime().AsTime().Equal(base.Add(30*time.Minute)))
}

func TestGetDeviceHistory_MaxSamplesKeepsOldest(t *testing.T) {
	base := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	var samples []*api2.HistorySample
	for i := 1; i <= 5; i++ {
		samples = append(samples, sampleAt(base.Add(time.Duration(i)*time.Minute)))
	}
	svc := newHistoryTestService(t, &historyHandler{samples: samples})

	stream := &fakeHistoryStream{ctx: context.Background()}
	err := svc.API().GetDeviceHistory(&api2.GetDeviceHistoryRequest{
		DeviceId:   "d1",
		Since:      timestamppb.New(base),
		MaxSamples: 2,
	}, stream)
	require.NoError(t, err)

	require.Len(t, stream.sent, 2)
	assert.True(t, stream.sent[0].GetTime().AsTime().Equal(base.Add(1*time.Minute)))
	assert.True(t, stream.sent[1].GetTime().AsTime().Equal(base.Add(2*time.Minute)))
}

func TestGetDeviceHistory_NegativeMaxSamplesRejected(t *testing.T) {
	svc := newHistoryTestService(t, &historyHandler{})

	err := svc.API().GetDeviceHistory(&api2.GetDeviceHistoryRequest{DeviceId: "d1", MaxSamples: -1}, &fakeHistoryStream{ctx: context.Background()})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestGetDeviceHistory_EmptyRangeReturnsNothingWithoutCallingHandler(t *testing.T) {
	h := &historyHandler{}
	svc := newHistoryTestService(t, h)

	now := time.Now()
	stream := &fakeHistoryStream{ctx: context.Background()}
	err := svc.API().GetDeviceHistory(&api2.GetDeviceHistoryRequest{
		DeviceId: "d1",
		Since:    timestamppb.New(now),
		Until:    timestamppb.New(now.Add(-time.Minute)),
	}, stream)
	require.NoError(t, err)
	assert.Empty(t, stream.sent)
	assert.Empty(t, h.gotDeviceID, "handler must not be called for an empty range")
}

func TestGetDeviceHistory_HandlerErrors(t *testing.T) {
	t.Run("grpc status passed through", func(t *testing.T) {
		svc := newHistoryTestService(t, &historyHandler{err: status.Error(codes.Unavailable, "ble busy")})
		err := svc.API().GetDeviceHistory(&api2.GetDeviceHistoryRequest{DeviceId: "d1"}, &fakeHistoryStream{ctx: context.Background()})
		assert.Equal(t, codes.Unavailable, status.Code(err))
	})
	t.Run("plain error rewritten to internal", func(t *testing.T) {
		svc := newHistoryTestService(t, &historyHandler{err: errors.New("boom")})
		err := svc.API().GetDeviceHistory(&api2.GetDeviceHistoryRequest{DeviceId: "d1"}, &fakeHistoryStream{ctx: context.Background()})
		assert.Equal(t, codes.Internal, status.Code(err))
	})
}
