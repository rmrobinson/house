package facade

import (
	"context"
	"errors"
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

// neverSendsUpstreamServer accepts the StreamUpdates RPC (so Dial and the
// call itself both "succeed") but errors before ever sending a message -
// standing in for an upstream that's reachable at the TCP/HTTP2 level but
// can't actually serve the stream (wrong service, crash-looping, etc).
type neverSendsUpstreamServer struct {
	api2.UnimplementedBridgeServiceServer
}

func (s *neverSendsUpstreamServer) StreamUpdates(req *api2.StreamUpdatesRequest, stream api2.BridgeService_StreamUpdatesServer) error {
	return errors.New("simulated: never delivers a message")
}

// TestConnect_BackoffOnlyResetsAfterFirstMessage guards against a bug where
// backoff was reset right after a successful Dial. That proves nothing about
// reachability: grpcutil.DialInsecure dials lazily and essentially never
// fails synchronously, so an upstream that's up at the TCP level but never
// actually delivers a message (wrong service, crash looping, ...) would
// otherwise reset backoff to the minimum on every single reconnect attempt,
// defeating the whole point of exponential backoff.
func TestConnect_BackoffOnlyResetsAfterFirstMessage(t *testing.T) {
	t.Run("grows when the stream never delivers a message", func(t *testing.T) {
		addr, _ := startFakeUpstream(t, &neverSendsUpstreamServer{})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		uc := &upstreamConn{addr: addr, f: newTestFacade(t)}
		uc.backoff.Store(int64(minReconnectBackoff))
		go uc.run(ctx)

		require.Eventually(t, func() bool {
			return time.Duration(uc.backoff.Load()) > minReconnectBackoff
		}, 5*time.Second, 20*time.Millisecond, "backoff should grow past the minimum when every attempt fails before delivering a message")
	})

	t.Run("resets once a message is actually received", func(t *testing.T) {
		addr, _ := startFakeUpstream(t, &fakeUpstreamServer{bridgeID: "b1"})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		uc := &upstreamConn{addr: addr, f: newTestFacade(t)}
		// Seed a high value, as if prior failures had already grown it - a
		// successful connection should collapse it back to the minimum.
		uc.backoff.Store(int64(maxReconnectBackoff))
		go uc.run(ctx)

		require.Eventually(t, func() bool {
			return time.Duration(uc.backoff.Load()) == minReconnectBackoff
		}, 2*time.Second, 10*time.Millisecond, "backoff should reset once the upstream actually delivers its InitialUpdate")
	})
}
