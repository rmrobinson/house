package house

import (
	"context"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/house/db"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

var ErrNotImplemented = status.Error(codes.Unimplemented, "method not implemented")

type Service struct {
	logger *zap.Logger

	db *db.Database
}

func NewService(logger *zap.Logger, db *db.Database) *Service {
	return &Service{
		logger: logger,
		db:     db,
	}
}

func (s *Service) ListBuildings(ctx context.Context, req *api2.ListBuildingsRequest) (*api2.ListBuildingsResponse, error) {
	buildings, err := s.db.GetBuildings(ctx)
	if err != nil {
		s.logger.Error("unable to get buildings", zap.Error(err))
		return nil, status.Error(codes.Internal, "unable to get buildings")
	}

	ret := &api2.ListBuildingsResponse{}
	for _, building := range buildings {
		ret.Buildings = append(ret.Buildings, &api2.Building{
			Id: building.ID,
			Config: &api2.Building_Config{
				Name: building.Name,
			},
		})
	}
	return ret, nil
}

func (s *Service) GetBuilding(ctx context.Context, req *api2.GetBuildingRequest) (*api2.Building, error) {
	building, err := s.db.GetBuilding(ctx, req.Id)
	if err != nil {
		s.logger.Error("unable to get building", zap.String("building_id", req.Id), zap.Error(err))
		return nil, status.Error(codes.Internal, "unable to get building")
	} else if building == nil {
		return nil, status.Error(codes.NotFound, "building doesn't exist")
	}

	rooms, err := s.db.GetBuildingRooms(ctx, req.Id)
	if err != nil {
		s.logger.Error("unable to get building rooms", zap.String("building_id", req.Id), zap.Error(err))
	}

	ret := &api2.Building{
		Id: building.ID,
		Config: &api2.Building_Config{
			Name: building.Name,
		},
	}

	for _, room := range rooms {
		ret.Rooms = append(ret.Rooms, roomDBToAPI(room))
	}
	return ret, nil
}

func (s *Service) LinkDevice(ctx context.Context, req *api2.LinkDeviceRequest) (*api2.Room, error) {
	room, err := s.db.GetRoom(ctx, req.RoomId)
	if err != nil {
		s.logger.Error("unable to get room", zap.String("room_id", req.RoomId), zap.Error(err))
		return nil, status.Error(codes.Internal, "unable to get room")
	} else if room == nil {
		return nil, status.Error(codes.NotFound, "room doesn't exist")
	}

	device, err := s.db.CreateDevice(ctx, req.DeviceId, *room)
	if err != nil {
		s.logger.Error("unable to link device to room", zap.String("device_id", req.DeviceId), zap.String("room_id", req.RoomId), zap.Error(err))
		return nil, status.Error(codes.Internal, "unable to link device")
	}

	room.Devices = append(room.Devices, *device)
	return roomDBToAPI(*room), nil
}

func (s *Service) UnlinkDevice(ctx context.Context, req *api2.UnlinkDeviceRequest) (*emptypb.Empty, error) {
	err := s.db.DeleteDevice(ctx, req.DeviceId)
	if err != nil {
		s.logger.Error("unable to unlink device", zap.String("device_id", req.DeviceId), zap.Error(err))
		return nil, status.Error(codes.Internal, "unable to unlink device")
	}

	return &emptypb.Empty{}, nil
}

func roomDBToAPI(room db.Room) *api2.Room {
	ret := &api2.Room{
		Id: room.ID,
		Config: &api2.Room_Config{
			Name: room.Name,
			Type: int32(room.Type),
		},
	}

	for _, device := range room.Devices {
		ret.Devices = append(ret.Devices, deviceToAPI(device))
	}

	return ret
}

func deviceToAPI(device db.Device) *apiDevice.Device {
	return &apiDevice.Device{
		Id: device.ID,
	}
}
