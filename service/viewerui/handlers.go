package main

import (
	"net/http"
	"net/url"
	"strconv"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/service/lib/houseview"
)

// handleRoot sends you straight to your building when there's only one (the
// common case), else lists them.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	buildings, err := houseview.ListBuildings(r.Context(), s.house)
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	if len(buildings) == 1 {
		http.Redirect(w, r, "/buildings/"+buildings[0].GetId(), http.StatusSeeOther)
		return
	}
	type row struct{ ID, Name string }
	var rows []row
	for _, b := range buildings {
		rows = append(rows, row{ID: b.GetId(), Name: b.GetConfig().GetName()})
	}
	s.renderPage(w, "buildings", rows)
}

// loadFloorPanel builds the floor list plus the rooms of floorID (the first
// floor when floorID is empty or unknown).
func (s *Server) loadFloorPanel(r *http.Request, buildingID, floorID string) (floorPanelView, error) {
	ctx := r.Context()
	floors, err := houseview.ListFloors(ctx, s.house, buildingID)
	if err != nil {
		return floorPanelView{}, err
	}
	sortFloors(floors)

	panel := floorPanelView{BuildingID: buildingID}
	if len(floors) == 0 {
		return panel, nil
	}
	active := floors[0]
	for _, f := range floors {
		if f.GetId() == floorID {
			active = f
		}
	}
	panel.FloorID = active.GetId()
	panel.FloorName = active.GetName()
	for _, f := range floors {
		panel.Floors = append(panel.Floors, floorView{ID: f.GetId(), Name: f.GetName(), Active: f.GetId() == active.GetId()})
	}

	rooms, err := houseview.ListRoomsByFloor(ctx, s.house, active.GetId())
	if err != nil {
		return floorPanelView{}, err
	}
	sortRooms(rooms)
	for _, room := range rooms {
		panel.Rooms = append(panel.Rooms, roomRowView{
			ID:         room.GetId(),
			Name:       room.GetConfig().GetName(),
			Occ:        occupancy(room.GetProperties()),
			Properties: houseview.PropertiesToView(room.GetProperties()),
			NowPlaying: roomNowPlaying(room.GetDevices()),
		})
	}
	return panel, nil
}

func (s *Server) handleBuilding(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	building, err := s.house.GetBuilding(r.Context(), &api2.GetBuildingRequest{Id: id})
	if err != nil {
		s.httpError(w, r, err)
		return
	}

	data := buildingPageData{ID: id, Name: building.GetConfig().GetName()}
	floorID := r.URL.Query().Get("floor")

	// ?room= lets a room be linked to directly (no htmx swap to have
	// populated the detail pane), opening on that room's floor.
	if roomID := r.URL.Query().Get("room"); roomID != "" {
		room, err := s.house.GetRoom(r.Context(), &api2.GetRoomRequest{Id: roomID})
		if err != nil {
			s.httpError(w, r, err)
			return
		}
		if room.GetBuildingId() != id {
			http.NotFound(w, r)
			return
		}
		floorID = room.GetFloorId()
		rv := roomToDetail(room)
		data.Room = &rv
	}

	data.Panel, err = s.loadFloorPanel(r, id, floorID)
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	selected := ""
	if data.Room != nil {
		selected = data.Room.ID
	}
	if data.Room, err = s.selectRoom(r, &data.Panel, selected); err != nil {
		s.httpError(w, r, err)
		return
	}
	s.renderPage(w, "building", data)
}

func (s *Server) handleFloor(w http.ResponseWriter, r *http.Request) {
	panel, err := s.loadFloorPanel(r, r.PathValue("id"), r.PathValue("floor_id"))
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	room, err := s.selectRoom(r, &panel, "")
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	s.renderFragment(w, "floor_panel_select", struct {
		Panel floorPanelView
		Room  *roomDetailView
	}{panel, room})
}

// selectRoom marks roomID active in panel and loads its detail. Returns nil
// with no error when roomID is empty: only the floor is selected, and the
// detail pane shows the floor summary instead.
func (s *Server) selectRoom(r *http.Request, panel *floorPanelView, roomID string) (*roomDetailView, error) {
	if roomID == "" {
		return nil, nil
	}
	for i := range panel.Rooms {
		panel.Rooms[i].Active = panel.Rooms[i].ID == roomID
	}
	room, err := s.house.GetRoom(r.Context(), &api2.GetRoomRequest{Id: roomID})
	if err != nil {
		return nil, err
	}
	rv := roomToDetail(room)
	return &rv, nil
}

