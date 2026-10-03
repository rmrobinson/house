package main

import (
	"bytes"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/houseview"
	"github.com/rmrobinson/house/service/lib/htmxutil"
)

// handleEvents relays HouseService.StreamHouseUpdates (room occupancy dots,
// the open room's properties grid, and a running log) plus the shared
// BridgeService update stream (device rows, so a toggle reflects a change
// made elsewhere) as SSE for htmx's sse extension. Each message is one or
// more hx-swap-oob fragments targeting ids already on the page - same
// approach as adminui's /events (service/adminui/sse.go).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.httpError(w, r, fmt.Errorf("streaming unsupported"))
		return
	}

	ctx := r.Context()
	buildingID := r.PathValue("id")
	names := s.roomNames(r, buildingID)

	roomUpdates, unsubRooms := s.roomHub(buildingID).Subscribe()
	defer unsubRooms()
	devUpdates, unsubDevs := s.devHub.Subscribe()
	defer unsubDevs()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// A comment line keeps idle proxies and load balancers from closing a
	// quiet stream; the browser ignores it.
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()

		case hu := <-roomUpdates:
			// viewerui only renders room-level occupancy/properties today;
			// a BuildingUpdate (Building.State) has nothing to swap in yet.
			ru := hu.GetRoom()
			if ru == nil {
				continue
			}
			if !s.writeFragments(w, "room_update_oob", roomUpdateView(ru, names.get(ru.GetRoomId()))) {
				return
			}
			flusher.Flush()

		case u := <-devUpdates:
			du := u.GetDeviceUpdate()
			if du == nil || u.GetAction() == api2.Update_REMOVED {
				continue
			}
			// A row for a device not on this page has no matching id and
			// is simply dropped by htmx.
			if !s.writeFragments(w, "device_row_oob", deviceToView(du.GetDevice())) {
				return
			}
			flusher.Flush()
		}
	}
}

// nameCache maps room ID -> name for the event log's labels.
type nameCache struct {
	mu sync.Mutex
	m  map[string]string
}

func (c *nameCache) get(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n, ok := c.m[id]; ok {
		return n
	}
	return id
}

func (s *Server) roomNames(r *http.Request, buildingID string) *nameCache {
	c := &nameCache{m: map[string]string{}}
	floors, err := houseview.ListFloors(r.Context(), s.house, buildingID)
	if err != nil {
		s.logger.Warn("event log: unable to list floors, rooms will be labelled by id", zap.Error(err))
		return c
	}
	for _, f := range floors {
		rooms, err := houseview.ListRoomsByFloor(r.Context(), s.house, f.GetId())
		if err != nil {
			continue
		}
		for _, room := range rooms {
			c.m[room.GetId()] = room.GetConfig().GetName()
		}
	}
	return c
}

type roomUpdateData struct {
	ID         string
	Name       string
	Occ        string
	Properties houseview.Properties
}

// OccLabel is the dot's text alternative.
func (r roomUpdateData) OccLabel() string { return occupancyLabel(r.Occ) }

func roomUpdateView(ru *api2.RoomUpdate, name string) roomUpdateData {
	return roomUpdateData{
		ID:         ru.GetRoomId(),
		Name:       name,
		Occ:        occupancy(ru.GetProperties()),
		Properties: houseview.PropertiesToView(ru.GetProperties()),
	}
}

func (s *Server) writeFragments(w http.ResponseWriter, name string, data any) bool {
	var frag bytes.Buffer
	if err := fragments.ExecuteTemplate(&frag, name, data); err != nil {
		s.logger.Error("template render failed", zap.String("fragment", name), zap.Error(err))
		return true
	}
	_, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", htmxutil.OneLine(frag.String()))
	return err == nil
}
