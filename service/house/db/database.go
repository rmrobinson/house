package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

//go:embed migrations/*.sql
var fs embed.FS

// ErrNotFound is returned when a requested id doesn't exist.
var ErrNotFound = errors.New("not found")

// ErrVersionMismatch is returned by an Update* call when the supplied
// version doesn't match the row's current version - the same optimistic
// concurrency contract as device.Device.version.
var ErrVersionMismatch = errors.New("version mismatch")

// ErrHasChildren is returned by a Delete* call when child records still
// exist (a building with floors, a floor with rooms, a room with linked
// devices). Callers must delete children first - no cascade.
var ErrHasChildren = errors.New("has child records")

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

// checkVersionedUpdate turns the RowsAffected of an `UPDATE/DELETE ... WHERE
// id=? AND version=?` into ErrNotFound or ErrVersionMismatch when it
// affected no rows - table is always a fixed internal constant, never
// caller-supplied, so building the query with it is safe.
func (db *Database) checkVersionedUpdate(ctx context.Context, res sql.Result, table, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	var exists int
	row := db.db.QueryRowContext(ctx, fmt.Sprintf("SELECT 1 FROM %s WHERE id=?", table), id)
	if err := row.Scan(&exists); err == sql.ErrNoRows {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	return ErrVersionMismatch
}

// hasRows reports whether any row in table has fk = id. table and fk are
// always fixed internal constants, never caller-supplied, so building the
// query with them is safe.
func (db *Database) hasRows(ctx context.Context, table, fk, id string) (bool, error) {
	var count int
	row := db.db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s=?", table, fk), id)
	if err := row.Scan(&count); err != nil {
		db.logger.Error("unable to check child records", zap.String("table", table), zap.String("id", id), zap.Error(err))
		return false, err
	}
	return count > 0, nil
}

// deleteVersionedRow deletes the row identified by id from table, enforcing
// that version matches the row's current version - the same optimistic
// concurrency contract as an Update* call, via checkVersionedUpdate. table
// is always a fixed internal constant, never caller-supplied, so building
// the query with it is safe.
func (db *Database) deleteVersionedRow(ctx context.Context, table, id, version string) error {
	res, err := db.db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE id=? AND version=?", table), id, version)
	if err != nil {
		db.logger.Error("unable to delete row", zap.String("table", table), zap.String("id", id), zap.Error(err))
		return err
	}
	return db.checkVersionedUpdate(ctx, res, table, id)
}

// deleteWithChildCheck deletes the row identified by id from table, first
// verifying no row in childTable still references it via childFK - shared by
// DeleteFloor/DeleteRoom, which differ only in these names.
func (db *Database) deleteWithChildCheck(ctx context.Context, table, childTable, childFK, id, version string) error {
	hasChildren, err := db.hasRows(ctx, childTable, childFK, id)
	if err != nil {
		return err
	}
	if hasChildren {
		return ErrHasChildren
	}

	return db.deleteVersionedRow(ctx, table, id, version)
}

/* ----- Building ----- */

// CreateBuilding inserts a new building into the database.
func (db *Database) CreateBuilding(ctx context.Context, b *Building) (*Building, error) {
	newID := uuid.NewString()
	newVersion := uuid.NewString()

	_, err := db.db.ExecContext(ctx, "INSERT INTO building (id, name, tz, lat, lon, version) VALUES (?, ?, ?, ?, ?, ?)",
		newID, b.Name, b.TZ, b.Location.Latitude, b.Location.Longitude, newVersion)
	if err != nil {
		db.logger.Error("unable to create building", zap.Error(err))
		return nil, err
	}

	b.ID = newID
	b.Version = newVersion
	return b, nil
}

// GetBuildings retrieves all stored buildings
func (db *Database) GetBuildings(ctx context.Context) ([]Building, error) {
	rows, err := db.db.QueryContext(ctx, "SELECT id,name,tz,lat,lon,version FROM building")
	if err != nil {
		db.logger.Error("unable to get buildings", zap.Error(err))
		return nil, err
	}
	defer rows.Close()

	var buildings []Building
	for rows.Next() {
		building := Building{}
		err = rows.Scan(&building.ID, &building.Name, &building.TZ, &building.Location.Latitude, &building.Location.Longitude, &building.Version)
		if err != nil && err != sql.ErrNoRows {
			db.logger.Error("unable to scan building row", zap.Error(err))
			return nil, err
		}
		buildings = append(buildings, building)
	}
	if err := rows.Err(); err != nil {
		db.logger.Error("error iterating building rows", zap.Error(err))
		return nil, err
	}
	return buildings, nil
}

// GetBuilding retrieves the building with the specified ID, or nil if it doesn't exist.
func (db *Database) GetBuilding(ctx context.Context, buildingID string) (*Building, error) {
	building := &Building{}
	row := db.db.QueryRowContext(ctx, "SELECT id,name,tz,lat,lon,version FROM building WHERE id=?", buildingID)

	var err error
	if err = row.Scan(&building.ID, &building.Name, &building.TZ, &building.Location.Latitude, &building.Location.Longitude, &building.Version); err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		db.logger.Error("unable to retrieve building", zap.Error(err))
		return nil, err
	}
	return building, nil
}

