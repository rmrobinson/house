package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	apiDevice "github.com/rmrobinson/house/api/device"
	apiTrait "github.com/rmrobinson/house/api/trait"
	weatherv1 "github.com/rmrobinson/house/api/weather/v1"
	"github.com/rmrobinson/house/service/lib/grpcutil"
	"github.com/rmrobinson/house/service/lib/houseview"
)

type fakeHouse struct {
	api2.UnimplementedHouseServiceServer
	updates chan *api2.HouseUpdate

	mu sync.Mutex
	// extraDevice, when set, is appended to room r1's Devices in both
	// ListRooms and GetRoom - used to put a media-capable device on a room
	// a test can then push BridgeService updates for.
	extraDevice *apiDevice.Device

	// exteriorFloor flags floor f2 as an exterior area (set before use).
	exteriorFloor bool
}

func (f *fakeHouse) setExtraDevice(d *apiDevice.Device) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extraDevice = d
}

func (f *fakeHouse) getExtraDevice() *apiDevice.Device {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.extraDevice
}

func (f *fakeHouse) ListBuildings(_ *api2.ListBuildingsRequest, s api2.HouseService_ListBuildingsServer) error {
	return s.Send(&api2.Building{Id: "b1", Config: &api2.Building_Config{Name: "Home"}})
}
func (f *fakeHouse) GetBuilding(context.Context, *api2.GetBuildingRequest) (*api2.Building, error) {
	return &api2.Building{Id: "b1", Config: &api2.Building_Config{Name: "Home"}}, nil
}
func (f *fakeHouse) ListFloors(_ *api2.ListFloorsRequest, s api2.HouseService_ListFloorsServer) error {
	s.Send(&api2.Floor{Id: "f2", Name: "Upstairs", SortOrder: 2, BuildingId: "b1", Exterior: f.exteriorFloor})
	return s.Send(&api2.Floor{Id: "f1", Name: "Main", SortOrder: 1, BuildingId: "b1"})
}
func (f *fakeHouse) ListRooms(_ *api2.ListRoomsRequest, s api2.HouseService_ListRoomsServer) error {
	occ := true
	s.Send(&api2.Room{Id: "r2", Config: &api2.Room_Config{Name: "Kitchen"}, Properties: &api2.Room_Properties{Occupied: &occ}})
	temp := 19.5
	r1 := &api2.Room{Id: "r1", Config: &api2.Room_Config{Name: "Bath"}, Properties: &api2.Room_Properties{TemperatureC: &temp}}
	if d := f.getExtraDevice(); d != nil {
		r1.Devices = []*apiDevice.Device{d}
	}
	return s.Send(r1)
}
func (f *fakeHouse) GetRoom(context.Context, *api2.GetRoomRequest) (*api2.Room, error) {
	temp := 21.4
	co2 := int32(640)
	devices := []*apiDevice.Device{lamp(false), cam()}
	if d := f.getExtraDevice(); d != nil {
		devices = append(devices, d)
	}
	return &api2.Room{
		Id: "r1", FloorId: "f1", BuildingId: "b1",
		Config:     &api2.Room_Config{Name: "Bath"},
		Properties: &api2.Room_Properties{TemperatureC: &temp, Co2Ppm: &co2},
		Devices:    devices,
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

// speaker builds a MediaPlayer fixture. playing picks PS_PLAYING vs
// PS_PAUSED so tests can exercise the play/pause button's two states;
// skipFwd/skipBack independently gate the next/previous buttons, matching
// how a real bridge (e.g. cast) reports them.
func speaker(name string, playing, skipFwd, skipBack bool) *apiDevice.Device {
	state := apiTrait.Media_PS_PAUSED
	if playing {
		state = apiTrait.Media_PS_PLAYING
	}
	return &apiDevice.Device{
		Id:      "speaker-" + name,
		Config:  &apiDevice.Device_Config{Name: name},
		Address: &apiDevice.Device_Address{IsReachable: true},
		Details: &apiDevice.Device_MediaPlayer{MediaPlayer: &apiDevice.MediaPlayer{
			Media: &apiTrait.Media{
				Attributes: &apiTrait.Media_Attributes{
					CanControl:      true,
					CanSkipForward:  proto.Bool(skipFwd),
					CanSkipBackward: proto.Bool(skipBack),
				},
				State: &apiTrait.Media_State{
					PlaybackState: state,
					MediaType:     apiTrait.Media_TYPE_SONG,
					SongDetails:   &apiTrait.Media_SongDetails{SongName: "Bohemian Rhapsody", Artists: []string{"Queen"}},
				},
			},
			Volume: &apiTrait.Volume{
				Attributes: &apiTrait.Volume_Attributes{CanControl: true, MaximumLevel: 16},
				State:      &apiTrait.Volume_State{Level: 7},
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
	// updates, when non-nil, feeds StreamUpdates - a test can push an
	// *api2.Update onto it to simulate a live BridgeService device update.
	updates chan *api2.Update
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
	if f.updates == nil {
		<-s.Context().Done()
		return nil
	}
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

func startTestServer(t *testing.T) (*Server, *fakeHouse, *fakeBridge) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	house := &fakeHouse{updates: make(chan *api2.HouseUpdate, 4)}
	bridge := &fakeBridge{updates: make(chan *api2.Update, 4)}
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
	assert.Equal(t, "/buildings/b1/overview", rec.Header().Get("Location"))
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

func TestBuildingPageShowsFloorSummaryByDefault(t *testing.T) {
	s, _, _ := startTestServer(t)
	body := get(s, "/buildings/b1").Body.String()
	assert.NotContains(t, body, `aria-current="true" hx-get="/rooms/`, "no room is selected")
	assert.Contains(t, body, `id="room-props-r1"`, "each room gets a summary box")
	assert.Contains(t, body, `id="room-sum-dot-r2" class="dot dot-yes"`, "summary dot is live-updatable")
	assert.Contains(t, body, `id="room-props-r2"`)
	assert.Contains(t, body, "19.5°C", "a room's readings are summarised")
	assert.NotContains(t, body, "Air quality", "unavailable measures are hidden")
	assert.NotContains(t, body, "Power", "unavailable measures are hidden")
}

func TestFloorFragmentSwapsDetailToFloorSummary(t *testing.T) {
	s, _, _ := startTestServer(t)
	body := get(s, "/buildings/b1/floors/f2").Body.String()
	assert.Contains(t, body, `<div id="detail" class="col-detail" hx-swap-oob="innerHTML">`)
	assert.Contains(t, body, `id="room-props-r2"`)
	assert.NotContains(t, body, "21.4°C", "no single room's detail")
}

func TestBuildingPageWithRoomShowsThatRoom(t *testing.T) {
	s, _, _ := startTestServer(t)
	body := get(s, "/buildings/b1?room=r1").Body.String()
	assert.Contains(t, body, "21.4°C")
	assert.Contains(t, body, "640 ppm")
	assert.NotContains(t, body, "Air quality")
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
			assert.Contains(t, got, `id="room-sum-dot-r1"`, "floor summary's dot is refreshed too")
			assert.Contains(t, got, "dot-yes")
			assert.Contains(t, got, `<div hx-swap-oob="afterbegin:#event-log"><div>Bath:`, "each entry is its own block")
			assert.Contains(t, got, "Bath:")
			return
		case <-time.After(50 * time.Millisecond):
			select {
			case house.updates <- &api2.HouseUpdate{Update: &api2.HouseUpdate_Room{Room: &api2.RoomUpdate{RoomId: "r1", Properties: &api2.Room_Properties{Occupied: &occ}}}}:
			default:
			}
		case <-deadline:
			t.Fatal("no SSE message")
		}
	}
}

// TestEventsMediaUpdateSkipsNoopButRefreshesOnChange guards against the
// now-playing row and event log being spammed by every BridgeService update
// for a media-capable device, even ones that don't touch playback (a volume
// change on a device that's mid-session still carries its unchanged Media
// trait alongside the Volume one - see nameCache.mediaChanged). Only a
// devUpdate whose houseview.MediaSignature actually differs from the last
// one seen should trigger the now_playing_oob/media_event_oob refresh.
func TestEventsMediaUpdateSkipsNoopButRefreshesOnChange(t *testing.T) {
	playing := speaker("Kitchen Speaker", true, true, true)
	s, house, bridge := startTestServer(t)
	house.setExtraDevice(playing)

	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/buildings/b1/events")
	require.NoError(t, err)
	defer resp.Body.Close()

	read := func() <-chan string {
		ch := make(chan string, 1)
		go func() {
			buf := make([]byte, 8192)
			n, _ := resp.Body.Read(buf)
			ch <- string(buf[:n])
		}()
		return ch
	}

	// Resend the unchanged, already-playing device until the hub delivers
	// something for it (a message sent before a subscriber connects is
	// lost - same caveat as TestEventsStreamsRoomUpdateAsOOB above).
	pending := read()
	var gotUnchanged string
	deadline := time.After(5 * time.Second)
	for gotUnchanged == "" {
		select {
		case got := <-pending:
			gotUnchanged = got
		case <-time.After(50 * time.Millisecond):
			select {
			case bridge.updates <- &api2.Update{Action: api2.Update_CHANGED, Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{DeviceId: playing.GetId(), Device: playing}}}:
			default:
			}
		case <-deadline:
			t.Fatal("no SSE message for unchanged update")
		}
	}
	assert.Contains(t, gotUnchanged, `id="device-`+domID(playing.GetId())+`"`, "the device row itself still refreshes")
	assert.NotContains(t, gotUnchanged, `id="room-nowplaying-r1"`, "unchanged media signature must not re-push now playing")
	assert.NotContains(t, gotUnchanged, `afterbegin:#event-log"><div>Kitchen Speaker:`, "unchanged media signature must not re-log an event")

	// Now actually change playback state - the room's device list (as
	// GetRoom would report it) needs to agree with the pushed update.
	paused := speaker("Kitchen Speaker", false, true, true)
	house.setExtraDevice(paused)

	pending = read()
	var gotChanged string
	deadline = time.After(5 * time.Second)
	for gotChanged == "" {
		select {
		case got := <-pending:
			gotChanged = got
		case <-time.After(50 * time.Millisecond):
			select {
			case bridge.updates <- &api2.Update{Action: api2.Update_CHANGED, Update: &api2.Update_DeviceUpdate{DeviceUpdate: &api2.DeviceUpdate{DeviceId: paused.GetId(), Device: paused}}}:
			default:
			}
		case <-deadline:
			t.Fatal("no SSE message for changed update")
		}
	}
	assert.Contains(t, gotChanged, `id="room-nowplaying-r1"`, "changed media signature must refresh now playing")
	assert.Contains(t, gotChanged, "Kitchen Speaker: Paused — Bohemian Rhapsody — Queen", "changed media signature must log an event")
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

func TestDomIDEscapesWhatSelectorsCannotHold(t *testing.T) {
	assert.Equal(t, "zigbee-0017880100b56fa7", domID("zigbee-0017880100b56fa7"), "already-safe IDs are untouched")
	assert.Equal(t, "54a91131-fc19-4d13-b46f-18b36772f531_3a2", domID("54a91131-fc19-4d13-b46f-18b36772f531:2"))
	assert.Equal(t, "a_5fb", domID("a_b"), "underscore is the escape character, so it is escaped too")
	assert.NotEqual(t, domID("a:b"), domID("a_3ab"), "distinct IDs never collide")
	assert.Equal(t, "10_2e0_2e0_2e1", domID("10.0.0.1"))
}

// A bridge whose device IDs contain ":" (monop-amp: "<bridge-uuid>:2") made
// every control on its rows throw in htmx - "#device-...:2" is not a valid
// selector - so nothing could be commanded from the UI.
func TestDeviceRowWithColonInIDHasSelectorSafeIDsAndTargets(t *testing.T) {
	d := lamp(false)
	d.Id = "54a91131-fc19-4d13-b46f-18b36772f531:2"
	view := deviceToView(d)

	var row, oob strings.Builder
	require.NoError(t, fragments.ExecuteTemplate(&row, "device_row", view))
	require.NoError(t, fragments.ExecuteTemplate(&oob, "device_row_oob", view))

	const safe = "device-54a91131-fc19-4d13-b46f-18b36772f531_3a2"
	assert.Contains(t, row.String(), `<tr id="`+safe+`"`)
	assert.Contains(t, row.String(), `hx-target="#`+safe+`"`, "the control targets the row by its safe id")
	assert.Contains(t, oob.String(), `id="`+safe+`" hx-swap-oob="true"`, "live pushes match the same id")
	assert.Contains(t, row.String(), `hx-post="/devices/54a91131-fc19-4d13-b46f-18b36772f531:2/commands"`,
		"the command URL keeps the real device ID")
	assert.NotContains(t, row.String()+oob.String(), `#device-54a91131-fc19-4d13-b46f-18b36772f531:2`)
	assert.NotContains(t, row.String()+oob.String(), `id="device-54a91131-fc19-4d13-b46f-18b36772f531:2"`)
}

func TestCameraPushRefreshesOnlyTheInfoCell(t *testing.T) {
	var sb strings.Builder
	require.NoError(t, fragments.ExecuteTemplate(&sb, "device_row_oob", deviceToView(cam())))
	assert.Contains(t, sb.String(), `id="device-info-cam1"`)
	assert.NotContains(t, sb.String(), "<tr", "replacing the row would drop its VIEW button")
}

func fanWithTemp() *apiDevice.Device {
	return &apiDevice.Device{
		Id:      "fan1",
		Address: &apiDevice.Device_Address{IsReachable: true},
		Config:  &apiDevice.Device_Config{Name: "Tower Fan"},
		Details: &apiDevice.Device_Fan{Fan: &apiDevice.Fan{
			Temperature: &apiTrait.Temperature{State: &apiTrait.Temperature_State{Value: 23.5}},
		}},
	}
}

func TestRoomDetailShowsDeviceReadingsRowAndToggle(t *testing.T) {
	s, house, _ := startTestServer(t)
	house.setExtraDevice(fanWithTemp())
	body := get(s, "/rooms/r1", "HX-Request", "true").Body.String()
	assert.Contains(t, body, "readings-toggle")
	assert.Contains(t, body, `id="device-readings-fan1"`)
	assert.Contains(t, body, "23.5°C")
	// A device with nothing to report still gets a hidden row for live fills.
	assert.Regexp(t, `id="device-readings-[^"]*" class="readings" hidden`, body)
}

func TestStandingDeskPresetsAreControlsAndSendMode(t *testing.T) {
	desk := &apiDevice.Device{
		Id:      "desk1",
		Config:  &apiDevice.Device_Config{Name: "Desk"},
		Address: &apiDevice.Device_Address{IsReachable: true},
		Details: &apiDevice.Device_StandingDesk{StandingDesk: &apiDevice.StandingDesk{
			Mode: &apiTrait.Mode{Attributes: &apiTrait.Mode_Attributes{CanControl: true, AvailableModes: []string{"preset_3_stand", "preset_4_sit"}}},
		}},
	}
	dv := deviceToView(desk)
	assert.Equal(t, []presetView{{"preset_3_stand", "3 STAND"}, {"preset_4_sit", "4 SIT"}}, dv.Presets)
	var sb strings.Builder
	require.NoError(t, fragments.ExecuteTemplate(&sb, "device_row", dv))
	assert.Contains(t, sb.String(), `{"mode": "preset_4_sit"}`)
	assert.Contains(t, sb.String(), "[ 4 SIT ]")
}

func TestPlayingMediaIsFirstReadingNotUnderName(t *testing.T) {
	dv := deviceToView(speaker("Kitchen Speaker", true, true, true))
	require.NotEmpty(t, dv.Readings)
	assert.Equal(t, "Playing", dv.Readings[0].Label)
	assert.Equal(t, dv.NowPlaying, dv.Readings[0].Value)
}

func TestDeviceReadingsOOBCarriesReadings(t *testing.T) {
	var sb strings.Builder
	require.NoError(t, fragments.ExecuteTemplate(&sb, "device_readings_oob", deviceToView(fanWithTemp())))
	assert.Contains(t, sb.String(), `hx-swap-oob="true"`)
	assert.Contains(t, sb.String(), "23.5°C")
	assert.NotContains(t, sb.String(), "hidden")
}

func TestDeviceToViewPopulatesMediaAndVolume(t *testing.T) {
	dv := deviceToView(speaker("Kitchen Speaker", true, false, true))
	assert.True(t, dv.CanControlMedia)
	assert.True(t, dv.IsPlaying)
	assert.False(t, dv.CanSkipForward)
	assert.True(t, dv.CanSkipBackward)
	assert.Equal(t, "Bohemian Rhapsody — Queen", dv.NowPlaying)
	assert.True(t, dv.CanControlVolume)
	assert.Equal(t, 7, dv.VolumeLevel)
	assert.Equal(t, 16, dv.VolumeMax)

	paused := deviceToView(speaker("Kitchen Speaker", false, true, true))
	assert.False(t, paused.IsPlaying)
}

func TestRoomNowPlayingPicksFirstActiveDevice(t *testing.T) {
	assert.Equal(t, "", roomNowPlaying([]*apiDevice.Device{lamp(false)}), "no media-capable device")

	devices := []*apiDevice.Device{lamp(false), speaker("Kitchen Speaker", true, true, true)}
	assert.Equal(t, "Kitchen Speaker: Bohemian Rhapsody — Queen", roomNowPlaying(devices))
}

func TestMediaEventLine(t *testing.T) {
	assert.Equal(t, "Kitchen Speaker: Bohemian Rhapsody — Queen", mediaEventLine(speaker("Kitchen Speaker", true, true, true)))
	assert.Equal(t, "Kitchen Speaker: Paused — Bohemian Rhapsody — Queen", mediaEventLine(speaker("Kitchen Speaker", false, true, true)))

	stopped := speaker("Kitchen Speaker", true, true, true)
	stopped.GetMediaPlayer().GetMedia().GetState().PlaybackState = apiTrait.Media_PS_STOPPED
	assert.Equal(t, "Kitchen Speaker: Stopped", mediaEventLine(stopped))

	finished := speaker("Kitchen Speaker", true, true, true)
	finished.GetMediaPlayer().GetMedia().GetState().PlaybackState = apiTrait.Media_PS_COMPLETED
	assert.Equal(t, "Kitchen Speaker: Finished", mediaEventLine(finished))

	assert.Equal(t, "", mediaEventLine(lamp(false)), "no media trait")
}

func TestDeviceRowRendersMediaControlsGatedOnSkipCapability(t *testing.T) {
	var sb strings.Builder
	require.NoError(t, fragments.ExecuteTemplate(&sb, "device_row_cells", deviceToView(speaker("Kitchen Speaker", true, false, true))))
	body := sb.String()

	assert.Contains(t, body, `[ || ]`, "playing shows the pause icon")
	assert.Contains(t, body, `hx-vals='{"playback": "pause"}'`)
	assert.Contains(t, body, `aria-label="Kitchen Speaker next" hx-post="/devices/speaker-Kitchen Speaker/commands" hx-vals='{"skip": "forward"}' hx-target="#device-speaker-Kitchen_20Speaker" hx-swap="outerHTML" hx-disabled-elt="this">[ &gt;| ]`)
	assert.Contains(t, body, ` disabled aria-label="Kitchen Speaker next"`, "can_skip_forward false disables next")
	assert.NotContains(t, body, ` disabled aria-label="Kitchen Speaker previous"`, "can_skip_backward true enables previous")
	assert.Contains(t, body, `class="volume" name="volume" min="0" max="16" step="1" value="7"`)
}

func TestNowPlayingRowHiddenWhenEmpty(t *testing.T) {
	var sb strings.Builder
	require.NoError(t, fragments.ExecuteTemplate(&sb, "now_playing", roomDetailView{ID: "r1"}))
	assert.Contains(t, sb.String(), `id="room-nowplaying-r1" class="nowplaying" hidden`)

	sb.Reset()
	require.NoError(t, fragments.ExecuteTemplate(&sb, "now_playing", roomDetailView{ID: "r1", NowPlaying: "Kitchen Speaker: Bohemian Rhapsody — Queen"}))
	assert.Contains(t, sb.String(), `id="room-nowplaying-r1" class="nowplaying">Kitchen Speaker: Bohemian Rhapsody — Queen</div>`)
}

func TestDeviceCommandPlaybackSkipAndVolume(t *testing.T) {
	s, _, bridge := startTestServer(t)

	post(s, "/devices/speaker1/commands", "playback=play", true)
	assert.Equal(t, command.Playback_ACTION_PLAY, bridge.gotCmd.GetPlayback().GetAction())

	post(s, "/devices/speaker1/commands", "playback=pause", true)
	assert.Equal(t, command.Playback_ACTION_PAUSE, bridge.gotCmd.GetPlayback().GetAction())

	post(s, "/devices/speaker1/commands", "skip=forward", true)
	assert.NotNil(t, bridge.gotCmd.GetSkipForward())

	post(s, "/devices/speaker1/commands", "skip=backward", true)
	assert.NotNil(t, bridge.gotCmd.GetSkipBackward())

	post(s, "/devices/speaker1/commands", "volume=9", true)
	assert.Equal(t, int32(9), bridge.gotCmd.GetVolumeAbsolute().GetLevel())

	rec := post(s, "/devices/speaker1/commands", "volume=-1", true)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRoomPropsCellsWater(t *testing.T) {
	render := func(w string) string {
		var sb strings.Builder
		require.NoError(t, fragments.ExecuteTemplate(&sb, "room_props_cells", roomDetailView{Properties: houseview.Properties{WaterDetected: w}}))
		return sb.String()
	}
	assert.Contains(t, render("Detected"), `stat alert"><div class="label">Water</div><div class="val">Detected`)
	assert.Contains(t, render("Dry"), `<div class="val">Dry`)
	assert.NotContains(t, render(""), "Water", "no water sensor -> hidden")
}

func thermostatDevice(mode apiTrait.Thermostat_Mode, heat, cool *float32) *apiDevice.Device {
	return &apiDevice.Device{
		Id:     "tstat",
		Config: &apiDevice.Device_Config{Name: "Main Thermostat"},
		Details: &apiDevice.Device_Thermostat{Thermostat: &apiDevice.Thermostat{
			Thermostat: &apiTrait.Thermostat{State: &apiTrait.Thermostat_State{
				TargetMode:                mode,
				HeatSetpointCelsius:       heat,
				CoolSetpointCelsius:       cool,
				CurrentTemperatureCelsius: 20.5,
			}},
		}},
	}
}

func TestBuildOverviewAggregatesHouseTilesThermostatsAndMotion(t *testing.T) {
	yes, no := true, false
	w1, w2, tIn1, tIn2, tOut := 120.0, 80.0, 20.0, 22.0, 5.0
	heat := float32(21)
	snap := houseSnapshot{
		Building: &api2.Building{Id: "b1", Config: &api2.Building_Config{Name: "Home"}, State: &api2.Building_State{Mode: "home", Occupied: &yes}},
		Floors: []*api2.Floor{
			{Id: "f1", Name: "Main"},
			{Id: "f2", Name: "Property", Exterior: true},
		},
		Rooms: map[string][]*api2.Room{
			"f1": {
				{Id: "r1", Config: &api2.Room_Config{Name: "Kitchen"}, Properties: &api2.Room_Properties{Occupied: &no, PowerDrawW: &w1, TemperatureC: &tIn1},
					Devices: []*apiDevice.Device{thermostatDevice(apiTrait.Thermostat_HEAT, &heat, nil)}},
				{Id: "r2", Config: &api2.Room_Config{Name: "Den"}, Properties: &api2.Room_Properties{Occupied: &yes, PowerDrawW: &w2, TemperatureC: &tIn2}},
			},
			"f2": {
				{Id: "r3", Config: &api2.Room_Config{Name: "Driveway"}, Properties: &api2.Room_Properties{Occupied: &yes, TemperatureC: &tOut}},
			},
		},
	}
	ov := buildOverview(snap)

	tiles := map[string]tileView{}
	for _, tl := range ov.Tiles {
		tiles[tl.Label] = tl
	}
	assert.Equal(t, "HOME", tiles["Mode"].Value)
	assert.Equal(t, "Occupied", tiles["House"].Value)
	assert.Equal(t, "200 W", tiles["Power"].Value)
	interior := map[string]tileView{}
	for _, tl := range ov.Interior {
		interior[tl.Label] = tl
	}
	assert.Equal(t, "21.0°C", interior["Temp"].Value, "exterior rooms don't skew the indoor average")
	assert.Equal(t, "Dry", tiles["Water"].Value)
	require.Len(t, ov.Exterior, 1)
	assert.Equal(t, "Weather", ov.Exterior[0].Label, "no reading -> one placeholder tile")

	require.Len(t, ov.Thermostats, 1)
	assert.Equal(t, "21.0°C", ov.Thermostats[0].Setpoint)
	assert.Equal(t, "20.5°C", ov.Thermostats[0].Current)
	assert.Equal(t, "Heat", ov.Thermostats[0].Mode)

	require.Len(t, ov.Floors, 2)
	assert.Equal(t, "yes", ov.Floors[0].Motion)
	assert.Equal(t, []string{"Den"}, ov.Floors[0].Active)
	assert.False(t, ov.Floors[0].Exterior)
	assert.True(t, ov.Floors[1].Exterior)
	assert.True(t, ov.ExteriorActive)
}

func TestBuildOverviewLeakAndUnknowns(t *testing.T) {
	yes := true
	snap := houseSnapshot{
		Building: &api2.Building{Id: "b1", Config: &api2.Building_Config{Name: "Home"}},
		Floors:   []*api2.Floor{{Id: "f1", Name: "Main"}},
		Rooms:    map[string][]*api2.Room{"f1": {{Id: "r1", Config: &api2.Room_Config{Name: "Sump"}, Properties: &api2.Room_Properties{WaterDetected: &yes}}}},
	}
	ov := buildOverview(snap)
	var water, power, mode tileView
	for _, tl := range ov.Tiles {
		switch tl.Label {
		case "Water":
			water = tl
		case "Power":
			power = tl
		case "Mode":
			mode = tl
		}
	}
	assert.True(t, water.Alert)
	assert.Equal(t, "Sump", water.Note)
	assert.Equal(t, "—", power.Value, "no metered device -> dash, not 0 W")
	assert.Equal(t, "—", mode.Value)
	assert.Equal(t, "unknown", ov.Floors[0].Motion)
	assert.False(t, ov.ExteriorActive)
}

func TestThermostatSetpointFollowsTargetMode(t *testing.T) {
	heat, cool := float32(19), float32(24)
	assert.Equal(t, "24.0°C", thermostatToView(thermostatDevice(apiTrait.Thermostat_COOL, &heat, &cool)).Setpoint)
	assert.Equal(t, "19.0–24.0°C", thermostatToView(thermostatDevice(apiTrait.Thermostat_AUTO, &heat, &cool)).Setpoint)
	assert.Equal(t, "Off", thermostatToView(thermostatDevice(apiTrait.Thermostat_OFF, &heat, &cool)).Setpoint)
	assert.Equal(t, "—", thermostatToView(thermostatDevice(apiTrait.Thermostat_HEAT, nil, &cool)).Setpoint)
}

func TestOverviewPageRendersAndLinksToFloors(t *testing.T) {
	s, _, _ := startTestServer(t)
	rec := get(s, "/buildings/b1/overview")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `sse-connect="/buildings/b1/overview/events"`)
	assert.Contains(t, body, `id="overview"`)
	assert.Contains(t, body, `href="/buildings/b1?floor=f1"`)
	assert.Contains(t, body, ">Interior<")
	assert.Contains(t, body, ">Exterior<")
	assert.Contains(t, body, `id="popup-slot"`)
}

func TestPopupRendersRoomCameras(t *testing.T) {
	s, _, _ := startTestServer(t)
	rec := get(s, "/rooms/r1/popup")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `id="motion-popup"`)
	assert.Contains(t, body, "MOTION · Bath")
	assert.Contains(t, body, "data-whep=")
}

func TestOverviewEventsOpensPopupOnExteriorMotionOnly(t *testing.T) {
	s, house, _ := startTestServer(t)
	house.exteriorFloor = true // f2 "Upstairs" is exterior, listing r1 (Bath, quiet) and r2 (Kitchen, already active)

	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/buildings/b1/overview/events")
	require.NoError(t, err)
	defer resp.Body.Close()

	yes := true
	push := func() {
		select {
		case house.updates <- &api2.HouseUpdate{Update: &api2.HouseUpdate_Room{Room: &api2.RoomUpdate{RoomId: "r1", Properties: &api2.Room_Properties{Occupied: &yes}}}}:
		default:
		}
	}
	done := make(chan string, 1)
	go func() {
		var got strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			got.Write(buf[:n])
			if strings.Contains(got.String(), "popup-slot") || err != nil {
				done <- got.String()
				return
			}
		}
	}()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-done:
			assert.Contains(t, got, `hx-get="/rooms/r1/popup"`)
			assert.Contains(t, got, `hx-trigger="load"`)
			return
		case <-time.After(50 * time.Millisecond):
			push()
		case <-deadline:
			t.Fatal("no popup fragment")
		}
	}
}

func TestOverviewMetricsFromDevices(t *testing.T) {
	reach := &apiDevice.Device_Address{IsReachable: true}
	humid := func(id string, h float32) *apiDevice.Device {
		return &apiDevice.Device{Id: id, Address: reach, Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
			AirProperties: &apiTrait.AirProperties{State: &apiTrait.AirProperties_State{HumidityPercentage: h}}}}}
	}
	lowBatt := &apiDevice.Device{Id: "door", Config: &apiDevice.Device_Config{Name: "Door"}, Address: reach, Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{
		Battery: &apiTrait.Battery{State: &apiTrait.Battery_State{CapacityRemainingPct: 4}}}}}
	deadLight := &apiDevice.Device{Id: "dl", Config: &apiDevice.Device_Config{Name: "Hall"}, Details: &apiDevice.Device_Light{Light: lamp(true).GetLight()}}
	ups := &apiDevice.Device{Id: "ups", Config: &apiDevice.Device_Config{Name: "Rack UPS"}, Address: reach, Details: &apiDevice.Device_Ups{Ups: &apiDevice.UPS{
		Battery: &apiTrait.Battery{State: &apiTrait.Battery_State{CapacityRemainingMins: 45}}}}}
	charger := &apiDevice.Device{Id: "ev", Config: &apiDevice.Device_Config{Name: "Wall Charger"}, Address: reach, Details: &apiDevice.Device_EvCharger{EvCharger: &apiDevice.EVCharger{
		WallPower:       &apiTrait.Power{State: &apiTrait.Power_State{PowerW: 7200}},
		ChargingSession: &apiTrait.ChargingSession{State: &apiTrait.ChargingSession_State{EnergyWh: 12300, DurationS: 3900}},
	}}}
	tv := speaker("Living Room Speaker", true, false, false)

	co2a, co2b := int32(900), int32(1200)
	radon := int32(157)
	yes := true
	snap := houseSnapshot{
		Building: &api2.Building{Id: "b1", Config: &api2.Building_Config{Name: "Home"}},
		Floors:   []*api2.Floor{{Id: "f1", Name: "Main"}, {Id: "f2", Name: "Property", Exterior: true}},
		Rooms: map[string][]*api2.Room{
			"f1": {
				{Id: "r1", Config: &api2.Room_Config{Name: "Den"}, Properties: &api2.Room_Properties{Co2Ppm: &co2a, RadonBqM3: &radon}, Devices: []*apiDevice.Device{humid("h1", 40), tv}},
				{Id: "r2", Config: &api2.Room_Config{Name: "Office"}, Properties: &api2.Room_Properties{Co2Ppm: &co2b}, Devices: []*apiDevice.Device{humid("h2", 50)}},
			},
			"f2": {{Id: "r3", Config: &api2.Room_Config{Name: "Porch"}, Properties: &api2.Room_Properties{Occupied: &yes}, Devices: []*apiDevice.Device{humid("h3", 99)}}},
		},
		Devices: []*apiDevice.Device{lowBatt, deadLight, ups, charger, lamp(false)},
	}
	ov := buildOverview(snap)
	by := func(tiles []tileView, label string) tileView {
		for _, tl := range tiles {
			if tl.Label == label {
				return tl
			}
		}
		t.Fatalf("no tile %q", label)
		return tileView{}
	}

	assert.Equal(t, "45%", by(ov.Interior, "Humidity").Value, "interior sensors only, exterior 99% excluded")
	co2 := by(ov.Interior, "CO₂ (worst)")
	assert.Equal(t, "1200 ppm", co2.Value)
	assert.Equal(t, "Office", co2.Note)
	assert.True(t, co2.Alert)
	assert.False(t, by(ov.Interior, "Radon (worst)").Alert)
	assert.Equal(t, "—", by(ov.Interior, "VOC (worst)").Value)

	assert.Equal(t, "45 min", by(ov.StatusTiles, "UPS").Value)
	health := by(ov.StatusTiles, "Devices")
	assert.True(t, health.Alert)
	assert.Contains(t, health.Value, "1 offline")
	assert.Contains(t, health.Value, "1 low battery")
	assert.Contains(t, health.Note, "Hall")
	assert.Contains(t, health.Note, "Door")
	assert.Equal(t, "0 of 2", by(ov.StatusTiles, "Lights on").Value, "the unreachable light's remembered on-state is not counted")

	require.Len(t, ov.Media, 1)
	assert.Equal(t, "Den", ov.Media[0].Room)
	require.Len(t, ov.Chargers, 1)
	assert.Equal(t, "Charging", ov.Chargers[0].State)
	assert.Equal(t, "7.20 kW", ov.Chargers[0].Power)
	assert.Equal(t, "12.3 kWh · 1h 5m", ov.Chargers[0].Session)
}

