package facade

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/lib/grpcutil"
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

func startFakeUpstream(t *testing.T, s api2.BridgeServiceServer) (addr string, grpcServer *grpc.Server) {
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
	f.Connect(ctx, addr, "")

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
	f.Connect(ctx, addr, "")

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

// The dial/backoff/reconnect mechanics (including "backoff only resets after
// the first message, not just a successful Dial") are bridgeconn.Conn's
// responsibility now, not upstreamConn's - see bridgeconn's own tests.

func TestConnect_ReconnectsAfterUpstreamRestart(t *testing.T) {
	srv := &fakeUpstreamServer{bridgeID: "b1", devices: []*device.Device{lightDevice("d1", true)}}
	addr, grpcServer := startFakeUpstream(t, srv)

	f := newTestFacade(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.Connect(ctx, addr, "")

	reachable := func(want bool) func() bool {
		return func() bool {
			b, err := f.GetBridge(context.Background(), &api2.GetBridgeRequest{Id: "b1"})
			return err == nil && b.GetIsReachable() == want
		}
	}
	require.Eventually(t, reachable(true), 2*time.Second, 10*time.Millisecond)

	grpcServer.Stop()
	require.Eventually(t, reachable(false), 2*time.Second, 10*time.Millisecond)

	// Restart on the same address, as a restarted bridge process would.
	lis, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	restarted := grpc.NewServer()
	api2.RegisterBridgeServiceServer(restarted, srv)
	go restarted.Serve(lis)
	t.Cleanup(restarted.Stop)

	require.Eventually(t, reachable(true), 10*time.Second, 50*time.Millisecond)
	d, err := f.GetDevice(context.Background(), &api2.GetDeviceRequest{Id: "d1"})
	require.NoError(t, err)
	assert.True(t, d.GetAddress().GetIsReachable())
}

// A downstream StreamUpdates subscriber (adminui, policyd) must see every
// device come back online after an upstream bridge restarts - not just the
// first few of the burst the reconnect's InitialUpdate produces.
func TestStreamUpdates_DownstreamSeesEveryDeviceRecoverAfterUpstreamRestart(t *testing.T) {
	const numDevices = 50
	var devices []*device.Device
	for i := 0; i < numDevices; i++ {
		devices = append(devices, lightDevice(fmt.Sprintf("d%d", i), true))
	}
	srv := &fakeUpstreamServer{bridgeID: "b1", devices: devices}
	upAddr, upServer := startFakeUpstream(t, srv)

	f := newTestFacade(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.Connect(ctx, upAddr, "")
	require.Eventually(t, func() bool {
		b, err := f.GetBridge(context.Background(), &api2.GetBridgeRequest{Id: "b1"})
		return err == nil && b.GetIsReachable()
	}, 2*time.Second, 10*time.Millisecond)

	// Serve the facade itself and subscribe to it the way adminui/policyd do.
	downAddr, _ := startFakeUpstream(t, f)
	conn, err := grpcutil.DialInsecure(downAddr)
	require.NoError(t, err)
	defer conn.Close()
	stream, err := api2.NewBridgeServiceClient(conn).StreamUpdates(ctx, &api2.StreamUpdatesRequest{})
	require.NoError(t, err)

	var mu sync.Mutex
	reachable := map[string]bool{}
	go func() {
		for {
			u, err := stream.Recv()
			if err != nil {
				return
			}
			if du := u.GetDeviceUpdate(); du != nil && du.GetDevice() != nil {
				mu.Lock()
				reachable[du.GetDeviceId()] = du.GetDevice().GetAddress().GetIsReachable()
				mu.Unlock()
			}
		}
	}()
	countReachable := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, r := range reachable {
			if r {
				n++
			}
		}
		return n
	}
	require.Eventually(t, func() bool { return countReachable() == numDevices }, 2*time.Second, 10*time.Millisecond)

	upServer.Stop()
	require.Eventually(t, func() bool { return countReachable() == 0 }, 2*time.Second, 10*time.Millisecond, "downstream should see every device go unreachable")

	lis, err := net.Listen("tcp", upAddr)
	require.NoError(t, err)
	restarted := grpc.NewServer()
	api2.RegisterBridgeServiceServer(restarted, srv)
	go restarted.Serve(lis)
	t.Cleanup(restarted.Stop)

	require.Eventually(t, func() bool { return countReachable() == numDevices }, 10*time.Second, 50*time.Millisecond, "downstream should see every device recover")
}
