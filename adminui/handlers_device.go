package main

import (
	"fmt"
	"net/http"

	api2 "github.com/rmrobinson/house/api"
)

type devicesPageData struct {
	Devices []deviceView
	// Filter is "all" or "unlinked" - which tab is active on /devices.
	Filter string
}

func (s *Server) loadDevicesPageData(r *http.Request) (devicesPageData, error) {
	ctx := r.Context()
	devices, err := s.listDevices(ctx)
	if err != nil {
		return devicesPageData{}, err
	}
	links, err := s.deviceRoomMap(ctx)
	if err != nil {
		return devicesPageData{}, err
	}

	labelCache := map[string]string{}
	resolveLabel := func(roomID string) (string, error) {
		if label, ok := labelCache[roomID]; ok {
			return label, nil
		}
		label, err := s.roomLabel(ctx, roomID)
		if err != nil {
			return "", err
		}
		labelCache[roomID] = label
		return label, nil
	}

	filter := r.URL.Query().Get("filter")
	if filter != "unlinked" {
		filter = "all"
	}

	data := devicesPageData{Filter: filter}
	for _, d := range devices {
		roomID := links[d.GetId()]
		if filter == "unlinked" && roomID != "" {
			continue
		}

		dv := deviceToView(d)
		dv.RoomID = roomID
		label, err := resolveLabel(roomID)
		if err != nil {
			return devicesPageData{}, err
		}
		dv.RoomName = label
		data.Devices = append(data.Devices, dv)
	}
	return data, nil
}

func (s *Server) handleDevicesList(w http.ResponseWriter, r *http.Request) {
	data, err := s.loadDevicesPageData(r)
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	s.renderPage(w, "devices", data)
}

// handleDeviceRoomPicker lists every room in the house (flat, see
// admin-ui-implementation.md's "Known scaling gap" note), for the
// Link/Move picker on a /devices row.
func (s *Server) handleDeviceRoomPicker(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	opts, err := s.allRoomOptions(r.Context())
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	s.renderFragment(w, "room_picker", roomPickerData{
		DeviceID: deviceID,
		Rooms:    opts,
	})
}

type roomPickerData struct {
	DeviceID string
	Rooms    []roomOption
}

func (s *Server) handleDeviceLink(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.httpError(w, r, err)
		return
	}
	roomID := r.FormValue("room_id")
	ctx := r.Context()

	deviceName := deviceID
	if d, derr := s.bridge.GetDevice(ctx, &api2.GetDeviceRequest{Id: deviceID}); derr == nil {
		deviceName = deviceDisplayName(d)
	}

	data, loadErr := s.loadDevicesPageData(r)
	if loadErr != nil {
		s.httpError(w, r, loadErr)
		return
	}

	resp, err := s.house.LinkDevice(ctx, &api2.LinkDeviceRequest{DeviceId: deviceID, RoomId: roomID})
	if err != nil {
		s.respond(w, "devices", data, grpcMessage(err), true)
		return
	}

	newRoomLabel, err := s.roomLabel(ctx, roomID)
	if err != nil {
		s.httpError(w, r, err)
		return
	}

	var flash string
	if resp.PreviousRoomId != nil {
		prevLabel, err := s.roomLabel(ctx, resp.GetPreviousRoomId())
		if err != nil {
			s.httpError(w, r, err)
			return
		}
		flash = fmt.Sprintf("%s moved to %s from %s", deviceName, newRoomLabel, prevLabel)
	} else {
		flash = fmt.Sprintf("%s linked to %s", deviceName, newRoomLabel)
	}

	data, loadErr = s.loadDevicesPageData(r)
	if loadErr != nil {
		s.httpError(w, r, loadErr)
		return
	}
	s.respond(w, "devices", data, flash, false)
}