func TestUPSOnBatteryAlerts(t *testing.T) {
	ups := &apiDevice.Device{Id: "ups", Config: &apiDevice.Device_Config{Name: "Rack UPS"}, Details: &apiDevice.Device_Ups{Ups: &apiDevice.UPS{
		Battery: &apiTrait.Battery{State: &apiTrait.Battery_State{Discharging: true, CapacityRemainingMins: 12}}}}}
	tile := upsTile([]*apiDevice.Device{ups})
	assert.Equal(t, "On battery", tile.Value)
	assert.True(t, tile.Alert)
}

func TestExteriorTilesFromWeatherReading(t *testing.T) {
	r := &weatherv1.WeatherReading{
		TempC: -3.2, FeelsLikeC: -9.0, HumidityPct: 81, DewPointC: -5.6, PressureHpa: 1013.4,
		WindSpeedMs: 5, WindGustMs: 9, WindDirDeg: 292, RainDailyMm: 1.25, RainMmHr: 0.4, UvIndex: 0.3,
		Condition: weatherv1.WeatherCondition_WEATHER_CONDITION_MOSTLY_CLOUDY,
	}
	tiles := map[string]tileView{}
	for _, tl := range exteriorTiles(r, "") {
		tiles[tl.Label] = tl
	}
	assert.Equal(t, "-3.2°C", tiles["Temp"].Value)
	assert.Equal(t, "feels like -9°C", tiles["Temp"].Note)
	assert.Equal(t, "Mostly cloudy", tiles["Conditions"].Value)
	assert.Equal(t, "81%", tiles["Humidity"].Value)
	assert.Equal(t, "18 km/h WNW", tiles["Wind"].Value)
	assert.Equal(t, "gusts 32 km/h", tiles["Wind"].Note)
	assert.Equal(t, "1.2 mm", tiles["Rain today"].Value)
	assert.Equal(t, "1013 hPa", tiles["Pressure"].Value)
}

