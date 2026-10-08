package main

import (
	"context"
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

func TestWatchFeedClearsAndReconnectsOnDrop(t *testing.T) {
	f := newFakeFrigate(t)
	fb, log := f.bridge(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fb.watchMotion(ctx)

	c := f.accept(t)
	send(t, c, `{"topic":"garage_camera/motion","payload":"ON"}`)
	send(t, c, `{"topic":"garage_camera/person/active","payload":1}`)
	require.Eventually(t, func() bool { return len(log.get()) == 2 }, 5*time.Second, 10*time.Millisecond)

	// Dropping the feed mid-presence must clear both: the OFF/zero would otherwise never arrive.
	c.Close()
	assert.Eventually(t, func() bool { return len(log.get()) == 3 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, "garage_camera:motion-off/occ-off", log.get()[2])

	// And it reconnects on its own, starting from a clean slate.
	c = f.accept(t)
	send(t, c, `{"topic":"backyard_camera/person/active","payload":1}`)
	assert.Eventually(t, func() bool { return len(log.get()) == 4 }, 5*time.Second, 10*time.Millisecond)
}
