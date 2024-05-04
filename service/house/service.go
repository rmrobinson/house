package house

import (
	"context"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/house/db"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	}

	if building == nil {
		return &api2.Building{}, nil
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
		ret.Rooms = append(ret.Rooms, &api2.Room{
			Id: room.ID,
			Config: &api2.Room_Config{
				Name: room.Name,
				Type: int32(room.Type),
			},
		})
	}
	return ret, nil
}
