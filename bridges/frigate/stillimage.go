package main

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/bridges/frigate/frigate"
	"github.com/rmrobinson/house/service/bridge"
)

const (
	// defaultStillRetention is how long after a detection ends its snapshot can still be fetched by a
	// request that doesn't name an event, so a notification sent just after the person left still gets
	// their picture.
	defaultStillRetention = 2 * time.Minute

	// defaultSnapshotWait is how long GetStillImage waits for Frigate to attach a snapshot to a detection
	// that doesn't have one yet before falling back to the camera's latest frame.
	defaultSnapshotWait = 3 * time.Second

	snapshotPollInterval = 50 * time.Millisecond
)

// lastEvent is the most recent qualifying detection on a camera, kept past its end for the retention window.
type lastEvent struct {
	id          string
	hasSnapshot bool
	// ended is zero while the detection is still being tracked.
	ended time.Time
}

// qualifies reports whether an event is one this bridge exposes as a visible detection: a label we care
// about, moving, and not judged a false positive.
func (fb *FrigateBridge) qualifies(o frigate.EventObject) bool {
	return fb.occupancyLabels[o.Label] && !o.Stationary && !o.FalsePositive
}

// handleEvent applies one Frigate tracked-object event to the camera's Detection state.
func (fb *FrigateBridge) handleEvent(e frigate.Event) {
	o := e.After
	if o.ID == "" || (e.Type != "new" && e.Type != "update" && e.Type != "end") {
		return
	}

	fb.mu.Lock()
	defer fb.mu.Unlock()

	camera, ok := fb.cameras[o.Camera]
	if !ok {
		return
	}

	changed := false
	idx := slices.IndexFunc(camera.Visible, func(v DetectedObject) bool { return v.EventID == o.ID })

	if e.Type == "end" || !fb.qualifies(o) {
		if idx >= 0 {
			camera.Visible = slices.Delete(camera.Visible, idx, idx+1)
			changed = true
		}
		if last := fb.last[o.Camera]; last != nil && last.id == o.ID {
			if e.Type == "end" && fb.qualifies(o) {
				// A real detection that finished: keep its picture fetchable for the retention window.
				if last.ended.IsZero() {
					last.ended = fb.now()
				}
			} else {
				// Judged false or stationary: its picture must not be served as "the person".
				delete(fb.last, o.Camera)
			}
		}
		if changed {
			fb.publish(camera)
		}
		return
	}

	subLabel, subScore := o.SubLabelName()
	obj := DetectedObject{
		Label:         o.Label,
		Score:         o.Score,
		EventID:       o.ID,
		HasStillImage: o.HasSnapshot,
		Zones:         o.CurrentZones,
		SubLabel:      subLabel,
		SubLabelScore: subScore,
	}
	if o.StartTime > 0 {
		obj.FirstSeen = time.Unix(0, int64(o.StartTime*float64(time.Second)))
	}

	if idx < 0 {
		camera.Visible = append(camera.Visible, obj)
		sort.SliceStable(camera.Visible, func(i, j int) bool { return camera.Visible[i].EventID < camera.Visible[j].EventID })
		changed = true
	} else {
		prev := camera.Visible[idx]
		// A score drifts on nearly every update; republishing for that alone would make every
		// subscriber wake several times a second. Keep the latest value but only publish real changes.
		changed = prev.HasStillImage != obj.HasStillImage || prev.SubLabel != obj.SubLabel ||
			!slices.Equal(prev.Zones, obj.Zones)
		obj.FirstSeen = prev.FirstSeen
		camera.Visible[idx] = obj
	}

	fb.last[o.Camera] = &lastEvent{id: o.ID, hasSnapshot: o.HasSnapshot}

	if changed {
		fb.publish(camera)
	}
}

// clearVisibleLocked forgets every tracked detection on a camera and reports whether there were any.
func (fb *FrigateBridge) clearVisibleLocked(camera *Camera) bool {
	if len(camera.Visible) == 0 {
		return false
	}
	camera.Visible = nil
	return true
}

