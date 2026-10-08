package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/rmrobinson/house/bridges/frigate/frigate"
)

// fakeFrigate serves /ws, sending each connection the frames pushed to the channel returned by conn().
type fakeFrigate struct {
	srv   *httptest.Server
	conns chan *websocket.Conn
}

func newFakeFrigate(t *testing.T) *fakeFrigate {
	t.Helper()
	f := &fakeFrigate{conns: make(chan *websocket.Conn, 4)}
	up := websocket.Upgrader{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/ws", r.URL.Path)
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.conns <- c
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeFrigate) accept(t *testing.T) *websocket.Conn {
	t.Helper()
	select {
	case c := <-f.conns:
		// Every connection must start by asking for the current state.
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		var req struct{ Topic string }
		require.NoError(t, c.ReadJSON(&req))
		assert.Equal(t, "onConnect", req.Topic)
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("bridge never connected to the motion feed")
		return nil
	}
}

func (f *fakeFrigate) bridge(t *testing.T) (*FrigateBridge, *publishLog) {
	t.Helper()
	u, err := url.Parse(f.srv.URL)
	require.NoError(t, err)
	ep, err := url.Parse("rtsp://x/y")
	require.NoError(t, err)

	log := &publishLog{}
	return &FrigateBridge{
		logger: zap.NewNop(),
		client: frigate.NewClient(zap.NewNop(), f.srv.Client(), u),
		cameras: map[string]*Camera{
			"garage_camera":   {Name: "garage_camera", Endpoint: ep},
			"backyard_camera": {Name: "backyard_camera", Endpoint: ep},
		},
		occupancyLabels: map[string]bool{"person": true},
		active:          map[string]map[string]int{},
		publish:         log.record,
		presenceGrace:   time.Hour,
	}, log
}

type publishLog struct {
	mu   sync.Mutex
	seen []string
}

func (p *publishLog) record(c *Camera) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := "off"
	if c.MotionDetected {
		state = "on"
	}
	occ := "off"
	if c.OccupancyDetected {
		occ = "on"
	}
	p.seen = append(p.seen, c.Name+":motion-"+state+"/occ-"+occ)
}

func (p *publishLog) get() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

func send(t *testing.T, c *websocket.Conn, frame string) {
	t.Helper()
	require.NoError(t, c.WriteMessage(websocket.TextMessage, []byte(frame)))
}

