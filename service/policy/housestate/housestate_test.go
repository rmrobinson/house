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
	"github.com/rmrobinson/house/service/lib/bridgeconn"
	"github.com/rmrobinson/house/service/lib/grpcutil"
	"github.com/rmrobinson/house/service/policy"
)

const testBuildingID = "b1"

// fakeHouseServer is a real (network-served) HouseService standing in for
// service/house, implementing the RPCs Adapter calls: StreamHouseUpdates,
// SetHouseMode, and - for GetDeviceRoom - ListDeviceLinks/GetRoom.
type fakeHouseServer struct {
	api2.UnimplementedHouseServiceServer

	mu        sync.Mutex
	building  *api2.Building
	setErr    error
	streamErr error // if set, the next StreamHouseUpdates call fails immediately with this, simulating a reconnect that can't re-establish

	links map[string]string // device_id -> room_id
	rooms map[string]string // room_id -> room name

	drop chan struct{} // sent to by a test to end the current StreamHouseUpdates call, simulating a dropped connection
}

func newFakeHouseServer() *fakeHouseServer {
	return &fakeHouseServer{
		drop:  make(chan struct{}, 1),
		links: make(map[string]string),
		rooms: make(map[string]string),
	}
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

// linkDevice records deviceID as linked to a room named roomName, minting a
// roomID for it if this is the first time roomName's been seen.
func (s *fakeHouseServer) linkDevice(deviceID, roomID, roomName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.links[deviceID] = roomID
	s.rooms[roomID] = roomName
}

func (s *fakeHouseServer) ListDeviceLinks(req *api2.ListDeviceLinksRequest, stream api2.HouseService_ListDeviceLinksServer) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := req.GetDeviceId()
	roomID, ok := s.links[id]
	if !ok {
		return nil
	}
	return stream.Send(&api2.DeviceRoomLink{DeviceId: id, RoomId: roomID})
}

func (s *fakeHouseServer) GetRoom(ctx context.Context, req *api2.GetRoomRequest) (*api2.Room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	name, ok := s.rooms[req.GetId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "room not found")
	}
	return &api2.Room{Id: req.GetId(), Config: &api2.Room_Config{Name: name}}, nil
}

