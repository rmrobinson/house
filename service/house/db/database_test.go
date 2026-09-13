package db

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// newTestDB opens a fresh in-memory database with migrations applied.
// MaxOpenConns is pinned to 1 - mattn/go-sqlite3's :memory: database is
// per-connection, so a pooled second connection would see an empty schema.
func newTestDB(t *testing.T) *Database {
	t.Helper()

	sqlDB, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })

	d, err := NewDatabase(zaptest.NewLogger(t), sqlDB)
	require.NoError(t, err)
	return d
}

func createTestBuilding(t *testing.T, d *Database) *Building {
	t.Helper()
	b, err := d.CreateBuilding(context.Background(), &Building{Name: "Home", TZ: "America/Toronto"})
	require.NoError(t, err)
	return b
}

func createTestFloor(t *testing.T, d *Database, buildingID string) *Floor {
	t.Helper()
	f, err := d.CreateFloor(context.Background(), &Floor{BuildingID: buildingID, Name: "First Floor", SortOrder: 1})
	require.NoError(t, err)
	return f
}

func createTestRoom(t *testing.T, d *Database, floorID string) *Room {
	t.Helper()
	r, err := d.CreateRoom(context.Background(), &Room{FloorID: floorID, Name: "Kitchen", Type: Kitchen})
	require.NoError(t, err)
	return r
}

