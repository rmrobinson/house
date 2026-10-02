package main

import (
	"cmp"
	"net/url"
	"slices"
	"strings"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	apiTrait "github.com/rmrobinson/house/api/trait"
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
	// Properties feeds the floor summary's per-room readings.
	Properties houseview.Properties
}

// OccLabel is the dot's text alternative.
func (r roomRowView) OccLabel() string { return occupancyLabel(r.Occ) }

type deviceView struct {
	ID     string
	Name   string
	Kind   string
	Online bool
	// CanToggle is true when the device has a controllable OnOff trait; On
	// is its current state.
	CanToggle bool
	On        bool
	// CanDim is true when the device has a controllable Brightness trait;
	// Level is its current 0-100 level.
	CanDim   bool
	Level    int
	IsCamera bool
	// ViewHref opens this camera's player. RowView is true when it's the
	// room's only camera, so its own row carries the [ VIEW ] button; with
	// several, the DEVICES header offers one [ VIEW ] that opens a picker.
	ViewHref string
	RowView  bool
	Error    string
}

type roomDetailView struct {
	ID         string
	Name       string
	Properties houseview.Properties
	Devices    []deviceView
	// Cameras are the linked Camera devices - one VIEW CAMERA button each.
	Cameras []deviceView
}

type floorPanelView struct {
	BuildingID string
	Floors     []floorView
	FloorID    string
	FloorName  string
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
	// ICEServers is a JSON array of STUN/TURN urls for the peer connection
	// ("[]" on a LAN).
	ICEServers string
}

// whepURL picks the stream URL a browser can play: the first WEBRTC_WHEP
// endpoint (endpoints are in preference order), else - for a bridge that
// only fills the deprecated State.url - that url if it is http(s). The URL is empty when nothing is playable, in which case reason says why, without echoing any url
// (RTSP urls commonly embed credentials).
func whepURL(ms *apiTrait.MediaStream) (streamURL, reason string) {
	state := ms.GetState()
	for _, ep := range state.GetEndpoints() {
		if ep.GetProtocol() == apiTrait.MediaStream_WEBRTC_WHEP && isHTTP(ep.GetUrl()) {
			return ep.GetUrl(), ""
		}
	}
	if len(state.GetEndpoints()) == 0 && isHTTP(state.GetUrl()) {
		return state.GetUrl(), ""
	}
	if len(state.GetEndpoints()) == 0 && state.GetUrl() == "" {
		return "", "This camera isn't reporting a stream URL."
	}
	return "", "This camera doesn't offer a WebRTC (WHEP) stream a browser can play."
}

func isHTTP(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
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

// occupancyLabel is the text alternative for the occupancy dot's colour.
func occupancyLabel(occ string) string {
	switch occ {
	case "yes":
		return "occupied"
	case "no":
		return "vacant"
	default:
		return "occupancy unknown"
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
	if b := houseview.Brightness(d); b != nil && b.GetAttributes().GetCanControl() {
		dv.CanDim = true
		dv.Level = int(max(0, min(100, b.GetState().GetLevel())))
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
		if dv.IsCamera {
			dv.ViewHref = "/rooms/" + rv.ID + "/camera/" + dv.ID
			rv.Cameras = append(rv.Cameras, dv)
		}
		rv.Devices = append(rv.Devices, dv)
	}
	if len(rv.Cameras) == 1 {
		for i := range rv.Devices {
			rv.Devices[i].RowView = rv.Devices[i].IsCamera
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
