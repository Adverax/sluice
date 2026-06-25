//go:build integration

package integration

import (
	"context"
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

// TestIntegration_RateLimitRedisDistributed proves the DISTRIBUTED fixed-window
// cap (COMP-009, FR-004, AC-013): two RedisRepository instances sharing ONE
// Redis must enforce a SINGLE global counter — the Lua INCR+PEXPIRE+PTTL script
// makes the increment atomic so interleaved requests from "two gateways" see a
// consistent count. We alternate the two instances against the same key and
// assert exactly `limit` requests are allowed within the window, the rest
// rejected with a positive Retry-After.
func TestIntegration_RateLimitRedisDistributed(t *testing.T) {
	requireDocker(t)
	uri, teardown := startRedis(t)
	defer teardown()

	// Two independent clients → two repo instances, as if two gateway processes
	// shared the same Redis.
	clientA := dialRedis(t, uri)
	clientB := dialRedis(t, uri)
	repoA := ratelimit.NewRedisRepository(clientA)
	repoB := ratelimit.NewRedisRepository(clientB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const (
		key    = "tenant-x"
		limit  = 6
		window = 5 * time.Second
	)

	allowed := 0
	rejected := 0
	var lastRetryAfter time.Duration
	for i := 0; i < limit*2; i++ {
		// Alternate between the two "gateways" to prove the shared global cap.
		repo := repoA
		if i%2 == 1 {
			repo = repoB
		}
		dec, err := repo.Allow(ctx, key, limit, window)
		if err != nil {
			t.Fatalf("Allow #%d: %v", i, err)
		}
		if dec.Allowed {
			allowed++
		} else {
			rejected++
			lastRetryAfter = dec.RetryAfter
		}
	}

	if allowed != limit {
		t.Fatalf("allowed = %d, want %d (distributed cap not shared)", allowed, limit)
	}
	if rejected != limit {
		t.Fatalf("rejected = %d, want %d", rejected, limit)
	}
	if lastRetryAfter <= 0 {
		t.Fatalf("Retry-After = %s, want > 0 on rejection", lastRetryAfter)
	}
}
