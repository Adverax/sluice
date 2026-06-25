# 01 — Health and readiness

> Component **COMP-012** · code `internal/health/**.go`, served via `internal/server/server.go` · capability CAP-004 · FR-008, FR-009.

## Why two endpoints, not one

An orchestrator asks two different questions about a process, and conflating them
causes outages. *Liveness* — "is this process wedged and in need of a restart?" —
must answer from the process alone; if it depended on Redis or Postgres, a brief
database blip would make Kubernetes kill and reschedule otherwise-healthy gateway
pods, amplifying the failure. *Readiness* — "should this pod receive traffic right
now?" — is exactly where the dependency check belongs: a pod that cannot reach its
backing stores should be pulled from the load-balancer rotation without being
killed.

`sluice` therefore ships two endpoints with deliberately different semantics:

- `GET /healthz` — liveness, **always 200** while the process runs.
- `GET /readyz` — readiness, **200** when every dependency is reachable, **503**
  otherwise, with a per-dependency reason map.

## 1. Liveness: `/healthz` is unconditional

The liveness handler does no work beyond confirming the goroutine that serves it is
alive. In `internal/server/server.go`:

```go
// GetHealthz implements the liveness probe (FR-008 / AC-025): always 200 with
// {"status":"ok"}.
func (s *Server) GetHealthz(_ context.Context, _ api.GetHealthzRequestObject) (api.GetHealthzResponseObject, error) {
	return api.GetHealthz200JSONResponse{Status: "ok"}, nil
}
```

There is no dependency call, no timeout, no branch — by construction it cannot
return anything but 200 while the process can route the request. (The `health.Handler`
also carries an equivalent `Live` method writing `{"status":"ok"}`, used when the
probe is served outside the generated server seam.)

## 2. Readiness: a small `Checker` port

Readiness is built on a narrow port so the health package never imports Redis or
Postgres directly. From `internal/health/health.go`:

```go
type Checker interface {
	// Name identifies the dependency in the readiness response.
	Name() string
	// Check returns nil when the dependency is healthy, or an error describing
	// why it is not.
	Check(ctx context.Context) error
}
```

The two real checkers live in `internal/health/checkers.go`. Each depends only on a
minimal "pinger" interface (`*redis.Client` and `*pgxpool.Pool` satisfy them; tests
substitute fakes), so the readiness logic stays decoupled from the concrete clients:

```go
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
```

`NewPostgresChecker` is structurally identical, named `"postgres"`, wrapping
`pool.Ping(ctx)`. The checker *name* (`"redis"`, `"postgres"`) becomes the key in
the `/readyz` body, and the error string becomes that key's value when the ping
fails — this is what surfaces as `redis:down` / `postgres:down` to the operator.

## 3. Evaluating checks: concurrent, individually bounded

`Handler.Evaluate` runs every registered checker **concurrently**, each under its own
deadline derived from `h.timeout`, so one slow dependency cannot starve the others
or blow the whole probe budget. From `internal/health/health.go`:

```go
func (h *Handler) Evaluate(ctx context.Context) Result {
	h.mu.RLock()
	checkers := make([]Checker, len(h.checkers))
	copy(checkers, h.checkers)
	h.mu.RUnlock()

	if len(checkers) == 0 {
		return Result{Healthy: true, Dependencies: map[string]string{}}
	}

	// Per-check timeout: each goroutine gets its own bounded deadline so one
	// slow checker cannot consume the entire budget at the expense of others.
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	results := make(chan checkResult, len(checkers))
	for _, c := range checkers {
		c := c // capture for goroutine
		go func() {
			results <- checkResult{name: c.Name(), err: c.Check(ctx)}
		}()
	}

	deps := make(map[string]string, len(checkers))
	healthy := true
	for range checkers {
		r := <-results
		if r.err != nil {
			healthy = false
			deps[r.name] = r.err.Error()
			h.logger.LogAttrs(ctx, slog.LevelWarn, "readiness check failed",
				slog.String("dependency", r.name),
				slog.String("error", r.err.Error()),
			)
		} else {
			deps[r.name] = "ok"
		}
	}

	return Result{Healthy: healthy, Dependencies: deps}
}
```

Three properties matter operationally:

- **Healthy is all-or-nothing.** `healthy` starts `true` and is flipped `false` by
  *any* failing checker. One down dependency makes the whole probe report `503`.
- **The result is transport-agnostic.** `Evaluate` returns a `Result{Healthy,
  Dependencies}`; the HTTP layer maps it to a status code. The same verdict is used
  no matter which handler serves it.
- **A failing check is logged at WARN.** Readiness failures emit a structured slog
  line (`"readiness check failed"` with `dependency` and `error`) so an operator can
  correlate a 503 with the underlying ping error. The timeout defaults to 2s when
  `New` is given a non-positive value; the running gateway passes
  `cfg.HealthCheckTimeout` (a dedicated, independently tunable budget — *not* the
  Redis dial timeout).

## 4. From `Result` to HTTP status

The generated server seam maps the verdict in `internal/server/server.go`:

```go
func (s *Server) GetReadyz(ctx context.Context, _ api.GetReadyzRequestObject) (api.GetReadyzResponseObject, error) {
	res := s.health.Evaluate(ctx)

	if res.Healthy {
		return api.GetReadyz200JSONResponse{
			Status:       "ok",
			Dependencies: res.Dependencies,
		}, nil
	}
	return api.GetReadyz503JSONResponse{
		Status:       "unavailable",
		Dependencies: res.Dependencies,
	}, nil
}
```

So a fully-healthy gateway answers:

```json
200 {"status":"ok","dependencies":{"redis":"ok","postgres":"ok"}}
```

and when (say) Redis is unreachable:

```json
503 {"status":"unavailable","dependencies":{"redis":"redis ping: ...","postgres":"ok"}}
```

See [diagrams/02-readiness-check.puml](diagrams/02-readiness-check.puml) for the full
sequence.

## 5. How an orchestrator probe wires in

The dependency checkers are registered once at startup, in `cmd/gateway/main.go`:

```go
healthHandler := health.New(logger, cfg.HealthCheckTimeout)
// ...
healthHandler.Register(
	health.NewRedisChecker(redisClient),
	health.NewPostgresChecker(pgPool),
)
```

`Register` is concurrency-safe (guarded by `h.mu`), but in practice it is called only
during boot. A Kubernetes manifest points a `livenessProbe` at `GET /healthz` and a
`readinessProbe` at `GET /readyz`; the gateway behavior described above is what those
probes observe. The exact probe manifest (paths, intervals, thresholds) is an
operator concern documented in the role docs — see *Related docs* below — not in the
gateway code.

> **Not determinable from code:** the orchestrator-side probe configuration
> (`livenessProbe`/`readinessProbe` intervals, failure thresholds) is not present in
> this repository; the code only defines the endpoints' behavior.

## Related docs

- Operator runbook: [`docs/role/operator/health-and-readiness.md`](../../../role/operator/health-and-readiness.md)
- Liveness/recovery interaction: [`../../02-resilience/resilience/`](../../02-resilience/resilience/)
  (panic recovery keeps the process alive so `/healthz` stays 200)
- [02 — Metrics](02-metrics.md) · [03 — Tracing and logging](03-tracing-and-logging.md)
