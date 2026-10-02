package housestate

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/grpcutil"
	"github.com/rmrobinson/house/service/policy"
)

const testBuildingID = "b1"

// fakeHouseServer is a real (network-served) HouseService standing in for
// service/house, implementing just the two RPCs Adapter calls.
type fakeHouseServer struct {
	api2.UnimplementedHouseServiceServer

	mu       sync.Mutex
	building *api2.Building
	getErr   error
	setErr   error
}

func (s *fakeHouseServer) GetBuilding(ctx context.Context, req *api2.GetBuildingRequest) (*api2.Building, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.building, nil
}

func (s *fakeHouseServer) SetHouseMode(ctx context.Context, req *api2.SetHouseModeRequest) (*api2.Building, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setErr != nil {
		return nil, s.setErr
	}
	s.building.State.Mode = req.GetMode()
	return s.building, nil
}

func (s *fakeHouseServer) setBuilding(b *api2.Building) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.building = b
}

func startFakeHouseServer(t *testing.T, s api2.HouseServiceServer) string {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	grpcServer := grpc.NewServer()
	api2.RegisterHouseServiceServer(grpcServer, s)

	go grpcServer.Serve(lis)
	t.Cleanup(grpcServer.Stop)

	return lis.Addr().String()
}

func dialHouseClient(t *testing.T, addr string) api2.HouseServiceClient {
	t.Helper()

	conn, err := grpcutil.DialInsecure(addr)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	return api2.NewHouseServiceClient(conn)
}

// fakeHomeAPI is a minimal policy.HomeAPI covering only GetHouseState/
// SetHouseState, enough to prove Adapter delegates unrecognized keys to the
// wrapped HomeAPI rather than answering them itself.
type fakeHomeAPI struct {
	policy.HomeAPI
	mu    sync.Mutex
	state map[string]any
}

func newFakeHomeAPI() *fakeHomeAPI {
	return &fakeHomeAPI{state: map[string]any{}}
}

func (f *fakeHomeAPI) GetHouseState(key string) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state[key], nil
}

func (f *fakeHomeAPI) SetHouseState(key string, value any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state[key] = value
	return nil
}

func occupiedBuilding(occupied bool, mode string) *api2.Building {
	return &api2.Building{
		Id: testBuildingID,
		State: &api2.Building_State{
			Occupied: &occupied,
			Mode:     mode,
		},
	}
}

func TestAdapterPollsAndCachesOccupiedAndMode(t *testing.T) {
	srv := &fakeHouseServer{}
	srv.setBuilding(occupiedBuilding(true, "home"))
	addr := startFakeHouseServer(t, srv)

	home := newFakeHomeAPI()
	a := New(zaptest.NewLogger(t), dialHouseClient(t, addr), testBuildingID, home, WithPollInterval(10*time.Millisecond))

	engine := policy.NewEngine(a, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	defer engine.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx, engine)

	occupied, err := a.GetHouseState("occupied")
	require.NoError(t, err)
	assert.Equal(t, true, occupied)

	mode, err := a.GetHouseState("mode")
	require.NoError(t, err)
	assert.Equal(t, "home", mode)
}

func TestAdapterGetHouseStateDelegatesUnknownKeys(t *testing.T) {
	srv := &fakeHouseServer{}
	srv.setBuilding(occupiedBuilding(false, ""))
	addr := startFakeHouseServer(t, srv)

	home := newFakeHomeAPI()
	require.NoError(t, home.SetHouseState("location.timezone", "America/Toronto"))

	a := New(zaptest.NewLogger(t), dialHouseClient(t, addr), testBuildingID, home)
	engine := policy.NewEngine(a, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	defer engine.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx, engine)

	tz, err := a.GetHouseState("location.timezone")
	require.NoError(t, err)
	assert.Equal(t, "America/Toronto", tz)
}

func TestAdapterSetHouseStateModeCallsSetHouseMode(t *testing.T) {
	srv := &fakeHouseServer{}
	srv.setBuilding(occupiedBuilding(false, "away"))
	addr := startFakeHouseServer(t, srv)

	home := newFakeHomeAPI()
	a := New(zaptest.NewLogger(t), dialHouseClient(t, addr), testBuildingID, home)
	engine := policy.NewEngine(a, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	defer engine.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx, engine)

	require.NoError(t, a.SetHouseState("mode", "home"))

	mode, err := a.GetHouseState("mode")
	require.NoError(t, err)
	assert.Equal(t, "home", mode)
}

func TestAdapterSetHouseStateRejectsNonStringMode(t *testing.T) {
	srv := &fakeHouseServer{}
	srv.setBuilding(occupiedBuilding(false, "away"))
	addr := startFakeHouseServer(t, srv)

	home := newFakeHomeAPI()
	a := New(zaptest.NewLogger(t), dialHouseClient(t, addr), testBuildingID, home)
	engine := policy.NewEngine(a, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	defer engine.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx, engine)

	err := a.SetHouseState("mode", 42)
	require.Error(t, err)
}

func TestAdapterSetHouseStateBeforeStartReturnsErrNotReady(t *testing.T) {
	home := newFakeHomeAPI()
	a := New(zaptest.NewLogger(t), nil, testBuildingID, home)

	err := a.SetHouseState("mode", "home")
	require.ErrorIs(t, err, ErrNotReady)
}

func TestAdapterGetHouseStateBeforeStartReturnsErrNotReady(t *testing.T) {
	home := newFakeHomeAPI()
	a := New(zaptest.NewLogger(t), nil, testBuildingID, home)

	_, err := a.GetHouseState("occupied")
	require.ErrorIs(t, err, ErrNotReady)
}

func TestAdapterPublishesHouseStateChangedOnPoll(t *testing.T) {
	srv := &fakeHouseServer{}
	srv.setBuilding(occupiedBuilding(false, ""))
	addr := startFakeHouseServer(t, srv)

	home := newFakeHomeAPI()
	a := New(zaptest.NewLogger(t), dialHouseClient(t, addr), testBuildingID, home, WithPollInterval(10*time.Millisecond))
	engine := policy.NewEngine(a, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	defer engine.Close()

	ch := engine.Bus().Subscribe(policy.HouseStateChangedTopic)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx, engine)

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for house state changed event")
	}
}

func TestAdapterPollFailureHoldsLastKnownState(t *testing.T) {
	srv := &fakeHouseServer{}
	srv.setBuilding(occupiedBuilding(true, "home"))
	addr := startFakeHouseServer(t, srv)

	home := newFakeHomeAPI()
	a := New(zaptest.NewLogger(t), dialHouseClient(t, addr), testBuildingID, home, WithPollInterval(10*time.Millisecond))
	engine := policy.NewEngine(a, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	defer engine.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx, engine)

	srv.mu.Lock()
	srv.getErr = status.Error(codes.Unavailable, "down")
	srv.mu.Unlock()

	time.Sleep(50 * time.Millisecond)

	occupied, err := a.GetHouseState("occupied")
	require.NoError(t, err)
	assert.Equal(t, true, occupied)
}
