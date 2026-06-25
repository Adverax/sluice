//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/adverax/sluice/internal/cache"
	"github.com/adverax/sluice/internal/ratelimit"
)

// startRedis boots a throwaway Redis container and returns its connection URL
// plus a teardown.
func startRedis(t *testing.T) (string, func()) {
	t.Helper()
	ctx := context.Background()

	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if isUnavailable(err) {
		t.Skipf("skipping: redis container unavailable (%v)", err)
	}
	if err != nil {
		t.Fatalf("start redis: %v", err)
	}

	uri, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("redis connection string: %v", err)
	}

	teardown := func() { _ = container.Terminate(context.Background()) }
	return uri, teardown
}

func dialRedis(t *testing.T, uri string) *goredis.Client {
	t.Helper()
	opts, err := goredis.ParseURL(uri)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	client := goredis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestIntegration_CacheRedisRepo exercises the deferred cache persistence path
// (COMP-004, FR-005) against REAL Redis: Set a value with a TTL, Get it back,
// confirm the TTL is live, and confirm a missing key is a clean miss (not an
// error). Counterpart to internal/cache/redisrepo_test.go's fake-backed test.
func TestIntegration_CacheRedisRepo(t *testing.T) {
	requireDocker(t)
	uri, teardown := startRedis(t)
	defer teardown()
	client := dialRedis(t, uri)

	repo := cache.NewRedisRepository(client)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const key = "model:hash"
	want := []byte(`{"content":"hello"}`)

	// Miss before write.
	if _, ok, err := repo.Get(ctx, key); err != nil || ok {
		t.Fatalf("pre-write Get: ok=%v err=%v, want clean miss", ok, err)
	}

	if err := repo.Set(ctx, key, want, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, ok, err := repo.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get: ok=false, want hit")
	}
	if string(got) != string(want) {
		t.Fatalf("Get value = %q, want %q", got, want)
	}

	// TTL is set and counting down (raw client confirms PX applied).
	ttl, err := client.TTL(ctx, "cache:"+key).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl <= 0 || ttl > time.Minute {
		t.Fatalf("TTL = %s, want (0, 1m]", ttl)
	}
}

// TestIntegration_RateLimitRedisDistributed proves the DISTRIBUTED token bucket
// (COMP-009, FR-004, AC-015a / AC-013) against REAL Redis: two RedisRepository
// instances sharing ONE Redis enforce a SINGLE shared bucket — the atomic Lua
// token-bucket script makes the refill+decrement race-free so interleaved
// requests from "two gateways" see one consistent balance. We drive the bucket
// with an INJECTED CLOCK (WithClock) so the test is deterministic and needs no
// real sleeps: the elapsed time the script refills against is the clock delta we
// control, not wall-clock.
func TestIntegration_RateLimitRedisDistributed(t *testing.T) {
	requireDocker(t)
	uri, teardown := startRedis(t)
	defer teardown()

	// Shared injected clock: both repos refill against the same controlled time
	// so we can advance "time" without sleeping.
	clk := time.Unix(1_700_000_000, 0)
	var clkMu sync.Mutex
	clock := func() time.Time {
		clkMu.Lock()
		defer clkMu.Unlock()
		return clk
	}
	advance := func(d time.Duration) {
		clkMu.Lock()
		defer clkMu.Unlock()
		clk = clk.Add(d)
	}

	const (
		key    = "tenant-x"
		burst  = 6
		limit  = 6 // rate = 6/s
		window = time.Second
	)

	// Two independent clients → two repo instances, as if two gateway processes
	// shared the same Redis. Both share the burst and clock.
	clientA := dialRedis(t, uri)
	clientB := dialRedis(t, uri)
	repoA := ratelimit.NewRedisRepository(clientA,
		ratelimit.WithBurst(burst), ratelimit.WithRedisClock(clock))
	repoB := ratelimit.NewRedisRepository(clientB,
		ratelimit.WithBurst(burst), ratelimit.WithRedisClock(clock))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	allow := func(i int) (bool, time.Duration) {
		repo := repoA
		if i%2 == 1 {
			repo = repoB
		}
		dec, err := repo.Allow(ctx, key, limit, window)
		if err != nil {
			t.Fatalf("Allow #%d: %v", i, err)
		}
		return dec.Allowed, dec.RetryAfter
	}

	// Phase 1 — immediate burst at t0: the shared bucket starts full (burst
	// tokens). Alternating 2*burst requests across both instances must admit
	// EXACTLY burst, the rest rejected with a positive Retry-After.
	allowed, rejected := 0, 0
	var lastRetryAfter time.Duration
	for i := 0; i < burst*2; i++ {
		ok, ra := allow(i)
		if ok {
			allowed++
		} else {
			rejected++
			lastRetryAfter = ra
		}
	}
	if allowed != burst {
		t.Fatalf("phase 1 burst: allowed = %d, want %d (distributed bucket not shared)", allowed, burst)
	}
	if rejected != burst {
		t.Fatalf("phase 1 burst: rejected = %d, want %d", rejected, burst)
	}
	if lastRetryAfter <= 0 {
		t.Fatalf("Retry-After = %s, want > 0 on rejection", lastRetryAfter)
	}

	// Phase 2 — steady state: advance the shared clock by one full window so
	// `rate` tokens refill (capped at burst). Another 2*burst alternating
	// requests must again admit no more than burst, and at least one.
	advance(window)
	allowed = 0
	for i := 0; i < burst*2; i++ {
		if ok, _ := allow(i); ok {
			allowed++
		}
	}
	if allowed > burst {
		t.Fatalf("phase 2 steady-state: allowed = %d, want <= %d (refill capped at burst)", allowed, burst)
	}
	if allowed == 0 {
		t.Fatal("phase 2 steady-state: allowed = 0 after a full-window refill, want > 0")
	}
}
