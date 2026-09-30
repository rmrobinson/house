package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/lib/houseview"
)

// buildingView/floorView/roomView/deviceView mirror the proto messages,
// reshaped for templates. Names of parent entities (e.g. a room's building
// and floor names) are resolved with their own RPCs rather than joined
// server-side - deliberate N+1, acceptable at admin-tool traffic levels (see
// admin-ui-implementation.md).
type buildingView struct {
	ID      string
	Name    string
	TZ      string
	Version string
	Lat     float64
	Lon     float64
}

type floorView struct {
	ID         string
	Name       string
	SortOrder  int32
	BuildingID string
	Version    string
}

type roomView struct {
	ID         string
	Name       string
	FloorID    string
	BuildingID string
	Version    string
	// Type isn't exposed as an editable field anywhere in this app (see
	// floor.html's "Add room" form) - carried through only so an edit form
	// can round-trip it via a hidden input instead of silently resetting it
	// to Unspecified on save.
	Type        int32
	Devices     []deviceView
	Aggregation aggregationView
	Properties  propertiesView
}

// aggregationView mirrors Room.Config.aggregation, reshaped for the edit
// form's five <select> elements (room.html) - each field is one of
// strategyValues' strings, matching the <option value="..."> the form
// posts back, so room.html never has to compare across the proto's own
// int32 enum (see strategyToStr/strategyFromStr).
type aggregationView struct {
	OccupancyStrategy   string
	TemperatureStrategy string
	LightStrategy       string
	AirQualityStrategy  string
	PowerStrategy       string
}

// propertiesView mirrors Room.Properties, pre-formatted for display -
// each field is "" when that metric is unset (no linked device has
// reported it yet), which room.html treats as "Unknown".
type propertiesView struct {
	Occupied        string
	TemperatureC    string
	LightLevelLux   string
	AirQualityIndex string
	Co2Ppm          string
	VocPpb          string
	RadonBqM3       string
	PowerDrawW      string
}

// strategyToStr/strategyFromStr convert api2.AggregationConfig_Strategy to
// and from the plain string room.html's aggregation <select>s use as their
// option values - the same "proto enum <-> template-friendly string"
// pattern onConditionFalseStr uses for policy pages (see policy_view.go).
// An unrecognized string (there shouldn't be one, short of a hand-crafted
// request) falls back to STRATEGY_UNSPECIFIED, same as the enum's own zero
// value.
func strategyToStr(s api2.AggregationConfig_Strategy) string {
	switch s {
	case api2.AggregationConfig_LATEST:
		return "latest"
	case api2.AggregationConfig_AVERAGE:
		return "average"
	case api2.AggregationConfig_MIN:
		return "min"
	case api2.AggregationConfig_MAX:
		return "max"
	case api2.AggregationConfig_SUM:
		return "sum"
	case api2.AggregationConfig_ANY:
		return "any"
	default:
		return "unspecified"
	}
}

func strategyFromStr(s string) api2.AggregationConfig_Strategy {
	switch s {
	case "latest":
		return api2.AggregationConfig_LATEST
	case "average":
		return api2.AggregationConfig_AVERAGE
	case "min":
		return api2.AggregationConfig_MIN
	case "max":
		return api2.AggregationConfig_MAX
	case "sum":
		return api2.AggregationConfig_SUM
	case "any":
		return api2.AggregationConfig_ANY
	default:
		return api2.AggregationConfig_STRATEGY_UNSPECIFIED
	}
}

// aggregationToView reads a.Get*() through a possibly-nil a - proto
// accessors on a nil message return each field's zero value, so this needs
// no nil check of its own.
func aggregationToView(a *api2.AggregationConfig) aggregationView {
	return aggregationView{
		OccupancyStrategy:   strategyToStr(a.GetOccupancyStrategy()),
		TemperatureStrategy: strategyToStr(a.GetTemperatureStrategy()),
		LightStrategy:       strategyToStr(a.GetLightStrategy()),
		AirQualityStrategy:  strategyToStr(a.GetAirQualityStrategy()),
		PowerStrategy:       strategyToStr(a.GetPowerStrategy()),
	}
}

