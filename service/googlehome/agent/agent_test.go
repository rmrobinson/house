package agent

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
)

// fakeHouseClient is a minimal api2.HouseServiceClient stand-in -
// roomNamesByDeviceID only ever calls ListRooms/ListDeviceLinks, so every
// other method panics (via the embedded nil interface) if a test reaches
// it, same convention as house.deviceconfig_test.go's fakeBridgeServer.
type fakeHouseClient struct {
	api2.HouseServiceClient
	rooms []*api2.Room
	links []*api2.DeviceRoomLink
}

func (f *fakeHouseClient) ListRooms(ctx context.Context, req *api2.ListRoomsRequest, opts ...grpc.CallOption) (api2.HouseService_ListRoomsClient, error) {
	return &fakeRoomStream{items: f.rooms}, nil
}

func (f *fakeHouseClient) ListDeviceLinks(ctx context.Context, req *api2.ListDeviceLinksRequest, opts ...grpc.CallOption) (api2.HouseService_ListDeviceLinksClient, error) {
	return &fakeLinkStream{items: f.links}, nil
}

type fakeRoomStream struct {
	grpc.ClientStream
	items []*api2.Room
	i     int
}

func (s *fakeRoomStream) Recv() (*api2.Room, error) {
	if s.i >= len(s.items) {
		return nil, io.EOF
	}
	item := s.items[s.i]
	s.i++
	return item, nil
}

type fakeLinkStream struct {
	grpc.ClientStream
	items []*api2.DeviceRoomLink
	i     int
}

func (s *fakeLinkStream) Recv() (*api2.DeviceRoomLink, error) {
	if s.i >= len(s.items) {
		return nil, io.EOF
	}
	item := s.items[s.i]
	s.i++
	return item, nil
}

func TestRoomNamesByDeviceID(t *testing.T) {
	house := &fakeHouseClient{
		rooms: []*api2.Room{
			{Id: "room1", Config: &api2.Room_Config{Name: "Office"}},
			{Id: "room2", Config: &api2.Room_Config{Name: "Primary Bedroom"}},
		},
		links: []*api2.DeviceRoomLink{
			{DeviceId: "d1", RoomId: "room1"},
			{DeviceId: "d2", RoomId: "room2"},
			// A link pointing at a room that no longer exists must be
			// dropped, not panic or produce an empty-string hint.
			{DeviceId: "d3", RoomId: "room-deleted"},
		},
	}
	a := &Agent{houseClient: house, buildingID: "b1", logger: zap.NewNop()}

	names, err := a.roomNamesByDeviceID(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "Office", names["d1"])
	assert.Equal(t, "Primary Bedroom", names["d2"])
	_, ok := names["d3"]
	assert.False(t, ok)
}

// TestRoomNamesByDeviceID_NoHouseClient covers the degrade-gracefully case:
// googlehome-agentd pointed at a standalone bridgefacaded (no HouseService
// at all) configures no houseClient, and SYNC must not fail just because
// RoomHint can't be looked up.
func TestRoomNamesByDeviceID_NoHouseClient(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}

	names, err := a.roomNamesByDeviceID(context.Background())
	require.NoError(t, err)
	assert.Nil(t, names)
}
