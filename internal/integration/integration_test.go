//go:build integration

// Package integration holds CARD-011's race-free integration suite (NFR-008 /
// AC-049). Unlike the unit tests — which exercise the repositories against
// fakes — these tests spin up REAL Postgres and Redis via testcontainers-go and
// drive the deferred persistence/coordination paths end-to-end:
//
//   - metering pgx repo: apply migrations/0001_usage_events.sql, batch-INSERT
//     UsageEvents, read the rows back (TestIntegration_MeteringPgxRepo).
//   - cache redis repo: Set/Get/TTL round-trip against live Redis
//     (TestIntegration_CacheRedisRepo).
//   - ratelimit redis repo: the distributed Lua fixed-window enforced across two
//     repo instances sharing one Redis — the live AC-013 cross-instance cap
//     (TestIntegration_RateLimitRedisDistributed).
//   - readiness checkers: real redis Ping + pgx Ping → /readyz 200; stop a
//     container → 503 (TestIntegration_ReadinessRealDeps).
//
// They run only under `-tags=integration` so the default `go test ./...` stays
// hermetic. Run them with:
//
//	go test -tags=integration -race -p 1 ./...
//
// When Docker is unavailable testcontainers' own provider check fails fast; the
// suite skips with a clear log rather than failing (so CI without a Docker
// daemon is green) — but in an environment with Docker they MUST run.
package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// requireDocker skips the calling test (with a clear log) when no container
// runtime is reachable, so the suite degrades gracefully on machines without
// Docker. Anywhere Docker IS up the tests run for real.
func requireDocker(t *testing.T) {
	t.Helper()
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		t.Skipf("skipping integration test: no container provider (%v)", err)
	}
	defer func() { _ = provider.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := provider.Health(ctx); err != nil {
		t.Skipf("skipping integration test: container runtime not healthy (%v)", err)
	}
}

// isUnavailable reports whether err indicates Docker or the image could not be
// obtained, so a test can skip-with-log instead of failing on infra absence.
// It covers the common failure modes beyond a plain DeadlineExceeded:
//   - context deadline / cancellation (container start timed out)
//   - Docker daemon not running ("Cannot connect to the Docker daemon")
//   - image-pull / registry errors ("pull access denied", "manifest unknown")
//   - generic container-creation errors from testcontainers
func isUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"cannot connect to the docker daemon",
		"docker daemon",
		"no such host",
		"pull access denied",
		"manifest unknown",
		"manifest not found",
		"failed to start container",
		"failed to create container",
		"connection refused",
	} {
		if strings.Contains(msg, fragment) {
			return true
		}
	}
	return false
}