// propertiesToView formats p's set fields for display, leaving an unset
// metric (including every field, if p itself is nil - no linked device has
// reported anything for this room yet) as "".
func propertiesToView(p *api2.Room_Properties) propertiesView {
	var pv propertiesView
	if p == nil {
		return pv
	}
	if p.Occupied != nil {
		if p.GetOccupied() {
			pv.Occupied = "Yes"
		} else {
			pv.Occupied = "No"
		}
	}
	if p.TemperatureC != nil {
		pv.TemperatureC = fmt.Sprintf("%.1f°C", p.GetTemperatureC())
	}
	if p.LightLevelLux != nil {
		pv.LightLevelLux = fmt.Sprintf("%d lux", p.GetLightLevelLux())
	}
	if p.AirQualityIndex != nil {
		pv.AirQualityIndex = fmt.Sprintf("%d", p.GetAirQualityIndex())
	}
	if p.Co2Ppm != nil {
		pv.Co2Ppm = fmt.Sprintf("%d ppm", p.GetCo2Ppm())
	}
	if p.VocPpb != nil {
		pv.VocPpb = fmt.Sprintf("%d ppb", p.GetVocPpb())
	}
	if p.RadonBqM3 != nil {
		pv.RadonBqM3 = fmt.Sprintf("%d Bq/m³", p.GetRadonBqM3())
	}
	if p.PowerDrawW != nil {
		pv.PowerDrawW = fmt.Sprintf("%.1f W", p.GetPowerDrawW())
	}
	return pv
}

// deviceView is shown both embedded in a room and on the flat /devices list.
// RoomID/RoomLabel are populated by the caller when known (empty for a
// device with no room link).
type deviceView struct {
	ID   string
	Name string
	Kind string
	// Manufacturer/Model are the bridge-reported, read-only identity of the
	// hardware - shown alongside Name to tell apart devices whose configured
	// names are vague or duplicated (e.g. several network clients all named
	// after their chipset). Model is the marketing name if the bridge set
	// one, else its model ID.
	Manufacturer string
	Model        string
	Online       bool
	RoomID       string
	RoomName     string
}

func buildingToView(b *api2.Building) buildingView {
	return buildingView{
		ID:      b.GetId(),
		Name:    b.GetConfig().GetName(),
		TZ:      b.GetConfig().GetTz(),
		Version: b.GetVersion(),
		Lat:     b.GetConfig().GetLat(),
		Lon:     b.GetConfig().GetLon(),
	}
}

func floorToView(f *api2.Floor) floorView {
	return floorView{
		ID:         f.GetId(),
		Name:       f.GetName(),
		SortOrder:  f.GetSortOrder(),
		BuildingID: f.GetBuildingId(),
		Version:    f.GetVersion(),
	}
}

func roomToView(r *api2.Room) roomView {
	rv := roomView{
		ID:          r.GetId(),
		Name:        r.GetConfig().GetName(),
		FloorID:     r.GetFloorId(),
		BuildingID:  r.GetBuildingId(),
		Version:     r.GetVersion(),
		Type:        r.GetConfig().GetType(),
		Aggregation: aggregationToView(r.GetConfig().GetAggregation()),
		Properties:  propertiesToView(r.GetProperties()),
	}
	for _, d := range houseview.SortDevices(r.GetDevices()) {
		rv.Devices = append(rv.Devices, deviceToView(d))
	}
	return rv
}

func deviceToView(d *apiDevice.Device) deviceView {
	model := d.GetModelName()
	if model == "" {
		model = d.GetModelId()
	}
	return deviceView{
		ID:           d.GetId(),
		Name:         houseview.DisplayName(d),
		Kind:         houseview.Kind(d),
		Manufacturer: d.GetManufacturer(),
		Model:        model,
		Online:       d.GetAddress().GetIsReachable(),
	}
}

// roomLabel resolves roomID to a "Building · Floor · Room" display string,
// or "Unlinked" if roomID is empty. Returns ("", err) only on a real RPC
// failure - a roomID that no longer resolves (room deleted after the link
// was read) renders as "(deleted room)" rather than failing the whole page.
func (s *Server) roomLabel(ctx context.Context, roomID string) (string, error) {
	if roomID == "" {
		return "Unlinked", nil
	}

	room, err := s.house.GetRoom(ctx, &api2.GetRoomRequest{Id: roomID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return "(deleted room)", nil
		}
		return "", err
	}

	floor, err := s.house.GetFloor(ctx, &api2.GetFloorRequest{Id: room.GetFloorId()})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return fmt.Sprintf("(deleted floor) · %s", room.GetConfig().GetName()), nil
		}
		return "", err
	}

	building, err := s.house.GetBuilding(ctx, &api2.GetBuildingRequest{Id: room.GetBuildingId()})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return fmt.Sprintf("(deleted building) · %s · %s", floor.GetName(), room.GetConfig().GetName()), nil
		}
		return "", err
	}

	return fmt.Sprintf("%s · %s · %s", building.GetConfig().GetName(), floor.GetName(), room.GetConfig().GetName()), nil
}

