package house

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	apiDevice "github.com/rmrobinson/house/api/device"
	apiTrait "github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/house/db"
)

// fakeBridgeClient is a minimal api2.BridgeServiceClient stand-in - Service
// only ever calls ListDevices (see resolveDevices) and GetDevice (see
// resolveLinkedDevices), so every other method panics if a test reaches it.
type fakeBridgeClient struct {
	listDevicesFn func(ctx context.Context, req *api2.ListDevicesRequest) (*api2.ListDevicesResponse, error)
	getDeviceFn   func(ctx context.Context, req *api2.GetDeviceRequest) (*apiDevice.Device, error)
}

func (f *fakeBridgeClient) GetBridge(ctx context.Context, in *api2.GetBridgeRequest, opts ...grpc.CallOption) (*api2.Bridge, error) {
	panic("not implemented in fake")
}
func (f *fakeBridgeClient) ListDevices(ctx context.Context, in *api2.ListDevicesRequest, opts ...grpc.CallOption) (*api2.ListDevicesResponse, error) {
	return f.listDevicesFn(ctx, in)
}
func (f *fakeBridgeClient) GetDevice(ctx context.Context, in *api2.GetDeviceRequest, opts ...grpc.CallOption) (*apiDevice.Device, error) {
	return f.getDeviceFn(ctx, in)
}
func (f *fakeBridgeClient) UpdateDeviceConfig(ctx context.Context, in *api2.UpdateDeviceConfigRequest, opts ...grpc.CallOption) (*apiDevice.Device, error) {
	panic("not implemented in fake")
}
func (f *fakeBridgeClient) ExecuteCommand(ctx context.Context, in *command.Command, opts ...grpc.CallOption) (*apiDevice.Device, error) {
	panic("not implemented in fake")
}
func (f *fakeBridgeClient) ExecuteCommandAsync(ctx context.Context, in *command.Command, opts ...grpc.CallOption) (*api2.ExecuteCommandAsyncResponse, error) {
	panic("not implemented in fake")
}
func (f *fakeBridgeClient) StreamUpdates(ctx context.Context, in *api2.StreamUpdatesRequest, opts ...grpc.CallOption) (api2.BridgeService_StreamUpdatesClient, error) {
	panic("not implemented in fake")
}

// fakeListRoomsServer is a minimal api2.HouseService_ListRoomsServer for
// exercising Service.ListRooms without a real network connection.
type fakeListRoomsServer struct {
	grpc.ServerStream
	ctx context.Context

	sent []*api2.Room
}

func (s *fakeListRoomsServer) Context() context.Context { return s.ctx }

func (s *fakeListRoomsServer) Send(r *api2.Room) error {
	s.sent = append(s.sent, r)
	return nil
}

// newTestService wires a Service against a fresh in-memory database.
// MaxOpenConns is pinned to 1 - modernc.org/sqlite's :memory: database is
// per-connection, so a pooled second connection would see an empty schema.
func newTestService(t *testing.T, bridgeClient api2.BridgeServiceClient) *Service {
	t.Helper()

	sqlDB, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })

	database, err := db.NewDatabase(zaptest.NewLogger(t), sqlDB)
	require.NoError(t, err)

	svc, err := NewService(context.Background(), zaptest.NewLogger(t), database, bridgeClient, "", nil)
	require.NoError(t, err)
	return svc
}

func createTestRoom(t *testing.T, s *Service) *api2.Room {
	t.Helper()
	ctx := context.Background()

	b, err := s.CreateBuilding(ctx, &api2.CreateBuildingRequest{Config: &api2.Building_Config{Name: "Home"}})
	require.NoError(t, err)
	floor, err := s.CreateFloor(ctx, &api2.CreateFloorRequest{BuildingId: b.Id, Name: "First Floor"})
	require.NoError(t, err)
	room, err := s.CreateRoom(ctx, &api2.CreateRoomRequest{FloorId: floor.Id, Config: &api2.Room_Config{Name: "Kitchen"}})
	require.NoError(t, err)
	return room
}

// fakeStreamHouseUpdatesServer is a minimal
// api2.HouseService_StreamHouseUpdatesServer for exercising
// Service.StreamHouseUpdates without a real network connection - sent is
// buffered so Send doesn't block the goroutine running StreamHouseUpdates
// on a slow test reader.
type fakeStreamHouseUpdatesServer struct {
	grpc.ServerStream
	ctx  context.Context
	sent chan *api2.HouseUpdate
}

