package postgres

import (
	"sync"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/authn"
)

// TTLCache memoises resolved keys.
//
// It exists because a credential lookup happens on every request, and putting
// PostgreSQL on the hot path of every completion is a latency and an
// availability decision nobody should make by accident.
//
// The cost is stated rather than hidden: a revoked or deactivated key keeps
// working for up to the TTL. Thirty seconds is chosen against that — long
// enough that a busy gateway is not querying the database per request, short
// enough that "I revoked it and it still works" is a sentence an operator can
// say twice before suspecting a bug rather than a cache.

// NewTTLCache returns a cache holding at most limit entries for ttl each.
func NewTTLCache(ttl time.Duration, limit int) *TTLCache {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if limit <= 0 {
		limit = 4096
	}
	return &TTLCache{ttl: ttl, limit: limit, entries: map[string]ttlEntry{}}
}

type ttlEntry struct {
	principal authn.Principal
	stored    time.Time
}

// TTLCache is a bounded, expiring key cache.
//
// Bounded because an unbounded one is a memory leak with a credential in it:
// an attacker sending random keys would otherwise grow this map without limit,
// and every entry is a full Principal.
type TTLCache struct {
	mu      sync.Mutex
	entries map[string]ttlEntry
	ttl     time.Duration
	limit   int
	// now is a field so a test can advance time without sleeping.
	now func() time.Time
}

func (c *TTLCache) clock() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}

// Get returns a cached principal if it has not expired.
func (c *TTLCache) Get(key string) (authn.Principal, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return authn.Principal{}, false
	}
	if c.clock().Sub(e.stored) > c.ttl {
		delete(c.entries, key)
		return authn.Principal{}, false
	}
	return e.principal, true
}

// Put stores a principal, evicting the oldest entry when full.
//
// Eviction is approximate — it drops an arbitrary entry rather than strictly
// the oldest — because a strictly-ordered eviction needs a heap and a lock on
// every read. The cache exists to spare the database the common case; being
// approximately LRU costs a few extra queries and no correctness.
func (c *TTLCache) Put(key string, p authn.Principal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.limit {
		c.evictLocked()
	}
	c.entries[key] = ttlEntry{principal: p, stored: c.clock()}
}

// Drop invalidates one entry, which is what a revocation calls.
func (c *TTLCache) Drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// evictLocked makes room. It also sweeps expired entries first, so a burst of
// traffic on expired keys reclaims space without needing eviction at all.
func (c *TTLCache) evictLocked() {
	now := c.clock()
	for k, e := range c.entries {
		if now.Sub(e.stored) > c.ttl {
			delete(c.entries, k)
		}
	}
	if len(c.entries) < c.limit {
		return
	}
	for k := range c.entries {
		delete(c.entries, k)
		break
	}
}

// Len reports how many entries are held, for a status endpoint and for tests.
func (c *TTLCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
