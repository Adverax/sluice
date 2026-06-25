//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/adverax/sluice/internal/health"
	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/proxy"
	"github.com/adverax/sluice/internal/server"
)

// TestIntegration_ReadinessRealDeps wires the REAL redis + pgx readiness
// checkers (FR-009) into the generated /readyz handler and proves the live
// happy path and the degraded path:
//
//   - both containers up   → GET /readyz == 200, deps redis:ok postgres:ok.
//   - stop the redis container → GET /readyz == 503, deps redis:<down reason>.
//
// This is the integration counterpart to internal/health/checkers_test.go's
// fake-pinger tests: here the Ping actually crosses the network to live deps.
func TestIntegration_ReadinessRealDeps(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()

	// --- Postgres (stays up the whole test) ---
	pg, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("sluice"),
		postgres.WithUsername("app"),
		postgres.WithPassword("app"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if isUnavailable(err) {
		t.Skipf("skipping: postgres unavailable (%v)", err)
	}
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	defer func() { _ = pg.Terminate(context.Background()) }()

	pgDSN, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("pg dsn: %v", err)
	}
	pgPool, err := pgxpool.New(ctx, pgDSN)
	if err != nil {
		t.Fatalf("pgx pool: %v", err)
	}
	defer pgPool.Close()

	// --- Redis (we will stop it to force the degraded verdict) ---
	rc, err := tcredis.Run(ctx, "redis:7-alpine")
	if isUnavailable(err) {
		t.Skipf("skipping: redis unavailable (%v)", err)
	}
	if err != nil {
		t.Fatalf("start redis: %v", err)
	}
	defer func() { _ = rc.Terminate(context.Background()) }()

	redisURI, err := rc.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("redis uri: %v", err)
	}
	rOpts, err := redis.ParseURL(redisURI)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	// Keep timeouts short so a stopped container fails the ping quickly.
	rOpts.DialTimeout = 2 * time.Second
	rOpts.ReadTimeout = 2 * time.Second
	redisClient := redis.NewClient(rOpts)
	defer func() { _ = redisClient.Close() }()

	// Build the server with real readiness checkers wired into /readyz.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hh := health.New(logger, 3*time.Second)
	hh.Register(
		health.NewRedisChecker(redisClient),
		health.NewPostgresChecker(pgPool),
	)
	router := proxy.NewRouter()
	router.Register("mock", provider.New())
	srv := server.New(router, hh, logger)
	ts := httptest.NewServer(srv.Handler(http.NewServeMux()))
	defer ts.Close()

	// Happy path: both deps up → 200.
	status, body := getReadyz(t, ts.URL)
	if status != http.StatusOK {
		t.Fatalf("readyz (deps up) status = %d body=%v, want 200", status, body)
	}
	if body["dependencies"]["redis"] != "ok" || body["dependencies"]["postgres"] != "ok" {
		t.Fatalf("deps up: dependencies = %v, want redis:ok postgres:ok", body["dependencies"])
	}

	// Degraded path: stop Redis, then /readyz must report 503 with redis down.
	if err := rc.Stop(context.Background(), nil); err != nil {
		t.Fatalf("stop redis container: %v", err)
	}
	status, body = getReadyz(t, ts.URL)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("readyz (redis down) status = %d body=%v, want 503", status, body)
	}
	if body["dependencies"]["redis"] == "ok" {
		t.Fatalf("redis down: dependencies = %v, want redis reported down", body["dependencies"])
	}
	if body["dependencies"]["postgres"] != "ok" {
		t.Fatalf("postgres should still be ok: %v", body["dependencies"])
	}
}

// getReadyz GETs /readyz and decodes the {status, dependencies{}} body.
func getReadyz(t *testing.T, base string) (int, map[string]map[string]string) {
	t.Helper()
	resp, err := http.Get(base + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var raw struct {
		Status       string            `json:"status"`
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode readyz body: %v", err)
	}
	return resp.StatusCode, map[string]map[string]string{"dependencies": raw.Dependencies}
}
