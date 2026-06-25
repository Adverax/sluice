# internal/config

Loads and validates gateway configuration from environment variables (ADR-0003).
All values have sane defaults so the service boots without any env vars set.
If a variable is set but malformed or `<= 0`, `Load` returns an error immediately
(fail-loud, NFR-004); unset variables silently use the default.

## Key types

| Type | Description |
|------|-------------|
| `Config` | Fully-resolved configuration; root of the DI graph. |
| `Server` | Listen address and inbound HTTP timeouts. |
| `Upstream` | Outbound HTTP client timeout (proxy wired in CARD-002). |
| `Redis` | Connection URL and dial/read timeouts (client wired in CARD-003). |
| `Postgres` | DSN and pool-acquire timeout (pool wired in CARD-003). |
| `Logging` | Level and format for the structured logger. |

## Key function

`Load() (*Config, error)` — reads env, applies defaults, calls `Validate()`.

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `GATEWAY_ADDR` | `:8080` | Listen address |
| `GATEWAY_READ_TIMEOUT` | `5s` | HTTP read timeout |
| `GATEWAY_WRITE_TIMEOUT` | `10s` | HTTP write timeout |
| `GATEWAY_IDLE_TIMEOUT` | `120s` | Keep-alive idle timeout |
| `GATEWAY_SHUTDOWN_TIMEOUT` | `30s` | Graceful drain budget |
| `GATEWAY_UPSTREAM_TIMEOUT` | `30s` | Upstream HTTP client timeout |
| `GATEWAY_REDIS_URL` | `redis://localhost:6379` | Redis connection string |
| `GATEWAY_REDIS_DIAL_TIMEOUT` | `5s` | Redis dial timeout |
| `GATEWAY_REDIS_READ_TIMEOUT` | `3s` | Redis read timeout |
| `GATEWAY_DB_DSN` | `postgres://app:app@localhost:5432/sluice?sslmode=disable` | Postgres DSN |
| `GATEWAY_DB_ACQUIRE_TIMEOUT` | `5s` | Postgres pool-acquire timeout |
| `GATEWAY_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `GATEWAY_LOG_FORMAT` | `json` | `json` (production) or `text` (local dev) |
| `GATEWAY_HEALTH_CHECK_TIMEOUT` | `3s` | Per-check deadline for `/readyz` dependency checks |
| `GATEWAY_WORKER_POOL_SIZE` | `100` | Worker pool size (CARD-008) |

Duration values use Go's `time.ParseDuration` syntax (e.g. `5s`, `1m30s`).
