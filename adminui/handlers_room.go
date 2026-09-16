package main

import (
	"fmt"
	"net/http"

	api2 "github.com/rmrobinson/house/api"
)

type roomPageData struct {
	Room     roomView
	Floor    floorView
	Building buildingView
}

func (s *Server) loadRoomPageData(r *http.Request, id string) (roomPageData, error) {
	ctx := r.Context()
	room, err := s.house.GetRoom(ctx, &api2.GetRoomRequest{Id: id})
	if err != nil {
		return roomPageData{}, err
	}
	floor, err := s.house.GetFloor(ctx, &api2.GetFloorRequest{Id: room.GetFloorId()})
	if err != nil {
		return roomPageData{}, err
	}
	building, err := s.house.GetBuilding(ctx, &api2.GetBuildingRequest{Id: room.GetBuildingId()})
	if err != nil {
		return roomPageData{}, err
	}
	return roomPageData{
		Room:     roomToView(room),
		Floor:    floorToView(floor),
		Building: buildingToView(building),
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

func (s *Server) handleRoomDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	data, loadErr := s.loadRoomPageData(r, id)
	if loadErr != nil {
		s.httpError(w, r, loadErr)
		return
	}

	if _, err := s.house.DeleteRoom(r.Context(), &api2.DeleteRoomRequest{Id: id}); err != nil {
		s.respond(w, "room", data, grpcMessage(err), true)
		return
	}
	redirectAfterDelete(w, "/floors/"+data.Floor.ID)
}

type devicePickerEntry struct {
	ID       string
	Name     string
	Location string
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
		currentRoomID := links[d.GetId()]
		if currentRoomID == roomID {
			continue
		}
		label, err := resolveLabel(currentRoomID)
		if err != nil {
			s.httpError(w, r, err)
			return
		}
		data.Devices = append(data.Devices, devicePickerEntry{
			ID:       d.GetId(),
			Name:     deviceDisplayName(d),
			Location: label,
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

	resp, err := s.house.LinkDevice(ctx, &api2.LinkDeviceRequest{DeviceId: deviceID, RoomId: roomID})
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
