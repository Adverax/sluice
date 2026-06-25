package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisRepository is the go-redis/v9 implementation of CacheRepository (COMP-004,
// ADR-0010). It stores raw response bytes under a namespaced key with a per-entry
// TTL (SET key value PX ttl) and reads them back (GET key).
//
// It depends on the narrow redis.Cmdable surface rather than the concrete
// *redis.Client, keeping the ACL boundary explicit and the adapter
// substitutable; *redis.Client, *redis.Ring and *redis.ClusterClient all satisfy
// it (ADR-0010). The client is injected (no globals, ADR-0008) — it is the same
// *redis.Client constructed in cmd/gateway and shared with the rate limiter and
// health checker.
type RedisRepository struct {
	client    redis.Cmdable
	keyPrefix string
}

// RedisOption configures a RedisRepository.
type RedisOption func(*RedisRepository)

// WithKeyPrefix overrides the Redis key namespace (default "cache:").
func WithKeyPrefix(prefix string) RedisOption {
	return func(r *RedisRepository) {
		if prefix != "" {
			r.keyPrefix = prefix
		}
	}
}

// NewRedisRepository builds the go-redis-backed cache repository.
func NewRedisRepository(client redis.Cmdable, opts ...RedisOption) *RedisRepository {
	r := &RedisRepository{
		client:    client,
		keyPrefix: "cache:",
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Get reads the cached value. A missing key (redis.Nil) is a clean miss
// (nil, false, nil), not an error. Any other backend error is returned so the
// middleware can fall through to the live handler (AC-017). The context carries
// the caller's deadline so the Redis call is bounded (NFR-004).
func (r *RedisRepository) Get(ctx context.Context, key string) ([]byte, bool, error) {
	val, err := r.client.Get(ctx, r.keyPrefix+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cache: redis get: %w", err)
	}
	return val, true, nil
}

// Set stores value under key with the given TTL. A non-positive TTL is rejected
// so a caller cannot accidentally store a non-expiring entry (which Redis treats
// as "no expiry"); the middleware always passes a positive TTL.
func (r *RedisRepository) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("cache: redis set: ttl must be > 0, got %s", ttl)
	}
	if err := r.client.Set(ctx, r.keyPrefix+key, value, ttl).Err(); err != nil {
		return fmt.Errorf("cache: redis set: %w", err)
	}
	return nil
}

var _ CacheRepository = (*RedisRepository)(nil)