// UpdateBuilding updates the specified building, enforcing that b.Version
// matches the row's current version. Returns ErrNotFound or
// ErrVersionMismatch as appropriate when it doesn't.
func (db *Database) UpdateBuilding(ctx context.Context, b *Building) (*Building, error) {
	newVersion := uuid.NewString()

	res, err := db.db.ExecContext(ctx, "UPDATE building SET name=?,tz=?,lat=?,lon=?,version=? WHERE id=? AND version=?",
		b.Name, b.TZ, b.Location.Latitude, b.Location.Longitude, newVersion, b.ID, b.Version)
	if err != nil {
		db.logger.Error("unable to update building", zap.String("building_id", b.ID), zap.Error(err))
		return nil, err
	}
	if err := db.checkVersionedUpdate(ctx, res, "building", b.ID); err != nil {
		return nil, err
	}

	b.Version = newVersion
	return b, nil
}

// DeleteBuilding deletes the specified building. Returns ErrHasChildren if
// any floors still reference it, or if any room does - including a legacy
// room predating migration 000003 (see scanRoom), which can have floor_id
// NULL while still carrying building_id directly, and so isn't reachable
// through the floor check alone.
func (db *Database) DeleteBuilding(ctx context.Context, buildingID, version string) error {
	hasFloors, err := db.hasRows(ctx, "floor", "building_id", buildingID)
	if err != nil {
		return err
	}
	if hasFloors {
		return ErrHasChildren
	}

	hasRooms, err := db.hasRows(ctx, "room", "building_id", buildingID)
	if err != nil {
		return err
	}
	if hasRooms {
		return ErrHasChildren
	}

	return db.deleteVersionedRow(ctx, "building", buildingID, version)
}

/* ----- Floor ----- */

// CreateFloor inserts a new floor into the database. f.BuildingID must
// reference an existing building.
func (db *Database) CreateFloor(ctx context.Context, f *Floor) (*Floor, error) {
	building, err := db.GetBuilding(ctx, f.BuildingID)
	if err != nil {
		return nil, err
	}
	if building == nil {
		return nil, ErrNotFound
	}

	newID := uuid.NewString()
	newVersion := uuid.NewString()

	_, err = db.db.ExecContext(ctx, "INSERT INTO floor (id, building_id, name, sort_order, version) VALUES (?, ?, ?, ?, ?)",
		newID, f.BuildingID, f.Name, f.SortOrder, newVersion)
	if err != nil {
		db.logger.Error("unable to create floor", zap.Error(err))
		return nil, err
	}

	f.ID = newID
	f.Version = newVersion
	return f, nil
}

