package ratelimit

import (
	"context"
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