func TestWeatherFeedStalenessAndNil(t *testing.T) {
	var nilFeed *weatherFeed
	r, why := nilFeed.current(time.Now())
	assert.Nil(t, r)
	assert.Contains(t, why, "no weather server")

	now := time.Now()
	f := &weatherFeed{latest: &weatherv1.WeatherReading{Timestamp: timestamppb.New(now.Add(-time.Minute))}}
	r, _ = f.current(now)
	assert.NotNil(t, r)

	f.latest = &weatherv1.WeatherReading{Timestamp: timestamppb.New(now.Add(-time.Hour))}
	r, why = f.current(now)
	assert.Nil(t, r)
	assert.Contains(t, why, "60 min ago")
}

// fakeWeather sends one reading per connection, then goes silent - like a
// half-open connection.
type fakeWeather struct {
	weatherv1.UnimplementedWeatherServiceServer
	conns atomic.Int32
}

func (f *fakeWeather) StreamReadings(_ *weatherv1.StreamRequest, s weatherv1.WeatherService_StreamReadingsServer) error {
	f.conns.Add(1)
	if err := s.Send(&weatherv1.WeatherReading{Timestamp: timestamppb.Now(), TempC: 7}); err != nil {
		return err
	}
	<-s.Context().Done()
	return nil
}

