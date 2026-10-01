package main

import (
	"net/http"
	"net/url"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
			ID:   room.GetId(),
			Name: room.GetConfig().GetName(),
			Occ:  occupancy(room.GetProperties()),
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

// selectRoom marks roomID (default: the panel's first room) active in panel
// and loads its detail. Returns nil with no error when the floor has no
// rooms.
func (s *Server) selectRoom(r *http.Request, panel *floorPanelView, roomID string) (*roomDetailView, error) {
	if roomID == "" && len(panel.Rooms) > 0 {
		roomID = panel.Rooms[0].ID
	}
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
	roomID := r.PathValue("id")
	d, err := s.bridge.GetDevice(r.Context(), &api2.GetDeviceRequest{Id: r.PathValue("device_id")})
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	cv := cameraView{RoomID: roomID, Name: houseview.DisplayName(d)}
	streamURL := d.GetCamera().GetMediaStream().GetState().GetUrl()
	if u, err := url.Parse(streamURL); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		cv.URL = streamURL
	} else if streamURL == "" {
		cv.Message = "This camera isn't reporting a stream URL."
	} else {
		cv.Message = "This camera's stream isn't WHEP-compatible (" + streamURL + ")."
	}
	s.renderFragment(w, "camera", cv)
}

// handleDeviceCommand turns a device on/off and swaps in its updated row.
// Only the OnOff command is wired for this first slice.
func (s *Server) handleDeviceCommand(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.httpError(w, r, err)
		return
	}

	d, err := s.bridge.ExecuteCommand(r.Context(), &command.Command{
		DeviceId: id,
		Details:  &command.Command_OnOff{OnOff: &command.OnOff{On: r.FormValue("on") == "true"}},
	})
	if err != nil {
		// Re-render the row from current state with the error inline, so
		// the toggle reflects reality rather than the click.
		dv := deviceView{ID: id, Error: houseview.Message(err)}
		if cur, getErr := s.bridge.GetDevice(r.Context(), &api2.GetDeviceRequest{Id: id}); getErr == nil {
			dv = deviceToView(cur)
			dv.Error = houseview.Message(err)
		} else if status.Code(getErr) == codes.NotFound {
			s.httpError(w, r, getErr)
			return
		}
		s.renderFragment(w, "device_row", dv)
		return
	}
	s.renderFragment(w, "device_row", deviceToView(d))
}
