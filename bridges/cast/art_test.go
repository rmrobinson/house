package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/protobuf/proto"

	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
)

func TestArtProxy_CacheHitAvoidsSecondUpstreamFetch(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("fake-image-bytes")) //nolint:errcheck
	}))
	defer upstream.Close()

	p := NewArtProxy(zaptest.NewLogger(t), "http://proxy.local")
	proxied := p.ProxyURL(upstream.URL + "/art.jpg")
	require.Contains(t, proxied, "/art/")

	path := proxied[len("http://proxy.local"):]

	// First request: cache miss, fetches upstream.
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodGet, path, nil)
	p.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)
	assert.Equal(t, "fake-image-bytes", rec1.Body.String())
	assert.Equal(t, int32(1), upstreamHits.Load())

	// Second request for the same key: cache hit, no second upstream fetch.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, path, nil)
	p.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, "fake-image-bytes", rec2.Body.String())
	assert.Equal(t, int32(1), upstreamHits.Load(), "second request must not re-fetch upstream")
}

// TestArtProxy_ConcurrentRequestsForSameUnfetchedKey exercises the exact
// scenario fetch's doc comment calls out as expected ("two requests racing on
// a not-yet-cached key can both fetch upstream once each") — run with
// `bazel test --config=race` this catches unsynchronized reads/writes of
// entry.fetched/contentType/data across the racing goroutines.
func TestArtProxy_ConcurrentRequestsForSameUnfetchedKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("fake-image-bytes")) //nolint:errcheck
	}))
	defer upstream.Close()

	p := NewArtProxy(zaptest.NewLogger(t), "http://proxy.local")
	proxied := p.ProxyURL(upstream.URL + "/art.jpg")
	path := proxied[len("http://proxy.local"):]

	const concurrency = 20
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			p.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, "fake-image-bytes", rec.Body.String())
		}()
	}
	wg.Wait()
}

func TestArtProxy_UpstreamFailureLogsAndReturnsError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	p := NewArtProxy(zaptest.NewLogger(t), "http://proxy.local")
	proxied := p.ProxyURL(upstream.URL + "/broken.jpg")
	path := proxied[len("http://proxy.local"):]

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	p.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestArtProxy_UnknownKeyIs404(t *testing.T) {
	p := NewArtProxy(zaptest.NewLogger(t), "http://proxy.local")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/art/never-registered", nil)
	p.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestArtProxy_EvictsOldestBeyondCapacity(t *testing.T) {
	p := NewArtProxy(zaptest.NewLogger(t), "http://proxy.local")
	p.capacity = 2

	p.ProxyURL("http://example.com/1.jpg")
	p.ProxyURL("http://example.com/2.jpg")
	p.ProxyURL("http://example.com/3.jpg") // evicts 1.jpg, the least recently used

	p.mu.Lock()
	defer p.mu.Unlock()
	assert.Len(t, p.entries, 2)
	assert.NotContains(t, p.entries, hashArtURL("http://example.com/1.jpg"))
	assert.Contains(t, p.entries, hashArtURL("http://example.com/2.jpg"))
	assert.Contains(t, p.entries, hashArtURL("http://example.com/3.jpg"))
}

func TestArtProxy_ProxyURLNeverFetches(t *testing.T) {
	// Regression guard for the "upstream failure must not fail the device
	// update" requirement: ProxyURL (called from rewriteArtURLs on every
	// status change) only registers the URL and returns a local path — it
	// must never dial out, even to an address nothing is listening on.
	p := NewArtProxy(zaptest.NewLogger(t), "http://proxy.local")
	url := p.ProxyURL("http://192.0.2.1:1/never-fetched.jpg") // TEST-NET-1, guaranteed unreachable
	assert.NotEmpty(t, url)

	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.entries[hashArtURL("http://192.0.2.1:1/never-fetched.jpg")]
	require.NotNil(t, entry)
	assert.False(t, entry.fetched)
}

func TestRewriteArtURLs(t *testing.T) {
	p := NewArtProxy(zaptest.NewLogger(t), "http://proxy.local")

	d := &device.Device{
		Details: &device.Device_MediaPlayer{
			MediaPlayer: &device.MediaPlayer{
				Media: &trait.Media{
					State: &trait.Media_State{
						SongDetails: &trait.Media_SongDetails{
							AlbumArtUrl: proto.String("http://example.com/album.jpg"),
						},
					},
				},
			},
		},
	}

	rewriteArtURLs(d, p)

	got := d.GetMediaPlayer().GetMedia().GetState().GetSongDetails().GetAlbumArtUrl()
	assert.Contains(t, got, "http://proxy.local/art/")
	assert.NotContains(t, got, "example.com")
}