// latestEventLocked returns the camera's last qualifying detection if it is still tracked or ended within
// the retention window.
func (fb *FrigateBridge) latestEventLocked(name string) (lastEvent, bool) {
	last := fb.last[name]
	if last == nil {
		return lastEvent{}, false
	}
	if !last.ended.IsZero() && fb.now().Sub(last.ended) > fb.stillRetention {
		return lastEvent{}, false
	}
	return *last, true
}

func (fb *FrigateBridge) cameraByID(id string) *Camera {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	for _, c := range fb.cameras {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// GetStillImage implements bridge.StillImageHandler. Unless asked for the latest frame it prefers the
// snapshot of the camera's latest qualifying detection, waiting briefly for Frigate to produce it, and
// falls back to the latest frame; the response says which it returned.
func (fb *FrigateBridge) GetStillImage(ctx context.Context, req *api2.GetStillImageRequest) (*api2.GetStillImageResponse, error) {
	camera := fb.cameraByID(req.GetDeviceId())
	if camera == nil {
		return nil, bridge.ErrDeviceNotFound
	}
	height := int(req.GetMaxHeight())

	if id := req.GetEventId(); id != "" {
		data, err := fb.client.EventSnapshot(ctx, id)
		if err != nil {
			fb.logger.Warn("unable to fetch event snapshot", zap.String("event_id", id), zap.Error(err))
			return nil, status.Error(codes.Unavailable, "unable to fetch the event snapshot")
		}
		return stillResponse(data, trait.StillImage_EVENT_SNAPSHOT, id), nil
	}

	if req.GetPreferredSource() != trait.StillImage_LATEST_FRAME {
		if id, ok := fb.waitForSnapshot(ctx, camera.Name); ok {
			data, err := fb.client.EventSnapshot(ctx, id)
			if err == nil {
				return stillResponse(data, trait.StillImage_EVENT_SNAPSHOT, id), nil
			}
			fb.logger.Warn("unable to fetch event snapshot, falling back to latest frame",
				zap.String("camera", camera.Name), zap.String("event_id", id), zap.Error(err))
		}
	}

	data, err := fb.client.LatestFrame(ctx, camera.Name, height)
	if err != nil {
		fb.logger.Warn("unable to fetch latest frame", zap.String("camera", camera.Name), zap.Error(err))
		return nil, status.Error(codes.Unavailable, fmt.Sprintf("unable to fetch a picture from %s", camera.Name))
	}
	return stillResponse(data, trait.StillImage_LATEST_FRAME, ""), nil
}

// waitForSnapshot returns the event id of the camera's latest qualifying detection once it has a snapshot,
// waiting up to snapshotWait for one to appear. It reports false if there is no such detection or it never
// got a snapshot in time.
func (fb *FrigateBridge) waitForSnapshot(ctx context.Context, name string) (string, bool) {
	deadline := time.NewTimer(fb.snapshotWait)
	defer deadline.Stop()
	tick := time.NewTicker(snapshotPollInterval)
	defer tick.Stop()

	for {
		fb.mu.Lock()
		last, ok := fb.latestEventLocked(name)
		fb.mu.Unlock()
		if !ok {
			return "", false
		}
		if last.hasSnapshot {
			return last.id, true
		}
		if !last.ended.IsZero() {
			// It ended without ever getting a snapshot; nothing to wait for.
			return "", false
		}

		select {
		case <-tick.C:
		case <-deadline.C:
			return "", false
		case <-ctx.Done():
			return "", false
		}
	}
}

func stillResponse(data []byte, source trait.StillImage_Source, eventID string) *api2.GetStillImageResponse {
	return &api2.GetStillImageResponse{
		Data:        data,
		ContentType: "image/jpeg",
		CapturedAt:  timestamppb.Now(),
		Source:      source,
		EventId:     eventID,
	}
}
