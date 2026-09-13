package house

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	apiDevice "github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/house/db"
)

// fakeBridgeClient is a minimal api2.BridgeServiceClient stand-in - Service
// only ever calls ListDevices (see resolveDevices), so every other method
// panics if a test reaches it.
type fakeBridgeClient struct {
	listDevicesFn func(ctx context.Context, req *api2.ListDevicesRequest) (*api2.ListDevicesResponse, error)
}

func (f *fakeBridgeClient) GetBridge(ctx context.Context, in *api2.GetBridgeRequest, opts ...grpc.CallOption) (*api2.Bridge, error) {
	panic("not implemented in fake")
}
func (f *fakeBridgeClient) ListDevices(ctx context.Context, in *api2.ListDevicesRequest, opts ...grpc.CallOption) (*api2.ListDevicesResponse, error) {
	return f.listDevicesFn(ctx, in)
}
func (f *fakeBridgeClient) GetDevice(ctx context.Context, in *api2.GetDeviceRequest, opts ...grpc.CallOption) (*apiDevice.Device, error) {
	panic("not implemented in fake")
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
// MaxOpenConns is pinned to 1 - mattn/go-sqlite3's :memory: database is
// per-connection, so a pooled second connection would see an empty schema.
func newTestService(t *testing.T, bridgeClient api2.BridgeServiceClient) *Service {
	t.Helper()

	sqlDB, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })

	database, err := db.NewDatabase(zaptest.NewLogger(t), sqlDB)
	require.NoError(t, err)

	return NewService(zaptest.NewLogger(t), database, bridgeClient)
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
		listDevicesFn: func(ctx context.Context, req *api2.ListDevicesRequest) (*api2.ListDevicesResponse, error) {
			return &api2.ListDevicesResponse{
				Devices: []*apiDevice.Device{
					{Id: "device-1", Config: &apiDevice.Device_Config{Name: "Resolved device-1"}},
				},
			}, nil
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
