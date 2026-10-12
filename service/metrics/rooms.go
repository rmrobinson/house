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
}

// RoomResolver keeps a device->room map from HouseService.ListDeviceLinks,
// refreshed on an interval. Points keep whatever room was current when they
// were written; moving a device doesn't rewrite history.
type RoomResolver struct {
	logger *zap.Logger

	mu    sync.RWMutex
	rooms map[string]string
}

// NewRoomResolver creates an empty resolver; call Run to populate it.
func NewRoomResolver(logger *zap.Logger) *RoomResolver {
	return &RoomResolver{logger: logger, rooms: make(map[string]string)}
}

// RoomID implements RoomLookup.
func (r *RoomResolver) RoomID(deviceID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.rooms[deviceID]
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

func (r *RoomResolver) refresh(ctx context.Context, client api2.HouseServiceClient) error {
	stream, err := client.ListDeviceLinks(ctx, &api2.ListDeviceLinksRequest{})
	if err != nil {
		return fmt.Errorf("list device links: %w", err)
	}

	rooms := make(map[string]string)
	for {
		link, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("recv device link: %w", err)
		}
		rooms[link.GetDeviceId()] = link.GetRoomId()
	}

	r.mu.Lock()
	r.rooms = rooms
	r.mu.Unlock()
	return nil
}
