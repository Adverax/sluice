// Package cache implements the response cache repository (COMP-004, FR-005) per
// ADR-0004 (default 5-minute TTL with a per-request X-Cache-TTL override) and
// ADR-0010 (the repository is a port; the concrete store — Redis — is injected).
//
// The CacheRepository interface is the anti-corruption layer (ACL) between the
// cache middleware (internal/middleware/cache.go) and go-redis/v9: the
// middleware depends ONLY on this interface, never on *redis.Client, so the
// backing store is substitutable and the middleware is unit-testable with an
// in-memory fake.
//
// The go-redis/v9 adapter (RedisRepository) lives in redisrepo.go.
package cache

import (
	"context"
	"time"
)

// CacheRepository is the port the cache middleware depends on (ADR-0010). It is
// a small key/value store with per-entry TTL.
//
//   - Get returns (value, true, nil) on a hit, (nil, false, nil) on a miss, and
//     (nil, false, err) when the backing store fails. The middleware treats any
//     error as a cache bypass (fall through to the live handler), so a store
//     outage never propagates to the client (AC-017).
//   - Set stores value under key with the given ttl. A non-nil error is logged
//     and ignored by the middleware (best-effort caching).
type CacheRepository interface {
	Get(ctx context.Context, key string) (value []byte, found bool, err error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}
