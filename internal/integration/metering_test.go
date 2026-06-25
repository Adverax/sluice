//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/adverax/sluice/internal/metering"
)

// startPostgres boots a throwaway Postgres container and returns a connected
// pgx pool plus a teardown. The caller is responsible for applying migrations.
func startPostgres(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("sluice"),
		postgres.WithUsername("app"),
		postgres.WithPassword("app"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if isUnavailable(err) {
		t.Skipf("skipping: postgres container unavailable (%v)", err)
	}
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx pool: %v", err)
	}

	teardown := func() {
		pool.Close()
		_ = container.Terminate(context.Background())
	}
	return pool, teardown
}

// applyMigrations runs migrations/0001_usage_events.sql against the pool so the
// usage_events table the pgx repo writes to actually exists.
func applyMigrations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	// Resolve the repo-root migrations dir relative to this test file.
	wd, err := os.Getwd() // .../internal/integration
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoRoot := filepath.Join(wd, "..", "..")
	sqlBytes, err := os.ReadFile(filepath.Join(repoRoot, "migrations", "0001_usage_events.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
}

// TestIntegration_MeteringPgxRepo proves the deferred metering persistence path
// (COMP-018, FR-014) against a REAL Postgres: apply the migration, batch-INSERT
// a set of UsageEvents through the pgx repository, then read the rows back and
// assert they landed with the right columns. This is the integration counterpart
// to the fake-backed unit test in internal/metering/pgxrepo_test.go.
func TestIntegration_MeteringPgxRepo(t *testing.T) {
	requireDocker(t)
	pool, teardown := startPostgres(t)
	defer teardown()
	applyMigrations(t, pool)

	repo := metering.NewPgxRepository(pool)

	now := time.Now().UTC().Truncate(time.Microsecond)
	events := []metering.UsageEvent{
		{
			Provider: "mock", Model: "mock-v1",
			PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
			Latency: 12 * time.Millisecond, Status: 200,
			RequestID: "req-1", Timestamp: now,
		},
		{
			Provider: "mock", Model: "mock-v1",
			PromptTokens: 20, CompletionTokens: 8, TotalTokens: 28,
			Latency: 7 * time.Millisecond, Status: 200,
			RequestID: "req-2", Timestamp: now.Add(time.Second),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := repo.Flush(ctx, events); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM usage_events`).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != len(events) {
		t.Fatalf("row count = %d, want %d", count, len(events))
	}

	// Verify a representative row round-trips with the right columns.
	var (
		provider, model, requestID string
		prompt, completion, total  int
		latencyMS                  int64
		status                     int
	)
	row := pool.QueryRow(ctx,
		`SELECT provider, model, prompt_tokens, completion_tokens, total_tokens, latency_ms, status, request_id
		 FROM usage_events WHERE request_id = $1`, "req-1")
	if err := row.Scan(&provider, &model, &prompt, &completion, &total, &latencyMS, &status, &requestID); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if provider != "mock" || model != "mock-v1" || prompt != 10 || completion != 5 || total != 15 {
		t.Fatalf("row mismatch: provider=%q model=%q p=%d c=%d t=%d", provider, model, prompt, completion, total)
	}
	if latencyMS != 12 || status != 200 {
		t.Fatalf("row mismatch: latency_ms=%d status=%d", latencyMS, status)
	}
}
