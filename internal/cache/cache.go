// Package cache is a small in-memory TTL cache with namespaces.
//
// Non-personal data (search, products, bonus offers, stores) lives in the
// Global namespace. Personal data must use Member(memberID), so it is never
// shared across users. Baskets, carts, orders and lists are never cached.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Standard TTLs.
const (
	TTLSearch   = 10 * time.Minute
	TTLProduct  = 10 * time.Minute
	TTLBonus    = 30 * time.Minute
	TTLStores   = 24 * time.Hour
	TTLPersonal = 15 * time.Minute
	// TTLNutriScore is long: a product's Nutri-Score rarely changes, and
	// looking it up costs a search request.
	TTLNutriScore = 24 * time.Hour
)

// Global is the namespace for data that is identical for every user.
const Global = "global"

// Member returns the namespace for one member's personal data.
func Member(memberID string) string {
	sum := sha256.Sum256([]byte(memberID))
	return "member:" + hex.EncodeToString(sum[:8])
}

type entry struct {
	value   any
	expires time.Time
}

// Cache is safe for concurrent use. Values must be treated as immutable by
// callers.
type Cache struct {
	mu      sync.Mutex
	entries map[string]entry
	max     int
	now     func() time.Time

	hits, misses atomic.Int64
}

// New returns a cache holding at most maxEntries entries.
func New(maxEntries int) *Cache {
	if maxEntries <= 0 {
		maxEntries = 2048
	}
	return &Cache{entries: map[string]entry{}, max: maxEntries, now: time.Now}
}

// SetClock overrides the clock (for tests).
func (c *Cache) SetClock(now func() time.Time) { c.now = now }

func fullKey(ns, key string) string { return ns + "\x00" + key }

// Get returns the value stored under (ns, key).
func (c *Cache) Get(ns, key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := fullKey(ns, key)
	e, ok := c.entries[k]
	if !ok || !c.now().Before(e.expires) {
		delete(c.entries, k)
		c.misses.Add(1)
		return nil, false
	}
	c.hits.Add(1)
	return e.value, true
}

// Set stores v under (ns, key) for ttl.
func (c *Cache) Set(ns, key string, v any, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := fullKey(ns, key)
	if _, exists := c.entries[k]; !exists && len(c.entries) >= c.max {
		c.evictLocked()
	}
	c.entries[k] = entry{value: v, expires: c.now().Add(ttl)}
}

// Invalidate drops every entry in ns whose key starts with prefix ("" drops
// the whole namespace).
func (c *Cache) Invalidate(ns, prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := fullKey(ns, prefix)
	for k := range c.entries {
		if strings.HasPrefix(k, p) {
			delete(c.entries, k)
		}
	}
}

func (c *Cache) evictLocked() {
	now := c.now()
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	if len(c.entries) < c.max {
		return
	}
	var oldest string
	var oldestExp time.Time
	for k, e := range c.entries {
		if oldest == "" || e.expires.Before(oldestExp) {
			oldest, oldestExp = k, e.expires
		}
	}
	delete(c.entries, oldest)
}

// Stats is a snapshot of cache counters.
type Stats struct {
	Entries int   `json:"entries"`
	Hits    int64 `json:"hits"`
	Misses  int64 `json:"misses"`
}

// Stats returns current counters.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	return Stats{Entries: n, Hits: c.hits.Load(), Misses: c.misses.Load()}
}

// GetOrLoad returns the cached T under (ns, key) or calls load and caches
// its result for ttl. Errors are not cached.
func GetOrLoad[T any](c *Cache, ns, key string, ttl time.Duration, load func() (T, error)) (T, error) {
	return GetOrLoadIf(c, ns, key, ttl, load, func(T) bool { return true })
}

// GetOrLoadIf is GetOrLoad, but caches a loaded value only when keep
// approves it — e.g. only complete results, so a partial one (some upstream
// requests failed) is retried on the next call instead of being served for
// the whole TTL.
func GetOrLoadIf[T any](c *Cache, ns, key string, ttl time.Duration, load func() (T, error), keep func(T) bool) (T, error) {
	if v, ok := c.Get(ns, key); ok {
		if t, ok := v.(T); ok {
			return t, nil
		}
	}
	t, err := load()
	if err != nil {
		return t, err
	}
	if keep(t) {
		c.Set(ns, key, t, ttl)
	}
	return t, nil
}
