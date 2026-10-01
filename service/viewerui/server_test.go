package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	apiDevice "github.com/rmrobinson/house/api/device"
	apiTrait "github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/lib/grpcutil"
)

type fakeHouse struct {
	api2.UnimplementedHouseServiceServer
	updates chan *api2.RoomUpdate
}

func (f *fakeHouse) ListBuildings(_ *api2.ListBuildingsRequest, s api2.HouseService_ListBuildingsServer) error {
	return s.Send(&api2.Building{Id: "b1", Config: &api2.Building_Config{Name: "Home"}})
}
func (f *fakeHouse) GetBuilding(context.Context, *api2.GetBuildingRequest) (*api2.Building, error) {
	return &api2.Building{Id: "b1", Config: &api2.Building_Config{Name: "Home"}}, nil
}
func (f *fakeHouse) ListFloors(_ *api2.ListFloorsRequest, s api2.HouseService_ListFloorsServer) error {
	s.Send(&api2.Floor{Id: "f2", Name: "Upstairs", SortOrder: 2, BuildingId: "b1"})
	return s.Send(&api2.Floor{Id: "f1", Name: "Main", SortOrder: 1, BuildingId: "b1"})
}
func (f *fakeHouse) ListRooms(_ *api2.ListRoomsRequest, s api2.HouseService_ListRoomsServer) error {
	occ := true
	s.Send(&api2.Room{Id: "r2", Config: &api2.Room_Config{Name: "Kitchen"}, Properties: &api2.Room_Properties{Occupied: &occ}})
	return s.Send(&api2.Room{Id: "r1", Config: &api2.Room_Config{Name: "Bath"}})
}
func (f *fakeHouse) GetRoom(context.Context, *api2.GetRoomRequest) (*api2.Room, error) {
	temp := 21.4
	return &api2.Room{
		Id: "r1", FloorId: "f1", BuildingId: "b1",
		Config:     &api2.Room_Config{Name: "Bath"},
		Properties: &api2.Room_Properties{TemperatureC: &temp},
		Devices:    []*apiDevice.Device{lamp(false), cam()},
	}, nil
}
func (f *fakeHouse) StreamHouseUpdates(_ *api2.StreamHouseUpdatesRequest, s api2.HouseService_StreamHouseUpdatesServer) error {
	for {
		select {
		case <-s.Context().Done():
			return nil
		case u := <-f.updates:
			if err := s.Send(u); err != nil {
				return err
			}
		}
	}
}

func lamp(on bool) *apiDevice.Device {
	return &apiDevice.Device{
		Id:      "lamp",
		Config:  &apiDevice.Device_Config{Name: "Lamp"},
		Address: &apiDevice.Device_Address{IsReachable: true},
		Details: &apiDevice.Device_Light{Light: &apiDevice.Light{OnOff: &apiTrait.OnOff{
			Attributes: &apiTrait.OnOff_Attributes{CanControl: true},
			State:      &apiTrait.OnOff_State{IsOn: on},
		}}},
	}
}

func cam() *apiDevice.Device {
	return &apiDevice.Device{
		Id:      "cam1",
		Config:  &apiDevice.Device_Config{Name: "Door cam"},
		Details: &apiDevice.Device_Camera{Camera: &apiDevice.Camera{MediaStream: &apiTrait.MediaStream{State: &apiTrait.MediaStream_State{Url: "http://go2rtc:1984/api/webrtc?src=door"}}}},
	}
}

type fakeBridge struct {
	api2.UnimplementedBridgeServiceServer
	gotCmd *command.Command
	// camURL overrides cam1's stream url when set; "none" means empty.
	camURL string
}

func (f *fakeBridge) GetDevice(_ context.Context, r *api2.GetDeviceRequest) (*apiDevice.Device, error) {
	if r.GetId() == "cam1" {
		c := cam()
		switch f.camURL {
		case "":
		case "none":
			c.GetCamera().GetMediaStream().GetState().Url = ""
		default:
			c.GetCamera().GetMediaStream().GetState().Url = f.camURL
		}
		return c, nil
	}
	return lamp(false), nil
}
func (f *fakeBridge) ExecuteCommand(_ context.Context, c *command.Command) (*apiDevice.Device, error) {
	f.gotCmd = c
	return lamp(c.GetOnOff().GetOn()), nil
}
func (f *fakeBridge) StreamUpdates(_ *api2.StreamUpdatesRequest, s api2.BridgeService_StreamUpdatesServer) error {
	<-s.Context().Done()
	return nil
}

func startTestServer(t *testing.T) (*Server, *fakeHouse, *fakeBridge) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	house := &fakeHouse{updates: make(chan *api2.RoomUpdate, 4)}
	bridge := &fakeBridge{}
	gs := grpc.NewServer()
	api2.RegisterHouseServiceServer(gs, house)
	api2.RegisterBridgeServiceServer(gs, bridge)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)

	conn, err := grpcutil.DialInsecure(lis.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newServer(ctx, zaptest.NewLogger(t), api2.NewHouseServiceClient(conn), api2.NewBridgeServiceClient(conn)), house, bridge
}

func get(s *Server, path string, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, r)
	return rec
}

func TestRootRedirectsToOnlyBuilding(t *testing.T) {
	s, _, _ := startTestServer(t)
	rec := get(s, "/")
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/buildings/b1", rec.Header().Get("Location"))
}

