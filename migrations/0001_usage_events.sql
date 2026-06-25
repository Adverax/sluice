-- 0001_usage_events.sql — COMP-018 MeteringRepository target table (FR-014).
-- Async usage metering persists one row per completed inference here. The
-- columns mirror metering.UsageEvent / the INSERT in internal/metering/pgxrepo.go.
-- CARD-011 (make up) applies this migration; the live pgx INSERT is
-- integration-tested there via testcontainers.

CREATE TABLE IF NOT EXISTS usage_events (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider          TEXT        NOT NULL,
    model             TEXT        NOT NULL,
    prompt_tokens     INTEGER     NOT NULL,
    completion_tokens INTEGER     NOT NULL,
    total_tokens      INTEGER     NOT NULL,
    latency_ms        BIGINT      NOT NULL,
    status            INTEGER     NOT NULL,
    request_id        TEXT        NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL
);

-- Time-ordered reads (usage reports, dashboards) over the metering table.
CREATE INDEX IF NOT EXISTS usage_events_created_at_idx ON usage_events (created_at);