// GetFloor retrieves the floor with the specified ID, or nil if it doesn't exist.
func (db *Database) GetFloor(ctx context.Context, floorID string) (*Floor, error) {
	f := &Floor{}
	row := db.db.QueryRowContext(ctx, "SELECT id,building_id,name,sort_order,version FROM floor WHERE id=?", floorID)

	if err := row.Scan(&f.ID, &f.BuildingID, &f.Name, &f.SortOrder, &f.Version); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		db.logger.Error("unable to retrieve floor", zap.Error(err))
		return nil, err
	}
	return f, nil
}

// ListFloors retrieves every floor of the specified building, ordered by SortOrder.
func (db *Database) ListFloors(ctx context.Context, buildingID string) ([]Floor, error) {
	rows, err := db.db.QueryContext(ctx, "SELECT id,building_id,name,sort_order,version FROM floor WHERE building_id=? ORDER BY sort_order", buildingID)
	if err != nil {
		db.logger.Error("unable to list floors", zap.String("building_id", buildingID), zap.Error(err))
		return nil, err
	}
	defer rows.Close()

	var floors []Floor
	for rows.Next() {
		f := Floor{}
		if err := rows.Scan(&f.ID, &f.BuildingID, &f.Name, &f.SortOrder, &f.Version); err != nil {
			db.logger.Error("unable to scan floor row", zap.Error(err))
			return nil, err
		}
		floors = append(floors, f)
	}
	if err := rows.Err(); err != nil {
		db.logger.Error("error iterating floor rows", zap.Error(err))
		return nil, err
	}
	return floors, nil
}

// UpdateFloor updates the specified floor, enforcing that f.Version matches
// the row's current version.
func (db *Database) UpdateFloor(ctx context.Context, f *Floor) (*Floor, error) {
	newVersion := uuid.NewString()

	res, err := db.db.ExecContext(ctx, "UPDATE floor SET name=?,sort_order=?,version=? WHERE id=? AND version=?",
		f.Name, f.SortOrder, newVersion, f.ID, f.Version)
	if err != nil {
		db.logger.Error("unable to update floor", zap.String("floor_id", f.ID), zap.Error(err))
		return nil, err
	}
	if err := db.checkVersionedUpdate(ctx, res, "floor", f.ID); err != nil {
		return nil, err
	}

	f.Version = newVersion
	return f, nil
}

// DeleteFloor deletes the specified floor. Returns ErrHasChildren if any
// rooms still reference it.
func (db *Database) DeleteFloor(ctx context.Context, floorID, version string) error {
	return db.deleteWithChildCheck(ctx, "floor", "room", "floor_id", floorID, version)
}

/* ----- Room ----- */

// CreateRoom inserts a new room into the database. r.FloorID must reference
// an existing floor; r.BuildingID is derived from it, not taken from r.
func (db *Database) CreateRoom(ctx context.Context, r *Room) (*Room, error) {
	floor, err := db.GetFloor(ctx, r.FloorID)
	if err != nil {
		return nil, err
	}
	if floor == nil {
		return nil, ErrNotFound
	}

	newID := uuid.NewString()
	newVersion := uuid.NewString()

	_, err = db.db.ExecContext(ctx, "INSERT INTO room (id, building_id, floor_id, name, type, version) VALUES (?, ?, ?, ?, ?, ?)",
		newID, floor.BuildingID, r.FloorID, r.Name, r.Type, newVersion)
	if err != nil {
		db.logger.Error("unable to create room", zap.Error(err))
		return nil, err
	}

	r.ID = newID
	r.BuildingID = floor.BuildingID
	r.Version = newVersion
	return r, nil
}

// UpdateRoom updates the specified room, enforcing that r.Version matches
// the row's current version. Room-to-floor reassignment isn't supported
// here.
func (db *Database) UpdateRoom(ctx context.Context, r *Room) (*Room, error) {
	newVersion := uuid.NewString()

	res, err := db.db.ExecContext(ctx, "UPDATE room SET name=?,type=?,version=? WHERE id=? AND version=?",
		r.Name, r.Type, newVersion, r.ID, r.Version)
	if err != nil {
		db.logger.Error("unable to update room", zap.String("room_id", r.ID), zap.Error(err))
		return nil, err
	}
	if err := db.checkVersionedUpdate(ctx, res, "room", r.ID); err != nil {
		return nil, err
	}

	r.Version = newVersion
	return r, nil
}

