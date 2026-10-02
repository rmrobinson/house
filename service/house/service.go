package house

import (
	"context"
	"errors"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
	"github.com/rmrobinson/house/service/house/db"
	"github.com/rmrobinson/house/service/lib/bridgeconn"
	"github.com/rmrobinson/house/service/lib/grpcutil"
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
	agg          *aggregator
}

// NewService constructs a Service, seeding its room-aggregation engine (see
// aggregation.go) from db and, if bridgeAddr is non-empty, starting a
// long-lived, reconnecting subscription to that endpoint's StreamUpdates
// (via bridgeconn.Conn) to keep it current for the lifetime of ctx.
// bridgeAddr/bridgeTLS should describe the same endpoint bridgeClient
// itself dials - an empty bridgeAddr leaves the aggregation engine seeded
// from the db but never updated live, the same "no bridge facade
// configured" degradation bridgeClient == nil already causes elsewhere in
// Service.
func NewService(ctx context.Context, logger *zap.Logger, db *db.Database, bridgeClient api2.BridgeServiceClient, bridgeAddr string, bridgeTLS *grpcutil.ClientTLSConfig) (*Service, error) {
	agg := newAggregator(logger)
	if err := agg.load(ctx, db); err != nil {
		return nil, err
	}

	if len(bridgeAddr) > 0 {
		conn := bridgeconn.New(logger, bridgeAddr, bridgeTLS)
		go conn.Run(ctx, agg.handleUpdate, nil)
	}

	return &Service{
		logger:       logger,
		db:           db,
		bridgeClient: bridgeClient,
		agg:          agg,
	}, nil
}

// defaultAvailableModes seeds Building.Config.available_modes for a
// CreateBuilding call that doesn't specify any - a sensible, overridable
// starting point rather than leaving a fresh building with no mode settable
// at all until explicitly reconfigured.
var defaultAvailableModes = []string{"home", "away", "vacation"}

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
	case errors.Is(err, db.ErrInvalidMode):
		return status.Error(codes.InvalidArgument, err.Error())
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
		if err := stream.Send(s.buildingToAPI(b)); err != nil {
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

	return s.buildingToAPI(*building), nil
}

func (s *Service) CreateBuilding(ctx context.Context, req *api2.CreateBuildingRequest) (*api2.Building, error) {
	availableModes := req.GetConfig().GetAvailableModes()
	if len(availableModes) == 0 {
		availableModes = defaultAvailableModes
	}

	b := &db.Building{
		Name: req.GetConfig().GetName(),
		TZ:   req.GetConfig().GetTz(),
		Location: db.Location{
			Latitude:  req.GetConfig().GetLat(),
			Longitude: req.GetConfig().GetLon(),
		},
		AvailableModes: availableModes,
	}

	res, err := s.db.CreateBuilding(ctx, b)
	if err != nil {
		s.logger.Error("unable to create building", zap.Error(err))
		return nil, status.Error(codes.Internal, "unable to create building")
	}

	return s.buildingToAPI(*res), nil
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
		AvailableModes: req.GetConfig().GetAvailableModes(),
	}

	res, err := s.db.UpdateBuilding(ctx, b)
	if err != nil {
		s.logger.Error("unable to update building", zap.String("building_id", req.GetId()), zap.Error(err))
		return nil, mapDBErr(err, "building")
	}
	// Shrinking available_modes can leave db.UpdateBuilding clearing the
	// building's mode as a side effect (see its doc comment) - keep the
	// aggregator's cached mode, which only otherwise changes via
	// SetHouseMode, from going stale against that.
	s.agg.setBuildingMode(res.ID, res.Mode)

	return s.buildingToAPI(*res), nil
}

func (s *Service) DeleteBuilding(ctx context.Context, req *api2.DeleteBuildingRequest) (*emptypb.Empty, error) {
	if err := s.db.DeleteBuilding(ctx, req.GetId(), req.GetVersion()); err != nil {
		s.logger.Error("unable to delete building", zap.String("building_id", req.GetId()), zap.Error(err))
		return nil, mapDBErr(err, "building")
	}
	s.agg.removeBuilding(req.GetId())
	return &emptypb.Empty{}, nil
}

