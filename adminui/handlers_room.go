package main

import (
	"fmt"
	"net/http"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
)

type roomPageData struct {
	Room     roomView
	Floor    floorView
	Building buildingView
}

// loadRoomPageData resolves room's parent floor/building, tolerating either
// being missing - a room predating migration 000003_add_floors_and_versions
// has no floor at all (floor_id NULL, surfaced as room.FloorId == "" - see
// scanRoom), so GetFloor 404s for it exactly like it would for a floor since
// deleted out from under a stale link. Either way this degrades to a
// placeholder rather than failing the whole page and making the room
// permanently inaccessible - the same tolerance roomLabel already gives a
// stale room/floor/building reference elsewhere in this app.
func (s *Server) loadRoomPageData(r *http.Request, id string) (roomPageData, error) {
	ctx := r.Context()
	room, err := s.house.GetRoom(ctx, &api2.GetRoomRequest{Id: id})
	if err != nil {
		return roomPageData{}, err
	}

	fv := floorView{Name: "(no floor)"}
	if floor, err := s.house.GetFloor(ctx, &api2.GetFloorRequest{Id: room.GetFloorId()}); err == nil {
		fv = floorToView(floor)
	} else if status.Code(err) != codes.NotFound {
		return roomPageData{}, err
	}

	bv := buildingView{Name: "(deleted building)"}
	if building, err := s.house.GetBuilding(ctx, &api2.GetBuildingRequest{Id: room.GetBuildingId()}); err == nil {
		bv = buildingToView(building)
	} else if status.Code(err) != codes.NotFound {
		return roomPageData{}, err
	}

	return roomPageData{
		Room:     roomToView(room),
		Floor:    fv,
		Building: bv,
	}, nil
}

func (s *Server) handleRoomGet(w http.ResponseWriter, r *http.Request) {
	data, err := s.loadRoomPageData(r, r.PathValue("id"))
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	s.renderPage(w, "room", data)
}

func (s *Server) handleRoomUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.httpError(w, r, err)
		return
	}

	// type isn't editable here - see roomView - so it arrives as a hidden
	// field round-tripping the current value rather than typed input.
	roomType, _ := strconv.Atoi(r.FormValue("type"))

	_, err := s.house.UpdateRoom(r.Context(), &api2.UpdateRoomRequest{
		Id:      id,
		Version: r.FormValue("version"),
		Config: &api2.Room_Config{
			Name: r.FormValue("name"),
			Type: int32(roomType),
		},
	})
	flash, isError := successOrError(err, "Room updated")

	data, loadErr := s.loadRoomPageData(r, id)
	if loadErr != nil {
		s.httpError(w, r, loadErr)
		return
	}
	s.respond(w, "room", data, flash, isError)
}

func (s *Server) handleRoomDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.httpError(w, r, err)
		return
	}

	data, loadErr := s.loadRoomPageData(r, id)
	if loadErr != nil {
		s.httpError(w, r, loadErr)
		return
	}

	if _, err := s.house.DeleteRoom(r.Context(), &api2.DeleteRoomRequest{Id: id, Version: r.FormValue("version")}); err != nil {
		s.respond(w, "room", data, grpcMessage(err), true)
		return
	}

	// data.Floor.ID is empty for a legacy floorless room (see
	// loadRoomPageData) - redirecting to "/floors/" would land on a floor
	// page for an empty id, which 404s. Fall back up the hierarchy to
	// whichever ancestor actually exists.
	switch {
	case data.Floor.ID != "":
		redirectAfterDelete(w, "/floors/"+data.Floor.ID)
	case data.Building.ID != "":
		redirectAfterDelete(w, "/buildings/"+data.Building.ID)
	default:
		redirectAfterDelete(w, "/buildings")
	}
}

type devicePickerEntry struct {
	ID       string
	Name     string
	Location string
	// Version is this device's current link version (empty if unlinked) -
	// carried through as a hidden field on its Select form, so
	// handleRoomLinkDevice can enforce it hasn't changed since this picker
	// was opened.
	Version string
}

