package health

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// RedisPinger is the minimal slice of the go-redis client the readiness check
// needs. *redis.Client satisfies it; tests can substitute a fake. Keeping the
// dependency narrow (interface segregation) means the checker never imports
// more of go-redis than it uses.
type RedisPinger interface {
	Ping(ctx context.Context) *redis.StatusCmd
}

// NewRedisChecker builds a readiness Checker named "redis" that pings the given
// go-redis client (FR-009). A failed ping surfaces as the dependency reason in
// the /readyz body (AC-027 redis:down). The client's own dial/read timeouts
// (config, NFR-004) plus the readiness per-check timeout bound the ping.
func NewRedisChecker(client RedisPinger) Checker {
	return CheckerFunc{
		CheckerName: "redis",
		CheckFunc: func(ctx context.Context) error {
			if err := client.Ping(ctx).Err(); err != nil {
				return fmt.Errorf("redis ping: %w", err)
			}
			return nil
		},
	}
}

// PostgresPinger is the minimal slice of the pgx pool the readiness check needs.
// *pgxpool.Pool satisfies it; tests can substitute a fake.
type PostgresPinger interface {
	Ping(ctx context.Context) error
}

// Compile-time proofs that the real clients satisfy the narrow ping ports.
var (
	_ RedisPinger    = (*redis.Client)(nil)
	_ PostgresPinger = (*pgxpool.Pool)(nil)
)

// NewPostgresChecker builds a readiness Checker named "postgres" that pings the
// given pgx pool (FR-009). A failed ping surfaces as the dependency reason in
// the /readyz body (AC-028 postgres:down). The pool's acquire timeout (config,
// NFR-004) plus the readiness per-check timeout bound the ping.
func NewPostgresChecker(pool PostgresPinger) Checker {
	return CheckerFunc{
		CheckerName: "postgres",
		CheckFunc: func(ctx context.Context) error {
			if err := pool.Ping(ctx); err != nil {
				return fmt.Errorf("postgres ping: %w", err)
			}
			return nil
		},
	}
}
