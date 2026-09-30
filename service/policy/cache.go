package policy

import (
	"sort"
	"sync"
)

// cacheEntry pairs a cached value with the caller-supplied kind it was
// tagged with (see stateCache.byKind).
type cacheEntry struct {
	kind  string
	value any
}

// stateCache holds the last known value per entity ID, populated as the
// engine consumes device/house state updates. It is not persisted across
// restarts: on restart it repopulates from the first message of each stream
// subscription, so a lookup before that first message arrives returns
// (nil, false).
type stateCache struct {
	mu      sync.RWMutex
	entries map[string]cacheEntry
}

func newStateCache() *stateCache {
	return &stateCache{
		entries: make(map[string]cacheEntry),
	}
}

// set records value under entityID, tagged with kind for byKind. kind is
// opaque to the cache - whatever the caller passes is what byKind matches
// against - so it imposes no schema of its own (see Engine.UpdateDeviceState).
func (c *stateCache) set(entityID, kind string, value any) {
	c.mu.Lock()
	c.entries[entityID] = cacheEntry{kind: kind, value: value}
	c.mu.Unlock()
}

func (c *stateCache) get(entityID string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	e, ok := c.entries[entityID]
	return e.value, ok
}

func (c *stateCache) delete(entityID string) {
	c.mu.Lock()
	delete(c.entries, entityID)
	c.mu.Unlock()
}

// byKind returns the sorted IDs of every cached entry tagged with kind.
func (c *stateCache) byKind(kind string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var ids []string
	for id, e := range c.entries {
		if e.kind == kind {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
