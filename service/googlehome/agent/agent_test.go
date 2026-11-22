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
	apiDevice "github.com/rmrobinson/house/api/device"
)

// fakeHouseClient is a minimal api2.HouseServiceClient stand-in -
// roomNamesByDeviceID only ever calls ListRooms, so every other method
// panics (via the embedded nil interface) if a test reaches it, same
// convention as house.deviceconfig_test.go's fakeBridgeServer.
type fakeHouseClient struct {
	api2.HouseServiceClient
	rooms []*api2.Room
}

func (f *fakeHouseClient) ListRooms(ctx context.Context, req *api2.ListRoomsRequest, opts ...grpc.CallOption) (api2.HouseService_ListRoomsClient, error) {
	return &fakeRoomStream{items: f.rooms}, nil
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

func TestRoomNamesByDeviceID(t *testing.T) {
	house := &fakeHouseClient{
		rooms: []*api2.Room{
			{
				Id:     "room1",
				Config: &api2.Room_Config{Name: "Office"},
				Devices: []*apiDevice.Device{
					{Id: "d1"},
				},
			},
			{
				Id:     "room2",
				Config: &api2.Room_Config{Name: "Primary Bedroom"},
				Devices: []*apiDevice.Device{
					{Id: "d2"},
				},
			},
			// A room with no devices at all must not panic or contribute
			// any entries.
			{Id: "room3", Config: &api2.Room_Config{Name: "Empty Room"}},
		},
	}
	a := &Agent{houseCfg: HouseServiceConfig{Client: house, BuildingID: "b1"}, logger: zap.NewNop()}

	names, err := a.roomNamesByDeviceID(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "Office", names["d1"])
	assert.Equal(t, "Primary Bedroom", names["d2"])
	assert.Len(t, names, 2)
}

// TestRoomNamesByDeviceID_NoHouseClient covers the degrade-gracefully case:
// googlehome-agentd pointed at a standalone bridgefacaded (no HouseService
// at all) configures no houseCfg, and SYNC must not fail just because
// RoomHint can't be looked up.
func TestRoomNamesByDeviceID_NoHouseClient(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}

	names, err := a.roomNamesByDeviceID(context.Background())
	require.NoError(t, err)
	assert.Nil(t, names)
}

// TestRoomNamesByDeviceID_ClientSetButNoBuildingID covers a misconfigured
// HouseServiceConfig (Client set, BuildingID empty) - main.go's own
// startup check already prevents constructing one today, but this guards
// the invariant at the type's own point of use too, so a future caller
// can't silently query ListRooms with an empty BuildingId and get back
// zero rows with no signal anything is wrong.
func TestRoomNamesByDeviceID_ClientSetButNoBuildingID(t *testing.T) {
	house := &fakeHouseClient{rooms: []*api2.Room{
		{Id: "room1", Config: &api2.Room_Config{Name: "Office"}, Devices: []*apiDevice.Device{{Id: "d1"}}},
	}}
	a := &Agent{houseCfg: HouseServiceConfig{Client: house, BuildingID: ""}, logger: zap.NewNop()}

	names, err := a.roomNamesByDeviceID(context.Background())
	require.NoError(t, err)
	assert.Nil(t, names)
}
