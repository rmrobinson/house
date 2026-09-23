package house

import (
	"context"
	"errors"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/house/db"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Service implements api2.HouseServiceServer: building/floor/room topology
// and device-to-room linking, backed by db.Database. Device state itself
// lives with the BridgeService facade, not here - bridgeClient is used only
// to resolve full Device data for devices linked to a room (see
// resolveDevices), since the db only stores the device_id -> room_id link.
type Service struct {
	logger *zap.Logger

	db           *db.Database
	bridgeClient api2.BridgeServiceClient
}

func NewService(logger *zap.Logger, db *db.Database, bridgeClient api2.BridgeServiceClient) *Service {
	return &Service{
		logger:       logger,
		db:           db,
		bridgeClient: bridgeClient,
	}
}

// mapDBErr translates a db package sentinel error into the matching gRPC
// status, or codes.Internal for anything else. what names the resource for
// the message (e.g. "building", "floor").
func mapDBErr(err error, what string) error {
	switch {
	case errors.Is(err, db.ErrNotFound):
		return status.Errorf(codes.NotFound, "%s doesn't exist", what)
	case errors.Is(err, db.ErrVersionMismatch):
		return status.Errorf(codes.FailedPrecondition, "%s has changed since the supplied version was read", what)
	case errors.Is(err, db.ErrHasChildren):
		return status.Errorf(codes.FailedPrecondition, "%s has child records; delete them first", what)
	default:
		return status.Errorf(codes.Internal, "unable to process %s", what)
	}
}

/* ----- Building ----- */

func (s *Service) ListBuildings(req *api2.ListBuildingsRequest, stream api2.HouseService_ListBuildingsServer) error {
	buildings, err := s.db.GetBuildings(stream.Context())
	if err != nil {
		s.logger.Error("unable to get buildings", zap.Error(err))
		return status.Error(codes.Internal, "unable to get buildings")
	}

	for _, b := range buildings {
		if err := stream.Send(buildingDBToAPI(b)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) GetBuilding(ctx context.Context, req *api2.GetBuildingRequest) (*api2.Building, error) {
	building, err := s.db.GetBuilding(ctx, req.GetId())
	if err != nil {
		s.logger.Error("unable to get building", zap.String("building_id", req.GetId()), zap.Error(err))
		return nil, status.Error(codes.Internal, "unable to get building")
	} else if building == nil {
		return nil, status.Error(codes.NotFound, "building doesn't exist")
	}

	return buildingDBToAPI(*building), nil
}

func (s *Service) CreateBuilding(ctx context.Context, req *api2.CreateBuildingRequest) (*api2.Building, error) {
	b := &db.Building{
		Name: req.GetConfig().GetName(),
		TZ:   req.GetConfig().GetTz(),
		Location: db.Location{
			Latitude:  req.GetConfig().GetLat(),
			Longitude: req.GetConfig().GetLon(),
		},
	}

	res, err := s.db.CreateBuilding(ctx, b)
	if err != nil {
		s.logger.Error("unable to create building", zap.Error(err))
		return nil, status.Error(codes.Internal, "unable to create building")
	}

	return buildingDBToAPI(*res), nil
}

func (s *Service) UpdateBuilding(ctx context.Context, req *api2.UpdateBuildingRequest) (*api2.Building, error) {
	b := &db.Building{
		ID:      req.GetId(),
		Version: req.GetVersion(),
		Name:    req.GetConfig().GetName(),
		TZ:      req.GetConfig().GetTz(),
		Location: db.Location{
			Latitude:  req.GetConfig().GetLat(),
			Longitude: req.GetConfig().GetLon(),
		},
	}

	res, err := s.db.UpdateBuilding(ctx, b)
	if err != nil {
		s.logger.Error("unable to update building", zap.String("building_id", req.GetId()), zap.Error(err))
		return nil, mapDBErr(err, "building")
	}

	return buildingDBToAPI(*res), nil
}

func (s *Service) DeleteBuilding(ctx context.Context, req *api2.DeleteBuildingRequest) (*emptypb.Empty, error) {
	if err := s.db.DeleteBuilding(ctx, req.GetId(), req.GetVersion()); err != nil {
		s.logger.Error("unable to delete building", zap.String("building_id", req.GetId()), zap.Error(err))
		return nil, mapDBErr(err, "building")
	}
	return &emptypb.Empty{}, nil
}

/* ----- Floor ----- */

func (s *Service) ListFloors(req *api2.ListFloorsRequest, stream api2.HouseService_ListFloorsServer) error {
	floors, err := s.db.ListFloors(stream.Context(), req.GetBuildingId())
	if err != nil {
		s.logger.Error("unable to list floors", zap.String("building_id", req.GetBuildingId()), zap.Error(err))
		return status.Error(codes.Internal, "unable to list floors")
	}

	for _, f := range floors {
		if err := stream.Send(floorDBToAPI(f)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) GetFloor(ctx context.Context, req *api2.GetFloorRequest) (*api2.Floor, error) {
	f, err := s.db.GetFloor(ctx, req.GetId())
	if err != nil {
		s.logger.Error("unable to get floor", zap.String("floor_id", req.GetId()), zap.Error(err))
		return nil, status.Error(codes.Internal, "unable to get floor")
	} else if f == nil {
		return nil, status.Error(codes.NotFound, "floor doesn't exist")
	}

	return floorDBToAPI(*f), nil
}

func (s *Service) CreateFloor(ctx context.Context, req *api2.CreateFloorRequest) (*api2.Floor, error) {
	f := &db.Floor{
		BuildingID: req.GetBuildingId(),
		Name:       req.GetName(),
		SortOrder:  req.GetSortOrder(),
	}

	res, err := s.db.CreateFloor(ctx, f)
	if err != nil {
		s.logger.Error("unable to create floor", zap.String("building_id", req.GetBuildingId()), zap.Error(err))
		return nil, mapDBErr(err, "building")
	}

	return floorDBToAPI(*res), nil
}

func (s *Service) UpdateFloor(ctx context.Context, req *api2.UpdateFloorRequest) (*api2.Floor, error) {
	f := &db.Floor{
		ID:        req.GetId(),
		Version:   req.GetVersion(),
		Name:      req.GetName(),
		SortOrder: req.GetSortOrder(),
	}

	res, err := s.db.UpdateFloor(ctx, f)
	if err != nil {
		s.logger.Error("unable to update floor", zap.String("floor_id", req.GetId()), zap.Error(err))
		return nil, mapDBErr(err, "floor")
	}

	return floorDBToAPI(*res), nil
}

func (s *Service) DeleteFloor(ctx context.Context, req *api2.DeleteFloorRequest) (*emptypb.Empty, error) {
	if err := s.db.DeleteFloor(ctx, req.GetId(), req.GetVersion()); err != nil {
		s.logger.Error("unable to delete floor", zap.String("floor_id", req.GetId()), zap.Error(err))
		return nil, mapDBErr(err, "floor")
	}
	return &emptypb.Empty{}, nil
}

/* ----- Room ----- */

func (s *Service) ListRooms(req *api2.ListRoomsRequest, stream api2.HouseService_ListRoomsServer) error {
	if req.BuildingId == nil && req.FloorId == nil {
		return status.Error(codes.InvalidArgument, "building_id or floor_id must be set")
	}

	ctx := stream.Context()
	rooms, err := s.db.ListRooms(ctx, req.BuildingId, req.FloorId)
	if err != nil {
		s.logger.Error("unable to list rooms", zap.Error(err))
		return status.Error(codes.Internal, "unable to list rooms")
	}

	// Resolve every room's device links and every device's full state in one
	// query and one bridge-facade call respectively, rather than one of each
	// per room - see roomDBToAPI/resolveDevices.
	roomIDs := make([]string, len(rooms))
	for i, r := range rooms {
		roomIDs[i] = r.ID
	}
	links, err := s.db.ListDeviceLinksForRooms(ctx, roomIDs)
	if err != nil {
		s.logger.Error("unable to list room device links", zap.Error(err))
		return status.Error(codes.Internal, "unable to list room devices")
	}
	linksByRoom := make(map[string][]db.Device, len(rooms))
	for _, l := range links {
		linksByRoom[l.RoomID] = append(linksByRoom[l.RoomID], l)
	}
	devices := s.resolveDevices(ctx)

	for _, r := range rooms {
		if err := stream.Send(roomDBToAPI(r, linksByRoom[r.ID], devices)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) GetRoom(ctx context.Context, req *api2.GetRoomRequest) (*api2.Room, error) {
	room, err := s.db.GetRoom(ctx, req.GetId())
	if err != nil {
		s.logger.Error("unable to get room", zap.String("room_id", req.GetId()), zap.Error(err))
		return nil, status.Error(codes.Internal, "unable to get room")
	} else if room == nil {
		return nil, status.Error(codes.NotFound, "room doesn't exist")
	}

	return s.roomToAPI(ctx, *room)
}

func (s *Service) CreateRoom(ctx context.Context, req *api2.CreateRoomRequest) (*api2.Room, error) {
	room := &db.Room{
		FloorID: req.GetFloorId(),
		Name:    req.GetConfig().GetName(),
		Type:    db.RoomType(req.GetConfig().GetType()),
	}

	res, err := s.db.CreateRoom(ctx, room)
	if err != nil {
		s.logger.Error("unable to create room", zap.String("floor_id", req.GetFloorId()), zap.Error(err))
		return nil, mapDBErr(err, "floor")
	}

	return s.roomToAPI(ctx, *res)
}

func (s *Service) UpdateRoom(ctx context.Context, req *api2.UpdateRoomRequest) (*api2.Room, error) {
	room := &db.Room{
		ID:      req.GetId(),
		Version: req.GetVersion(),
		Name:    req.GetConfig().GetName(),
		Type:    db.RoomType(req.GetConfig().GetType()),
	}

	res, err := s.db.UpdateRoom(ctx, room)
	if err != nil {
		s.logger.Error("unable to update room", zap.String("room_id", req.GetId()), zap.Error(err))
		return nil, mapDBErr(err, "room")
	}

	return s.roomToAPI(ctx, *res)
}

func (s *Service) DeleteRoom(ctx context.Context, req *api2.DeleteRoomRequest) (*emptypb.Empty, error) {
	if err := s.db.DeleteRoom(ctx, req.GetId(), req.GetVersion()); err != nil {
		s.logger.Error("unable to delete room", zap.String("room_id", req.GetId()), zap.Error(err))
		return nil, mapDBErr(err, "room")
	}
	return &emptypb.Empty{}, nil
}

/* ----- Device <-> Room linking ----- */

func (s *Service) LinkDevice(ctx context.Context, req *api2.LinkDeviceRequest) (*api2.LinkDeviceResponse, error) {
	room, err := s.db.GetRoom(ctx, req.GetRoomId())
	if err != nil {
		s.logger.Error("unable to get room", zap.String("room_id", req.GetRoomId()), zap.Error(err))
		return nil, status.Error(codes.Internal, "unable to get room")
	} else if room == nil {
		return nil, status.Error(codes.NotFound, "room doesn't exist")
	}

	link, previousRoomID, err := s.db.LinkDevice(ctx, req.GetDeviceId(), req.GetRoomId(), req.GetVersion())
	if err != nil {
		s.logger.Error("unable to link device", zap.String("device_id", req.GetDeviceId()), zap.String("room_id", req.GetRoomId()), zap.Error(err))
		return nil, mapDBErr(err, "device link")
	}

	return &api2.LinkDeviceResponse{
		Link: &api2.DeviceRoomLink{
			DeviceId: link.ID,
			RoomId:   link.RoomID,
			Version:  link.Version,
		},
		PreviousRoomId: previousRoomID,
	}, nil
}

func (s *Service) UnlinkDevice(ctx context.Context, req *api2.UnlinkDeviceRequest) (*emptypb.Empty, error) {
	if err := s.db.UnlinkDevice(ctx, req.GetDeviceId()); err != nil {
		s.logger.Error("unable to unlink device", zap.String("device_id", req.GetDeviceId()), zap.Error(err))
		return nil, status.Error(codes.Internal, "unable to unlink device")
	}
	return &emptypb.Empty{}, nil
}

func (s *Service) ListDeviceLinks(req *api2.ListDeviceLinksRequest, stream api2.HouseService_ListDeviceLinksServer) error {
	links, err := s.db.ListDeviceLinks(stream.Context(), req.BuildingId, req.RoomId, req.DeviceId)
	if err != nil {
		s.logger.Error("unable to list device links", zap.Error(err))
		return status.Error(codes.Internal, "unable to list device links")
	}

	for _, l := range links {
		if err := stream.Send(&api2.DeviceRoomLink{DeviceId: l.ID, RoomId: l.RoomID, Version: l.Version}); err != nil {
			return err
		}
	}
	return nil
}

/* ----- db <-> API conversions ----- */

func buildingDBToAPI(b db.Building) *api2.Building {
	return &api2.Building{
		Id:      b.ID,
		Version: b.Version,
		Config: &api2.Building_Config{
			Name: b.Name,
			Tz:   b.TZ,
			Lat:  b.Location.Latitude,
			Lon:  b.Location.Longitude,
		},
	}
}

func floorDBToAPI(f db.Floor) *api2.Floor {
	return &api2.Floor{
		Id:         f.ID,
		Name:       f.Name,
		SortOrder:  f.SortOrder,
		BuildingId: f.BuildingID,
		Version:    f.Version,
	}
}

// roomToAPI resolves room's linked devices through the BridgeService facade
// so Room.devices carries real device state, not just IDs - a single-room
// wrapper around roomDBToAPI for callers (GetRoom, CreateRoom, UpdateRoom)
// that only need one room; ListRooms resolves links and devices in bulk
// itself instead, to avoid paying one query and one facade call per room.
func (s *Service) roomToAPI(ctx context.Context, room db.Room) (*api2.Room, error) {
	links, err := s.db.ListDeviceLinks(ctx, nil, &room.ID, nil)
	if err != nil {
		s.logger.Error("unable to list room device links", zap.String("room_id", room.ID), zap.Error(err))
		return nil, status.Error(codes.Internal, "unable to list room devices")
	}

	return roomDBToAPI(room, links, s.resolveDevices(ctx)), nil
}

// roomDBToAPI converts room to its API representation, embedding full Device
// state for each of links - the devices themselves are only ever stored via
// the BridgeService facade, never duplicated into this service's own DB.
// devices is keyed by device ID (see resolveDevices); a link whose device
// isn't present there (bridge unreachable, device since removed, or no
// bridge client configured) is still included as a minimal ID-only stub
// rather than silently dropped - the link itself is still real.
func roomDBToAPI(room db.Room, links []db.Device, devices map[string]*apiDevice.Device) *api2.Room {
	ret := &api2.Room{
		Id:         room.ID,
		BuildingId: room.BuildingID,
		FloorId:    room.FloorID,
		Version:    room.Version,
		Config: &api2.Room_Config{
			Name: room.Name,
			Type: int32(room.Type),
		},
	}

	for _, link := range links {
		if d, ok := devices[link.ID]; ok {
			ret.Devices = append(ret.Devices, d)
		} else {
			ret.Devices = append(ret.Devices, &apiDevice.Device{Id: link.ID})
		}
	}

	return ret
}

// resolveDevices fetches full Device state for every device known to the
// bridge facade in a single call, keyed by device ID, so resolving a room's
// (or many rooms') linked devices costs one round-trip regardless of how
// many devices are linked - see roomDBToAPI. Returns nil if no bridge client
// is configured or the facade couldn't be reached; callers should treat a
// missing entry as "couldn't be resolved" and fall back to an ID-only stub,
// not as an error, since the underlying link is still real.
func (s *Service) resolveDevices(ctx context.Context) map[string]*apiDevice.Device {
	if s.bridgeClient == nil {
		return nil
	}

	resp, err := s.bridgeClient.ListDevices(ctx, &api2.ListDevicesRequest{})
	if err != nil {
		s.logger.Warn("unable to resolve linked devices via bridge facade", zap.Error(err))
		return nil
	}

	out := make(map[string]*apiDevice.Device, len(resp.GetDevices()))
	for _, d := range resp.GetDevices() {
		out[d.GetId()] = d
	}
	return out
}
