package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
)

// RoomLookup resolves a device to the room it is linked into.
type RoomLookup interface {
	// RoomID returns the room ID for deviceID, or "" if it isn't linked.
	RoomID(deviceID string) string
	// RoomName returns the display name of the room deviceID is linked into,
	// or "" if it isn't linked.
	RoomName(deviceID string) string
}

type roomRef struct {
	id   string
	name string
}

// RoomResolver keeps a device->room map built from HouseService.ListRooms,
// refreshed on an interval. Points keep whatever room was current when they
// were written; moving a device or renaming a room doesn't rewrite history.
type RoomResolver struct {
	logger *zap.Logger

	mu    sync.RWMutex
	rooms map[string]roomRef
}

// NewRoomResolver creates an empty resolver; call Run to populate it.
func NewRoomResolver(logger *zap.Logger) *RoomResolver {
	return &RoomResolver{logger: logger, rooms: make(map[string]roomRef)}
}

// RoomID implements RoomLookup.
func (r *RoomResolver) RoomID(deviceID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.rooms[deviceID].id
}

// RoomName implements RoomLookup.
func (r *RoomResolver) RoomName(deviceID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.rooms[deviceID].name
}

// Run refreshes immediately and then every interval until ctx is done. A
// failed refresh keeps the previous map, so a housed restart doesn't blank
// every room tag.
func (r *RoomResolver) Run(ctx context.Context, client api2.HouseServiceClient, interval time.Duration) {
	for {
		if err := r.refresh(ctx, client); err != nil && ctx.Err() == nil {
			r.logger.Warn("unable to refresh device room links", zap.Error(err))
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// refresh maps every linked device to its room by scanning each building's
// ListRooms. This is the lookup the policy engine's home.getDeviceRoom uses
// (see policy/housestate's GetDeviceRoom), done once for all devices per
// refresh instead of once per query. ListRooms resolves a room's linked
// devices server-side, and lists a linked device the bridge can't currently
// report as an ID-only stub, so every link is seen.
func (r *RoomResolver) refresh(ctx context.Context, client api2.HouseServiceClient) error {
	buildings, err := client.ListBuildings(ctx, &api2.ListBuildingsRequest{})
	if err != nil {
		return fmt.Errorf("list buildings: %w", err)
	}
	var buildingIDs []string
	for {
		b, err := buildings.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("recv building: %w", err)
		}
		buildingIDs = append(buildingIDs, b.GetId())
	}

	rooms := make(map[string]roomRef)
	for _, id := range buildingIDs {
		id := id
		stream, err := client.ListRooms(ctx, &api2.ListRoomsRequest{BuildingId: &id})
		if err != nil {
			return fmt.Errorf("list rooms for building %s: %w", id, err)
		}
		for {
			room, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return fmt.Errorf("recv room for building %s: %w", id, err)
			}
			for _, d := range room.GetDevices() {
				rooms[d.GetId()] = roomRef{id: room.GetId(), name: room.GetConfig().GetName()}
			}
		}
	}

	r.mu.Lock()
	r.rooms = rooms
	r.mu.Unlock()
	return nil
}
