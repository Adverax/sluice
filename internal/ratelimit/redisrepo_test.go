package ratelimit

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeScripter is a minimal redis.Scripter that emulates the atomic token-bucket
// Lua script in memory, so the RedisRepository adapter can be unit-tested
// without a live Redis (the real client is integration-tested via testcontainers
// in internal/integration). It supports forcing an error to exercise the
// fail-open path.
//
// The emulation mirrors the Lua exactly: per key it keeps {tokens, ts}, refills
// from the elapsed wall-clock delta passed in ARGV[3] (now_ms), and decrements a
// token when the bucket holds at least one. ARGV[1]=rate, ARGV[2]=burst,
// ARGV[3]=now_ms, ARGV[4]=ttl_ms.
type fakeScripter struct {
	mu      sync.Mutex
	buckets map[string]*fakeBucket
	failed  bool
}

type fakeBucket struct {
	tokens float64
	ts     int64 // milliseconds
}

func newFakeScripter() *fakeScripter {
	return &fakeScripter{buckets: make(map[string]*fakeBucket)}
}

func (f *fakeScripter) Eval(ctx context.Context, _ string, keys []string, args ...any) *redis.Cmd {
	cmd := redis.NewCmd(ctx)
	if f.failed {
		cmd.SetErr(errors.New("redis: connection refused"))
		return cmd
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	rate := toFloat(args[0])
	burst := toFloat(args[1])
	nowMS := toInt64(args[2])

	key := keys[0]
	b, ok := f.buckets[key]
	if !ok {
		b = &fakeBucket{tokens: burst, ts: nowMS}
		f.buckets[key] = b
	}

	elapsedMS := nowMS - b.ts
	if elapsedMS < 0 {
		elapsedMS = 0
	}
	b.tokens = math.Min(burst, b.tokens+(float64(elapsedMS)/1000.0)*rate)

	var allowed, retryAfterMS int64
	if b.tokens >= 1 {
		allowed = 1
		b.tokens--
	} else if rate > 0 {
		retryAfterMS = int64(math.Ceil((1 - b.tokens) / rate * 1000.0))
	}
	b.ts = nowMS

	cmd.SetVal([]any{allowed, retryAfterMS})
	return cmd
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

func (f *fakeScripter) EvalRO(ctx context.Context, s string, keys []string, args ...any) *redis.Cmd {
	return f.Eval(ctx, s, keys, args...)
}

func (f *fakeScripter) EvalSha(ctx context.Context, _ string, keys []string, args ...any) *redis.Cmd {
	// Force the Script.Run fallback to plain EVAL by reporting the sha missing.
	cmd := redis.NewCmd(ctx)
	cmd.SetErr(redis.ErrNoScript)
	return cmd
}

func (f *fakeScripter) EvalShaRO(ctx context.Context, s string, keys []string, args ...any) *redis.Cmd {
	return f.EvalSha(ctx, s, keys, args...)
}

func (f *fakeScripter) ScriptExists(ctx context.Context, _ ...string) *redis.BoolSliceCmd {
	cmd := redis.NewBoolSliceCmd(ctx)
	cmd.SetVal([]bool{false})
	return cmd
}

func (f *fakeScripter) ScriptLoad(ctx context.Context, _ string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx)
	cmd.SetVal("fakesha")
	return cmd
}

var _ redis.Scripter = (*fakeScripter)(nil)

// TestRedisRepository_TokenBucket verifies the adapter implements token-bucket
// semantics against the fake scripter: a full bucket allows up to `burst`
// immediate requests, then denies (with a positive Retry-After) until tokens
// refill at `rate`, after which a request is allowed again. This is the
// distributed global cap (AC-015a / AC-013).
func TestRedisRepository_TokenBucket(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0)
	repo := NewRedisRepository(newFakeScripter(),
		WithBurst(5),
		WithRedisClock(func() time.Time { return clk }),
	)
	ctx := context.Background()

	// rate = limit/window = 5 per second; burst = 5. The bucket starts full.
	const limit = 5
	const window = time.Second

	// Burst: the first `burst` requests at t0 are all allowed.
	for i := 0; i < 5; i++ {
		d, err := repo.Allow(ctx, "k", limit, window)
		if err != nil {
			t.Fatalf("burst request %d: err %v", i+1, err)
		}
		if !d.Allowed {
			t.Fatalf("burst request %d: denied, want allowed", i+1)
		}
	}

	// Bucket now empty: the next request is denied with a positive Retry-After.
	d, err := repo.Allow(ctx, "k", limit, window)
	if err != nil {
		t.Fatalf("post-burst: err %v", err)
	}
	if d.Allowed {
		t.Fatal("post-burst request: allowed, want denied (bucket empty)")
	}
	if d.RetryAfter <= 0 {
		t.Fatal("denied decision must carry a positive RetryAfter")
	}

	// Advance the clock by 1s → 5 tokens refill (rate=5/s), capped at burst=5.
	clk = clk.Add(time.Second)
	for i := 0; i < 5; i++ {
		d, err := repo.Allow(ctx, "k", limit, window)
		if err != nil {
			t.Fatalf("refilled request %d: err %v", i+1, err)
		}
		if !d.Allowed {
			t.Fatalf("refilled request %d: denied, want allowed after refill", i+1)
		}
	}
	// And empty again.
	d, _ = repo.Allow(ctx, "k", limit, window)
	if d.Allowed {
		t.Fatal("after refilled burst: allowed, want denied")
	}
}