func TestWeatherFeedReconnectsWhenStreamGoesSilent(t *testing.T) {
	old := weatherIdleTimeout
	weatherIdleTimeout = 200 * time.Millisecond
	t.Cleanup(func() { weatherIdleTimeout = old })

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	fw := &fakeWeather{}
	gs := grpc.NewServer()
	weatherv1.RegisterWeatherServiceServer(gs, fw)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	conn, err := grpcutil.DialInsecure(lis.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	feed := startWeatherFeed(ctx, zaptest.NewLogger(t), conn, "")

	require.Eventually(t, func() bool { return fw.conns.Load() >= 2 }, 5*time.Second, 20*time.Millisecond, "silent stream must be re-dialled")
	r, _ := feed.current(time.Now())
	require.NotNil(t, r)
	assert.Equal(t, 7.0, r.GetTempC())
}

func TestDeviceHealthIgnoresUnreportedBatteryAndStaleLights(t *testing.T) {
	reach := &apiDevice.Device_Address{IsReachable: true}
	// Battery trait present but no state yet: "not reported", not 0%.
	unreported := &apiDevice.Device{Id: "s1", Address: reach, Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{Battery: &apiTrait.Battery{}}}}
	assert.Equal(t, "All OK", deviceHealthTile([]*apiDevice.Device{unreported}).Value)

	// An unreachable light's remembered "on" isn't counted as on.
	offline := lamp(true)
	offline.Address = &apiDevice.Device_Address{IsReachable: false}
	online := lamp(true)
	online.Id, online.Address = "lamp2", reach
	assert.Equal(t, "1 of 2", lightsTile([]*apiDevice.Device{offline, online}).Value)
}

