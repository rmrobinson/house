package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/bridges/frigate/frigate"
)

func personEvent(typ, id, cam string, mut func(*frigate.EventObject)) frigate.Event {
	o := frigate.EventObject{ID: id, Camera: cam, Label: "person", Score: 0.72, Active: true}
	if mut != nil {
		mut(&o)
	}
	return frigate.Event{Type: typ, After: o}
}

func visibleIDs(c *Camera) []string {
	var ids []string
	for _, o := range c.Visible {
		ids = append(ids, o.EventID)
	}
	return ids
}

// stillBridge returns a bridge with two enabled cameras, backed by a fake Frigate HTTP server serving both
// image URLs. snapshots maps an event id to whether its snapshot exists yet.
type fakeImages struct {
	srv       *httptest.Server
	mu        sync.Mutex
	snapshots map[string]bool
	hits      []string
	badType   atomic.Bool
}

func newStillBridge(t *testing.T) (*FrigateBridge, *publishLog, *fakeImages) {
	t.Helper()
	imgs := &fakeImages{snapshots: map[string]bool{}}
	imgs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		imgs.mu.Lock()
		imgs.hits = append(imgs.hits, r.URL.RequestURI())
		imgs.mu.Unlock()
		if imgs.badType.Load() {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>"))
			return
		}
		switch r.URL.Path {
		case "/api/garage_camera/latest.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("latest-garage"))
		case "/api/events/ev1/snapshot.jpg", "/api/events/ev2/snapshot.jpg":
			imgs.mu.Lock()
			ok := imgs.snapshots[r.URL.Path]
			imgs.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("snap" + r.URL.Path))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(imgs.srv.Close)

	u, err := url.Parse(imgs.srv.URL)
	require.NoError(t, err)
	ep, err := url.Parse("rtsp://x/y")
	require.NoError(t, err)

	log := &publishLog{}
	fb := &FrigateBridge{
		logger: zap.NewNop(),
		client: frigate.NewClient(zap.NewNop(), imgs.srv.Client(), u),
		cameras: map[string]*Camera{
			"garage_camera":   {ID: "gid", Name: "garage_camera", Endpoint: ep, Enabled: true},
			"backyard_camera": {ID: "bid", Name: "backyard_camera", Endpoint: ep, Enabled: true},
		},
		occupancyLabels: map[string]bool{"person": true},
		active:          map[string]map[string]int{},
		publish:         log.record,
		presenceGrace:   time.Hour,
		last:            map[string]*lastEvent{},
		stillRetention:  time.Minute,
		snapshotWait:    300 * time.Millisecond,
		now:             time.Now,
	}
	return fb, log, imgs
}

func TestHandleEventTracksVisibleLifecycle(t *testing.T) {
	fb, log, _ := newStillBridge(t)
	g := fb.cameras["garage_camera"]

	fb.handleEvent(personEvent("new", "ev1", "garage_camera", nil))
	assert.Equal(t, []string{"ev1"}, visibleIDs(g))
	assert.False(t, g.Visible[0].HasStillImage)
	assert.Len(t, log.get(), 1)

	// A score-only change is kept but not republished.
	fb.handleEvent(personEvent("update", "ev1", "garage_camera", func(o *frigate.EventObject) { o.Score = 0.9 }))
	assert.Len(t, log.get(), 1)
	assert.InDelta(t, 0.9, g.Visible[0].Score, 1e-9)

	// The snapshot arriving is a real change.
	fb.handleEvent(personEvent("update", "ev1", "garage_camera", func(o *frigate.EventObject) { o.HasSnapshot = true }))
	assert.Len(t, log.get(), 2)
	assert.True(t, g.Visible[0].HasStillImage)

	fb.handleEvent(personEvent("end", "ev1", "garage_camera", func(o *frigate.EventObject) { o.HasSnapshot = true }))
	assert.Empty(t, g.Visible)
	assert.Len(t, log.get(), 3)
}

func TestHandleEventFiltersAndIsolatesCameras(t *testing.T) {
	fb, _, _ := newStillBridge(t)

	fb.handleEvent(personEvent("new", "car", "garage_camera", func(o *frigate.EventObject) { o.Label = "car" }))
	fb.handleEvent(personEvent("new", "still", "garage_camera", func(o *frigate.EventObject) { o.Stationary = true }))
	fb.handleEvent(personEvent("new", "fp", "garage_camera", func(o *frigate.EventObject) { o.FalsePositive = true }))
	fb.handleEvent(personEvent("new", "other", "elsewhere", nil))
	fb.handleEvent(frigate.Event{Type: "bogus"})
	assert.Empty(t, fb.cameras["garage_camera"].Visible)

	fb.handleEvent(personEvent("new", "ev1", "backyard_camera", nil))
	assert.Empty(t, fb.cameras["garage_camera"].Visible)
	assert.Equal(t, []string{"ev1"}, visibleIDs(fb.cameras["backyard_camera"]))

	// A person who goes stationary stops being visible.
	fb.handleEvent(personEvent("update", "ev1", "backyard_camera", func(o *frigate.EventObject) { o.Stationary = true }))
	assert.Empty(t, fb.cameras["backyard_camera"].Visible)
}