// StreamHouseUpdates sends one BuildingUpdate for the current building
// state, then blocks until the client disconnects or a test forces a drop
// (see Adapter's reconnect-on-failure tests).
func (s *fakeHouseServer) StreamHouseUpdates(req *api2.StreamHouseUpdatesRequest, stream api2.HouseService_StreamHouseUpdatesServer) error {
	s.mu.Lock()
	err := s.streamErr
	building := s.building
	s.mu.Unlock()
	if err != nil {
		return err
	}

	update := &api2.HouseUpdate{Update: &api2.HouseUpdate_Building{Building: &api2.BuildingUpdate{
		BuildingId: req.GetBuildingId(),
		State:      building.GetState(),
	}}}
	if err := stream.Send(update); err != nil {
		return err
	}

	select {
	case <-stream.Context().Done():
		return stream.Context().Err()
	case <-s.drop:
		return status.Error(codes.Unavailable, "dropped")
	}
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

func TestAdapterSubscribesAndCachesOccupiedAndMode(t *testing.T) {
	srv := newFakeHouseServer()
	srv.setBuilding(occupiedBuilding(true, "home"))
	addr := startFakeHouseServer(t, srv)

	home := newFakeHomeAPI()
	a := New(zaptest.NewLogger(t), dialHouseClient(t, addr), testBuildingID, home)

	engine := policy.NewEngine(a, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	defer engine.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx, engine)

	require.Eventually(t, func() bool {
		occupied, err := a.GetHouseState("occupied")
		return err == nil && occupied == true
	}, 2*time.Second, 10*time.Millisecond, "occupied should be cached from the stream's initial BuildingUpdate")

	mode, err := a.GetHouseState("mode")
	require.NoError(t, err)
	assert.Equal(t, "home", mode)
}

// TestAdapterGetHouseStateOccupiedUnknownBeforeFirstSignal guards against a
// real bug caught in review: Building.State.occupied is nil ("no room has
// ever reported occupancy", api/house.proto) before any room signals, and
// that must surface as ErrOccupancyUnknown, not be collapsed into a plain
// false that looks like a confirmed "unoccupied".
func TestAdapterGetHouseStateOccupiedUnknownBeforeFirstSignal(t *testing.T) {
	srv := newFakeHouseServer()
	srv.setBuilding(&api2.Building{
		Id:    testBuildingID,
		State: &api2.Building_State{Mode: "away"},
	})
	addr := startFakeHouseServer(t, srv)

	home := newFakeHomeAPI()
	a := New(zaptest.NewLogger(t), dialHouseClient(t, addr), testBuildingID, home)
	engine := policy.NewEngine(a, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	defer engine.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx, engine)

	require.Eventually(t, func() bool {
		mode, err := a.GetHouseState("mode")
		return err == nil && mode == "away"
	}, 2*time.Second, 10*time.Millisecond, "the stream's first BuildingUpdate should be applied")

	occupied, err := a.GetHouseState("occupied")
	assert.ErrorIs(t, err, ErrOccupancyUnknown)
	assert.Nil(t, occupied)
}

func TestAdapterGetHouseStateDelegatesUnknownKeys(t *testing.T) {
	srv := newFakeHouseServer()
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
	srv := newFakeHouseServer()
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
	srv := newFakeHouseServer()
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

func TestAdapterPublishesHouseStateChangedOnStreamUpdate(t *testing.T) {
	srv := newFakeHouseServer()
	srv.setBuilding(occupiedBuilding(false, ""))
	addr := startFakeHouseServer(t, srv)

	home := newFakeHomeAPI()
	a := New(zaptest.NewLogger(t), dialHouseClient(t, addr), testBuildingID, home)
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

// TestAdapterHoldsLastKnownStateWhenReconnectFails covers the case
// poll-based housestate used to handle via a failed GetBuilding: the stream
// itself can drop (network blip, server restart) and the subsequent
// reconnect attempt can keep failing - Adapter must keep answering
// GetHouseState with the last state it actually saw instead of ErrNotReady
// or a zero value while it retries in the background.
func TestAdapterHoldsLastKnownStateWhenReconnectFails(t *testing.T) {
	srv := newFakeHouseServer()
	srv.setBuilding(occupiedBuilding(true, "home"))
	addr := startFakeHouseServer(t, srv)

	home := newFakeHomeAPI()
	a := New(zaptest.NewLogger(t), dialHouseClient(t, addr), testBuildingID, home)
	engine := policy.NewEngine(a, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	defer engine.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx, engine)

	require.Eventually(t, func() bool {
		occupied, err := a.GetHouseState("occupied")
		return err == nil && occupied == true
	}, 2*time.Second, 10*time.Millisecond)

	// Make the next reconnect attempt fail, then drop the current stream.
	srv.mu.Lock()
	srv.streamErr = status.Error(codes.Unavailable, "down")
	srv.mu.Unlock()
	srv.drop <- struct{}{}

	require.Eventually(t, func() bool {
		return a.backoff.Current() > bridgeconn.MinBackoff
	}, 5*time.Second, 20*time.Millisecond, "a failed reconnect should grow the backoff past the minimum")

	occupied, err := a.GetHouseState("occupied")
	require.NoError(t, err)
	assert.Equal(t, true, occupied)
}

func TestAdapterGetDeviceRoomReturnsLinkedRoomName(t *testing.T) {
	srv := newFakeHouseServer()
	srv.setBuilding(occupiedBuilding(false, ""))
	srv.linkDevice("light-1", "room-1", "Kitchen")
	addr := startFakeHouseServer(t, srv)

	a := New(zaptest.NewLogger(t), dialHouseClient(t, addr), testBuildingID, newFakeHomeAPI())
	engine := policy.NewEngine(a, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	defer engine.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx, engine)

	room, err := a.GetDeviceRoom("light-1")
	require.NoError(t, err)
	assert.Equal(t, "Kitchen", room)
}

func TestAdapterGetDeviceRoomReturnsEmptyWhenUnlinked(t *testing.T) {
	srv := newFakeHouseServer()
	srv.setBuilding(occupiedBuilding(false, ""))
	addr := startFakeHouseServer(t, srv)

	a := New(zaptest.NewLogger(t), dialHouseClient(t, addr), testBuildingID, newFakeHomeAPI())
	engine := policy.NewEngine(a, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	defer engine.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx, engine)

	room, err := a.GetDeviceRoom("light-1")
	require.NoError(t, err, "an unlinked device is a normal state, not a failure")
	assert.Equal(t, "", room)
}

// TestAdapterGetDeviceRoomReturnsEmptyWhenRoomDeleted guards a device whose
// link survives after its room itself is deleted - GetRoom's NotFound must
// be treated the same as "unlinked", not surfaced as an error.
func TestAdapterGetDeviceRoomReturnsEmptyWhenRoomDeleted(t *testing.T) {
	srv := newFakeHouseServer()
	srv.setBuilding(occupiedBuilding(false, ""))
	srv.linkDevice("light-1", "room-1", "Kitchen")
	srv.mu.Lock()
	delete(srv.rooms, "room-1")
	srv.mu.Unlock()
	addr := startFakeHouseServer(t, srv)

	a := New(zaptest.NewLogger(t), dialHouseClient(t, addr), testBuildingID, newFakeHomeAPI())
	engine := policy.NewEngine(a, policy.NewConditionRegistry(), zaptest.NewLogger(t))
	defer engine.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx, engine)

	room, err := a.GetDeviceRoom("light-1")
	require.NoError(t, err)
	assert.Equal(t, "", room)
}

func TestAdapterGetDeviceRoomBeforeStartReturnsErrNotReady(t *testing.T) {
	srv := newFakeHouseServer()
	srv.setBuilding(occupiedBuilding(false, ""))
	addr := startFakeHouseServer(t, srv)

	a := New(zaptest.NewLogger(t), dialHouseClient(t, addr), testBuildingID, newFakeHomeAPI())

	_, err := a.GetDeviceRoom("light-1")
	assert.ErrorIs(t, err, ErrNotReady)
}
