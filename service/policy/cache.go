package policy

import "sync"

// stateCache holds the last known value per entity ID, populated as the
// engine consumes device/house state updates. It is not persisted across
// restarts: on restart it repopulates from the first message of each stream
// subscription, so a lookup before that first message arrives returns
// (nil, false).
type stateCache struct {
	mu     sync.RWMutex
	values map[string]any
}

func newStateCache() *stateCache {
	return &stateCache{
		values: make(map[string]any),
	}
}

func (c *stateCache) set(entityID string, value any) {
	c.mu.Lock()
	c.values[entityID] = value
	c.mu.Unlock()
}

func (c *stateCache) get(entityID string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	v, ok := c.values[entityID]
	return v, ok
}

func (c *stateCache) delete(entityID string) {
	c.mu.Lock()
	delete(c.values, entityID)
	c.mu.Unlock()
}