func (s *fakeStreamHouseUpdatesServer) Context() context.Context { return s.ctx }

func (s *fakeStreamHouseUpdatesServer) Send(u *api2.HouseUpdate) error {
	s.sent <- u
	return nil
}

// recvRoomUpdate reads from sent until a RoomUpdate-branch HouseUpdate
// arrives (discarding any BuildingUpdate-branch ones along the way - every
// StreamHouseUpdates subscription gets exactly one of those as part of its
// initial snapshot, and potentially more as occupied changes) or timeout
// elapses.
func recvRoomUpdate(t *testing.T, sent chan *api2.HouseUpdate, timeout time.Duration) *api2.RoomUpdate {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case u := <-sent:
			if room := u.GetRoom(); room != nil {
				return room
			}
		case <-deadline:
			t.Fatal("timed out waiting for a RoomUpdate")
			return nil
		}
	}
}

func TestStreamHouseUpdates_RequiresBuildingID(t *testing.T) {
	s := newTestService(t, nil)
	err := s.StreamHouseUpdates(&api2.StreamHouseUpdatesRequest{}, &fakeStreamHouseUpdatesServer{ctx: context.Background(), sent: make(chan *api2.HouseUpdate, 1)})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestStreamHouseUpdates_SnapshotThenLiveUpdatesFilteredByBuilding(t *testing.T) {
	s := newTestService(t, nil)
	ctx := context.Background()

	room := createTestRoom(t, s)

	// A second, unrelated building/room - its updates must never reach a
	// stream scoped to room's building.
	otherBuilding, err := s.CreateBuilding(ctx, &api2.CreateBuildingRequest{Config: &api2.Building_Config{Name: "Cottage"}})
	require.NoError(t, err)
	otherFloor, err := s.CreateFloor(ctx, &api2.CreateFloorRequest{BuildingId: otherBuilding.Id, Name: "Main"})
	require.NoError(t, err)
	otherRoom, err := s.CreateRoom(ctx, &api2.CreateRoomRequest{FloorId: otherFloor.Id, Config: &api2.Room_Config{Name: "Loft"}})
	require.NoError(t, err)

	_, err = s.LinkDevice(ctx, &api2.LinkDeviceRequest{DeviceId: "sensor-1", RoomId: room.Id})
	require.NoError(t, err)
	_, err = s.LinkDevice(ctx, &api2.LinkDeviceRequest{DeviceId: "sensor-2", RoomId: otherRoom.Id})
	require.NoError(t, err)

	sendMotion := func(deviceID string, motion bool) {
		s.agg.handleUpdate(&api2.Update{
			Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{
				Device: &apiDevice.Device{
					Id: deviceID,
					Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
						Presence: &apiTrait.Presence{State: &apiTrait.Presence_State{MotionDetected: motion}},
					}},
				},
			}},
		})
	}

	// A reading before the stream even starts, so the initial snapshot has
	// something to send.
	sendMotion("sensor-1", true)
	sendMotion("sensor-2", true)

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	fake := &fakeStreamHouseUpdatesServer{ctx: streamCtx, sent: make(chan *api2.HouseUpdate, 10)}

	done := make(chan error, 1)
	go func() {
		done <- s.StreamHouseUpdates(&api2.StreamHouseUpdatesRequest{BuildingId: room.BuildingId}, fake)
	}()

	select {
	case u := <-fake.sent:
		building := u.GetBuilding()
		require.NotNil(t, building, "first message must be the building-state snapshot")
		assert.Equal(t, room.BuildingId, building.GetBuildingId())
		require.NotNil(t, building.GetState().Occupied)
		assert.True(t, building.GetState().GetOccupied(), "sensor-1's motion before the stream started must already be reflected")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for building-state snapshot")
	}

	room1Update := recvRoomUpdate(t, fake.sent, 2*time.Second)
	assert.Equal(t, room.Id, room1Update.RoomId, "room snapshot must only cover room's own building")

	sendMotion("sensor-1", false)
	liveUpdate := recvRoomUpdate(t, fake.sent, 2*time.Second)
	assert.Equal(t, room.Id, liveUpdate.RoomId)
	require.NotNil(t, liveUpdate.Properties.Occupied)
	assert.False(t, liveUpdate.Properties.GetOccupied())

	// A change in the other building's room never reaches this stream.
	sendMotion("sensor-2", false)
	select {
	case u := <-fake.sent:
		t.Fatalf("unexpected update from another building: %+v", u)
	case <-time.After(200 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("StreamHouseUpdates did not return after context cancellation")
	}
}