func TestBuildingPageListsFloorsInOrderAndFirstFloorRooms(t *testing.T) {
	s, _, _ := startTestServer(t)
	body := get(s, "/buildings/b1").Body.String()

	assert.Less(t, strings.Index(body, "Main"), strings.Index(body, "Upstairs"), "floors by sort_order")
	// Rooms sorted by name; Kitchen is occupied, Bath unknown.
	assert.Less(t, strings.Index(body, "Bath"), strings.Index(body, "Kitchen"))
	assert.Contains(t, body, `id="room-dot-r2" class="dot dot-yes"`)
	assert.Contains(t, body, `id="room-dot-r1" class="dot dot-unknown"`)
	assert.Contains(t, body, `sse-connect="/buildings/b1/events"`)
}

func TestBuildingPageSelectsFirstRoomByDefault(t *testing.T) {
	s, _, _ := startTestServer(t)
	body := get(s, "/buildings/b1").Body.String()
	assert.Contains(t, body, `<li class="active"><button class="row" hx-get="/rooms/r1"`, "Bath (first by name) is active")
	assert.Contains(t, body, "21.4°C", "its detail is rendered, not a placeholder")
	assert.NotContains(t, body, "Select a room")
}

func TestFloorFragmentAlsoSwapsDetailToFirstRoom(t *testing.T) {
	s, _, _ := startTestServer(t)
	body := get(s, "/buildings/b1/floors/f2").Body.String()
	assert.Contains(t, body, `<div id="detail" class="col-detail" hx-swap-oob="innerHTML">`)
	assert.Contains(t, body, "21.4°C")
}

func TestFloorFragmentMarksActiveFloor(t *testing.T) {
	s, _, _ := startTestServer(t)
	body := get(s, "/buildings/b1/floors/f2").Body.String()
	assert.Contains(t, body, `<li class="active"><button class="row" hx-get="/buildings/b1/floors/f2"`)
	assert.NotContains(t, body, "<html")
}

func TestRoomDetailShowsPropertiesTogglesAndCameraButton(t *testing.T) {
	s, _, _ := startTestServer(t)
	body := get(s, "/rooms/r1", "HX-Request", "true").Body.String()

	assert.Contains(t, body, "21.4°C", "70.5°F shown in Celsius")
	assert.Contains(t, body, "[ OFF ]")
	assert.Contains(t, body, `hx-post="/devices/lamp/commands"`)
	assert.Contains(t, body, `hx-vals='{"on": "true"}'`)
	assert.Contains(t, body, `hx-get="/rooms/r1/camera/cam1"`)
	assert.Contains(t, body, "[ VIEW CAMERA ]")
}

func TestRoomDirectNavigationRedirectsToFullPage(t *testing.T) {
	s, _, _ := startTestServer(t)
	rec := get(s, "/rooms/r1")
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/buildings/b1?room=r1", rec.Header().Get("Location"))

	page := get(s, "/buildings/b1?room=r1").Body.String()
	assert.Contains(t, page, "21.4°C", "room pre-rendered in the detail pane")
	assert.Contains(t, page, `class="active"><button class="row" hx-get="/rooms/r1"`)
}

func TestCameraFragmentCarriesWHEPURL(t *testing.T) {
	s, _, _ := startTestServer(t)
	body := get(s, "/rooms/r1/camera/cam1", "HX-Request", "true").Body.String()
	assert.Contains(t, body, `data-whep="http://go2rtc:1984/api/webrtc?src=door"`)
}

func TestCameraFragmentRejectsNonHTTPURL(t *testing.T) {
	s, _, bridge := startTestServer(t)
	bridge.camURL = "rtsp://cam.local/stream"
	body := get(s, "/rooms/r1/camera/cam1", "HX-Request", "true").Body.String()
	assert.NotContains(t, body, "data-whep")
	assert.Contains(t, body, "isn&#39;t WHEP-compatible")

	bridge.camURL = "none"
	body = get(s, "/rooms/r1/camera/cam1", "HX-Request", "true").Body.String()
	assert.Contains(t, body, "isn&#39;t reporting a stream URL")
}

func TestDeviceCommandSendsOnOffAndReturnsUpdatedRow(t *testing.T) {
	s, _, bridge := startTestServer(t)
	r := httptest.NewRequest("POST", "/devices/lamp/commands", strings.NewReader("on=true"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, r)

	require.NotNil(t, bridge.gotCmd)
	assert.Equal(t, "lamp", bridge.gotCmd.GetDeviceId())
	assert.True(t, bridge.gotCmd.GetOnOff().GetOn())
	assert.Contains(t, rec.Body.String(), "[ ON ]")
	assert.NotContains(t, rec.Body.String(), "hx-swap-oob")
}

func TestEventsStreamsRoomUpdateAsOOB(t *testing.T) {
	s, house, _ := startTestServer(t)

	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/buildings/b1/events")
	require.NoError(t, err)
	defer resp.Body.Close()

	// The hub's upstream stream starts lazily on first subscribe and a
	// message sent before it connects is lost, so keep sending until one
	// arrives.
	occ := true
	done := make(chan string, 1)
	deadline := time.After(5 * time.Second)
	go func() {
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		done <- string(buf[:n])
	}()
	for {
		select {
		case got := <-done:
			assert.Contains(t, got, `id="room-dot-r1"`)
			assert.Contains(t, got, "dot-yes")
			assert.Contains(t, got, `<div hx-swap-oob="afterbegin:#event-log"><div>Bath:`, "each entry is its own block")
			assert.Contains(t, got, "Bath:")
			return
		case <-time.After(50 * time.Millisecond):
			select {
			case house.updates <- &api2.RoomUpdate{RoomId: "r1", Properties: &api2.Room_Properties{Occupied: &occ}}:
			default:
			}
		case <-deadline:
			t.Fatal("no SSE message")
		}
	}
}