type devicePickerData struct {
	TargetRoomID string
	Devices      []devicePickerEntry
}

// handleRoomDevicePicker lists every device not already linked to this room,
// each showing its current room (or "Unlinked") as a badge - the "Add
// device" picker on /rooms/{id}.
func (s *Server) handleRoomDevicePicker(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("id")
	ctx := r.Context()

	devices, err := s.listDevices(ctx)
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	links, err := s.deviceRoomMap(ctx)
	if err != nil {
		s.httpError(w, r, err)
		return
	}

	labelCache := map[string]string{}
	resolveLabel := func(currentRoomID string) (string, error) {
		if label, ok := labelCache[currentRoomID]; ok {
			return label, nil
		}
		label, err := s.roomLabel(ctx, currentRoomID)
		if err != nil {
			return "", err
		}
		labelCache[currentRoomID] = label
		return label, nil
	}

	data := devicePickerData{TargetRoomID: roomID}
	for _, d := range devices {
		current := links[d.GetId()]
		if current.RoomID == roomID {
			continue
		}
		label, err := resolveLabel(current.RoomID)
		if err != nil {
			s.httpError(w, r, err)
			return
		}
		data.Devices = append(data.Devices, devicePickerEntry{
			ID:       d.GetId(),
			Name:     deviceDisplayName(d),
			Location: label,
			Version:  current.Version,
		})
	}

	s.renderFragment(w, "device_picker", data)
}

func (s *Server) handleRoomLinkDevice(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.httpError(w, r, err)
		return
	}
	deviceID := r.FormValue("device_id")
	ctx := r.Context()

	data, loadErr := s.loadRoomPageData(r, roomID)
	if loadErr != nil {
		s.httpError(w, r, loadErr)
		return
	}

	resp, err := s.house.LinkDevice(ctx, &api2.LinkDeviceRequest{DeviceId: deviceID, RoomId: roomID, Version: r.FormValue("version")})
	if err != nil {
		s.respond(w, "room", data, grpcMessage(err), true)
		return
	}

	deviceName := deviceID
	if d, derr := s.bridge.GetDevice(ctx, &api2.GetDeviceRequest{Id: deviceID}); derr == nil {
		deviceName = deviceDisplayName(d)
	}

	var flash string
	if resp.PreviousRoomId != nil {
		prevLabel, err := s.roomLabel(ctx, resp.GetPreviousRoomId())
		if err != nil {
			s.httpError(w, r, err)
			return
		}
		flash = fmt.Sprintf("%s moved to %s from %s", deviceName, data.Room.Name, prevLabel)
	} else {
		flash = fmt.Sprintf("%s linked to %s", deviceName, data.Room.Name)
	}

	data, loadErr = s.loadRoomPageData(r, roomID)
	if loadErr != nil {
		s.httpError(w, r, loadErr)
		return
	}
	s.respond(w, "room", data, flash, false)
}

func (s *Server) handleRoomUnlinkDevice(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.httpError(w, r, err)
		return
	}
	deviceID := r.FormValue("device_id")
	ctx := r.Context()

	deviceName := deviceID
	if d, derr := s.bridge.GetDevice(ctx, &api2.GetDeviceRequest{Id: deviceID}); derr == nil {
		deviceName = deviceDisplayName(d)
	}

	data, loadErr := s.loadRoomPageData(r, roomID)
	if loadErr != nil {
		s.httpError(w, r, loadErr)
		return
	}

	if _, err := s.house.UnlinkDevice(ctx, &api2.UnlinkDeviceRequest{DeviceId: deviceID}); err != nil {
		s.respond(w, "room", data, grpcMessage(err), true)
		return
	}

	data, loadErr = s.loadRoomPageData(r, roomID)
	if loadErr != nil {
		s.httpError(w, r, loadErr)
		return
	}
	s.respond(w, "room", data, fmt.Sprintf("%s unlinked from %s", deviceName, data.Room.Name), false)
}
