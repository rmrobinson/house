package bridgeconn

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
)

// fakeServer streams a fixed set of updates once, then blocks until the
// client disconnects (mirroring a real BridgeService).
type fakeServer struct {
	api2.UnimplementedBridgeServiceServer
	updates []*api2.Update
}

func (s *fakeServer) StreamUpdates(req *api2.StreamUpdatesRequest, stream api2.BridgeService_StreamUpdatesServer) error {
	for _, u := range s.updates {
		if err := stream.Send(u); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

// neverSendsServer accepts the StreamUpdates RPC (so Dial and the call
// itself both "succeed") but errors before ever sending a message - standing
// in for an upstream that's reachable at the TCP/HTTP2 level but can't
// actually serve the stream (wrong service, crash-looping, etc).
type neverSendsServer struct {
	api2.UnimplementedBridgeServiceServer
}

func (s *neverSendsServer) StreamUpdates(req *api2.StreamUpdatesRequest, stream api2.BridgeService_StreamUpdatesServer) error {
	return errors.New("simulated: never delivers a message")
}

func startFakeServer(t *testing.T, s api2.BridgeServiceServer) string {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	grpcServer := grpc.NewServer()
	api2.RegisterBridgeServiceServer(grpcServer, s)

	go grpcServer.Serve(lis)
	t.Cleanup(grpcServer.Stop)

	return lis.Addr().String()
}

func TestRun_DeliversUpdatesAndSetsClient(t *testing.T) {
	addr := startFakeServer(t, &fakeServer{updates: []*api2.Update{{Action: api2.Update_INITIAL}}})

	c := New(zaptest.NewLogger(t), addr, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var received int
	go c.Run(ctx, func(u *api2.Update) {
		mu.Lock()
		received++
		mu.Unlock()
	}, nil)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return received == 1
	}, 2*time.Second, 10*time.Millisecond)

	assert.NotNil(t, c.Client(), "Client should be set once connected")
}

func TestRun_ClientClearedAndOnDropCalledWhenConnectionEnds(t *testing.T) {
	srv := &fakeServer{updates: []*api2.Update{{Action: api2.Update_INITIAL}}}
	addr := startFakeServer(t, srv)

	c := New(zaptest.NewLogger(t), addr, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var drops atomic.Int64
	go c.Run(ctx, func(u *api2.Update) {}, func() { drops.Add(1) })

	require.Eventually(t, func() bool { return c.Client() != nil }, 2*time.Second, 10*time.Millisecond)

	cancel()

	require.Eventually(t, func() bool { return c.Client() == nil }, 2*time.Second, 10*time.Millisecond,
		"Client should go back to nil once the connection ends")
	require.Eventually(t, func() bool { return drops.Load() == 1 }, 2*time.Second, 10*time.Millisecond,
		"onDrop should fire exactly once for the dropped connection")
}

// TestRun_BackoffOnlyResetsAfterFirstMessage guards against a bug where
// backoff was reset right after a successful Dial. That proves nothing about
// reachability: grpcutil.DialInsecure dials lazily and essentially never
// fails synchronously, so an upstream that's up at the TCP level but never
// actually delivers a message (wrong service, crash looping, ...) would
// otherwise reset backoff to the minimum on every single reconnect attempt,
// defeating the whole point of exponential backoff.
func TestRun_BackoffOnlyResetsAfterFirstMessage(t *testing.T) {
	t.Run("grows when the stream never delivers a message", func(t *testing.T) {
		addr := startFakeServer(t, &neverSendsServer{})

		c := New(zaptest.NewLogger(t), addr, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go c.Run(ctx, func(u *api2.Update) {}, nil)

		require.Eventually(t, func() bool {
			return time.Duration(c.backoff.Load()) > MinBackoff
		}, 5*time.Second, 20*time.Millisecond, "backoff should grow past the minimum when every attempt fails before delivering a message")
	})

	t.Run("resets once a message is actually received", func(t *testing.T) {
		addr := startFakeServer(t, &fakeServer{updates: []*api2.Update{{Action: api2.Update_INITIAL}}})

		c := New(zaptest.NewLogger(t), addr, nil)
		// Seed a high value, as if prior failures had already grown it - a
		// successful connection should collapse it back to the minimum.
		c.backoff.Store(int64(MaxBackoff))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go c.Run(ctx, func(u *api2.Update) {}, nil)

		require.Eventually(t, func() bool {
			return time.Duration(c.backoff.Load()) == MinBackoff
		}, 2*time.Second, 10*time.Millisecond, "backoff should reset once the upstream actually delivers a message")
	})
}