// TestStreamHouseUpdates_OtherBuildingBurstDoesNotStarveBuffer is the
// end-to-end regression case for the shared-Source buffer-starvation bug:
// aggregator.updates is one house-wide Source (bridge.Source's sink buffer
// is a fixed 10 slots), so a burst of updates for a building this stream
// didn't ask for must never crowd out updates for the building it did ask
// for. Before StreamHouseUpdates filtered at NewFilteredSink time (rather
// than after reading the sink back), more than 10 of otherRoom's updates
// arriving before room's own update would have evicted it from the buffer.
func TestStreamHouseUpdates_OtherBuildingBurstDoesNotStarveBuffer(t *testing.T) {
	s := newTestService(t, nil)
	ctx := context.Background()

	room := createTestRoom(t, s)

	otherBuilding, err := s.CreateBuilding(ctx, &api2.CreateBuildingRequest{Config: &api2.Building_Config{Name: "Cottage"}})
	require.NoError(t, err)
	otherFloor, err := s.CreateFloor(ctx, &api2.CreateFloorRequest{BuildingId: otherBuilding.Id, Name: "Main"})
	require.NoError(t, err)
	otherRoom, err := s.CreateRoom(ctx, &api2.CreateRoomRequest{FloorId: otherFloor.Id, Config: &api2.Room_Config{Name: "Loft"}})
	require.NoError(t, err)

	_, err = s.LinkDevice(ctx, &api2.LinkDeviceRequest{DeviceId: "sensor-1", RoomId: room.Id})
	require.NoError(t, err)
	_, err = s.LinkDevice(ctx, &api2.LinkDeviceRequest{DeviceId: "sensor-2", RoomId: otherRoom.Id})
	require.NoError(t, err)

	sendMotion := func(deviceID string, motion bool) {
		s.agg.handleUpdate(&api2.Update{
			Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{
				Device: &apiDevice.Device{
					Id: deviceID,
					Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
						Presence: &apiTrait.Presence{State: &apiTrait.Presence_State{MotionDetected: motion}},
					}},
				},
			}},
		})
	}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Buffered large enough that the test's own reader never blocks Send -
	// this test is about the sink's internal 10-slot buffer, not this one.
	fake := &fakeStreamHouseUpdatesServer{ctx: streamCtx, sent: make(chan *api2.HouseUpdate, 100)}

	done := make(chan error, 1)
	go func() {
		done <- s.StreamHouseUpdates(&api2.StreamHouseUpdatesRequest{BuildingId: room.BuildingId}, fake)
	}()

	// Let StreamHouseUpdates subscribe and drain its initial snapshot
	// (a building-state message, plus no room ones - no Properties have been
	// computed yet) before flooding, so the burst below lands on the
	// live-update path, not the snapshot.
	time.Sleep(50 * time.Millisecond)

	// Flood well past the sink's fixed buffer size (10) with updates for
	// the OTHER building, alternating so each one actually changes
	// Properties and gets published.
	for i := 0; i < 30; i++ {
		sendMotion("sensor-2", i%2 == 0)
	}

	// room's own update must still arrive, not have been evicted by the
	// flood above.
	sendMotion("sensor-1", true)
	roomUpdate := recvRoomUpdate(t, fake.sent, 2*time.Second)
	assert.Equal(t, room.Id, roomUpdate.RoomId, "room's update must survive a burst of another building's updates")

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("StreamHouseUpdates did not return after context cancellation")
	}
}

func TestResolveDevices_NilClientReturnsNil(t *testing.T) {
	s := newTestService(t, nil)
	assert.Nil(t, s.resolveDevices(context.Background()))
}

func TestResolveDevices_ClientErrorReturnsNil(t *testing.T) {
	client := &fakeBridgeClient{
		listDevicesFn: func(ctx context.Context, req *api2.ListDevicesRequest) (*api2.ListDevicesResponse, error) {
			return nil, errors.New("bridge unreachable")
		},
	}
	s := newTestService(t, client)
	assert.Nil(t, s.resolveDevices(context.Background()))
}

func TestResolveDevices_Success(t *testing.T) {
	client := &fakeBridgeClient{
		listDevicesFn: func(ctx context.Context, req *api2.ListDevicesRequest) (*api2.ListDevicesResponse, error) {
			return &api2.ListDevicesResponse{
				Devices: []*apiDevice.Device{
					{Id: "device-1", Config: &apiDevice.Device_Config{Name: "Kitchen Light"}},
				},
			}, nil
		},
	}
	s := newTestService(t, client)
	devices := s.resolveDevices(context.Background())
	require.Contains(t, devices, "device-1")
	assert.Equal(t, "Kitchen Light", devices["device-1"].GetConfig().GetName())
}

