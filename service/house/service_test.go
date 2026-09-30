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
	sent chan *api2.RoomUpdate
}

func (s *fakeStreamHouseUpdatesServer) Context() context.Context { return s.ctx }

func (s *fakeStreamHouseUpdatesServer) Send(u *api2.RoomUpdate) error {
	s.sent <- u
	return nil
}

func TestStreamHouseUpdates_RequiresBuildingID(t *testing.T) {
	s := newTestService(t, nil)
	err := s.StreamHouseUpdates(&api2.StreamHouseUpdatesRequest{}, &fakeStreamHouseUpdatesServer{ctx: context.Background(), sent: make(chan *api2.RoomUpdate, 1)})
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
	fake := &fakeStreamHouseUpdatesServer{ctx: streamCtx, sent: make(chan *api2.RoomUpdate, 10)}

	done := make(chan error, 1)
	go func() {
		done <- s.StreamHouseUpdates(&api2.StreamHouseUpdatesRequest{BuildingId: room.BuildingId}, fake)
	}()

	select {
	case u := <-fake.sent:
		assert.Equal(t, room.Id, u.RoomId, "initial snapshot must only cover room's own building")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial snapshot")
	}

	sendMotion("sensor-1", false)
	select {
	case u := <-fake.sent:
		assert.Equal(t, room.Id, u.RoomId)
		require.NotNil(t, u.Properties.Occupied)
		assert.False(t, u.Properties.GetOccupied())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for live update")
	}

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