// SetHouseMode sets a building's current mode, validating it against that
// building's Config.available_modes (an empty mode always clears it,
// regardless of available_modes - see SetHouseModeRequest's doc comment).
// The validity check and the write happen together inside
// db.SetBuildingMode's transaction, so a concurrent UpdateBuilding changing
// available_modes can't race this into writing a since-invalidated mode.
func (s *Service) SetHouseMode(ctx context.Context, req *api2.SetHouseModeRequest) (*api2.Building, error) {
	res, err := s.db.SetBuildingMode(ctx, req.GetBuildingId(), req.GetMode())
	if err != nil {
		s.logger.Error("unable to set building mode", zap.String("building_id", req.GetBuildingId()), zap.Error(err))
		return nil, mapDBErr(err, "building")
	}
	s.agg.setBuildingMode(req.GetBuildingId(), res.Mode)

	return s.buildingToAPI(*res), nil
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
		if err := stream.Send(roomDBToAPI(r, linksByRoom[r.ID], devices, s.agg.getProperties(r.ID))); err != nil {
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
		FloorID:     req.GetFloorId(),
		Name:        req.GetConfig().GetName(),
		Type:        db.RoomType(req.GetConfig().GetType()),
		Aggregation: apiAggregationToDB(req.GetConfig().GetAggregation()),
	}

	res, err := s.db.CreateRoom(ctx, room)
	if err != nil {
		s.logger.Error("unable to create room", zap.String("floor_id", req.GetFloorId()), zap.Error(err))
		return nil, mapDBErr(err, "floor")
	}
	s.agg.registerRoom(res.ID, res.BuildingID)
	s.agg.setRoomAggregation(res.ID, dbAggregationToAPI(res.Aggregation))

	return s.roomToAPI(ctx, *res)
}

func (s *Service) UpdateRoom(ctx context.Context, req *api2.UpdateRoomRequest) (*api2.Room, error) {
	room := &db.Room{
		ID:          req.GetId(),
		Version:     req.GetVersion(),
		Name:        req.GetConfig().GetName(),
		Type:        db.RoomType(req.GetConfig().GetType()),
		Aggregation: apiAggregationToDB(req.GetConfig().GetAggregation()),
	}

	res, err := s.db.UpdateRoom(ctx, room)
	if err != nil {
		s.logger.Error("unable to update room", zap.String("room_id", req.GetId()), zap.Error(err))
		return nil, mapDBErr(err, "room")
	}
	s.agg.setRoomAggregation(res.ID, dbAggregationToAPI(res.Aggregation))

	return s.roomToAPI(ctx, *res)
}