func TestGetRoom_EmbedsResolvedDevices(t *testing.T) {
	client := &fakeBridgeClient{
		getDeviceFn: func(ctx context.Context, req *api2.GetDeviceRequest) (*apiDevice.Device, error) {
			require.Equal(t, "device-1", req.GetId())
			return &apiDevice.Device{Id: "device-1", Config: &apiDevice.Device_Config{Name: "Resolved device-1"}}, nil
		},
	}
	s := newTestService(t, client)
	ctx := context.Background()

	room := createTestRoom(t, s)

	_, err := s.LinkDevice(ctx, &api2.LinkDeviceRequest{DeviceId: "device-1", RoomId: room.Id})
	require.NoError(t, err)

	got, err := s.GetRoom(ctx, &api2.GetRoomRequest{Id: room.Id})
	require.NoError(t, err)
	require.Len(t, got.Devices, 1)
	assert.Equal(t, "Resolved device-1", got.Devices[0].GetConfig().GetName())
}

func TestListRooms_RequiresBuildingOrFloorFilter(t *testing.T) {
	s := newTestService(t, nil)
	ctx := context.Background()

	createTestRoom(t, s)

	stream := &fakeListRoomsServer{ctx: ctx}
	err := s.ListRooms(&api2.ListRoomsRequest{}, stream)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.InvalidArgument, st.Code())
}

