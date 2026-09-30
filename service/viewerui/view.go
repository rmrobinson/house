package main

import (
	"cmp"
	"slices"
	"strings"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/lib/houseview"
)

type floorView struct {
	ID     string
	Name   string
	Active bool
}

type roomRowView struct {
	ID     string
	Name   string
	Active bool
	// Occ is "yes", "no" or "unknown" (no linked sensor has reported) -
	// used as a CSS class suffix for the occupancy dot.
	Occ string
}

type deviceView struct {
	ID     string
	Name   string
	Kind   string
	Online bool
	// CanToggle is true when the device has a controllable OnOff trait; On
	// is its current state.
	CanToggle bool
	On        bool
	IsCamera  bool
	Error     string
}

type roomDetailView struct {
	ID         string
	Name       string
	Properties houseview.Properties
	Devices    []deviceView
	// Camera is the first linked Camera device, if any - drives the VIEW
	// CAMERA button.
	Camera *deviceView
}

type floorPanelView struct {
	BuildingID string
	Floors     []floorView
	FloorID    string
	Rooms      []roomRowView
}

type buildingPageData struct {
	ID    string
	Name  string
	Panel floorPanelView
	// Room is preloaded when a room is requested directly (?room=) rather
	// than via an htmx swap; nil otherwise.
	Room *roomDetailView
}

type cameraView struct {
	RoomID string
	Name   string
	// URL is the WHEP endpoint; empty when the camera reports no usable
	// http(s) stream URL, in which case Message says why.
	URL     string
	Message string
}

func occupancy(p *api2.Room_Properties) string {
	switch {
	case p == nil || p.Occupied == nil:
		return "unknown"
	case p.GetOccupied():
		return "yes"
	default:
		return "no"
	}
}

func deviceToView(d *apiDevice.Device) deviceView {
	dv := deviceView{
		ID:       d.GetId(),
		Name:     houseview.DisplayName(d),
		Kind:     houseview.Kind(d),
		Online:   d.GetAddress().GetIsReachable(),
		IsCamera: d.GetCamera() != nil,
	}
	if onOff := houseview.OnOff(d); onOff != nil && onOff.GetAttributes().GetCanControl() {
		dv.CanToggle = true
		dv.On = onOff.GetState().GetIsOn()
	}
	return dv
}

func roomToDetail(r *api2.Room) roomDetailView {
	rv := roomDetailView{
		ID:         r.GetId(),
		Name:       r.GetConfig().GetName(),
		Properties: houseview.PropertiesToView(r.GetProperties()),
	}
	for _, d := range houseview.SortDevices(r.GetDevices()) {
		dv := deviceToView(d)
		rv.Devices = append(rv.Devices, dv)
		if dv.IsCamera && rv.Camera == nil {
			c := dv
			rv.Camera = &c
		}
	}
	return rv
}

// sortFloors orders floors by sort_order (then name), matching adminui's
// level-list order.
func sortFloors(floors []*api2.Floor) {
	slices.SortFunc(floors, func(a, b *api2.Floor) int {
		return cmp.Or(
			cmp.Compare(a.GetSortOrder(), b.GetSortOrder()),
			cmp.Compare(strings.ToLower(a.GetName()), strings.ToLower(b.GetName())),
		)
	})
}

// sortRooms orders rooms by name - Room has no stored position field, so
// name is the stable order available today.
func sortRooms(rooms []*api2.Room) {
	slices.SortFunc(rooms, func(a, b *api2.Room) int {
		return cmp.Or(
			cmp.Compare(strings.ToLower(a.GetConfig().GetName()), strings.ToLower(b.GetConfig().GetName())),
			cmp.Compare(a.GetId(), b.GetId()),
		)
	})
}
