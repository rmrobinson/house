package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rmrobinson/house/service/house/db"
	"go.uber.org/zap"
)

var (
	dbPath = flag.String("db", "", "Path to the database to use")
	action = flag.String("action", "", "What action to take")

	id         = flag.String("id", "", "ID of the resource the action targets")
	buildingID = flag.String("building_id", "", "Building ID")
	floorID    = flag.String("floor_id", "", "Floor ID")
	roomID     = flag.String("room_id", "", "Room ID")
	deviceID   = flag.String("device_id", "", "Device ID")
	version    = flag.String("version", "", "Current version, required for Update* actions")

	name      = flag.String("name", "", "The name to give")
	lat       = flag.Float64("lat", 0, "The latitude")
	lon       = flag.Float64("lon", 0, "The longitude")
	tz        = flag.String("tz", "UTC", "The timezone")
	roomType  = flag.Int("room_type", 0, "Which room type this is")
	sortOrder = flag.Int("sort_order", 0, "Floor sort order")
)

// optionalFlag returns nil for an unset (empty) string flag, or its value
// otherwise - db methods that accept optional filters take *string.
func optionalFlag(f *string) *string {
	if *f == "" {
		return nil
	}
	return f
}

func main() {
	flag.Parse()

	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	if _, err := os.Stat(*dbPath); os.IsNotExist(err) {
		logger.Debug("database file missing; creating", zap.String("db_path", *dbPath))
		dbFile, err := os.Create(*dbPath)
		if err != nil {
			logger.Fatal("unable to create database file", zap.Error(err))
		}
		dbFile.Close()
	}

	dsn := fmt.Sprintf("file:%s?parseTime=true", *dbPath)
	sqlDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		logger.Fatal("unable to open db", zap.Error(err))
	} else if sqlDB == nil {
		logger.Fatal("empty database")
	}
	defer sqlDB.Close()

	buildingDB, err := db.NewDatabase(logger, sqlDB)
	if err != nil {
		logger.Fatal("unable to initialize db", zap.Error(err))
	}

	ctx := context.Background()

	switch *action {
	case "GetBuilding":
		building, err := buildingDB.GetBuilding(ctx, *id)
		if err != nil {
			logger.Fatal("error getting building", zap.Error(err))
		}
		if building == nil {
			logger.Info("building not found")
			return
		}
		logger.Info("building found", zap.String("id", building.ID), zap.String("name", building.Name), zap.String("version", building.Version))

		floors, err := buildingDB.ListFloors(ctx, *id)
		if err != nil {
			logger.Error("unable to list floors for building", zap.Error(err))
			return
		}
		for _, floor := range floors {
			logger.Info("floor found", zap.String("id", floor.ID), zap.String("name", floor.Name), zap.Int32("sort_order", floor.SortOrder))
		}

	case "CreateBuilding":
		building := &db.Building{
			Name: *name,
			TZ:   *tz,
			Location: db.Location{
				Latitude:  *lat,
				Longitude: *lon,
			},
		}
		res, err := buildingDB.CreateBuilding(ctx, building)
		if err != nil {
			logger.Fatal("error creating building", zap.Error(err))
		}
		logger.Info("building created", zap.String("id", res.ID), zap.String("name", res.Name), zap.String("version", res.Version))

	case "UpdateBuilding":
		building := &db.Building{
			ID:      *id,
			Version: *version,
			Name:    *name,
			TZ:      *tz,
			Location: db.Location{
				Latitude:  *lat,
				Longitude: *lon,
			},
		}
		res, err := buildingDB.UpdateBuilding(ctx, building)
		if err != nil {
			logger.Fatal("error updating building", zap.Error(err))
		}
		logger.Info("building updated", zap.String("id", res.ID), zap.String("version", res.Version))

	case "DeleteBuilding":
		if err := buildingDB.DeleteBuilding(ctx, *id); err != nil {
			logger.Fatal("error deleting building", zap.Error(err))
		}
		logger.Info("building deleted", zap.String("id", *id))

	case "ListFloors":
		floors, err := buildingDB.ListFloors(ctx, *buildingID)
		if err != nil {
			logger.Fatal("error listing floors", zap.Error(err))
		}
		for _, floor := range floors {
			logger.Info("floor found", zap.String("id", floor.ID), zap.String("name", floor.Name), zap.Int32("sort_order", floor.SortOrder))
		}

	case "GetFloor":
		floor, err := buildingDB.GetFloor(ctx, *id)
		if err != nil {
			logger.Fatal("error getting floor", zap.Error(err))
		}
		if floor == nil {
			logger.Info("floor not found")
			return
		}
		logger.Info("floor found", zap.String("id", floor.ID), zap.String("name", floor.Name), zap.String("version", floor.Version))

	case "CreateFloor":
		floor := &db.Floor{
			BuildingID: *buildingID,
			Name:       *name,
			SortOrder:  int32(*sortOrder),
		}
		res, err := buildingDB.CreateFloor(ctx, floor)
		if err != nil {
			logger.Fatal("error creating floor", zap.Error(err))
		}
		logger.Info("floor created", zap.String("id", res.ID), zap.String("name", res.Name), zap.String("version", res.Version))

	case "UpdateFloor":
		floor := &db.Floor{
			ID:        *id,
			Version:   *version,
			Name:      *name,
			SortOrder: int32(*sortOrder),
		}
		res, err := buildingDB.UpdateFloor(ctx, floor)
		if err != nil {
			logger.Fatal("error updating floor", zap.Error(err))
		}
		logger.Info("floor updated", zap.String("id", res.ID), zap.String("version", res.Version))

	case "DeleteFloor":
		if err := buildingDB.DeleteFloor(ctx, *id); err != nil {
			logger.Fatal("error deleting floor", zap.Error(err))
		}
		logger.Info("floor deleted", zap.String("id", *id))

	case "GetRoom":
		room, err := buildingDB.GetRoom(ctx, *id)
		if err != nil {
			logger.Fatal("error getting room", zap.Error(err))
		}
		if room == nil {
			logger.Info("room not found")
			return
		}
		logger.Info("room found",
			zap.String("id", room.ID), zap.String("name", room.Name), zap.Int("type", int(room.Type)),
			zap.String("floor_id", room.FloorID), zap.String("version", room.Version))

	case "ListRooms":
		rooms, err := buildingDB.ListRooms(ctx, optionalFlag(buildingID), optionalFlag(floorID))
		if err != nil {
			logger.Fatal("error listing rooms", zap.Error(err))
		}
		for _, room := range rooms {
			logger.Info("room found", zap.String("id", room.ID), zap.String("name", room.Name), zap.String("floor_id", room.FloorID))
		}

	case "CreateRoom":
		room := &db.Room{
			Name:    *name,
			FloorID: *floorID,
			Type:    db.RoomType(*roomType),
		}
		res, err := buildingDB.CreateRoom(ctx, room)
		if err != nil {
			logger.Fatal("error creating room", zap.Error(err))
		}
		logger.Info("room created", zap.String("id", res.ID), zap.String("name", res.Name), zap.String("version", res.Version))

	case "UpdateRoom":
		room := &db.Room{
			ID:      *id,
			Version: *version,
			Name:    *name,
			Type:    db.RoomType(*roomType),
		}
		res, err := buildingDB.UpdateRoom(ctx, room)
		if err != nil {
			logger.Fatal("error updating room", zap.Error(err))
		}
		logger.Info("room updated", zap.String("id", res.ID), zap.String("version", res.Version))

	case "DeleteRoom":
		if err := buildingDB.DeleteRoom(ctx, *id); err != nil {
			logger.Fatal("error deleting room", zap.Error(err))
		}
		logger.Info("room deleted", zap.String("id", *id))

	case "LinkDevice":
		link, previousRoomID, err := buildingDB.LinkDevice(ctx, *deviceID, *roomID)
		if err != nil {
			logger.Fatal("error linking device", zap.Error(err))
		}
		fields := []zap.Field{zap.String("device_id", link.ID), zap.String("room_id", link.RoomID)}
		if previousRoomID != nil {
			fields = append(fields, zap.String("previous_room_id", *previousRoomID))
		}
		logger.Info("device linked", fields...)

	case "UnlinkDevice":
		if err := buildingDB.UnlinkDevice(ctx, *deviceID); err != nil {
			logger.Fatal("error unlinking device", zap.Error(err))
		}
		logger.Info("device unlinked", zap.String("device_id", *deviceID))

	case "ListDeviceLinks":
		links, err := buildingDB.ListDeviceLinks(ctx, optionalFlag(buildingID), optionalFlag(roomID), optionalFlag(deviceID))
		if err != nil {
			logger.Fatal("error listing device links", zap.Error(err))
		}
		for _, link := range links {
			logger.Info("device link found", zap.String("device_id", link.ID), zap.String("room_id", link.RoomID))
		}

	default:
		logger.Fatal("unknown action", zap.String("action", *action))
	}
}