func TestCreateFloor_UnknownBuildingIsNotFound(t *testing.T) {
	s := newTestService(t, nil)
	ctx := context.Background()

	_, err := s.CreateFloor(ctx, &api2.CreateFloorRequest{BuildingId: "does-not-exist", Name: "Ghost Floor"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
}

func TestUpdateBuilding_VersionMismatchIsFailedPrecondition(t *testing.T) {
	s := newTestService(t, nil)
	ctx := context.Background()

	b, err := s.CreateBuilding(ctx, &api2.CreateBuildingRequest{Config: &api2.Building_Config{Name: "Home"}})
	require.NoError(t, err)

	_, err = s.UpdateBuilding(ctx, &api2.UpdateBuildingRequest{Id: b.Id, Version: "stale", Config: &api2.Building_Config{Name: "New"}})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
}

func TestDeleteFloor_BlockedByRoomIsFailedPrecondition(t *testing.T) {
	s := newTestService(t, nil)
	ctx := context.Background()

	room := createTestRoom(t, s)

	_, err := s.DeleteFloor(ctx, &api2.DeleteFloorRequest{Id: room.FloorId})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
}

// TestDeleteRoom_VersionMismatchIsFailedPrecondition covers the same
// mapDBErr wiring TestDeleteFloor_BlockedByRoomIsFailedPrecondition covers
// for ErrHasChildren, but for the version check added to every Delete*
// handler alongside Update*'s existing one.
func TestDeleteRoom_VersionMismatchIsFailedPrecondition(t *testing.T) {
	s := newTestService(t, nil)
	ctx := context.Background()

	room := createTestRoom(t, s)

	_, err := s.DeleteRoom(ctx, &api2.DeleteRoomRequest{Id: room.Id, Version: "stale"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())

	// The current version is accepted.
	_, err = s.DeleteRoom(ctx, &api2.DeleteRoomRequest{Id: room.Id, Version: room.Version})
	require.NoError(t, err)
}

func TestLinkDevice_ReturnsPreviousRoomID(t *testing.T) {
	s := newTestService(t, nil)
	ctx := context.Background()

	roomA := createTestRoom(t, s)
	roomB, err := s.CreateRoom(ctx, &api2.CreateRoomRequest{FloorId: roomA.FloorId, Config: &api2.Room_Config{Name: "Office"}})
	require.NoError(t, err)

	resp, err := s.LinkDevice(ctx, &api2.LinkDeviceRequest{DeviceId: "device-1", RoomId: roomA.Id})
	require.NoError(t, err)
	assert.Nil(t, resp.PreviousRoomId)

	resp, err = s.LinkDevice(ctx, &api2.LinkDeviceRequest{DeviceId: "device-1", RoomId: roomB.Id})
	require.NoError(t, err)
	require.NotNil(t, resp.PreviousRoomId)
	assert.Equal(t, roomA.Id, *resp.PreviousRoomId)
}

func TestLinkDevice_VersionMismatchIsFailedPrecondition(t *testing.T) {
	s := newTestService(t, nil)
	ctx := context.Background()

	roomA := createTestRoom(t, s)
	roomB, err := s.CreateRoom(ctx, &api2.CreateRoomRequest{FloorId: roomA.FloorId, Config: &api2.Room_Config{Name: "Office"}})
	require.NoError(t, err)

	resp, err := s.LinkDevice(ctx, &api2.LinkDeviceRequest{DeviceId: "device-1", RoomId: roomA.Id})
	require.NoError(t, err)

	_, err = s.LinkDevice(ctx, &api2.LinkDeviceRequest{DeviceId: "device-1", RoomId: roomB.Id, Version: "stale"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())

	// The current version is accepted.
	_, err = s.LinkDevice(ctx, &api2.LinkDeviceRequest{DeviceId: "device-1", RoomId: roomB.Id, Version: resp.Link.Version})
	require.NoError(t, err)
}

func TestCreateBuilding_DefaultsAvailableModes(t *testing.T) {
	s := newTestService(t, nil)
	ctx := context.Background()

	b, err := s.CreateBuilding(ctx, &api2.CreateBuildingRequest{Config: &api2.Building_Config{Name: "Home"}})
	require.NoError(t, err)
	assert.Equal(t, defaultAvailableModes, b.Config.AvailableModes)
	assert.Empty(t, b.State.Mode)
	assert.Nil(t, b.State.Occupied, "no room has reported any occupancy signal yet")

	// An explicit list overrides the default.
	custom, err := s.CreateBuilding(ctx, &api2.CreateBuildingRequest{
		Config: &api2.Building_Config{Name: "Cabin", AvailableModes: []string{"open", "closed"}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"open", "closed"}, custom.Config.AvailableModes)
}

func TestSetHouseMode(t *testing.T) {
	s := newTestService(t, nil)
	ctx := context.Background()

	b, err := s.CreateBuilding(ctx, &api2.CreateBuildingRequest{Config: &api2.Building_Config{Name: "Home"}})
	require.NoError(t, err)

	updated, err := s.SetHouseMode(ctx, &api2.SetHouseModeRequest{BuildingId: b.Id, Mode: "away"})
	require.NoError(t, err)
	assert.Equal(t, "away", updated.State.Mode)

	got, err := s.GetBuilding(ctx, &api2.GetBuildingRequest{Id: b.Id})
	require.NoError(t, err)
	assert.Equal(t, "away", got.State.Mode)

	// A mode outside available_modes is rejected.
	_, err = s.SetHouseMode(ctx, &api2.SetHouseModeRequest{BuildingId: b.Id, Mode: "bogus"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.InvalidArgument, st.Code())

	// Clearing is always allowed, regardless of available_modes.
	cleared, err := s.SetHouseMode(ctx, &api2.SetHouseModeRequest{BuildingId: b.Id, Mode: ""})
	require.NoError(t, err)
	assert.Empty(t, cleared.State.Mode)

	_, err = s.SetHouseMode(ctx, &api2.SetHouseModeRequest{BuildingId: "does-not-exist", Mode: "home"})
	require.Error(t, err)
	st, ok = status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
}

// TestGetBuilding_OccupiedReflectsRoomMotion covers the house.Service wiring
// from a linked room's Presence motion to Building.State.occupied - the
// decay/window behaviour itself is covered at the aggregator level by
// TestAggregator_BuildingOccupied.
func TestGetBuilding_OccupiedReflectsRoomMotion(t *testing.T) {
	s := newTestService(t, nil)
	ctx := context.Background()

	room := createTestRoom(t, s)

	before, err := s.GetBuilding(ctx, &api2.GetBuildingRequest{Id: room.BuildingId})
	require.NoError(t, err)
	assert.Nil(t, before.State.Occupied)

	_, err = s.LinkDevice(ctx, &api2.LinkDeviceRequest{DeviceId: "sensor-1", RoomId: room.Id})
	require.NoError(t, err)
	s.agg.handleUpdate(&api2.Update{
		Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{
			Device: &apiDevice.Device{
				Id: "sensor-1",
				Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
					Presence: &apiTrait.Presence{State: &apiTrait.Presence_State{MotionDetected: true}},
				}},
			},
		}},
	})

	after, err := s.GetBuilding(ctx, &api2.GetBuildingRequest{Id: room.BuildingId})
	require.NoError(t, err)
	require.NotNil(t, after.State.Occupied)
	assert.True(t, *after.State.Occupied)
}
