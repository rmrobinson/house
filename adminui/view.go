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
	Devices    []deviceView
}

// deviceView is shown both embedded in a room and on the flat /devices list.
// RoomID/RoomLabel are populated by the caller when known (empty for a
// device with no room link).
type deviceView struct {
	ID       string
	Name     string
	Kind     string
	Online   bool
	RoomID   string
	RoomName string
}

func buildingToView(b *api2.Building) buildingView {
	return buildingView{
		ID:      b.GetId(),
		Name:    b.GetConfig().GetName(),
		TZ:      b.GetConfig().GetTz(),
		Version: b.GetVersion(),
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
		ID:         r.GetId(),
		Name:       r.GetConfig().GetName(),
		FloorID:    r.GetFloorId(),
		BuildingID: r.GetBuildingId(),
		Version:    r.GetVersion(),
	}
	for _, d := range r.GetDevices() {
		rv.Devices = append(rv.Devices, deviceToView(d))
	}
	return rv
}

func deviceToView(d *apiDevice.Device) deviceView {
	return deviceView{
		ID:     d.GetId(),
		Name:   deviceDisplayName(d),
		Kind:   deviceKind(d),
		Online: d.GetAddress().GetIsReachable(),
	}
}

// deviceDisplayName falls back to d's ID when Config.Name is unset - a
// device with no configured name would otherwise render as a blank label
// throughout this app (device tables, link/move confirmations).
func deviceDisplayName(d *apiDevice.Device) string {
	if name := d.GetConfig().GetName(); name != "" {
		return name
	}
	return d.GetId()
}

// deviceKind returns a short label for whichever "details" oneof case is
// set, for display only - mirrors the type switch shape used for dispatch in
// service/bridge/api.go's deviceSupportsCommand.
func deviceKind(d *apiDevice.Device) string {
	switch {
	case d.GetAvReceiver() != nil:
		return "AV Receiver"
	case d.GetClock() != nil:
		return "Clock"
	case d.GetLight() != nil:
		return "Light"
	case d.GetSensor() != nil:
		return "Sensor"
	case d.GetThermostat() != nil:
		return "Thermostat"
	case d.GetUps() != nil:
		return "UPS"
	case d.GetEvCharger() != nil:
		return "EV Charger"
	case d.GetMediaPlayer() != nil:
		return "Media Player"
	case d.GetTelevision() != nil:
		return "Television"
	case d.GetConnectedDevice() != nil:
		return "Connected Device"
	case d.GetCamera() != nil:
		return "Camera"
	case d.GetFan() != nil:
		return "Fan"
	case d.GetStandingDesk() != nil:
		return "Standing Desk"
	default:
		return "Generic"
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
	return streamAll(func(cb func(*api2.Building) error) error {
		stream, err := s.house.ListBuildings(ctx, &api2.ListBuildingsRequest{})
		if err != nil {
			return err
		}
		return recvAll(stream, cb)
	})
}

func (s *Server) listFloors(ctx context.Context, buildingID string) ([]*api2.Floor, error) {
	return streamAll(func(cb func(*api2.Floor) error) error {
		stream, err := s.house.ListFloors(ctx, &api2.ListFloorsRequest{BuildingId: buildingID})
		if err != nil {
			return err
		}
		return recvAll(stream, cb)
	})
}

func (s *Server) listRoomsByFloor(ctx context.Context, floorID string) ([]*api2.Room, error) {
	return streamAll(func(cb func(*api2.Room) error) error {
		stream, err := s.house.ListRooms(ctx, &api2.ListRoomsRequest{FloorId: &floorID})
		if err != nil {
			return err
		}
		return recvAll(stream, cb)
	})
}

// listDevices wraps the BridgeService facade's ListDevices - unlike
// HouseService's List* RPCs this one isn't server-streaming, so no recvAll
// needed.
func (s *Server) listDevices(ctx context.Context) ([]*apiDevice.Device, error) {
	resp, err := s.bridge.ListDevices(ctx, &api2.ListDevicesRequest{})
	if err != nil {
		return nil, err
	}
	return resp.GetDevices(), nil
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

// deviceRoomMap returns every current device_id -> room_id link, unfiltered.
func (s *Server) deviceRoomMap(ctx context.Context) (map[string]string, error) {
	stream, err := s.house.ListDeviceLinks(ctx, &api2.ListDeviceLinksRequest{})
	if err != nil {
		return nil, err
	}

	out := map[string]string{}
	for {
		link, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out[link.GetDeviceId()] = link.GetRoomId()
	}
}

// grpcRecvStream is the shape every HouseService List* streaming client
// shares - satisfied by api2.HouseService_ListBuildingsClient,
// _ListFloorsClient and _ListRoomsClient without needing generics over the
// generated types themselves.
type grpcRecvStream[T any] interface {
	Recv() (T, error)
}

func recvAll[T any](stream grpcRecvStream[T], cb func(T) error) error {
	for {
		item, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := cb(item); err != nil {
			return err
		}
	}
}

// streamAll collects every item a List* RPC streams back, via open (which
// starts the stream and forwards each item to cb).
func streamAll[T any](open func(cb func(T) error) error) ([]T, error) {
	var items []T
	err := open(func(item T) error {
		items = append(items, item)
		return nil
	})
	return items, err
}
