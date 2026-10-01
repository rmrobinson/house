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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
	co2 := int32(640)
	return &api2.Room{
		Id: "r1", FloorId: "f1", BuildingId: "b1",
		Config:     &api2.Room_Config{Name: "Bath"},
		Properties: &api2.Room_Properties{TemperatureC: &temp, Co2Ppm: &co2},
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

func lamp(on bool) *apiDevice.Device { return dimLamp(on, 40) }

func dimLamp(on bool, level int32) *apiDevice.Device {
	return &apiDevice.Device{
		Id:      "lamp",
		Config:  &apiDevice.Device_Config{Name: "Lamp"},
		Address: &apiDevice.Device_Address{IsReachable: true},
		Details: &apiDevice.Device_Light{Light: &apiDevice.Light{
			OnOff: &apiTrait.OnOff{
				Attributes: &apiTrait.OnOff_Attributes{CanControl: true},
				State:      &apiTrait.OnOff_State{IsOn: on},
			},
			Brightness: &apiTrait.Brightness{
				Attributes: &apiTrait.Brightness_Attributes{CanControl: true},
				State:      &apiTrait.Brightness_State{Level: level},
			},
		}},
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
	// camEndpoints, when set, are cam1's MediaStream endpoints.
	camEndpoints []*apiTrait.MediaStream_Endpoint
	// cmdErr, when set, is returned from ExecuteCommand.
	cmdErr error
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
		c.GetCamera().GetMediaStream().GetState().Endpoints = f.camEndpoints
		return c, nil
	}
	return lamp(false), nil
}
func (f *fakeBridge) ExecuteCommand(_ context.Context, c *command.Command) (*apiDevice.Device, error) {
	f.gotCmd = c
	if f.cmdErr != nil {
		return nil, f.cmdErr
	}
	if b := c.GetBrightnessAbsolute(); b != nil {
		return dimLamp(true, b.GetBrightnessPercent()), nil
	}
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

func post(s *Server, path, body string, htmx bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if htmx {
		r.Header.Set("HX-Request", "true")
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, r)
	return rec
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
	assert.Contains(t, body, `<li class="active"><button class="row" aria-current="true" hx-get="/rooms/r1"`, "Bath (first by name) is active")
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
	assert.Contains(t, body, `<li class="active"><button class="row" aria-current="true" hx-get="/buildings/b1/floors/f2"`)
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
	assert.Contains(t, body, `<button class="action" hx-get="/rooms/r1/camera/cam1"`, "a lone camera gets its own row button")
	assert.Contains(t, body, "[ VIEW ]")
	assert.NotContains(t, body, "<dialog")
}

func TestRoomDirectNavigationRedirectsToFullPage(t *testing.T) {
	s, _, _ := startTestServer(t)
	rec := get(s, "/rooms/r1")
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/buildings/b1?room=r1", rec.Header().Get("Location"))

	page := get(s, "/buildings/b1?room=r1").Body.String()
	assert.Contains(t, page, "21.4°C", "room pre-rendered in the detail pane")
	assert.Contains(t, page, `class="active"><button class="row" aria-current="true" hx-get="/rooms/r1"`)
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
	assert.Contains(t, body, "WebRTC (WHEP)")

	bridge.camURL = "none"
	body = get(s, "/rooms/r1/camera/cam1", "HX-Request", "true").Body.String()
	assert.Contains(t, body, "isn&#39;t reporting a stream URL")
}

func TestDeviceCommandSendsOnOffAndReturnsUpdatedRow(t *testing.T) {
	s, _, bridge := startTestServer(t)
	rec := post(s, "/devices/lamp/commands", "on=true", true)

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

func TestDeviceCommandRejectsRequestsWithoutHXRequest(t *testing.T) {
	s, _, bridge := startTestServer(t)
	rec := post(s, "/devices/lamp/commands", "on=true", false)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Nil(t, bridge.gotCmd, "a cross-site form post must not reach the device")
}

func TestDeviceCommandRejectsBadValues(t *testing.T) {
	s, _, bridge := startTestServer(t)
	for _, body := range []string{"", "on=garbage", "on=", "brightness=101", "brightness=-1", "brightness=abc", "brightness="} {
		rec := post(s, "/devices/lamp/commands", body, true)
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
	}
	assert.Nil(t, bridge.gotCmd)
}

func TestDeviceCommandBrightness(t *testing.T) {
	s, _, bridge := startTestServer(t)
	rec := post(s, "/devices/lamp/commands", "brightness=75", true)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, bridge.gotCmd.GetBrightnessAbsolute())
	assert.EqualValues(t, 75, bridge.gotCmd.GetBrightnessAbsolute().GetBrightnessPercent())
	assert.Contains(t, rec.Body.String(), `value="75"`)

	post(s, "/devices/lamp/commands", "brightness=0", true)
	assert.EqualValues(t, 0, bridge.gotCmd.GetBrightnessAbsolute().GetBrightnessPercent(), "0 is a valid level")
}

func TestRoomDetailShowsDimmerBesideToggle(t *testing.T) {
	s, _, _ := startTestServer(t)
	body := get(s, "/rooms/r1", "HX-Request", "true").Body.String()
	assert.Contains(t, body, `type="range"`)
	assert.Contains(t, body, `value="40"`)
	assert.Contains(t, body, "[ OFF ]")
}

func TestDeviceCommandFailureKeepsRowWithError(t *testing.T) {
	s, _, bridge := startTestServer(t)
	bridge.cmdErr = status.Error(codes.Unavailable, "bridge offline")
	rec := post(s, "/devices/lamp/commands", "on=true", true)
	assert.Contains(t, rec.Body.String(), "bridge offline")
	assert.Contains(t, rec.Body.String(), `id="device-lamp"`)
}

func TestCameraMessageDoesNotLeakCredentials(t *testing.T) {
	s, _, bridge := startTestServer(t)
	bridge.camURL = "rtsp://admin:hunter2@cam.local/stream"
	body := get(s, "/rooms/r1/camera/cam1", "HX-Request", "true").Body.String()
	assert.NotContains(t, body, "hunter2")
	assert.NotContains(t, body, "cam.local")
}

func TestCameraMustBeLinkedToRoomAndBeACamera(t *testing.T) {
	s, _, _ := startTestServer(t)
	// "lamp" is in the room but isn't a camera; "cam2" is a camera not in it.
	assert.Equal(t, http.StatusNotFound, get(s, "/rooms/r1/camera/lamp", "HX-Request", "true").Code)
	assert.Equal(t, http.StatusNotFound, get(s, "/rooms/r1/camera/cam2", "HX-Request", "true").Code)
}

func TestRoomFromAnotherBuildingIs404(t *testing.T) {
	s, _, _ := startTestServer(t)
	assert.Equal(t, http.StatusNotFound, get(s, "/buildings/b2?room=r1").Code)
}

func TestResponsesCarrySecurityHeaders(t *testing.T) {
	s, _, _ := startTestServer(t)
	rec := get(s, "/buildings/b1")
	assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Empty(t, get(s, "/static/htmx.min.js").Header().Get("Cache-Control"))
}

func TestDeviceRowOOBCarriesDimmerLevel(t *testing.T) {
	u := &api2.Update{Action: api2.Update_CHANGED, Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{Device: dimLamp(true, 90)}}}
	var sb strings.Builder
	require.NoError(t, fragments.ExecuteTemplate(&sb, "device_row_oob", deviceToView(u.GetDeviceUpdate().GetDevice())))
	assert.Contains(t, sb.String(), `hx-swap-oob="true"`)
	assert.Contains(t, sb.String(), `value="90"`)
}

func TestCameraPrefersWHEPEndpointOverLegacyURL(t *testing.T) {
	s, _, bridge := startTestServer(t)
	bridge.camURL = "rtsp://admin:hunter2@cam.local/door"
	bridge.camEndpoints = []*apiTrait.MediaStream_Endpoint{
		{Protocol: apiTrait.MediaStream_RTSP, Url: "rtsp://admin:hunter2@cam.local/door"},
		{Protocol: apiTrait.MediaStream_WEBRTC_WHEP, Url: "http://go2rtc:1984/api/webrtc?src=door"},
	}
	body := get(s, "/rooms/r1/camera/cam1", "HX-Request", "true").Body.String()
	assert.Contains(t, body, `data-whep="http://go2rtc:1984/api/webrtc?src=door"`)
	assert.NotContains(t, body, "hunter2")
}

func TestCameraWithOnlyNonWHEPEndpointsExplainsWithoutLeaking(t *testing.T) {
	s, _, bridge := startTestServer(t)
	bridge.camURL = "http://legacy.local/stream" // ignored: endpoints are authoritative
	bridge.camEndpoints = []*apiTrait.MediaStream_Endpoint{
		{Protocol: apiTrait.MediaStream_RTSP, Url: "rtsp://admin:hunter2@cam.local/door"},
		{Protocol: apiTrait.MediaStream_HLS, Url: "http://cam.local/door.m3u8"},
	}
	body := get(s, "/rooms/r1/camera/cam1", "HX-Request", "true").Body.String()
	assert.NotContains(t, body, "data-whep")
	assert.NotContains(t, body, "hunter2")
	assert.Contains(t, body, "WebRTC (WHEP)")
}

func TestCameraPlayerCarriesICEServers(t *testing.T) {
	s, _, _ := startTestServer(t)
	s.iceServersJSON = `["stun:stun.example:3478"]`
	body := get(s, "/rooms/r1/camera/cam1", "HX-Request", "true").Body.String()
	assert.Contains(t, body, "stun:stun.example:3478")
}

func TestRoomDetailOffersEveryCamera(t *testing.T) {
	cam2 := cam()
	cam2.Id, cam2.Config.Name = "cam2", "Garage cam"
	rv := roomToDetail(&api2.Room{Id: "r1", Devices: []*apiDevice.Device{cam(), cam2}})
	require.Len(t, rv.Cameras, 2)
	var sb strings.Builder
	require.NoError(t, fragments.ExecuteTemplate(&sb, "room_detail", rv))
	assert.Contains(t, sb.String(), "/rooms/r1/camera/cam1")
	assert.Contains(t, sb.String(), "/rooms/r1/camera/cam2")
	assert.Contains(t, sb.String(), "<dialog", "several cameras: one VIEW opens a picker")
	assert.Contains(t, sb.String(), ">Garage cam</button>")
	assert.Equal(t, 1, strings.Count(sb.String(), "[ VIEW ]"), "a single VIEW, not one per camera")
}

func TestA11yAttributes(t *testing.T) {
	s, _, _ := startTestServer(t)
	page := get(s, "/buildings/b1").Body.String()
	assert.Contains(t, page, `aria-current="true"`)
	assert.Contains(t, page, `aria-label="occupancy unknown"`)
	body := get(s, "/rooms/r1", "HX-Request", "true").Body.String()
	assert.Contains(t, body, `aria-pressed="false"`)
	assert.Contains(t, body, `<output class="dim level">40%</output>`)
}

func TestRoomDetailShowsAirQualityTilesOnlyWhenReported(t *testing.T) {
	s, _, _ := startTestServer(t)
	body := get(s, "/rooms/r1", "HX-Request", "true").Body.String()
	assert.Contains(t, body, "640 ppm")
	assert.NotContains(t, body, ">VOC<", "unreported metrics get no tile")
	assert.NotContains(t, body, ">Radon<")
}

func TestCameraPushRefreshesOnlyTheInfoCell(t *testing.T) {
	var sb strings.Builder
	require.NoError(t, fragments.ExecuteTemplate(&sb, "device_row_oob", deviceToView(cam())))
	assert.Contains(t, sb.String(), `id="device-info-cam1"`)
	assert.NotContains(t, sb.String(), "<tr", "replacing the row would drop its VIEW button")
}