func (s *Server) handleRoom(w http.ResponseWriter, r *http.Request) {
	room, err := s.house.GetRoom(r.Context(), &api2.GetRoomRequest{Id: r.PathValue("id")})
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	// A direct browser navigation (not an htmx swap) gets the full page.
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, "/buildings/"+room.GetBuildingId()+"?room="+url.QueryEscape(room.GetId()), http.StatusSeeOther)
		return
	}
	s.renderFragment(w, "room_detail", roomToDetail(room))
}

// handleCamera swaps the detail pane for a <video> that negotiates WHEP
// against the camera's media_stream url (see templates/partials/camera.html).
func (s *Server) handleCamera(w http.ResponseWriter, r *http.Request) {
	roomID, deviceID := r.PathValue("id"), r.PathValue("device_id")

	// Only serve a stream for a camera actually linked to this room.
	room, err := s.house.GetRoom(r.Context(), &api2.GetRoomRequest{Id: roomID})
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	linked := false
	for _, rd := range room.GetDevices() {
		linked = linked || rd.GetId() == deviceID
	}
	d, err := s.bridge.GetDevice(r.Context(), &api2.GetDeviceRequest{Id: deviceID})
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	if !linked || d.GetCamera() == nil {
		http.NotFound(w, r)
		return
	}

	cv := cameraView{RoomID: roomID, Name: houseview.DisplayName(d), ICEServers: s.iceServersJSON}
	cv.URL, cv.Message = whepURL(d.GetCamera().GetMediaStream())
	s.renderFragment(w, "camera", cv)
}

// handleDeviceCommand applies one control to a device and swaps in its
// updated row: form field "on" (true|false) sends OnOff, "brightness"
// (0-100) sends BrightnessAbsolute, "playback" (play|pause) sends Playback,
// "skip" (forward|backward) sends SkipForward/SkipBackward, "volume" (0-N)
// sends VolumeAbsolute, "mode" (a Mode value, e.g. a standing desk preset)
// sends Mode.
func (s *Server) handleDeviceCommand(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	cmd := &command.Command{DeviceId: id}
	switch {
	case r.PostForm.Has("brightness"):
		level, err := strconv.Atoi(r.PostForm.Get("brightness"))
		if err != nil || level < 0 || level > 100 {
			http.Error(w, "brightness must be an integer from 0 to 100", http.StatusBadRequest)
			return
		}
		cmd.Details = &command.Command_BrightnessAbsolute{BrightnessAbsolute: &command.BrightnessAbsolute{BrightnessPercent: int32(level)}}
	case r.PostForm.Get("on") == "true", r.PostForm.Get("on") == "false":
		cmd.Details = &command.Command_OnOff{OnOff: &command.OnOff{On: r.PostForm.Get("on") == "true"}}
	case r.PostForm.Get("playback") == "play":
		cmd.Details = &command.Command_Playback{Playback: &command.Playback{Action: command.Playback_ACTION_PLAY}}
	case r.PostForm.Get("playback") == "pause":
		cmd.Details = &command.Command_Playback{Playback: &command.Playback{Action: command.Playback_ACTION_PAUSE}}
	case r.PostForm.Get("skip") == "forward":
		cmd.Details = &command.Command_SkipForward{SkipForward: &command.SkipForward{}}
	case r.PostForm.Get("skip") == "backward":
		cmd.Details = &command.Command_SkipBackward{SkipBackward: &command.SkipBackward{}}
	case r.PostForm.Get("mode") != "":
		cmd.Details = &command.Command_Mode{Mode: &command.Mode{Value: r.PostForm.Get("mode")}}
	case r.PostForm.Has("volume"):
		level, err := strconv.Atoi(r.PostForm.Get("volume"))
		if err != nil || level < 0 {
			http.Error(w, "volume must be a non-negative integer", http.StatusBadRequest)
			return
		}
		cmd.Details = &command.Command_VolumeAbsolute{VolumeAbsolute: &command.VolumeAbsolute{Level: int32(level)}}
	default:
		http.Error(w, `expected on=true|false, brightness=0-100, playback=play|pause, skip=forward|backward, volume=0-N, or mode=<value>`, http.StatusBadRequest)
		return
	}

	d, err := s.bridge.ExecuteCommand(r.Context(), cmd)
	if err != nil {
		// Re-render the row from current state with the error inline, so
		// the controls reflect reality rather than the click. If even the
		// current state can't be read there's no row to show: surface the
		// command's own error instead.
		cur, getErr := s.bridge.GetDevice(r.Context(), &api2.GetDeviceRequest{Id: id})
		if getErr != nil {
			s.httpError(w, r, err)
			return
		}
		dv := deviceToView(cur)
		dv.Error = houseview.Message(err)
		s.renderFragment(w, "device_row", dv)
		return
	}
	s.renderFragment(w, "device_row", deviceToView(d))
}
