package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

//go:embed migrations/*.sql
var fs embed.FS

// Database contains a handle to interface with the building DB
type Database struct {
	logger *zap.Logger

	db *sql.DB
}

// NewDatabase creates a new handle to access the building database.
// If necessary, the linked migrations will be run.
func NewDatabase(logger *zap.Logger, db *sql.DB) (*Database, error) {
	migrations, err := iofs.New(fs, "migrations")
	if err != nil {
		logger.Error("unable to open embedded migrations")
	}
	driver, err := sqlite3.WithInstance(db, &sqlite3.Config{})
	if err != nil {
		logger.Error("unable to create migration driver", zap.Error(err))
		return nil, err
	}
	m, err := migrate.NewWithInstance(
		"iofs", migrations,
		"sqlite3", driver)
	if err != nil {
		logger.Error("unable to create migration", zap.Error(err))
		return nil, err
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		logger.Error("error running migration", zap.Error(err))
		return nil, err
	}

	return &Database{
		logger: logger,
		db:     db,
	}, nil
}

// CreateBuilding inserts a new building into the database.
func (db *Database) CreateBuilding(ctx context.Context, b *Building) (*Building, error) {
	newID := uuid.NewString()

	_, err := db.db.ExecContext(ctx, "INSERT INTO building (id, name, tz, lat, lon) VALUES (?, ?, ?, ?, ?)", newID, b.Name, b.TZ, b.Location.Latitude, b.Location.Longitude)
	if err != nil {
		db.logger.Error("unable to create building", zap.Error(err))
		return nil, err
	}

	b.ID = newID
	return b, nil
}

// GetBuildings retrieves all stored buildings
func (db *Database) GetBuildings(ctx context.Context) ([]Building, error) {
	rows, err := db.db.QueryContext(ctx, "SELECT id,name,tz,lat,lon FROM building")
	if err != nil {
		db.logger.Error("unable to get buildings", zap.Error(err))
		return nil, err
	}
	defer rows.Close()

	var buildings []Building
	for rows.Next() {
		building := Building{}
		err = rows.Scan(&building.ID, &building.Name, &building.TZ, &building.Location.Latitude, &building.Location.Longitude)
		if err != nil && err != sql.ErrNoRows {
			db.logger.Error("unable to scan building row", zap.Error(err))
			return nil, err
		}
		buildings = append(buildings, building)
	}
	return buildings, nil
}

// GetBuilding retrieves all the linked properties of the building and returns them.
func (db *Database) GetBuilding(ctx context.Context, buildingID string) (*Building, error) {
	building := &Building{}
	row := db.db.QueryRowContext(ctx, "SELECT id,name,tz,lat,lon FROM building WHERE id=?", buildingID)

	var err error
	if err = row.Scan(&building.ID, &building.Name, &building.TZ, &building.Location.Latitude, &building.Location.Longitude); err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		db.logger.Error("unable to retrieve building", zap.Error(err))
		return nil, err
	}
	return building, nil
}

func (db *Database) CreateRoom(ctx context.Context, r *Room) (*Room, error) {
	newID := uuid.NewString()

	_, err := db.db.ExecContext(ctx, "INSERT INTO room (id, building_id, name, type) VALUES (?, ?, ?, ?)", newID, r.BuildingID, r.Name, r.Type)
	if err != nil {
		db.logger.Error("unable to create room", zap.Error(err))
		return nil, err
	}

	r.ID = newID
	return r, nil
}

func (db *Database) UpdateRoom(ctx context.Context, r *Room) (*Room, error) {
	_, err := db.db.ExecContext(ctx, "UPDATE room SET name=?,type=? WHERE id=?", r.Name, r.Type, r.ID)
	if err != nil {
		db.logger.Error("unable to update room", zap.String("room_id", r.ID), zap.Error(err))
		return nil, err
	}

	return r, nil
}

func (db *Database) DeleteRoom(ctx context.Context, roomID string) error {
	_, err := db.db.ExecContext(ctx, "DELETE FROM room WHERE id = ?", roomID)
	if err != nil {
		db.logger.Error("unable to delete room", zap.String("room_id", roomID), zap.Error(err))
		return err
	}

	return nil
}

func (db *Database) GetRoom(ctx context.Context, roomID string) (*Room, error) {
	room := &Room{}
	row := db.db.QueryRowContext(ctx, "SELECT id,building_id,name,type FROM room WHERE id=?", roomID)

	var err error
	if err = row.Scan(&room.ID, &room.BuildingID, &room.Name, &room.Type); err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		db.logger.Error("unable to retrieve room", zap.Error(err))
		return nil, err
	}
	return room, nil
}

func (db *Database) GetBuildingRooms(ctx context.Context, buildingID string) ([]Room, error) {
	rows, err := db.db.QueryContext(ctx, "SELECT room.id AS room_id,room.building_id,room.name,room.type,device_room.id FROM room LEFT JOIN device_room ON room.id=device_room.room_id WHERE room.building_id=?", buildingID)
	if err != nil {
		db.logger.Error("unable to get building rooms and devices", zap.String("building_id", buildingID), zap.Error(err))
		return nil, err
	}
	defer rows.Close()

	rooms := map[string]Room{}
	for rows.Next() {
		room := Room{}
		var deviceID sql.NullString
		err = rows.Scan(&room.ID, &room.BuildingID, &room.Name, &room.Type, &deviceID)
		if err != nil && err != sql.ErrNoRows {
			db.logger.Error("unable to scan room row", zap.String("building_id", buildingID), zap.Error(err))
			return nil, err
		}

		if deviceID.Valid {
			device := Device{}
			device.ID = deviceID.String
			device.RoomID = room.ID

			if r, found := rooms[room.ID]; found {
				room.Devices = append(r.Devices, device)
			} else {
				room.Devices = []Device{device}
			}
		}

		rooms[room.ID] = room
	}

	var ret []Room
	for _, r := range rooms {
		ret = append(ret, r)
	}
	return ret, nil
}

func (db *Database) CreateDevice(ctx context.Context, deviceID string, room Room) (*Device, error) {
	_, err := db.db.ExecContext(ctx, "INSERT INTO device_room (id, room_id) VALUES (?, ?)", deviceID, room.ID)
	if err != nil {
		db.logger.Error("unable to create device", zap.String("device_id", deviceID), zap.Error(err))
		return nil, err
	}

	return &Device{
		ID:     deviceID,
		RoomID: room.ID,
	}, nil
}

func (db *Database) DeleteDevice(ctx context.Context, deviceID string) error {
	_, err := db.db.ExecContext(ctx, "DELETE FROM device_room WHERE id = ?", deviceID)
	if err != nil {
		db.logger.Error("unable to delete device", zap.String("device_id", deviceID), zap.Error(err))
		return err
	}

	return nil
}
