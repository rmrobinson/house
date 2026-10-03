package main

import (
	"fmt"
	"net/http"
	"net/url"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/lib/houseview"
)

type devicesPageData struct {
	Devices []deviceView
	Filter  devicesFilter
}

// devicesFilter is /devices' set of independent, combinable filters, carried
// in its query string (?unlinked=1&connected=1) so a filtered view is
// bookmarkable and survives a reload.
type devicesFilter struct {
	// Unlinked shows only devices not linked to any room.
	Unlinked bool
	// Connected shows only devices their bridge currently reports reachable.
	Connected bool
}

func parseDevicesFilter(q url.Values) devicesFilter {
	return devicesFilter{
		Unlinked:  q.Get("unlinked") == "1",
		Connected: q.Get("connected") == "1",
	}
}

// devicesFilterFromRequest reads the filter from r's own query string on a
// plain page load, or - for an htmx action like a link/move, whose POST URL
// carries no query string of its own - from the HX-Current-URL header htmx
// sends with every request, so the re-rendered list keeps whatever filters
// the page was showing when the action was taken.
func devicesFilterFromRequest(r *http.Request) devicesFilter {
	if r.Header.Get("HX-Request") == "true" {
		if u, err := url.Parse(r.Header.Get("HX-Current-URL")); err == nil {
			return parseDevicesFilter(u.Query())
		}
	}
	return parseDevicesFilter(r.URL.Query())
}

// URL returns /devices with f applied.
func (f devicesFilter) URL() string {
	q := url.Values{}
	if f.Unlinked {
		q.Set("unlinked", "1")
	}
	if f.Connected {
		q.Set("connected", "1")
	}
	if len(q) == 0 {
		return "/devices"
	}
	return "/devices?" + q.Encode()
}

// ToggleUnlinked/ToggleConnected return the /devices URL with that one
// filter flipped and the other left as-is - the href of each filter toggle.
func (f devicesFilter) ToggleUnlinked() string {
	f.Unlinked = !f.Unlinked
	return f.URL()
}

func (f devicesFilter) ToggleConnected() string {
	f.Connected = !f.Connected
	return f.URL()
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

	filter := devicesFilterFromRequest(r)
	data := devicesPageData{Filter: filter}
	for _, d := range devices {
		roomID := links[d.GetId()].RoomID
		if filter.Unlinked && roomID != "" {
			continue
		}
		if filter.Connected && !d.GetAddress().GetIsReachable() {
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
// admin-ui-implementation.md's "Known scaling gap" note) except the
// device's current one - mirrors handleRoomDevicePicker excluding the
// current room's own devices - for the Link/Move picker on a /devices row.
func (s *Server) handleDeviceRoomPicker(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	ctx := r.Context()

	opts, err := s.allRoomOptions(ctx)
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	links, err := s.deviceRoomMap(ctx)
	if err != nil {
		s.httpError(w, r, err)
		return
	}

	deviceName := deviceID
	if d, derr := s.bridge.GetDevice(ctx, &api2.GetDeviceRequest{Id: deviceID}); derr == nil {
		deviceName = houseview.DisplayName(d)
	}

	current := links[deviceID]
	filtered := opts[:0]
	for _, opt := range opts {
		if opt.ID == current.RoomID {
			continue
		}
		filtered = append(filtered, opt)
	}

	s.renderFragment(w, "room_picker", roomPickerData{
		DeviceID:   deviceID,
		DeviceName: deviceName,
		Version:    current.Version,
		Rooms:      filtered,
	})
}

// handleDeviceRenamePicker opens the rename dialog for a /devices row,
// seeded with the device's current name and version so the Save POST can
// enforce the same optimistic-concurrency contract as any other
// BridgeService write (see house.DeviceConfigOverlay.UpdateDeviceConfig).
func (s *Server) handleDeviceRenamePicker(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	ctx := r.Context()

	d, err := s.bridge.GetDevice(ctx, &api2.GetDeviceRequest{Id: deviceID})
	if err != nil {
		s.httpError(w, r, err)
		return
	}

	s.renderFragment(w, "device_rename", deviceRenameData{
		DeviceID:    deviceID,
		CurrentName: houseview.DisplayName(d),
		Version:     d.GetVersion(),
	})
}

type deviceRenameData struct {
	DeviceID    string
	CurrentName string
	// Version is the device's current Device.version at the time the
	// dialog was opened, round-tripped through the form so
	// handleDeviceRename can detect it's changed since - see
	// house.DeviceConfigOverlay.UpdateDeviceConfig.
	Version string
	// RoomID is set when this dialog was opened from a room's device list
	// (/rooms/{RoomID}/devices/{DeviceID}/rename) rather than /devices, so
	// the form can post back to the right page's rename endpoint - see
	// device_rename.html.
	RoomID string
}

// handleDeviceRename saves a new display name for a device - persisted as a
// housedb override (service/house.DeviceConfigOverlay), not written to the
// device itself, via BridgeService.UpdateDeviceConfig, the same existing RPC
// every other BridgeService write already uses.
func (s *Server) handleDeviceRename(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.httpError(w, r, err)
		return
	}
	name := r.FormValue("name")
	ctx := r.Context()

	_, err := s.bridge.UpdateDeviceConfig(ctx, &api2.UpdateDeviceConfigRequest{
		Id:      deviceID,
		Version: r.FormValue("version"),
		Config:  &apiDevice.Device_Config{Name: name},
	})
	if err != nil {
		data, loadErr := s.loadDevicesPageData(r)
		if loadErr != nil {
			s.httpError(w, r, loadErr)
			return
		}
		s.respond(w, "devices", data, houseview.Message(err), true)
		return
	}

	data, loadErr := s.loadDevicesPageData(r)
	if loadErr != nil {
		s.httpError(w, r, loadErr)
		return
	}
	s.respond(w, "devices", data, fmt.Sprintf("Renamed to %s", name), false)
}

type roomPickerData struct {
	DeviceID   string
	DeviceName string
	// Version is deviceID's current link version (empty if it isn't linked
	// yet) - carried through as a hidden field on every room option's Select
	// form, so handleDeviceLink can enforce it hasn't changed since this
	// picker was opened.
	Version string
	Rooms   []roomOption
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
		deviceName = houseview.DisplayName(d)
	}

	resp, err := s.house.LinkDevice(ctx, &api2.LinkDeviceRequest{DeviceId: deviceID, RoomId: roomID, Version: r.FormValue("version")})
	if err != nil {
		data, loadErr := s.loadDevicesPageData(r)
		if loadErr != nil {
			s.httpError(w, r, loadErr)
			return
		}
		s.respond(w, "devices", data, houseview.Message(err), true)
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

	data, loadErr := s.loadDevicesPageData(r)
	if loadErr != nil {
		s.httpError(w, r, loadErr)
		return
	}
	s.respond(w, "devices", data, flash, false)
}
