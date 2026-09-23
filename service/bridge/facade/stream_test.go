package facade

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/device"
)

// fakeStreamUpdatesServer is a minimal api2.BridgeService_StreamUpdatesServer
// for exercising Facade.StreamUpdates without a real network connection.
type fakeStreamUpdatesServer struct {
	grpc.ServerStream
	ctx context.Context

	mu   sync.Mutex
	sent []*api2.Update
}

func (s *fakeStreamUpdatesServer) Context() context.Context { return s.ctx }

func (s *fakeStreamUpdatesServer) Send(u *api2.Update) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, u)
	return nil
}

func (s *fakeStreamUpdatesServer) snapshot() []*api2.Update {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*api2.Update, len(s.sent))
	copy(out, s.sent)
	return out
}

func TestStreamUpdates_SynthesizesInitialBurstFromCache(t *testing.T) {
	f := newTestFacade(t)

	uc := &upstreamConn{addr: "bridge-1:1234", f: f}
	f.ingestInitial(uc, &api2.InitialUpdate{
		Bridge:  testBridge("b1", true),
		Devices: []*device.Device{lightDevice("d1", true)},
	})

	ctx, cancel := context.WithCancel(context.Background())
	stream := &fakeStreamUpdatesServer{ctx: ctx}

	done := make(chan error, 1)
	go func() {
		done <- f.StreamUpdates(&api2.StreamUpdatesRequest{}, stream)
	}()

	// One BridgeUpdate per known bridge - including the facade's own self
	// entry, seeded at construction - then one DeviceUpdate per known device.
	require.Eventually(t, func() bool {
		return len(stream.snapshot()) >= 3
	}, time.Second, 10*time.Millisecond)

	sent := stream.snapshot()
	require.Len(t, sent, 3)

	bridgeIDs := map[string]bool{}
	for _, u := range sent[:2] {
		assert.Equal(t, api2.Update_INITIAL, u.Action)
		require.NotNil(t, u.GetBridgeUpdate())
		bridgeIDs[u.GetBridgeUpdate().GetBridgeId()] = true
	}
	assert.True(t, bridgeIDs[testSelfBridgeID])
	assert.True(t, bridgeIDs["b1"])

	assert.Equal(t, api2.Update_INITIAL, sent[2].Action)
	require.NotNil(t, sent[2].GetDeviceUpdate())
	assert.Equal(t, "d1", sent[2].GetDeviceUpdate().GetDeviceId())
	assert.Equal(t, "b1", sent[2].GetDeviceUpdate().GetBridgeId())
	assert.Equal(t, testSelfAddress, sent[2].GetDeviceUpdate().GetDevice().GetAddress().GetAddress())

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("StreamUpdates did not return after context cancellation")
	}
}