// listBuildings/listFloors/listRoomsByFloor wrap HouseService's streaming
// List* RPCs into a plain slice - every caller in this app wants the whole
// list at once (admin-tool traffic, not a large enough result set to
// justify consuming the stream incrementally).
func (s *Server) listBuildings(ctx context.Context) ([]*api2.Building, error) {
	return houseview.StreamAll(func(cb func(*api2.Building) error) error {
		stream, err := s.house.ListBuildings(ctx, &api2.ListBuildingsRequest{})
		if err != nil {
			return err
		}
		return houseview.RecvAll(stream, cb)
	})
}

func (s *Server) listFloors(ctx context.Context, buildingID string) ([]*api2.Floor, error) {
	return houseview.StreamAll(func(cb func(*api2.Floor) error) error {
		stream, err := s.house.ListFloors(ctx, &api2.ListFloorsRequest{BuildingId: buildingID})
		if err != nil {
			return err
		}
		return houseview.RecvAll(stream, cb)
	})
}

func (s *Server) listRoomsByFloor(ctx context.Context, floorID string) ([]*api2.Room, error) {
	return houseview.StreamAll(func(cb func(*api2.Room) error) error {
		stream, err := s.house.ListRooms(ctx, &api2.ListRoomsRequest{FloorId: &floorID})
		if err != nil {
			return err
		}
		return houseview.RecvAll(stream, cb)
	})
}

// listDevices wraps the BridgeService facade's ListDevices - unlike
// HouseService's List* RPCs this one isn't server-streaming, so no recvAll
// needed. The result is sorted (see sortDevices).
func (s *Server) listDevices(ctx context.Context) ([]*apiDevice.Device, error) {
	resp, err := s.bridge.ListDevices(ctx, &api2.ListDevicesRequest{})
	if err != nil {
		return nil, err
	}
	return houseview.SortDevices(resp.GetDevices()), nil
}

// roomOption is one entry in the flat building/floor/room picker used by the
// /devices link-or-move flow (see admin-ui-implementation.md's "Known
// scaling gap" note - flat today, grouped later once it's actually painful).
type roomOption struct {
	ID    string
	Label string
}

// allRoomOptions enumerates every room across every building by walking
// ListBuildings -> ListFloors -> ListRooms, since HouseService.ListRooms
// requires a building_id or floor_id filter (no unscoped "all rooms" RPC).
func (s *Server) allRoomOptions(ctx context.Context) ([]roomOption, error) {
	buildings, err := s.listBuildings(ctx)
	if err != nil {
		return nil, err
	}

	var opts []roomOption
	for _, b := range buildings {
		floors, err := s.listFloors(ctx, b.GetId())
		if err != nil {
			return nil, err
		}

		for _, f := range floors {
			rooms, err := s.listRoomsByFloor(ctx, f.GetId())
			if err != nil {
				return nil, err
			}

			for _, r := range rooms {
				opts = append(opts, roomOption{
					ID:    r.GetId(),
					Label: fmt.Sprintf("%s · %s · %s", b.GetConfig().GetName(), f.GetName(), r.GetConfig().GetName()),
				})
			}
		}
	}
	return opts, nil
}

// deviceLink is one device's current room_id and link version - the version
// lets a Link/Move action round-trip it back as LinkDeviceRequest.version,
// the same optimistic concurrency contract Update*/Delete* already use.
type deviceLink struct {
	RoomID  string
	Version string
}

// deviceRoomMap returns every current device_id -> room link, unfiltered.
func (s *Server) deviceRoomMap(ctx context.Context) (map[string]deviceLink, error) {
	stream, err := s.house.ListDeviceLinks(ctx, &api2.ListDeviceLinksRequest{})
	if err != nil {
		return nil, err
	}

	out := map[string]deviceLink{}
	for {
		link, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out[link.GetDeviceId()] = deviceLink{RoomID: link.GetRoomId(), Version: link.GetVersion()}
	}
}
