package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// tokenBucketScript is an atomic Redis token-bucket limiter (spec: "token
// bucket … + Redis"). Per key it stores a hash {tokens, ts} where tokens is the
// fractional token balance and ts is the last-refill time in milliseconds.
//
// On each call it:
//  1. reads the stored {tokens, ts} (defaulting to a full bucket on first use),
//  2. refills tokens = min(burst, tokens + elapsed_seconds * rate) using the
//     wall-clock delta since ts,
//  3. allows the request iff tokens >= 1, decrementing one token,
//  4. persists {tokens, now} with a TTL so idle keys self-evict.
//
// Doing the read-refill-decrement-persist in ONE Lua script makes the whole
// check atomic across instances — there is no read-modify-write race, so N
// gateways sharing one Redis enforce a SINGLE shared bucket (AC-015a / AC-013).
//
// `now` is passed from Go (ARGV) rather than read via redis.call('TIME') so the
// algorithm is deterministic and testable with an injected clock (the fake-
// backed unit test and the integration test both drive it explicitly). The
// trade-off is that bucket refill follows the caller's clock; for a single
// gateway fleet with NTP-synced clocks this is fine, and it buys reproducible
// tests over per-server monotonicity.
//
// KEYS[1] = bucket key
// ARGV[1] = rate    (tokens per second, float)
// ARGV[2] = burst   (bucket capacity, integer)
// ARGV[3] = now_ms  (caller's clock in milliseconds, integer)
// ARGV[4] = ttl_ms  (key expiry in milliseconds, integer)
//
// Returns {allowed (1|0), retry_after_ms}. retry_after_ms is 0 when allowed and
// otherwise the time until one token refills.
var tokenBucketScript = redis.NewScript(`
local rate    = tonumber(ARGV[1])
local burst   = tonumber(ARGV[2])
local now_ms  = tonumber(ARGV[3])
local ttl_ms  = tonumber(ARGV[4])

local data    = redis.call("HMGET", KEYS[1], "tokens", "ts")
local tokens  = tonumber(data[1])
local ts      = tonumber(data[2])

if tokens == nil or ts == nil then
	-- First sighting of this key: start with a full bucket.
	tokens = burst
	ts     = now_ms
end

-- Refill based on elapsed wall-clock since the last update (never negative).
local elapsed_ms = now_ms - ts
if elapsed_ms < 0 then
	elapsed_ms = 0
end
tokens = math.min(burst, tokens + (elapsed_ms / 1000.0) * rate)

local allowed = 0
local retry_after_ms = 0
if tokens >= 1 then
	allowed = 1
	tokens = tokens - 1
else
	-- Time until the bucket holds one whole token again.
	if rate > 0 then
		retry_after_ms = math.ceil((1 - tokens) / rate * 1000.0)
	end
end

redis.call("HSET", KEYS[1], "tokens", tokens, "ts", now_ms)
redis.call("PEXPIRE", KEYS[1], ttl_ms)

return {allowed, retry_after_ms}
`)

// RedisRepository is the go-redis/v9 implementation of RateLimitRepository
// (COMP-009, ADR-0010). It enforces a global TOKEN BUCKET per key atomically via
// a Lua script, shared across all instances pointing at the same Redis.
//
// It depends on redis.Scripter (the narrow run-a-script surface) rather than the
// concrete *redis.Client, keeping the ACL boundary explicit and the adapter
// substitutable; *redis.Client, *redis.Ring and *redis.ClusterClient all
// satisfy it (ADR-0010).
type RedisRepository struct {
	client    redis.Scripter
	keyPrefix string
	burst     int
	now       func() time.Time
}

// RedisOption configures a RedisRepository.
type RedisOption func(*RedisRepository)

// WithKeyPrefix overrides the Redis key namespace (default "ratelimit:").
func WithKeyPrefix(prefix string) RedisOption {
	return func(r *RedisRepository) {
		if prefix != "" {
			r.keyPrefix = prefix
		}
	}
}

// WithBurst sets the token-bucket capacity (the maximum momentary burst). It
// reuses GATEWAY_RATELIMIT_BURST in production. A value <= 0 is ignored, in
// which case the burst defaults to the per-call `limit` (steady-state == burst).
func WithBurst(burst int) RedisOption {
	return func(r *RedisRepository) {
		if burst > 0 {
			r.burst = burst
		}
	}
}

// WithRedisClock injects the clock used for token refill. It defaults to
// time.Now; tests pass a fake clock for deterministic, sleep-free token
// accounting. (Named distinctly from the Registry's WithClock since both live in
// this package.)
func WithRedisClock(now func() time.Time) RedisOption {
	return func(r *RedisRepository) {
		if now != nil {
			r.now = now
		}
	}
}

// NewRedisRepository builds the go-redis-backed distributed limiter. The client
// is injected (no globals, ADR-0008); it is the same *redis.Client constructed
// in cmd/gateway and shared with the health checker.
func NewRedisRepository(client redis.Scripter, opts ...RedisOption) *RedisRepository {
	r := &RedisRepository{
		client:    client,
		keyPrefix: "ratelimit:",
		now:       time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Allow runs the atomic token-bucket check in Redis. The `limit`/`window` pair
// defines the steady-state refill RATE (limit tokens per window); the bucket
// CAPACITY (burst) comes from WithBurst, defaulting to `limit` when unset so a
// caller that only knows the per-window limit still gets sensible behaviour.
//
// On a backend error it returns the error (the middleware fails open to the
// local limiter). The context carries the caller's deadline so the Redis call is
// bounded (NFR-004).
func (r *RedisRepository) Allow(ctx context.Context, key string, limit int, window time.Duration) (Decision, error) {
	redisKey := r.keyPrefix + key

	if window <= 0 {
		window = time.Second
	}
	// rate = tokens per second.
	rate := float64(limit) / window.Seconds()

	burst := r.burst
	if burst <= 0 {
		burst = limit
	}
	if burst <= 0 {
		burst = 1
	}

	nowMS := r.now().UnixMilli()
	// TTL: a few refill windows so an idle key self-evicts but an active key is
	// never reclaimed mid-use. One refill window is the time to fill the whole
	// bucket; 5× that is a safe, generous margin.
	ttlMS := window.Milliseconds() * 5
	if ttlMS <= 0 {
		ttlMS = time.Second.Milliseconds() * 5
	}

	res, err := tokenBucketScript.Run(ctx, r.client, []string{redisKey},
		rate, burst, nowMS, ttlMS).Result()
	if err != nil {
		return Decision{}, fmt.Errorf("ratelimit: redis token-bucket: %w", err)
	}

	vals, ok := res.([]any)
	if !ok || len(vals) != 2 {
		return Decision{}, fmt.Errorf("ratelimit: redis token-bucket: unexpected reply %T", res)
	}
	allowed, _ := vals[0].(int64)
	retryAfterMS, _ := vals[1].(int64)

	if allowed == 1 {
		return Decision{Allowed: true}, nil
	}

	retryAfter := time.Duration(retryAfterMS) * time.Millisecond
	if retryAfter <= 0 {
		// Fall back to one refill period (1/rate) when the script could not derive
		// a hint (e.g. rate == 0).
		if rate > 0 {
			retryAfter = time.Duration(float64(time.Second) / rate)
		} else {
			retryAfter = window
		}
	}
	return Decision{Allowed: false, RetryAfter: retryAfter}, nil
}

var _ RateLimitRepository = (*RedisRepository)(nil)
