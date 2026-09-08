package main

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	defaultArtCacheCapacity = 200
	artFetchTimeout         = 10 * time.Second
	// maxArtBytes bounds a single fetch so a misbehaving or huge upstream
	// can't exhaust memory — cover art is always small in practice.
	maxArtBytes = 10 << 20
)

// ArtProxy serves cached copies of remote art (album art, show art, ...) so
// clients on the protected VLAN never need direct internet egress. It has no
// Cast-specific dependency — the call site that rewrites a device's art URLs
// to point here lives in bridge.go, so this file can be lifted out into a
// shared house facility later without change (cameras will want the same
// thing).
//
// Fetching is lazy: ProxyURL only registers the upstream URL and returns the
// local URL a client should use; the actual HTTP fetch happens on first
// request to that URL, inside ServeHTTP. This means a slow or failing
// upstream can never fail a device update — nothing on the normalize path
// touches the network.
type ArtProxy struct {
	logger     *zap.Logger
	httpClient *http.Client
	baseURL    string

	mu       sync.Mutex
	entries  map[string]*artEntry
	order    *list.List
	elements map[string]*list.Element
	capacity int
}

type artEntry struct {
	upstream    string
	fetched     bool
	contentType string
	data        []byte
}

// NewArtProxy creates a proxy whose served URLs are rooted at baseURL (e.g.
// "http://192.168.1.50:8081").
func NewArtProxy(logger *zap.Logger, baseURL string) *ArtProxy {
	return &ArtProxy{
		logger:     logger,
		httpClient: &http.Client{Timeout: artFetchTimeout},
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		entries:    make(map[string]*artEntry),
		order:      list.New(),
		elements:   make(map[string]*list.Element),
		capacity:   defaultArtCacheCapacity,
	}
}

// ProxyURL returns the local URL a client should use to fetch upstream
// through this proxy, registering upstream if it hasn't been seen before.
// Returns "" if upstream is "".
func (p *ArtProxy) ProxyURL(upstream string) string {
	if upstream == "" {
		return ""
	}

	key := hashArtURL(upstream)

	p.mu.Lock()
	if _, ok := p.entries[key]; !ok {
		p.entries[key] = &artEntry{upstream: upstream}
		p.touchLocked(key)
		p.evictIfNeededLocked()
	}
	p.mu.Unlock()

	return p.baseURL + "/art/" + key
}

// ServeHTTP implements GET /art/{key}: serves a cached copy, fetching from
// the registered upstream URL on first request. Not registered under
// "/art/{key}" itself — mount it at the "/art/" prefix.
func (p *ArtProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/art/")
	if key == "" || key == r.URL.Path {
		http.NotFound(w, r)
		return
	}

	p.mu.Lock()
	entry, ok := p.entries[key]
	var needsFetch bool
	if ok {
		p.touchLocked(key)
		needsFetch = !entry.fetched
	}
	p.mu.Unlock()

	if !ok {
		http.NotFound(w, r)
		return
	}

	// entry.fetched/contentType/data are only ever written by fetch, under
	// p.mu — needsFetch was read under the same lock above, so this call
	// itself (and the racing duplicate fetch a concurrent request on the same
	// not-yet-cached key might also make, per fetch's doc comment) is the only
	// unsynchronized part; the fields themselves are read back under the lock
	// below, never directly off entry.
	if needsFetch {
		if err := p.fetch(entry); err != nil {
			p.logger.Warn("unable to fetch upstream art", zap.String("upstream", entry.upstream), zap.Error(err))
			http.Error(w, "unable to fetch art", http.StatusBadGateway)
			return
		}
	}

	p.mu.Lock()
	contentType, data := entry.contentType, entry.data
	p.mu.Unlock()

	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Write(data) //nolint:errcheck
}

// fetch is not synchronized against concurrent calls for the same entry — two
// requests racing on a not-yet-cached key can both fetch upstream once each.
// Harmless (the second write just overwrites the first with equal data) and
// not worth a per-key lock for how rarely two clients request the same
// not-yet-cached art within the same round trip.
func (p *ArtProxy) fetch(entry *artEntry) error {
	resp, err := p.httpClient.Get(entry.upstream)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream returned %s", resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxArtBytes))
	if err != nil {
		return err
	}

	p.mu.Lock()
	entry.contentType = resp.Header.Get("Content-Type")
	entry.data = data
	entry.fetched = true
	p.mu.Unlock()

	return nil
}

func (p *ArtProxy) touchLocked(key string) {
	if elem, ok := p.elements[key]; ok {
		p.order.MoveToFront(elem)
		return
	}
	p.elements[key] = p.order.PushFront(key)
}

func (p *ArtProxy) evictIfNeededLocked() {
	for len(p.entries) > p.capacity {
		back := p.order.Back()
		if back == nil {
			return
		}
		key := back.Value.(string)
		p.order.Remove(back)
		delete(p.elements, key)
		delete(p.entries, key)
	}
}

func hashArtURL(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}
