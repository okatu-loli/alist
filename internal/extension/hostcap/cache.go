package hostcap

import (
	"sync"
	"time"
)

// hostCache is a per-plugin in-memory cache. It is constructed per Host instance
// (one plugin), so its whole keyspace is already namespaced to that plugin;
// entries never leak across plugins. Gated on the cache capability grant: an
// ungranted plugin gets a cache whose operations are inert no-ops (Get always
// misses) rather than an error surface, since a cache is a best-effort
// optimization.
type hostCache struct {
	granted bool
	mu      sync.Mutex
	m       map[string]cacheEntry
}

type cacheEntry struct {
	value   []byte
	expires time.Time // zero = no expiry
}

func newHostCache(granted bool) *hostCache {
	return &hostCache{granted: granted, m: map[string]cacheEntry{}}
}

func (c *hostCache) Get(key string) ([]byte, bool) {
	if !c.granted {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return nil, false
	}
	if !e.expires.IsZero() && time.Now().After(e.expires) {
		delete(c.m, key)
		return nil, false
	}
	out := make([]byte, len(e.value))
	copy(out, e.value)
	return out, true
}

func (c *hostCache) Set(key string, value []byte, ttl time.Duration) {
	if !c.granted {
		return
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	e := cacheEntry{value: cp}
	if ttl > 0 {
		e.expires = time.Now().Add(ttl)
	}
	c.mu.Lock()
	c.m[key] = e
	c.mu.Unlock()
}

func (c *hostCache) Delete(key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}