func TestBuilding_CreateGetUpdateDelete(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	b := createTestBuilding(t, d)
	assert.NotEmpty(t, b.ID)
	assert.NotEmpty(t, b.Version)

	got, err := d.GetBuilding(ctx, b.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "Home", got.Name)

	got.Name = "Cottage"
	updated, err := d.UpdateBuilding(ctx, got)
	require.NoError(t, err)
	assert.Equal(t, "Cottage", updated.Name)
	assert.NotEqual(t, b.Version, updated.Version)

	// Stale version is rejected.
	_, err = d.UpdateBuilding(ctx, &Building{ID: b.ID, Version: b.Version, Name: "Stale"})
	assert.ErrorIs(t, err, ErrVersionMismatch)

	// Unknown id is NotFound, not a version mismatch.
	_, err = d.UpdateBuilding(ctx, &Building{ID: "nope", Version: "v1", Name: "X"})
	assert.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, d.DeleteBuilding(ctx, b.ID))

	got, err = d.GetBuilding(ctx, b.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestBuilding_DeleteBlockedByFloor(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	b := createTestBuilding(t, d)
	createTestFloor(t, d, b.ID)

	err := d.DeleteBuilding(ctx, b.ID)
	assert.ErrorIs(t, err, ErrHasChildren)
}

func TestFloor_CreateGetListUpdateDelete(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	b := createTestBuilding(t, d)

	f2, err := d.CreateFloor(ctx, &Floor{BuildingID: b.ID, Name: "Second Floor", SortOrder: 2})
	require.NoError(t, err)
	f1, err := d.CreateFloor(ctx, &Floor{BuildingID: b.ID, Name: "First Floor", SortOrder: 1})
	require.NoError(t, err)

	got, err := d.GetFloor(ctx, f1.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "First Floor", got.Name)

	floors, err := d.ListFloors(ctx, b.ID)
	require.NoError(t, err)
	require.Len(t, floors, 2)
	// Ordered by sort_order, regardless of creation order.
	assert.Equal(t, f1.ID, floors[0].ID)
	assert.Equal(t, f2.ID, floors[1].ID)

	f1.Name = "Ground Floor"
	updated, err := d.UpdateFloor(ctx, f1)
	require.NoError(t, err)
	assert.Equal(t, "Ground Floor", updated.Name)

	_, err = d.UpdateFloor(ctx, &Floor{ID: f1.ID, Version: "stale", Name: "X"})
	assert.ErrorIs(t, err, ErrVersionMismatch)

	require.NoError(t, d.DeleteFloor(ctx, f2.ID))
	floors, err = d.ListFloors(ctx, b.ID)
	require.NoError(t, err)
	assert.Len(t, floors, 1)
}

func TestFloor_CreateUnknownBuildingIsNotFound(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	_, err := d.CreateFloor(ctx, &Floor{BuildingID: "does-not-exist", Name: "Ghost Floor"})
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestFloor_DeleteBlockedByRoom(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	b := createTestBuilding(t, d)
	f := createTestFloor(t, d, b.ID)
	createTestRoom(t, d, f.ID)

	err := d.DeleteFloor(ctx, f.ID)
	assert.ErrorIs(t, err, ErrHasChildren)
}

func TestRoom_CreateDerivesBuildingIDFromFloor(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	b := createTestBuilding(t, d)
	f := createTestFloor(t, d, b.ID)

	r, err := d.CreateRoom(ctx, &Room{FloorID: f.ID, Name: "Kitchen", Type: Kitchen})
	require.NoError(t, err)
	assert.Equal(t, b.ID, r.BuildingID)
	assert.NotEmpty(t, r.Version)

	_, err = d.CreateRoom(ctx, &Room{FloorID: "does-not-exist", Name: "Ghost Room"})
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestRoom_GetListUpdateDelete(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	b := createTestBuilding(t, d)
	f1 := createTestFloor(t, d, b.ID)
	f2, err := d.CreateFloor(ctx, &Floor{BuildingID: b.ID, Name: "Second Floor", SortOrder: 2})
	require.NoError(t, err)

	r1 := createTestRoom(t, d, f1.ID)
	r2, err := d.CreateRoom(ctx, &Room{FloorID: f2.ID, Name: "Bedroom", Type: Bedroom})
	require.NoError(t, err)

	got, err := d.GetRoom(ctx, r1.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "Kitchen", got.Name)

	// Scoped to one floor.
	rooms, err := d.ListRooms(ctx, nil, &f1.ID)
	require.NoError(t, err)
	require.Len(t, rooms, 1)
	assert.Equal(t, r1.ID, rooms[0].ID)

	// All rooms across every floor of the building.
	rooms, err = d.ListRooms(ctx, &b.ID, nil)
	require.NoError(t, err)
	assert.Len(t, rooms, 2)

	r1.Name = "Kitchenette"
	updated, err := d.UpdateRoom(ctx, r1)
	require.NoError(t, err)
	assert.Equal(t, "Kitchenette", updated.Name)

	_, err = d.UpdateRoom(ctx, &Room{ID: r1.ID, Version: "stale", Name: "X"})
	assert.ErrorIs(t, err, ErrVersionMismatch)

	require.NoError(t, d.DeleteRoom(ctx, r2.ID))
	rooms, err = d.ListRooms(ctx, &b.ID, nil)
	require.NoError(t, err)
	assert.Len(t, rooms, 1)
}

func TestRoom_DeleteBlockedByLinkedDevice(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	b := createTestBuilding(t, d)
	f := createTestFloor(t, d, b.ID)
	r := createTestRoom(t, d, f.ID)

	_, _, err := d.LinkDevice(ctx, "device-1", r.ID)
	require.NoError(t, err)

	err = d.DeleteRoom(ctx, r.ID)
	assert.ErrorIs(t, err, ErrHasChildren)
}

func TestLinkDevice_UpsertReportsPreviousRoom(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	b := createTestBuilding(t, d)
	f := createTestFloor(t, d, b.ID)
	roomA := createTestRoom(t, d, f.ID)
	roomB, err := d.CreateRoom(ctx, &Room{FloorID: f.ID, Name: "Office", Type: Office})
	require.NoError(t, err)

	link, prev, err := d.LinkDevice(ctx, "device-1", roomA.ID)
	require.NoError(t, err)
	assert.Equal(t, roomA.ID, link.RoomID)
	assert.Nil(t, prev)

	// Moving the device is a single upsert call.
	link, prev, err = d.LinkDevice(ctx, "device-1", roomB.ID)
	require.NoError(t, err)
	assert.Equal(t, roomB.ID, link.RoomID)
	require.NotNil(t, prev)
	assert.Equal(t, roomA.ID, *prev)

	require.NoError(t, d.UnlinkDevice(ctx, "device-1"))

	// Re-linking after an unlink has no previous room.
	_, prev, err = d.LinkDevice(ctx, "device-1", roomA.ID)
	require.NoError(t, err)
	assert.Nil(t, prev)
}

func TestListDeviceLinks_Filters(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	b1 := createTestBuilding(t, d)
	f1 := createTestFloor(t, d, b1.ID)
	room1 := createTestRoom(t, d, f1.ID)

	b2, err := d.CreateBuilding(ctx, &Building{Name: "Other Building"})
	require.NoError(t, err)
	f2 := createTestFloor(t, d, b2.ID)
	room2 := createTestRoom(t, d, f2.ID)

	_, _, err = d.LinkDevice(ctx, "device-1", room1.ID)
	require.NoError(t, err)
	_, _, err = d.LinkDevice(ctx, "device-2", room1.ID)
	require.NoError(t, err)
	_, _, err = d.LinkDevice(ctx, "device-3", room2.ID)
	require.NoError(t, err)

	all, err := d.ListDeviceLinks(ctx, nil, nil, nil)
	require.NoError(t, err)
	assert.Len(t, all, 3)

	byBuilding, err := d.ListDeviceLinks(ctx, &b1.ID, nil, nil)
	require.NoError(t, err)
	assert.Len(t, byBuilding, 2)

	byRoom, err := d.ListDeviceLinks(ctx, nil, &room2.ID, nil)
	require.NoError(t, err)
	require.Len(t, byRoom, 1)
	assert.Equal(t, "device-3", byRoom[0].ID)

	deviceID := "device-1"
	byDevice, err := d.ListDeviceLinks(ctx, nil, nil, &deviceID)
	require.NoError(t, err)
	require.Len(t, byDevice, 1)
	assert.Equal(t, room1.ID, byDevice[0].RoomID)
}
