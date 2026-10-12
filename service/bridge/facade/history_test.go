package facade

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
)

// historyUpstreamServer is a fakeUpstreamServer that also serves GetDeviceHistory,
// recording the request it was given.
type historyUpstreamServer struct {
	fakeUpstreamServer

	samples []*api2.HistorySample
	gotReq  chan *api2.GetDeviceHistoryRequest
}

func (s *historyUpstreamServer) GetDeviceHistory(req *api2.GetDeviceHistoryRequest, stream api2.BridgeService_GetDeviceHistoryServer) error {
	s.gotReq <- req
	for _, sample := range s.samples {
		if err := stream.Send(sample); err != nil {
			return err
		}
	}
	return nil
}

type fakeHistoryServerStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent []*api2.HistorySample
}

func (s *fakeHistoryServerStream) Context() context.Context { return s.ctx }

func (s *fakeHistoryServerStream) Send(m *api2.HistorySample) error {
	s.sent = append(s.sent, m)
	return nil
}

func TestGetDeviceHistory_RoutesToOwningBridgeAndRelaysSamples(t *testing.T) {
	srv := &historyUpstreamServer{
		fakeUpstreamServer: fakeUpstreamServer{bridgeID: "b1", devices: []*device.Device{lightDevice("d1", true)}},
		samples: []*api2.HistorySample{
			{Time: timestamppb.New(time.Unix(1000, 0)), Snapshot: &device.Device{Id: "d1"}},
			{Time: timestamppb.New(time.Unix(1300, 0)), Snapshot: &device.Device{Id: "d1"}},
		},
		gotReq: make(chan *api2.GetDeviceHistoryRequest, 1),
	}
	addr, _ := startFakeUpstream(t, srv)

	f := newTestFacade(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.Connect(ctx, addr, "")

	require.Eventually(t, func() bool {
		_, err := f.GetDevice(context.Background(), &api2.GetDeviceRequest{Id: "d1"})
		return err == nil
	}, 2*time.Second, 10*time.Millisecond)

	stream := &fakeHistoryServerStream{ctx: context.Background()}
	err := f.GetDeviceHistory(&api2.GetDeviceHistoryRequest{DeviceId: "d1", MaxSamples: 7}, stream)
	require.NoError(t, err)

	select {
	case req := <-srv.gotReq:
		assert.Equal(t, "d1", req.GetDeviceId())
		assert.EqualValues(t, 7, req.GetMaxSamples())
	case <-time.After(time.Second):
		t.Fatal("upstream never received the history request")
	}

	require.Len(t, stream.sent, 2)
	assert.Equal(t, int64(1000), stream.sent[0].GetTime().GetSeconds())
	assert.Equal(t, int64(1300), stream.sent[1].GetTime().GetSeconds())
}

func TestGetDeviceHistory_UnknownDevice(t *testing.T) {
	f := newTestFacade(t)

	err := f.GetDeviceHistory(&api2.GetDeviceHistoryRequest{DeviceId: "nope"}, &fakeHistoryServerStream{ctx: context.Background()})
	assert.ErrorIs(t, err, bridge.ErrDeviceNotFound)
	assert.Equal(t, codes.NotFound, status.Code(err))
}