func TestHandleEventCarriesSubLabelAndZones(t *testing.T) {
	fb, _, _ := newStillBridge(t)
	fb.handleEvent(personEvent("new", "ev1", "garage_camera", nil))
	fb.handleEvent(personEvent("update", "ev1", "garage_camera", func(o *frigate.EventObject) {
		o.SubLabel = []any{"robert", 0.97}
		o.CurrentZones = []string{"driveway"}
	}))

	obj := fb.cameras["garage_camera"].ToDevice().GetCamera().GetDetection().GetState().GetVisible()
	require.Len(t, obj, 1)
	assert.Equal(t, "robert", obj[0].GetSubLabel())
	assert.InDelta(t, 0.97, obj[0].GetSubLabelScore(), 1e-6)
	assert.Equal(t, []string{"driveway"}, obj[0].GetZones())
}

func TestWatchFeedDeliversEvents(t *testing.T) {
	f := newFakeFrigate(t)
	fb, _ := f.bridge(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fb.watchMotion(ctx)

	c := f.accept(t)
	send(t, c, `{"topic":"events","payload":"{\"type\":\"new\",\"before\":{},\"after\":{\"id\":\"ev1\",\"camera\":\"garage_camera\",\"label\":\"person\",\"score\":0.7,\"has_snapshot\":false,\"stationary\":false,\"false_positive\":false,\"sub_label\":null,\"current_zones\":[]}}"}`)
	assert.Eventually(t, func() bool {
		fb.mu.Lock()
		defer fb.mu.Unlock()
		return len(fb.cameras["garage_camera"].Visible) == 1
	}, 5*time.Second, 10*time.Millisecond)
}

func TestVisibleClearedWhenFeedStaysDown(t *testing.T) {
	fb, _, _ := newStillBridge(t)
	gen := fb.currentFeedGen()
	fb.handleEvent(personEvent("new", "ev1", "garage_camera", nil))

	fb.clearPresence(gen)
	assert.Empty(t, fb.cameras["garage_camera"].Visible)
	_, ok := fb.latestEventLocked("garage_camera")
	assert.False(t, ok)
}

func TestApplySnapshotClearsIdleCameraVisible(t *testing.T) {
	fb, _, _ := newStillBridge(t)
	fb.handleEvent(personEvent("new", "ev1", "garage_camera", nil))
	fb.handleEvent(personEvent("new", "ev2", "backyard_camera", nil))

	fb.applySnapshot(map[string]frigate.CameraActivity{
		"backyard_camera": {Objects: []frigate.ActivityObject{{Label: "person"}}},
	})
	assert.Empty(t, fb.cameras["garage_camera"].Visible, "idle camera reconciled")
	assert.Len(t, fb.cameras["backyard_camera"].Visible, 1, "busy camera left to the event stream")
}

func TestToDeviceDetectionAndStillImageTraits(t *testing.T) {
	fb, _, _ := newStillBridge(t)
	fb.SetOccupancyLabels([]string{"person", "car"})

	cam := fb.cameras["garage_camera"].ToDevice().GetCamera()
	assert.Equal(t, []string{"car", "person"}, cam.GetDetection().GetAttributes().GetLabels())
	assert.Equal(t, []string{"image/jpeg"}, cam.GetStillImage().GetAttributes().GetContentTypes())
	assert.Empty(t, cam.GetDetection().GetState().GetVisible())

	fb.cameras["garage_camera"].Enabled = false
	cam = fb.cameras["garage_camera"].ToDevice().GetCamera()
	assert.Nil(t, cam.GetStillImage())
}

func TestGetStillImagePrefersEventSnapshot(t *testing.T) {
	fb, _, imgs := newStillBridge(t)
	imgs.snapshots["/api/events/ev1/snapshot.jpg"] = true
	fb.handleEvent(personEvent("new", "ev1", "garage_camera", func(o *frigate.EventObject) { o.HasSnapshot = true }))

	resp, err := fb.GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "gid", MaxHeight: 720})
	require.NoError(t, err)
	assert.Equal(t, trait.StillImage_EVENT_SNAPSHOT, resp.GetSource())
	assert.Equal(t, "ev1", resp.GetEventId())
	assert.Equal(t, "image/jpeg", resp.GetContentType())
	assert.Equal(t, "snap/api/events/ev1/snapshot.jpg", string(resp.GetData()))
	assert.Contains(t, imgs.hits, "/api/events/ev1/snapshot.jpg")
}

