package cache

import (
	"errors"
	"testing"
	"time"
)

func TestTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	c := New(10)
	c.SetClock(func() time.Time { return now })
	c.Set(Global, "k", 1, time.Minute)
	if v, ok := c.Get(Global, "k"); !ok || v.(int) != 1 {
		t.Fatal("miss before expiry")
	}
	now = now.Add(time.Minute)
	if _, ok := c.Get(Global, "k"); ok {
		t.Fatal("hit after expiry")
	}
}

func TestMemberIsolation(t *testing.T) {
	c := New(10)
	a, b := Member("1"), Member("2")
	if a == b || a == Global {
		t.Fatal("namespaces collide")
	}
	c.Set(a, "personal:x", "alice", time.Hour)
	if _, ok := c.Get(b, "personal:x"); ok {
		t.Fatal("member B sees member A's data")
	}
	if _, ok := c.Get(Global, "personal:x"); ok {
		t.Fatal("global sees member data")
	}
}

func TestInvalidatePrefix(t *testing.T) {
	c := New(10)
	ns := Member("1")
	c.Set(ns, "frequent:25", 1, time.Hour)
	c.Set(ns, "frequent:50", 1, time.Hour)
	c.Set(ns, "personal:x", 1, time.Hour)
	c.Set(Member("2"), "frequent:25", 1, time.Hour)
	c.Invalidate(ns, "frequent:")
	if _, ok := c.Get(ns, "frequent:25"); ok {
		t.Fatal("not invalidated")
	}
	if _, ok := c.Get(ns, "personal:x"); !ok {
		t.Fatal("unrelated key dropped")
	}
	if _, ok := c.Get(Member("2"), "frequent:25"); !ok {
		t.Fatal("other member's key dropped")
	}
}

func TestGetOrLoad(t *testing.T) {
	c := New(10)
	calls := 0
	load := func() (int, error) { calls++; return 42, nil }
	for range 3 {
		if v, err := GetOrLoad(c, Global, "k", time.Hour, load); err != nil || v != 42 {
			t.Fatal(v, err)
		}
	}
	if calls != 1 {
		t.Fatalf("loaded %d times", calls)
	}
	fails := 0
	failing := func() (int, error) { fails++; return 0, errors.New("x") }
	_, _ = GetOrLoad(c, Global, "e", time.Hour, failing)
	_, _ = GetOrLoad(c, Global, "e", time.Hour, failing)
	if fails != 2 {
		t.Fatal("errors must not be cached")
	}
	st := c.Stats()
	if st.Hits < 2 || st.Entries != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestEviction(t *testing.T) {
	now := time.Unix(0, 0)
	c := New(2)
	c.SetClock(func() time.Time { return now })
	c.Set(Global, "a", 1, time.Minute)
	c.Set(Global, "b", 1, time.Hour)
	c.Set(Global, "c", 1, time.Hour)
	if _, ok := c.Get(Global, "a"); ok {
		t.Fatal("entry closest to expiry should be evicted")
	}
	if c.Stats().Entries != 2 {
		t.Fatal("cap exceeded")
	}
}

func TestGetOrLoadIfSkipsRejectedValues(t *testing.T) {
	c := New(10)
	calls := 0
	load := func() (int, error) { calls++; return calls, nil }
	complete := func(v int) bool { return v >= 2 }
	for range 3 {
		_, _ = GetOrLoadIf(c, Global, "k", time.Hour, load, complete)
	}
	// 1st (incomplete) not cached, 2nd (complete) cached, 3rd served from cache.
	if calls != 2 {
		t.Fatalf("loads = %d, want 2", calls)
	}
	if v, ok := c.Get(Global, "k"); !ok || v.(int) != 2 {
		t.Fatalf("cached %v %v", v, ok)
	}
}