// DeleteRoom deletes the specified room. Returns ErrHasChildren if any
// devices are still linked to it.
func (db *Database) DeleteRoom(ctx context.Context, roomID, version string) error {
	return db.deleteWithChildCheck(ctx, "room", "device_room", "room_id", roomID, version)
}

// scanRoom scans one room row, tolerating a NULL floor_id - rooms created
// before migration 000003 added the column predate any floor assignment, so
// NULL there means "not yet assigned to a floor" rather than data
// corruption; it surfaces as FloorID == "" rather than a scan error.
func scanRoom(row interface{ Scan(...any) error }, r *Room) error {
	var floorID sql.NullString
	if err := row.Scan(&r.ID, &r.BuildingID, &floorID, &r.Name, &r.Type, &r.Version); err != nil {
		return err
	}
	r.FloorID = floorID.String
	return nil
}

// GetRoom retrieves the room with the specified ID, or nil if it doesn't
// exist. It does not populate Devices - use ListDeviceLinks for that.
func (db *Database) GetRoom(ctx context.Context, roomID string) (*Room, error) {
	room := &Room{}
	row := db.db.QueryRowContext(ctx, "SELECT id,building_id,floor_id,name,type,version FROM room WHERE id=?", roomID)

	if err := scanRoom(row, room); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		db.logger.Error("unable to retrieve room", zap.Error(err))
		return nil, err
	}
	return room, nil
}

// ListRooms retrieves rooms scoped to floorID if set, otherwise every room
// across every floor of buildingID.
func (db *Database) ListRooms(ctx context.Context, buildingID, floorID *string) ([]Room, error) {
	query := "SELECT id,building_id,floor_id,name,type,version FROM room"
	var args []any
	switch {
	case floorID != nil:
		query += " WHERE floor_id=?"
		args = append(args, *floorID)
	case buildingID != nil:
		query += " WHERE building_id=?"
		args = append(args, *buildingID)
	}

	rows, err := db.db.QueryContext(ctx, query, args...)
	if err != nil {
		db.logger.Error("unable to list rooms", zap.Error(err))
		return nil, err
	}
	defer rows.Close()

	var rooms []Room
	for rows.Next() {
		r := Room{}
		if err := scanRoom(rows, &r); err != nil {
			db.logger.Error("unable to scan room row", zap.Error(err))
			return nil, err
		}
		rooms = append(rooms, r)
	}
	if err := rows.Err(); err != nil {
		db.logger.Error("error iterating room rows", zap.Error(err))
		return nil, err
	}
	return rooms, nil
}

/* ----- Device <-> Room linking ----- */

// LinkDevice upserts the device_id -> room_id mapping, keyed on deviceID, so
// moving a device to a new room is a single call. If the device is already
// linked, expectedVersion must match its current version - checked
// atomically as part of the upsert's own WHERE clause below, not via a
// separate read-then-write, which would leave a race window - or
// ErrVersionMismatch is returned. An empty expectedVersion accepts any
// current version, which is the only option for a device's first-ever link
// (nothing to race against yet); it also means a race between two
// concurrent first links of the same never-before-linked device isn't
// caught here, same as Create* has no equivalent protection - a narrower
// case than the stale-edit race this version check exists for.
// previousRoomID is nil if the device wasn't previously linked to any room.
func (db *Database) LinkDevice(ctx context.Context, deviceID, roomID, expectedVersion string) (link *Device, previousRoomID *string, err error) {
	var prev sql.NullString
	row := db.db.QueryRowContext(ctx, "SELECT room_id FROM device_room WHERE id=?", deviceID)
	if scanErr := row.Scan(&prev); scanErr != nil && scanErr != sql.ErrNoRows {
		db.logger.Error("unable to check existing device link", zap.String("device_id", deviceID), zap.Error(scanErr))
		return nil, nil, scanErr
	}
	hadLink := prev.Valid

	newVersion := uuid.NewString()
	res, err := db.db.ExecContext(ctx,
		`INSERT INTO device_room (id, room_id, version) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET room_id=excluded.room_id, version=excluded.version
		 WHERE ?='' OR device_room.version=?`,
		deviceID, roomID, newVersion, expectedVersion, expectedVersion)
	if err != nil {
		db.logger.Error("unable to link device", zap.String("device_id", deviceID), zap.String("room_id", roomID), zap.Error(err))
		return nil, nil, err
	}

	if hadLink {
		n, raErr := res.RowsAffected()
		if raErr != nil {
			return nil, nil, raErr
		}
		if n == 0 {
			return nil, nil, ErrVersionMismatch
		}
		p := prev.String
		previousRoomID = &p
	}

	return &Device{ID: deviceID, RoomID: roomID, Version: newVersion}, previousRoomID, nil
}