func TestGetStillImageWaitsForSnapshot(t *testing.T) {
	fb, _, imgs := newStillBridge(t)
	imgs.snapshots["/api/events/ev1/snapshot.jpg"] = true
	fb.handleEvent(personEvent("new", "ev1", "garage_camera", nil))

	go func() {
		time.Sleep(100 * time.Millisecond)
		fb.handleEvent(personEvent("update", "ev1", "garage_camera", func(o *frigate.EventObject) { o.HasSnapshot = true }))
	}()

	resp, err := fb.GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "gid"})
	require.NoError(t, err)
	assert.Equal(t, trait.StillImage_EVENT_SNAPSHOT, resp.GetSource())
}

func TestGetStillImageFallsBackToLatestFrame(t *testing.T) {
	fb, _, _ := newStillBridge(t)

	// No detection at all.
	resp, err := fb.GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "gid"})
	require.NoError(t, err)
	assert.Equal(t, trait.StillImage_LATEST_FRAME, resp.GetSource())
	assert.Equal(t, "latest-garage", string(resp.GetData()))

	// A detection whose snapshot never shows up within the wait.
	fb.handleEvent(personEvent("new", "ev1", "garage_camera", nil))
	start := time.Now()
	resp, err = fb.GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "gid"})
	require.NoError(t, err)
	assert.Equal(t, trait.StillImage_LATEST_FRAME, resp.GetSource())
	assert.GreaterOrEqual(t, time.Since(start), 250*time.Millisecond)

	// A snapshot that is advertised but 404s.
	fb.handleEvent(personEvent("update", "ev1", "garage_camera", func(o *frigate.EventObject) { o.HasSnapshot = true }))
	resp, err = fb.GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "gid"})
	require.NoError(t, err)
	assert.Equal(t, trait.StillImage_LATEST_FRAME, resp.GetSource())

	// Explicitly asked for the latest frame, even with a good snapshot.
	resp, err = fb.GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "gid", PreferredSource: trait.StillImage_LATEST_FRAME})
	require.NoError(t, err)
	assert.Equal(t, trait.StillImage_LATEST_FRAME, resp.GetSource())
}

func TestGetStillImageRetention(t *testing.T) {
	fb, _, imgs := newStillBridge(t)
	imgs.snapshots["/api/events/ev1/snapshot.jpg"] = true
	now := time.Now()
	fb.now = func() time.Time { return now }

	fb.handleEvent(personEvent("new", "ev1", "garage_camera", func(o *frigate.EventObject) { o.HasSnapshot = true }))
	fb.handleEvent(personEvent("end", "ev1", "garage_camera", func(o *frigate.EventObject) { o.HasSnapshot = true }))

	now = now.Add(30 * time.Second)
	resp, err := fb.GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "gid"})
	require.NoError(t, err)
	assert.Equal(t, trait.StillImage_EVENT_SNAPSHOT, resp.GetSource(), "ended detection still fetchable inside retention")

	now = now.Add(2 * time.Minute)
	resp, err = fb.GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "gid"})
	require.NoError(t, err)
	assert.Equal(t, trait.StillImage_LATEST_FRAME, resp.GetSource(), "expired")
}

func TestGetStillImageByEventIDAndErrors(t *testing.T) {
	fb, _, imgs := newStillBridge(t)
	imgs.snapshots["/api/events/ev2/snapshot.jpg"] = true

	resp, err := fb.GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "gid", EventId: "ev2"})
	require.NoError(t, err)
	assert.Equal(t, "ev2", resp.GetEventId())

	// A named event that can't be fetched is an error, not a silent substitute for another picture.
	_, err = fb.GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "gid", EventId: "ev1"})
	assert.Equal(t, codes.Unavailable, status.Code(err))

	_, err = fb.GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "nope"})
	assert.Error(t, err)

	// Non-image responses are rejected rather than forwarded as a picture.
	imgs.badType.Store(true)
	_, err = fb.GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "gid"})
	assert.Equal(t, codes.Unavailable, status.Code(err))
}

func TestFalsePositiveEndDropsItsPicture(t *testing.T) {
	fb, _, imgs := newStillBridge(t)
	imgs.snapshots["/api/events/ev1/snapshot.jpg"] = true
	fb.handleEvent(personEvent("new", "ev1", "garage_camera", func(o *frigate.EventObject) { o.HasSnapshot = true }))
	fb.handleEvent(personEvent("end", "ev1", "garage_camera", func(o *frigate.EventObject) { o.HasSnapshot = true; o.FalsePositive = true }))

	resp, err := fb.GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "gid"})
	require.NoError(t, err)
	assert.Equal(t, trait.StillImage_LATEST_FRAME, resp.GetSource(), "a false positive's snapshot must not be served")
}

func TestLatestFrameUsesHeightParameter(t *testing.T) {
	fb, _, imgs := newStillBridge(t)
	_, err := fb.GetStillImage(context.Background(), &api2.GetStillImageRequest{DeviceId: "gid", MaxHeight: 720, PreferredSource: trait.StillImage_LATEST_FRAME})
	require.NoError(t, err)
	assert.Contains(t, imgs.hits, "/api/garage_camera/latest.jpg?height=720")
}
