package metrics

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
)

// fakeHouseServer serves a fixed set of buildings and, per building, rooms.
type fakeHouseServer struct {
	api2.UnimplementedHouseServiceServer

	mu        sync.Mutex
	buildings []string
	rooms     map[string][]*api2.Room
	roomsErr  error
}

func (f *fakeHouseServer) ListBuildings(_ *api2.ListBuildingsRequest, stream api2.HouseService_ListBuildingsServer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.buildings {
		if err := stream.Send(&api2.Building{Id: id}); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeHouseServer) ListRooms(req *api2.ListRoomsRequest, stream api2.HouseService_ListRoomsServer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.roomsErr != nil {
		return f.roomsErr
	}
	for _, r := range f.rooms[req.GetBuildingId()] {
		if err := stream.Send(r); err != nil {
			return err
		}
	}
	return nil
}

func room(id, name string, deviceIDs ...string) *api2.Room {
	r := &api2.Room{Id: id, Config: &api2.Room_Config{Name: name}}
	for _, d := range deviceIDs {
		r.Devices = append(r.Devices, &apiDevice.Device{Id: d})
	}
	return r
}

func newRoomResolverHarness(t *testing.T, srv *fakeHouseServer) (*RoomResolver, api2.HouseServiceClient) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	api2.RegisterHouseServiceServer(gs, srv)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	return NewRoomResolver(zaptest.NewLogger(t)), api2.NewHouseServiceClient(conn)
}

func TestRoomResolverResolvesIDAndNameAcrossBuildings(t *testing.T) {
	srv := &fakeHouseServer{
		buildings: []string{"b1", "b2"},
		rooms: map[string][]*api2.Room{
			"b1": {room("r-office", "Office", "2930245624", "lamp"), room("r-bed", "Bedroom", "2930170133")},
			"b2": {room("r-shed", "Shed", "freezer")},
		},
	}
	r, client := newRoomResolverHarness(t, srv)

	require.NoError(t, r.refresh(context.Background(), client))

	// Two sensors with the same device name are told apart by their rooms.
	assert.Equal(t, "r-office", r.RoomID("2930245624"))
	assert.Equal(t, "Office", r.RoomName("2930245624"))
	assert.Equal(t, "r-bed", r.RoomID("2930170133"))
	assert.Equal(t, "Bedroom", r.RoomName("2930170133"))
	assert.Equal(t, "Shed", r.RoomName("freezer"))

	// A device that isn't linked anywhere has neither.
	assert.Empty(t, r.RoomID("stray"))
	assert.Empty(t, r.RoomName("stray"))
}

func TestRoomResolverKeepsPreviousRoomsWhenARefreshFails(t *testing.T) {
	srv := &fakeHouseServer{
		buildings: []string{"b1"},
		rooms:     map[string][]*api2.Room{"b1": {room("r-office", "Office", "lamp")}},
	}
	r, client := newRoomResolverHarness(t, srv)
	require.NoError(t, r.refresh(context.Background(), client))

	srv.mu.Lock()
	srv.roomsErr = status.Error(codes.Unavailable, "housed restarting")
	srv.mu.Unlock()

	assert.Error(t, r.refresh(context.Background(), client))
	// A housed restart must not blank every room tag.
	assert.Equal(t, "Office", r.RoomName("lamp"))
	assert.Equal(t, "r-office", r.RoomID("lamp"))
}

func TestRoomResolverFollowsADeviceMovingRooms(t *testing.T) {
	srv := &fakeHouseServer{
		buildings: []string{"b1"},
		rooms:     map[string][]*api2.Room{"b1": {room("r-a", "Den", "lamp"), room("r-b", "Hall")}},
	}
	r, client := newRoomResolverHarness(t, srv)
	require.NoError(t, r.refresh(context.Background(), client))
	assert.Equal(t, "Den", r.RoomName("lamp"))

	srv.mu.Lock()
	srv.rooms = map[string][]*api2.Room{"b1": {room("r-a", "Den"), room("r-b", "Hall", "lamp")}}
	srv.mu.Unlock()
	require.NoError(t, r.refresh(context.Background(), client))

	assert.Equal(t, "Hall", r.RoomName("lamp"))
}

// namedRooms is a RoomLookup with explicit room names, for label tests.
type namedRooms map[string]string // device ID -> room name

func (n namedRooms) RoomID(id string) string {
	if n[id] == "" {
		return ""
	}
	return "room-" + n[id]
}
func (n namedRooms) RoomName(id string) string { return n[id] }

func TestDeviceLabel(t *testing.T) {
	for name, tc := range map[string]struct {
		rooms      RoomLookup
		id, device string
		want       string
	}{
		"name and room, as the policy notifications write them": {
			namedRooms{"s1": "Office"}, "s1", "Airthings Wave Plus", "Airthings Wave Plus (Office)",
		},
		"no room is just the name": {
			namedRooms{}, "s1", "Airthings Wave Plus", "Airthings Wave Plus",
		},
		"no name falls back to the id, like GetDeviceName": {
			namedRooms{"s1": "Office"}, "s1", "", "s1 (Office)",
		},
		"no name and no room is the id": {
			namedRooms{}, "s1", "", "s1",
		},
		"no room lookup at all is just the name": {
			nil, "s1", "Lamp", "Lamp",
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := NewService(zaptest.NewLogger(t), &recordingSink{}, NewSampler(DefaultSampling()), tc.rooms)
			assert.Equal(t, tc.want, s.deviceLabel(tc.id, tc.device))
		})
	}
}

func TestSameNamedDevicesGetDistinctLabels(t *testing.T) {
	s := NewService(zaptest.NewLogger(t), &recordingSink{}, NewSampler(DefaultSampling()),
		namedRooms{"2930245624": "Office", "2930170133": "Bedroom"})

	a := s.deviceLabel("2930245624", "Airthings Wave Plus")
	b := s.deviceLabel("2930170133", "Airthings Wave Plus")

	assert.NotEqual(t, a, b)
}