// UnlinkDevice removes deviceID's room link, if any.
func (db *Database) UnlinkDevice(ctx context.Context, deviceID string) error {
	_, err := db.db.ExecContext(ctx, "DELETE FROM device_room WHERE id = ?", deviceID)
	if err != nil {
		db.logger.Error("unable to unlink device", zap.String("device_id", deviceID), zap.Error(err))
		return err
	}

	return nil
}

// ListDeviceLinks retrieves device_id/room_id links, filtered by whichever
// of buildingID, roomID and deviceID are non-nil.
func (db *Database) ListDeviceLinks(ctx context.Context, buildingID, roomID, deviceID *string) ([]Device, error) {
	query := "SELECT device_room.id, device_room.room_id, device_room.version FROM device_room"
	var conditions []string
	var args []any

	if buildingID != nil {
		query += " JOIN room ON device_room.room_id = room.id"
		conditions = append(conditions, "room.building_id = ?")
		args = append(args, *buildingID)
	}
	if roomID != nil {
		conditions = append(conditions, "device_room.room_id = ?")
		args = append(args, *roomID)
	}
	if deviceID != nil {
		conditions = append(conditions, "device_room.id = ?")
		args = append(args, *deviceID)
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}

	rows, err := db.db.QueryContext(ctx, query, args...)
	if err != nil {
		db.logger.Error("unable to list device links", zap.Error(err))
		return nil, err
	}
	defer rows.Close()

	var links []Device
	for rows.Next() {
		d := Device{}
		if err := rows.Scan(&d.ID, &d.RoomID, &d.Version); err != nil {
			db.logger.Error("unable to scan device link row", zap.Error(err))
			return nil, err
		}
		links = append(links, d)
	}
	if err := rows.Err(); err != nil {
		db.logger.Error("error iterating device link rows", zap.Error(err))
		return nil, err
	}
	return links, nil
}

// ListDeviceLinksForRooms retrieves the device_id/room_id links for every
// room in roomIDs in a single query - used by callers resolving devices for
// many rooms at once (see house.Service.ListRooms) instead of calling
// ListDeviceLinks once per room.
func (db *Database) ListDeviceLinksForRooms(ctx context.Context, roomIDs []string) ([]Device, error) {
	if len(roomIDs) == 0 {
		return nil, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(roomIDs)), ",")
	query := fmt.Sprintf("SELECT id, room_id, version FROM device_room WHERE room_id IN (%s)", placeholders)
	args := make([]any, len(roomIDs))
	for i, id := range roomIDs {
		args[i] = id
	}

	rows, err := db.db.QueryContext(ctx, query, args...)
	if err != nil {
		db.logger.Error("unable to list device links for rooms", zap.Error(err))
		return nil, err
	}
	defer rows.Close()

	var links []Device
	for rows.Next() {
		d := Device{}
		if err := rows.Scan(&d.ID, &d.RoomID, &d.Version); err != nil {
			db.logger.Error("unable to scan device link row", zap.Error(err))
			return nil, err
		}
		links = append(links, d)
	}
	if err := rows.Err(); err != nil {
		db.logger.Error("error iterating device link rows", zap.Error(err))
		return nil, err
	}
	return links, nil
}