func TestWatchFeedSeparatesMotionFromOccupancy(t *testing.T) {
	f := newFakeFrigate(t)
	fb, log := f.bridge(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fb.watchMotion(ctx)

	c := f.accept(t)
	send(t, c, `{"topic":"stats","payload":"{}"}`)                          // ignored
	send(t, c, `{"topic":"events","payload":"{}"}`)                         // ignored
	send(t, c, `{"topic":"garage_camera/person","payload":1}`)              // includes stationary: ignored
	send(t, c, `{"topic":"garage_camera/car/active","payload":1}`)          // not an occupancy label
	send(t, c, `{"topic":"garage_camera/all/active","payload":1}`)          // aggregate: ignored
	send(t, c, `{"topic":"unconfigured_camera/motion","payload":"ON"}`)     // not one of ours
	send(t, c, `{"topic":"unconfigured_camera/person/active","payload":1}`) // not one of ours
	send(t, c, `{"topic":"garage_camera/motion","payload":"ON"}`)           // motion only
	send(t, c, `{"topic":"garage_camera/motion","payload":"ON"}`)           // duplicate: no republish
	send(t, c, `{"topic":"garage_camera/person/active","payload":1}`)       // + occupancy
	send(t, c, `{"topic":"garage_camera/person/active","payload":2}`)       // still occupied: no republish
	send(t, c, `{"topic":"garage_camera/motion","payload":"OFF"}`)          // motion ends, person remains
	send(t, c, `{"topic":"garage_camera/person/active","payload":0}`)       // occupancy ends

	want := []string{
		"garage_camera:motion-on/occ-off",
		"garage_camera:motion-on/occ-on",
		"garage_camera:motion-off/occ-on",
		"garage_camera:motion-off/occ-off",
	}
	assert.Eventually(t, func() bool { return len(log.get()) == len(want) }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, want, log.get())
}

func TestSetOccupancyLabelsAddsLabels(t *testing.T) {
	f := newFakeFrigate(t)
	fb, log := f.bridge(t)
	fb.SetOccupancyLabels([]string{"person", "car"})

	fb.setActiveCount("garage_camera", "car", 1)
	fb.setActiveCount("garage_camera", "person", 1) // already occupied: no republish
	fb.setActiveCount("garage_camera", "car", 0)    // person still there
	fb.setActiveCount("garage_camera", "person", 0)

	assert.Equal(t, []string{"garage_camera:motion-off/occ-on", "garage_camera:motion-off/occ-off"}, log.get())
}

func TestWatchFeedHoldsPresenceAcrossBriefDrop(t *testing.T) {
	f := newFakeFrigate(t)
	fb, log := f.bridge(t) // presenceGrace is an hour: a drop never goes stale here

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fb.watchMotion(ctx)

	c := f.accept(t)
	send(t, c, `{"topic":"garage_camera/person/active","payload":1}`)
	require.Eventually(t, func() bool { return len(log.get()) == 1 }, 5*time.Second, 10*time.Millisecond)

	// Frigate doesn't replay state on connect, so a blip must not report the person gone.
	c.Close()
	c = f.accept(t)
	assert.Equal(t, []string{"garage_camera:motion-off/occ-on"}, log.get())

	// The reconnected feed carries on from the held state.
	send(t, c, `{"topic":"garage_camera/person/active","payload":"0"}`) // string count, as MQTT relays it
	assert.Eventually(t, func() bool { return len(log.get()) == 2 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, "garage_camera:motion-off/occ-off", log.get()[1])
}

func TestWatchFeedClearsPresenceWhenDownTooLong(t *testing.T) {
	f := newFakeFrigate(t)
	fb, log := f.bridge(t)
	fb.presenceGrace = 50 * time.Millisecond // shorter than the 1s reconnect backoff

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fb.watchMotion(ctx)

	c := f.accept(t)
	send(t, c, `{"topic":"garage_camera/motion","payload":"ON"}`)
	send(t, c, `{"topic":"garage_camera/person/active","payload":1}`)
	require.Eventually(t, func() bool { return len(log.get()) == 2 }, 5*time.Second, 10*time.Millisecond)

	// Down past the grace period: a missed zero could leave it stuck on, so fail safe to absent.
	c.Close()
	assert.Eventually(t, func() bool { return len(log.get()) == 3 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, "garage_camera:motion-off/occ-off", log.get()[2])

	// And it reconnects on its own, starting from a clean slate.
	c = f.accept(t)
	send(t, c, `{"topic":"backyard_camera/person/active","payload":1}`)
	assert.Eventually(t, func() bool { return len(log.get()) == 4 }, 5*time.Second, 10*time.Millisecond)
}

func TestWatchFeedPublishesNothingOnShutdown(t *testing.T) {
	f := newFakeFrigate(t)
	fb, log := f.bridge(t)
	fb.presenceGrace = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { fb.watchMotion(ctx); close(done) }()

	c := f.accept(t)
	send(t, c, `{"topic":"garage_camera/person/active","payload":1}`)
	require.Eventually(t, func() bool { return len(log.get()) == 1 }, 5*time.Second, 10*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watchMotion did not stop on cancel")
	}
	time.Sleep(100 * time.Millisecond)
	assert.Len(t, log.get(), 1, "shutdown must not publish cleared presence")
}

// snapshot builds a camera_activity frame the way Frigate sends it: a JSON document inside a JSON string.
func snapshot(t *testing.T, cameras map[string]any) string {
	t.Helper()
	inner, err := json.Marshal(cameras)
	require.NoError(t, err)
	frame, err := json.Marshal(map[string]string{"topic": "camera_activity", "payload": string(inner)})
	require.NoError(t, err)
	return string(frame)
}

func activity(motion bool, objects ...map[string]any) map[string]any {
	return map[string]any{"motion": motion, "objects": objects, "config": map[string]any{"detect": true}}
}

func object(label string, stationary bool) map[string]any {
	return map[string]any{"id": "x", "label": label, "stationary": stationary, "score": 0.9}
}

func TestSnapshotSeedsStateOnConnect(t *testing.T) {
	f := newFakeFrigate(t)
	fb, log := f.bridge(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fb.watchMotion(ctx)

	c := f.accept(t)
	send(t, c, snapshot(t, map[string]any{
		// Someone already in frame, plus a parked car: occupied and in motion.
		"garage_camera": activity(true, object("person", false), object("car", true)),
		// Only a stationary person: no occupancy, no motion.
		"backyard_camera": activity(false, object("person", true)),
		// Not one of ours.
		"unconfigured_camera": activity(true, object("person", false)),
	}))

	assert.Eventually(t, func() bool { return len(log.get()) == 2 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"garage_camera:motion-off/occ-on", "garage_camera:motion-on/occ-on"}, log.get())
	assert.False(t, fb.cameras["backyard_camera"].OccupancyDetected)
}

func TestSnapshotOnReconnectCorrectsMissedUpdates(t *testing.T) {
	f := newFakeFrigate(t)
	fb, log := f.bridge(t) // presenceGrace is an hour: the drop is "brief"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fb.watchMotion(ctx)

	c := f.accept(t)
	send(t, c, `{"topic":"garage_camera/person/active","payload":1}`)
	send(t, c, `{"topic":"backyard_camera/person/active","payload":1}`)
	require.Eventually(t, func() bool { return len(log.get()) == 2 }, 5*time.Second, 10*time.Millisecond)

	// Both people leave while the feed is down. The reconnect snapshot says the garage is empty but the
	// backyard still has someone, so only the garage changes.
	c.Close()
	c = f.accept(t)
	send(t, c, snapshot(t, map[string]any{
		"garage_camera":   activity(false),
		"backyard_camera": activity(false, object("person", false)),
	}))

	assert.Eventually(t, func() bool { return len(log.get()) == 3 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, "garage_camera:motion-off/occ-off", log.get()[2])
	assert.True(t, fb.cameras["backyard_camera"].OccupancyDetected)

	// The counts were rebuilt from the snapshot, so a later zero still clears the backyard.
	send(t, c, `{"topic":"backyard_camera/person/active","payload":0}`)
	assert.Eventually(t, func() bool { return len(log.get()) == 4 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, "backyard_camera:motion-off/occ-off", log.get()[3])
}

func TestStaleClearDoesNotWipeStateRestoredByReconnect(t *testing.T) {
	f := newFakeFrigate(t)
	fb, log := f.bridge(t)

	fb.setActiveCount("garage_camera", "person", 1)
	staleGen := fb.currentFeedGen() // a clear armed during an outage...

	fb.markFeedConnected() // ...then the feed reconnects and restores state...
	fb.clearPresence(staleGen)

	// ...so the clear that was already in flight must be a no-op.
	assert.True(t, fb.cameras["garage_camera"].OccupancyDetected)
	assert.Equal(t, []string{"garage_camera:motion-off/occ-on"}, log.get())

	fb.clearPresence(fb.currentFeedGen()) // a current-generation clear still works
	assert.False(t, fb.cameras["garage_camera"].OccupancyDetected)
}

func TestSnapshotTreatsOmittedCameraAsIdle(t *testing.T) {
	f := newFakeFrigate(t)
	fb, log := f.bridge(t)

	fb.setMotion("garage_camera", true)
	fb.setActiveCount("garage_camera", "person", 1)

	// The snapshot reports on the backyard only: the garage has gone quiet or been disabled.
	fb.applySnapshot(map[string]frigate.CameraActivity{"backyard_camera": {}})

	assert.False(t, fb.cameras["garage_camera"].MotionDetected)
	assert.False(t, fb.cameras["garage_camera"].OccupancyDetected)
	assert.Equal(t, "garage_camera:motion-off/occ-off", log.get()[len(log.get())-1])
}

func TestGraceCountsFromTheFirstFailureNotEachRetry(t *testing.T) {
	f := newFakeFrigate(t)
	fb, log := f.bridge(t)
	fb.presenceGrace = 1500 * time.Millisecond // retries come at ~1s and ~3s; resetting per retry would slip past 4s

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fb.watchMotion(ctx)

	c := f.accept(t)
	send(t, c, `{"topic":"garage_camera/person/active","payload":1}`)
	require.Eventually(t, func() bool { return len(log.get()) == 1 }, 5*time.Second, 10*time.Millisecond)

	// Take Frigate away entirely so every redial fails.
	c.Close()
	f.srv.Close()

	assert.Eventually(t, func() bool { return len(log.get()) == 2 }, 2500*time.Millisecond, 10*time.Millisecond,
		"presence should clear ~1.5s after the drop, not after the last retry")
}
