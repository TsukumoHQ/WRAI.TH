package relay

import (
	"sync"
	"time"
)

// memoryReadCache remembers, per (project, agent, scope, key), the memory id a
// get_memory last returned, so a later set_memory by the same agent carries
// causal context without the caller passing based_on (DEC-wraith-memory-causal-1).
// In memory only — recording a read must never write the DB. Bounded by size
// and TTL; lost on restart, which only degrades to legacy last-writer-wins.
// The zero value is ready to use.
type memoryReadCache struct {
	mu      sync.Mutex
	entries map[memoryReadKey]memoryRead
	max     int              // 0 = memoryReadCacheMax
	ttl     time.Duration    // 0 = memoryReadCacheTTL
	now     func() time.Time // nil = time.Now
}

const (
	memoryReadCacheMax = 10000
	memoryReadCacheTTL = 24 * time.Hour
)

type memoryReadKey struct{ project, agent, scope, key string }

type memoryRead struct {
	id     string
	readAt time.Time
}

func (c *memoryReadCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// record stores the id of the single live row a get_memory returned. When
// full, the oldest read is evicted.
func (c *memoryReadCache) record(project, agent, scope, key, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[memoryReadKey]memoryRead{}
	}
	max := c.max
	if max <= 0 {
		max = memoryReadCacheMax
	}
	k := memoryReadKey{project, agent, scope, key}
	if _, ok := c.entries[k]; !ok && len(c.entries) >= max {
		var oldest memoryReadKey
		var oldestAt time.Time
		first := true
		for ek, ev := range c.entries {
			if first || ev.readAt.Before(oldestAt) {
				oldest, oldestAt, first = ek, ev.readAt, false
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[k] = memoryRead{id: id, readAt: c.clock()}
}

// lookup returns the cached read id, or "" when absent or older than the TTL.
func (c *memoryReadCache) lookup(project, agent, scope, key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	ttl := c.ttl
	if ttl <= 0 {
		ttl = memoryReadCacheTTL
	}
	k := memoryReadKey{project, agent, scope, key}
	r, ok := c.entries[k]
	if !ok {
		return ""
	}
	if c.clock().Sub(r.readAt) > ttl {
		delete(c.entries, k)
		return ""
	}
	return r.id
}
