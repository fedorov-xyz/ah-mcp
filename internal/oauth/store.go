package oauth

import (
	"sync"
	"time"
)

// maxStoreEntries bounds every in-memory store, so unauthenticated callers
// cannot exhaust memory by starting authorizations.
const maxStoreEntries = 10000

type storeEntry[T any] struct {
	v       T
	expires time.Time
}

// store is a mutex-protected map with per-entry expiry. Expired entries are
// swept lazily.
type store[T any] struct {
	mu      sync.Mutex
	entries map[string]storeEntry[T]
	now     func() time.Time
}

func newStore[T any](now func() time.Time) *store[T] {
	return &store[T]{entries: map[string]storeEntry[T]{}, now: now}
}

// put stores v until ttl elapses. It reports false when the store is full.
func (s *store[T]) put(key string, v T, ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= maxStoreEntries {
		s.sweepLocked()
		if len(s.entries) >= maxStoreEntries {
			return false
		}
	}
	s.entries[key] = storeEntry[T]{v: v, expires: s.now().Add(ttl)}
	return true
}

// get returns a live entry without removing it.
func (s *store[T]) get(key string) (T, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok || !s.now().Before(e.expires) {
		delete(s.entries, key)
		var zero T
		return zero, false
	}
	return e.v, true
}

// take removes and returns a live entry: a second take of the same key
// always fails (single use).
func (s *store[T]) take(key string) (T, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	delete(s.entries, key)
	if !ok || !s.now().Before(e.expires) {
		var zero T
		return zero, false
	}
	return e.v, true
}

func (s *store[T]) sweepLocked() {
	now := s.now()
	for k, e := range s.entries {
		if !now.Before(e.expires) {
			delete(s.entries, k)
		}
	}
}

func (s *store[T]) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	return len(s.entries)
}
