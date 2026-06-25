package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestRegistry_PerKeyIsolation verifies each key gets its own bucket: draining
// key-A's burst does not affect key-B (no shared/anonymous bucket — ADR-0001).
func TestRegistry_PerKeyIsolation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	reg := NewRegistry(5, 5, WithClock(func() time.Time { return now }))

	for i := 0; i < 5; i++ {
		if d := reg.Allow("A"); !d.Allowed {
			t.Fatalf("A request %d: denied, want allowed", i+1)
		}
	}
	d := reg.Allow("A")
	if d.Allowed {
		t.Fatal("A: 6th request allowed, want denied")
	}
	if d.RetryAfter <= 0 {
		t.Fatal("A: denied decision must carry a positive RetryAfter")
	}

	// key-B has its own fresh bucket.
	if d := reg.Allow("B"); !d.Allowed {
		t.Fatal("B: first request denied, want allowed (independent bucket)")
	}
}

// TestMemoryRepository_FixedWindow verifies the global counter caps at limit
// within a window and resets after the window rolls over.
func TestMemoryRepository_FixedWindow(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0)
	repo := NewMemoryRepository(WithMemoryClock(func() time.Time { return clk }))
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		d, err := repo.Allow(ctx, "k", 3, time.Second)
		if err != nil {
			t.Fatalf("Allow err: %v", err)
		}
		if !d.Allowed {
			t.Fatalf("request %d: denied, want allowed", i+1)
		}
	}
	d, _ := repo.Allow(ctx, "k", 3, time.Second)
	if d.Allowed {
		t.Fatal("4th request: allowed, want denied")
	}

	// Roll the window over.
	clk = clk.Add(2 * time.Second)
	d, _ = repo.Allow(ctx, "k", 3, time.Second)
	if !d.Allowed {
		t.Fatal("after window roll: denied, want allowed")
	}
}

// TestRegistry_BoundedSize verifies that the registry does NOT grow without
// bound (MANDATORY FIX 2). With a maxKeys cap of N and N+extra distinct keys
// inserted the registry size must stay ≤ N.
func TestRegistry_BoundedSize(t *testing.T) {
	const cap = 10
	const extra = 5

	now := time.Unix(1_700_000_000, 0)
	reg := NewRegistry(5, 5,
		WithClock(func() time.Time { return now }),
		WithMaxKeys(cap),
		WithSweepInterval(time.Hour), // disable sweep; we're testing the hard cap
	)
	defer reg.Close()

	for i := 0; i < cap+extra; i++ {
		reg.Allow(fmt.Sprintf("key-%d", i))
	}

	if got := reg.Len(); got > cap {
		t.Fatalf("registry size after inserting %d keys with cap %d: got %d, want ≤ %d",
			cap+extra, cap, got, cap)
	}
}

// TestRegistry_IdleSweep verifies that a periodic sweep removes limiters whose
// token bucket is full (idle). After all keys have refilled (time advanced
// beyond one full refill period) the sweep should evict all of them.
func TestRegistry_IdleSweep(t *testing.T) {
	// Fixed clock starting at t0; we advance it manually.
	t0 := time.Unix(1_700_000_000, 0)
	clk := t0

	reg := NewRegistry(1, 1,
		WithClock(func() time.Time { return clk }),
		WithSweepInterval(time.Hour), // we call sweepIdle() directly
	)
	defer reg.Close()

	// Insert keys by consuming a token (bucket goes non-full → won't be swept).
	const keys = 5
	for i := 0; i < keys; i++ {
		reg.Allow(fmt.Sprintf("key-%d", i))
	}
	if got := reg.Len(); got != keys {
		t.Fatalf("before sweep: got %d keys, want %d", got, keys)
	}

	// Advance the clock enough for all buckets to fully refill (rps=1, burst=1 →
	// one token refills in 1 second).
	clk = t0.Add(2 * time.Second)

	// Trigger the sweep directly (avoids real-time ticker in tests).
	reg.sweepIdle()

	if got := reg.Len(); got != 0 {
		t.Fatalf("after idle sweep with full buckets: got %d keys, want 0 (all swept)", got)
	}
}

// TestRegistry_LRUEviction verifies that when the hard cap is reached the
// LEAST recently used entry is evicted, not an arbitrary one.
func TestRegistry_LRUEviction(t *testing.T) {
	const cap = 3
	base := time.Unix(1_700_000_000, 0)
	tick := 0
	clk := func() time.Time {
		t := base.Add(time.Duration(tick) * time.Millisecond)
		tick++
		return t
	}

	reg := NewRegistry(5, 5,
		WithClock(clk),
		WithMaxKeys(cap),
		WithSweepInterval(time.Hour),
	)
	defer reg.Close()

	// Insert exactly cap keys in order — each gets a later lastAccess.
	for i := 0; i < cap; i++ {
		reg.Allow(fmt.Sprintf("key-%d", i))
	}

	// "key-0" was accessed first (oldest lastAccess). Inserting one more key
	// should evict "key-0".
	reg.Allow("key-new")

	if got := reg.Len(); got != cap {
		t.Fatalf("after LRU eviction: registry size %d, want %d", got, cap)
	}

	// "key-0" must be gone.
	keys := reg.sortedKeys()
	for _, k := range keys {
		if k == "key-0" {
			t.Fatalf("key-0 was NOT evicted despite being LRU; remaining keys: %v", keys)
		}
	}
	// "key-new" must be present.
	found := false
	for _, k := range keys {
		if k == "key-new" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("key-new not found after insertion; remaining keys: %v", keys)
	}
}

// TestMemoryRepository_Concurrent confirms the shared counter is atomic under
// concurrent access: exactly `limit` of N concurrent calls are allowed.
func TestMemoryRepository_Concurrent(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()
	const limit, total = 50, 200

	var allowed int
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(total)
	for i := 0; i < total; i++ {
		go func() {
			defer wg.Done()
			d, err := repo.Allow(ctx, "k", limit, time.Hour)
			if err == nil && d.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowed != limit {
		t.Fatalf("allowed=%d, want exactly %d", allowed, limit)
	}
}
