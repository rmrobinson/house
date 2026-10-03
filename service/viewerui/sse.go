package main

import (
	"bytes"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	apiDevice "github.com/rmrobinson/house/api/device"
	apiTrait "github.com/rmrobinson/house/api/trait"
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
			if m := houseview.Media(du.GetDevice()); m != nil && names.mediaChanged(du.GetDevice().GetId(), houseview.MediaSignature(m)) {
				if !s.handleMediaUpdate(w, r, names, du.GetDevice()) {
					return
				}
			}
			flusher.Flush()
		}
	}
}

// nameCache maps room ID -> name for the event log's labels, plus device ID
// -> room ID so a BridgeService update (which carries a Device, with no room
// reference of its own - only Room lists its Devices) can be routed to the
// room whose "now playing" row and event-log line it should refresh, plus
// device ID -> last-seen houseview.MediaSignature so an update that doesn't
// actually change playback (e.g. a volume tweak on a device that's mid-
// session, which still carries its unchanged Media trait alongside the
// Volume one) doesn't re-trigger the now-playing refresh or a duplicate
// event-log line.
type nameCache struct {
	mu         sync.Mutex
	m          map[string]string
	deviceRoom map[string]string
	mediaSig   map[string]string
}

func (c *nameCache) get(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n, ok := c.m[id]; ok {
		return n
	}
	return id
}

// roomFor returns the room ID deviceID is linked to, or "" if it isn't
// linked to any room on this building's floors (or the cache predates the
// link - built once per SSE connection, like the rest of nameCache).
func (c *nameCache) roomFor(deviceID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deviceRoom[deviceID]
}

// mediaChanged reports whether deviceID's playback signature differs from
// the last one seen (seeded from the device's state as of connection time -
// see roomNames), and records sig as the new baseline either way. A device
// not carrying a Media trait has no entry and is never consulted here.
func (c *nameCache) mediaChanged(deviceID, sig string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mediaSig[deviceID] == sig {
		return false
	}
	c.mediaSig[deviceID] = sig
	return true
}

func (s *Server) roomNames(r *http.Request, buildingID string) *nameCache {
	c := &nameCache{m: map[string]string{}, deviceRoom: map[string]string{}, mediaSig: map[string]string{}}
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
			for _, d := range room.GetDevices() {
				c.deviceRoom[d.GetId()] = room.GetId()
				if m := houseview.Media(d); m != nil {
					c.mediaSig[d.GetId()] = houseview.MediaSignature(m)
				}
			}
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

// roomNowPlayingView feeds now_playing_oob (room_detail.html) - the same
// ID/NowPlaying shape roomRowView and roomDetailView already carry for the
// non-live render.
type roomNowPlayingView struct {
	ID         string
	NowPlaying string
}

// handleMediaUpdate pushes the owning room's recomputed "now playing" row
// and an event-log line for d, a device whose update just carried a Media
// trait. Returns false (like writeFragments) only on a write error that
// should end the SSE connection.
//
// The room is re-fetched rather than summarized from d alone: a room can
// have more than one media-capable device, and roomNowPlaying picks across
// all of them, so an accurate "now playing" row needs the room's current
// device list, not just the one that changed. The event-log line, by
// contrast, is about d specifically - it uses d's own state directly.
func (s *Server) handleMediaUpdate(w http.ResponseWriter, r *http.Request, names *nameCache, d *apiDevice.Device) bool {
	roomID := names.roomFor(d.GetId())
	if roomID == "" {
		// Not linked to a room on this building's floors (or linked after
		// this cache was built) - nothing on the page to refresh.
		return true
	}

	room, err := s.house.GetRoom(r.Context(), &api2.GetRoomRequest{Id: roomID})
	if err != nil {
		s.logger.Warn("event log: unable to refresh room for now-playing update", zap.String("room_id", roomID), zap.Error(err))
		return true
	}
	if !s.writeFragments(w, "now_playing_oob", roomNowPlayingView{ID: roomID, NowPlaying: roomNowPlaying(room.GetDevices())}) {
		return false
	}

	if line := mediaEventLine(d); line != "" {
		if !s.writeFragments(w, "media_event_oob", struct{ Line string }{line}) {
			return false
		}
	}
	return true
}

// mediaEventLine renders one event-log entry for d's current media state -
// "" for a state not worth logging (no media trait, or State unset). Unlike
// houseview.MediaSummary (which goes silent once playback stops, so the
// "now playing" row doesn't linger on stale content), a stopped or finished
// session still gets its own line here - that transition is exactly the
// "stops" half of what the event log is for.
func mediaEventLine(d *apiDevice.Device) string {
	m := houseview.Media(d)
	name := houseview.DisplayName(d)
	switch m.GetState().GetPlaybackState() {
	case apiTrait.Media_PS_PLAYING, apiTrait.Media_PS_PAUSED, apiTrait.Media_PS_BUFFERING,
		apiTrait.Media_PS_FAST_FORWARD, apiTrait.Media_PS_REWIND:
		if s := houseview.MediaSummary(m); s != "" {
			return name + ": " + s
		}
		return ""
	case apiTrait.Media_PS_STOPPED:
		return name + ": Stopped"
	case apiTrait.Media_PS_COMPLETED:
		return name + ": Finished"
	default:
		return ""
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
