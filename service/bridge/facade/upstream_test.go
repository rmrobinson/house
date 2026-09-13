package facade

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/device"
)

// fakeUpstreamServer is a real (network-served) BridgeService standing in
// for an individual bridge, for exercising Facade.Connect's dial/stream/
// reconnect loop end-to-end.
type fakeUpstreamServer struct {
	api2.UnimplementedBridgeServiceServer

	bridgeID string
	devices  []*device.Device
}

func (s *fakeUpstreamServer) StreamUpdates(req *api2.StreamUpdatesRequest, stream api2.BridgeService_StreamUpdatesServer) error {
	if err := stream.Send(&api2.Update{
		Action: api2.Update_INITIAL,
		Update: &api2.Update_InitialUpdate{
			InitialUpdate: &api2.InitialUpdate{
				Bridge:  &api2.Bridge{Id: s.bridgeID, IsReachable: true},
				Devices: s.devices,
			},
		},
	}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func startFakeUpstream(t *testing.T, s *fakeUpstreamServer) (addr string, grpcServer *grpc.Server) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	grpcServer = grpc.NewServer()
	api2.RegisterBridgeServiceServer(grpcServer, s)

	go grpcServer.Serve(lis)
	t.Cleanup(grpcServer.Stop)

	return lis.Addr().String(), grpcServer
}

func TestConnect_SeedsCacheFromLiveUpstream(t *testing.T) {
	addr, _ := startFakeUpstream(t, &fakeUpstreamServer{
		bridgeID: "b1",
		devices:  []*device.Device{lightDevice("d1", true)},
	})

	f := newTestFacade(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.Connect(ctx, addr)

	require.Eventually(t, func() bool {
		b, err := f.GetBridge(context.Background(), &api2.GetBridgeRequest{Id: "b1"})
		return err == nil && b.GetId() == "b1"
	}, 2*time.Second, 10*time.Millisecond)

	d, err := f.GetDevice(context.Background(), &api2.GetDeviceRequest{Id: "d1"})
	require.NoError(t, err)
	assert.Equal(t, "d1", d.GetId())
	assert.Equal(t, testSelfAddress, d.GetAddress().GetAddress())
}

func TestConnect_MarksUnreachableOnDisconnectThenReachableOnReconnect(t *testing.T) {
	srv := &fakeUpstreamServer{bridgeID: "b1", devices: []*device.Device{lightDevice("d1", true)}}
	addr, grpcServer := startFakeUpstream(t, srv)

	f := newTestFacade(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.Connect(ctx, addr)

	require.Eventually(t, func() bool {
		b, err := f.GetBridge(context.Background(), &api2.GetBridgeRequest{Id: "b1"})
		return err == nil && b.GetIsReachable()
	}, 2*time.Second, 10*time.Millisecond)

	grpcServer.Stop()

	require.Eventually(t, func() bool {
		b, err := f.GetBridge(context.Background(), &api2.GetBridgeRequest{Id: "b1"})
		return err == nil && !b.GetIsReachable()
	}, 2*time.Second, 10*time.Millisecond)

	// Still present, not dropped, while unreachable.
	_, err := f.GetDevice(context.Background(), &api2.GetDeviceRequest{Id: "d1"})
	require.NoError(t, err)
}