// TestRedisRepository_SteadyStateRate verifies that, once the burst is spent,
// requests are admitted at the steady-state `rate`: advancing the clock by one
// token-period (1/rate) refills exactly one token, so exactly one request is
// admitted per period.
func TestRedisRepository_SteadyStateRate(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0)
	repo := NewRedisRepository(newFakeScripter(),
		WithBurst(2),
		WithRedisClock(func() time.Time { return clk }),
	)
	ctx := context.Background()

	const limit = 10 // rate = 10/s → one token every 100ms
	const window = time.Second
	const tokenPeriod = 100 * time.Millisecond

	// Spend the burst (2 tokens).
	for i := 0; i < 2; i++ {
		if d, _ := repo.Allow(ctx, "k", limit, window); !d.Allowed {
			t.Fatalf("burst request %d denied, want allowed", i+1)
		}
	}
	// Empty now.
	if d, _ := repo.Allow(ctx, "k", limit, window); d.Allowed {
		t.Fatal("after burst: allowed, want denied")
	}

	// Each token-period refills exactly one token → exactly one allow per period.
	for i := 0; i < 3; i++ {
		clk = clk.Add(tokenPeriod)
		if d, _ := repo.Allow(ctx, "k", limit, window); !d.Allowed {
			t.Fatalf("steady-state period %d: denied, want one allow per period", i+1)
		}
		// A second immediate request in the same instant is denied (only one token).
		if d, _ := repo.Allow(ctx, "k", limit, window); d.Allowed {
			t.Fatalf("steady-state period %d: second request allowed, want denied", i+1)
		}
	}
}

// TestRateLimit_DistributedTokenBucket proves the DISTRIBUTED token bucket is
// SHARED across instances (AC-015a / AC-013): two RedisRepository instances
// pointing at ONE backing store (the shared fakeScripter, standing in for one
// Redis) enforce a single bucket. Alternating requests across the two "gateways"
// must allow no more than `burst` immediately, then admit at the steady-state
// `rate` — never more than burst + (elapsed * rate) in total.
func TestRateLimit_DistributedTokenBucket(t *testing.T) {
	shared := newFakeScripter() // one Redis, shared by both instances.
	clk := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return clk }

	const (
		burst  = 6
		limit  = 6 // rate = 6/s
		window = time.Second
	)
	repoA := NewRedisRepository(shared, WithBurst(burst), WithRedisClock(clock))
	repoB := NewRedisRepository(shared, WithBurst(burst), WithRedisClock(clock))
	ctx := context.Background()

	const key = "tenant-x"

	allow := func(i int) bool {
		repo := repoA
		if i%2 == 1 {
			repo = repoB
		}
		d, err := repo.Allow(ctx, key, limit, window)
		if err != nil {
			t.Fatalf("Allow #%d: %v", i, err)
		}
		return d.Allowed
	}

	// At t0 the shared bucket holds `burst` tokens. Fire 2*burst alternating
	// requests across both instances: exactly `burst` may pass (the rest denied),
	// proving the cap is GLOBAL and not per-instance.
	allowed := 0
	for i := 0; i < burst*2; i++ {
		if allow(i) {
			allowed++
		}
	}
	if allowed != burst {
		t.Fatalf("immediate burst across two instances: allowed=%d, want exactly %d (cap not shared)", allowed, burst)
	}

	// Steady state: advance the clock by one full window → `rate` tokens refill
	// (capped at burst). Another 2*burst alternating requests must again admit no
	// more than `burst`.
	clk = clk.Add(window)
	allowed = 0
	for i := 0; i < burst*2; i++ {
		if allow(i) {
			allowed++
		}
	}
	if allowed > burst {
		t.Fatalf("steady-state across two instances: allowed=%d, want <= %d (refill capped at burst)", allowed, burst)
	}
	if allowed == 0 {
		t.Fatal("steady-state: allowed=0 after a full-window refill, want > 0")
	}
}

// TestRedisRepository_BackendError verifies a backend failure surfaces as an
// error (which the middleware treats as fail-open).
func TestRedisRepository_BackendError(t *testing.T) {
	fs := newFakeScripter()
	fs.failed = true
	repo := NewRedisRepository(fs)

	_, err := repo.Allow(context.Background(), "k", 5, time.Second)
	if err == nil {
		t.Fatal("expected error on backend failure, got nil")
	}
}