func (s *Service) DeleteRoom(ctx context.Context, req *api2.DeleteRoomRequest) (*emptypb.Empty, error) {
	if err := s.db.DeleteRoom(ctx, req.GetId(), req.GetVersion()); err != nil {
		s.logger.Error("unable to delete room", zap.String("room_id", req.GetId()), zap.Error(err))
		return nil, mapDBErr(err, "room")
	}
	s.agg.removeRoom(req.GetId())
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
	s.agg.setDeviceRoom(req.GetDeviceId(), req.GetRoomId())

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
	s.agg.removeDeviceRoom(req.GetDeviceId())
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

/* ----- House-level update stream ----- */

// houseUpdateBuildingID returns the building_id a HouseUpdate is scoped to,
// whichever branch of its oneof is set: a RoomUpdate via
// aggregator.buildingOf, a BuildingUpdate directly from its own field.
func (s *Service) houseUpdateBuildingID(u *api2.HouseUpdate) string {
	if room := u.GetRoom(); room != nil {
		return s.agg.buildingOf(room.GetRoomId())
	}
	return u.GetBuilding().GetBuildingId()
}

// StreamHouseUpdates reports every change to a computed Room.Properties for
// a room in req.BuildingId, and every change to the building's own
// Building.State, starting with one RoomUpdate per room that already has a
// known value (see aggregator.propertiesForBuilding) and one BuildingUpdate
// for the building's current State, then live updates as they happen -
// mirroring the INITIAL-snapshot-then-live shape of BridgeService.
// StreamUpdates (service/bridge/facade.Facade.StreamUpdates), just scoped to
// one building and to server-computed state instead of raw device state.
func (s *Service) StreamHouseUpdates(req *api2.StreamHouseUpdatesRequest, stream api2.HouseService_StreamHouseUpdatesServer) error {
	buildingID := req.GetBuildingId()
	if len(buildingID) < 1 {
		return status.Error(codes.InvalidArgument, "building_id must be set")
	}

	// Subscribe before snapshotting so no update landing between the
	// snapshot and the subscribe is missed; a client may see a harmless
	// duplicate in that window instead - the same tradeoff facade.
	// StreamUpdates makes for the same reason.
	//
	// aggregator.updates is one house-wide Source shared by every building,
	// so filter to buildingID here, at NewFilteredSink time - before a
	// message ever reaches this sink's fixed-size buffer - rather than
	// after reading it back off the sink. Filtering post-buffer would let a
	// burst of updates for other buildings fill this client's buffer and
	// crowd out updates for the one building it actually asked for.
	sink := s.agg.updates.NewFilteredSink(func(msg proto.Message) bool {
		update, ok := msg.(*api2.HouseUpdate)
		if !ok {
			// Let bridge.Pump's own type-assert guard produce the real error.
			return true
		}
		return s.houseUpdateBuildingID(update) == buildingID
	})
	defer sink.Close()

	building, err := s.db.GetBuilding(stream.Context(), buildingID)
	if err != nil {
		s.logger.Error("unable to get building", zap.String("building_id", buildingID), zap.Error(err))
		return status.Error(codes.Internal, "unable to get building")
	} else if building == nil {
		return status.Error(codes.NotFound, "building doesn't exist")
	}
	buildingUpdate := &api2.HouseUpdate{Update: &api2.HouseUpdate_Building{Building: &api2.BuildingUpdate{
		BuildingId: buildingID,
		State:      s.buildingToAPI(*building).GetState(),
	}}}
	if err := stream.Send(buildingUpdate); err != nil {
		return err
	}

	for roomID, props := range s.agg.propertiesForBuilding(buildingID) {
		roomUpdate := &api2.HouseUpdate{Update: &api2.HouseUpdate_Room{Room: &api2.RoomUpdate{RoomId: roomID, Properties: props}}}
		if err := stream.Send(roomUpdate); err != nil {
			return err
		}
	}

	// bridge.Pump relays sink to stream.Send until the client disconnects
	// (nil) or this sink falls behind and is disconnected
	// (bridge.ErrStreamFellBehind) - the client should reconnect for a
	// fresh initial snapshot, same as BridgeService.StreamUpdates
	// (facade.go, api.go).
	return bridge.Pump(stream.Context(), sink, func(update *api2.HouseUpdate) error {
		if err := stream.Send(update); err != nil {
			s.logger.Error("unable to send house update", zap.Error(err))
			return err
		}
		return nil
	})
}

/* ----- db <-> API conversions ----- */

// buildingToAPI converts b to its API representation, including its
// server-computed State.occupied (see aggregator.buildingOccupied) -
// callers that already have the db.Building in hand (ListBuildings,
// GetBuilding, CreateBuilding, UpdateBuilding, SetHouseMode) all go through
// this rather than buildingDBToAPI directly, so none of them forget to
// populate State.
func (s *Service) buildingToAPI(b db.Building) *api2.Building {
	return buildingDBToAPI(b, s.agg.buildingOccupied(b.ID))
}

// buildingDBToAPI converts b to its API representation. occupied is the
// caller's aggregator.buildingOccupied(b.ID) result - nil if no room in the
// building has ever reported any occupancy signal.
func buildingDBToAPI(b db.Building, occupied *bool) *api2.Building {
	return &api2.Building{
		Id:      b.ID,
		Version: b.Version,
		Config: &api2.Building_Config{
			Name:           b.Name,
			Tz:             b.TZ,
			Lat:            b.Location.Latitude,
			Lon:            b.Location.Longitude,
			AvailableModes: b.AvailableModes,
		},
		State: &api2.Building_State{
			Occupied: occupied,
			Mode:     b.Mode,
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

	return roomDBToAPI(room, links, s.resolveLinkedDevices(ctx, links), s.agg.getProperties(room.ID)), nil
}

// resolveLinkedDevices fetches full Device state for exactly links via
// individual GetDevice calls, keyed by device ID - the single-room
// counterpart to resolveDevices, which fetches the facade's entire device
// inventory in one call to amortize across every room in a ListRooms
// response. A single room only ever has a handful of linked devices, so a
// handful of GetDevice calls (served from the facade's in-memory cache) is
// cheaper than pulling every device in the house just to resolve them.
// Returns nil if no bridge client is configured; a link whose GetDevice
// call fails is simply left out of the result, same as resolveDevices'
// contract - callers fall back to an ID-only stub (see roomDBToAPI).
func (s *Service) resolveLinkedDevices(ctx context.Context, links []db.Device) map[string]*apiDevice.Device {
	if s.bridgeClient == nil {
		return nil
	}

	out := make(map[string]*apiDevice.Device, len(links))
	for _, link := range links {
		d, err := s.bridgeClient.GetDevice(ctx, &api2.GetDeviceRequest{Id: link.ID})
		if err != nil {
			continue
		}
		out[d.GetId()] = d
	}
	return out
}

// roomDBToAPI converts room to its API representation, embedding full Device
// state for each of links - the devices themselves are only ever stored via
// the BridgeService facade, never duplicated into this service's own DB.
// devices is keyed by device ID (see resolveDevices); a link whose device
// isn't present there (bridge unreachable, device since removed, or no
// bridge client configured) is still included as a minimal ID-only stub
// rather than silently dropped - the link itself is still real. properties
// is the caller's aggregator.getProperties(room.ID) result - nil if no
// linked Sensor device has reported yet.
func roomDBToAPI(room db.Room, links []db.Device, devices map[string]*apiDevice.Device, properties *api2.Room_Properties) *api2.Room {
	ret := &api2.Room{
		Id:         room.ID,
		BuildingId: room.BuildingID,
		FloorId:    room.FloorID,
		Version:    room.Version,
		Config: &api2.Room_Config{
			Name:        room.Name,
			Type:        int32(room.Type),
			Aggregation: dbAggregationToAPI(room.Aggregation),
		},
		Properties: properties,
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
