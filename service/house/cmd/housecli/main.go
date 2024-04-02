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
	dbPath   = flag.String("db", "", "Path to the database to use")
	action   = flag.String("action", "", "What action to take")
	id       = flag.String("id", "", "ID of the action to manipulate")
	name     = flag.String("name", "", "The name to give")
	lat      = flag.Float64("lat", 0, "The latitude")
	lon      = flag.Float64("lon", 0, "The longitude")
	tz       = flag.String("tz", "UTC", "The timezone")
	roomType = flag.Int("room_type", 0, "Which room type this is")
)

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

	if *action == "GetBuilding" {
		building, err := buildingDB.GetBuilding(context.Background(), *id)
		if err != nil {
			logger.Fatal("error getting building", zap.Error(err))
		}

		if building == nil {
			logger.Info("building not found")
		} else {
			logger.Info("building found", zap.String("id", building.ID), zap.String("name", building.Name))

			rooms, err := buildingDB.GetBuildingRooms(context.Background(), *id)
			if err != nil {
				logger.Error("unable to get rooms for building", zap.Error(err))
				return
			}

			for _, room := range rooms {
				logger.Info("room found", zap.String("id", room.ID), zap.String("name", room.Name))
			}
		}
	} else if *action == "CreateBuilding" {
		building := &db.Building{
			Name: *name,
			TZ:   *tz,
			Location: db.Location{
				Latitude:  *lat,
				Longitude: *lon,
			},
		}

		res, err := buildingDB.CreateBuilding(context.Background(), building)
		if err != nil {
			logger.Fatal("error creating building", zap.Error(err))
		}
		logger.Info("building created", zap.String("id", res.ID), zap.String("name", res.Name))
	} else if *action == "GetRoom" {
		room, err := buildingDB.GetRoom(context.Background(), *id)
		if err != nil {
			logger.Fatal("error getting room", zap.Error(err))
		}

		if room == nil {
			logger.Info("room not found")
		} else {
			logger.Info("room found", zap.String("id", room.ID), zap.String("name", room.Name), zap.Int("type", int(room.Type)))
		}
	} else if *action == "CreateRoom" {
		room := &db.Room{
			Name:       *name,
			BuildingID: *id,
			Type:       db.RoomType(*roomType),
		}

		res, err := buildingDB.CreateRoom(context.Background(), room)
		if err != nil {
			logger.Fatal("error creating room", zap.Error(err))
		}
		logger.Info("room created", zap.String("id", res.ID), zap.String("name", res.Name))
	}
}
