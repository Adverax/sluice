package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// fixedWindowScript is an atomic fixed-window counter. It INCRs the per-key
// counter and, on the first increment of a window, sets the window TTL. It
// returns the post-increment count and the remaining TTL (ms) so the caller can
// derive a Retry-After hint. Running INCR+PEXPIRE+PTTL in one Lua script makes
// the check atomic across instances (no read-modify-write race) — this is what
// enforces the GLOBAL cap when several gateways share one Redis (AC-013).
//
// KEYS[1] = counter key
// ARGV[1] = window in milliseconds
var fixedWindowScript = redis.NewScript(`
local current = redis.call("INCR", KEYS[1])
if current == 1 then
	redis.call("PEXPIRE", KEYS[1], ARGV[1])
end
local ttl = redis.call("PTTL", KEYS[1])
return {current, ttl}
`)

// RedisRepository is the go-redis/v9 implementation of RateLimitRepository
// (COMP-009, ADR-0010). It enforces a global fixed-window cap per key atomically
// via a Lua script, shared across all instances pointing at the same Redis.
//
// It depends on redis.Scripter (the narrow run-a-script surface) rather than the
// concrete *redis.Client, keeping the ACL boundary explicit and the adapter
// substitutable; *redis.Client, *redis.Ring and *redis.ClusterClient all
// satisfy it (ADR-0010).
type RedisRepository struct {
	client    redis.Scripter
	keyPrefix string
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

// NewRedisRepository builds the go-redis-backed distributed limiter. The client
// is injected (no globals, ADR-0008); it is the same *redis.Client constructed
// in cmd/gateway and shared with the health checker.
func NewRedisRepository(client redis.Scripter, opts ...RedisOption) *RedisRepository {
	r := &RedisRepository{
		client:    client,
		keyPrefix: "ratelimit:",
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Allow runs the atomic fixed-window check in Redis. On a backend error it
// returns the error (the middleware fails open to the local limiter). The
// context carries the caller's deadline so the Redis call is bounded (NFR-004).
func (r *RedisRepository) Allow(ctx context.Context, key string, limit int, window time.Duration) (Decision, error) {
	redisKey := r.keyPrefix + key
	windowMS := window.Milliseconds()
	if windowMS <= 0 {
		windowMS = time.Second.Milliseconds()
	}

	res, err := fixedWindowScript.Run(ctx, r.client, []string{redisKey}, windowMS).Result()
	if err != nil {
		return Decision{}, fmt.Errorf("ratelimit: redis fixed-window: %w", err)
	}

	vals, ok := res.([]any)
	if !ok || len(vals) != 2 {
		return Decision{}, fmt.Errorf("ratelimit: redis fixed-window: unexpected reply %T", res)
	}
	count, _ := vals[0].(int64)
	ttlMS, _ := vals[1].(int64)

	if int(count) > limit {
		retryAfter := time.Duration(ttlMS) * time.Millisecond
		if retryAfter <= 0 {
			retryAfter = window
		}
		return Decision{Allowed: false, RetryAfter: retryAfter}, nil
	}
	return Decision{Allowed: true}, nil
}

var _ RateLimitRepository = (*RedisRepository)(nil)