func TestOverviewEventsRefreshIsThrottled(t *testing.T) {
	oldD, oldG := overviewDebounce, overviewMinGap
	overviewDebounce, overviewMinGap = 20*time.Millisecond, 600*time.Millisecond
	t.Cleanup(func() { overviewDebounce, overviewMinGap = oldD, oldG })

	s, house, _ := startTestServer(t)
	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/buildings/b1/overview/events")
	require.NoError(t, err)
	defer resp.Body.Close()

	// Count overview re-renders over ~2s of constant room updates.
	var mu sync.Mutex
	renders := 0
	go func() {
		buf := make([]byte, 1<<16)
		for {
			n, err := resp.Body.Read(buf)
			mu.Lock()
			renders += strings.Count(string(buf[:n]), `id="overview" hx-swap-oob`)
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	temp := 20.0
	stop := time.After(2 * time.Second)
tick:
	for {
		select {
		case <-stop:
			break tick
		case <-time.After(10 * time.Millisecond):
			select {
			case house.updates <- &api2.HouseUpdate{Update: &api2.HouseUpdate_Room{Room: &api2.RoomUpdate{RoomId: "r1", Properties: &api2.Room_Properties{TemperatureC: &temp}}}}:
			default:
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, renders, 1, "updates must produce a refresh")
	assert.LessOrEqual(t, renders, 4, "but at most one per overviewMinGap (~600ms) over 2s")
}
