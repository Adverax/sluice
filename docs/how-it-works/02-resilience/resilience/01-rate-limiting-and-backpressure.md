# 01 — Rate limiting and backpressure (CAP-002)

This capability answers one question twice, at two different boundaries:

1. **Rate limiting** — *is this client sending too fast?* Decided per API key,
   at the outer edge of the middleware chain, **before any proxy/provider work
   runs** (INV-004). A "no" is `429 Too Many Requests` + `Retry-After`.
2. **Backpressure** — *is the gateway already doing too much upstream work?*
   Decided by a bounded worker pool deep in the provider-call seam. A "no" is
   `503 Service Unavailable` + `Retry-After`, returned **before** a goroutine is
   ever spawned for the work.

Both exist because the gateway's primary job is availability: under 2–3× overload
it must not crash, must serve or cleanly reject every request, and must recover
(NFR-002). Rate limiting protects against a single noisy client; backpressure
protects the *whole instance* and bounds the goroutines waiting on upstream
(NFR-006). They are independent: a request can pass the rate limiter and still be
shed by the pool.

See [diagrams/01-rate-limit-backpressure-flow.puml](diagrams/01-rate-limit-backpressure-flow.puml)
for the end-to-end decision flow.

---

## 1. Why two tiers of rate limiting

A single gateway instance can bound a client's burst cheaply, in-process, with no
network round-trip. But `sluice` may run as a fleet of instances behind a load
balancer, and a per-key limit only means something if it is *shared* across them.
So the rate-limit middleware (`internal/middleware/ratelimit.go`) composes two
tiers, and **a request must pass BOTH**:

- **Tier 1 — local token bucket** (`internal/ratelimit/ratelimit.go`): the fast
  in-process path, `golang.org/x/time/rate`, one limiter per key.
- **Tier 2 — distributed token bucket** (`internal/ratelimit/redisrepo.go`): a
  global cap shared by every instance pointing at the same Redis (ADR-0010).

The package doc states the contract directly:

```go
// The middleware consults the local limiter first, then the
// repository, so a request must pass BOTH to be served (AC-013).
```

The middleware depends only on the `RateLimitRepository` **port** — it never
imports Redis — so the distributed tier is substitutable (the Redis adapter in
`redisrepo.go`, or the in-process `MemoryRepository` default in `memrepo.go`):

```go
type RateLimitRepository interface {
	Allow(ctx context.Context, key string, limit int, window time.Duration) (Decision, error)
}
```

## 2. Resolving the key — the ephemeral key lifecycle (ADR-0001)

Before either tier runs, the middleware must decide *what key* this request
counts against. `resolveKey` (in `ratelimit.go` middleware) uses a three-step
precedence:

1. **`Authorization` header present** → use it as the key. `Bearer <token>` and
   the raw token form are both accepted; the token is treated as an identifier
   only — it is **not** validated against any store (an explicit ADR-0001
   non-goal).
2. **No header, but a well-formed `sluice_api_key` cookie** → reuse it. "Well
   formed" means the `eph_` prefix followed by exactly 32 lowercase hex chars
   (`isWellFormedEphemeralKey`). Reusing it stops a client from dodging its limit
   by simply dropping the header.
3. **Neither** → mint a fresh ephemeral key with `crypto/rand`:

```go
func mintEphemeralKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return ephemeralKeyPrefix + hex.EncodeToString(b[:])
}
```

When a key is minted, the middleware advertises it to the client **before it
might reject** — so even a `429` carries the key the client should reuse — via
both the `X-Sluice-Api-Key` response header and an `HttpOnly` `sluice_api_key`
cookie:

```go
if minted {
	w.Header().Set(apiKeyHeader, key)
	if m.cookie {
		http.SetCookie(w, &http.Cookie{
			Name:     apiKeyCookie,
			Value:    key,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
	}
}
```

There is one deliberate **fail-closed** branch here: if `crypto/rand` fails and
the minter returns `""`, the request is rejected with `500`. An empty key must
never reach the registry, because it would create one shared anonymous bucket and
let all keyless clients collectively bypass per-key enforcement.

## 3. Tier 1 — the local token bucket

Each distinct key gets its own `*rate.Limiter` from a bounded `Registry`. The
check consumes a token if one is available, and otherwise derives a `Retry-After`
hint from how long until the next token refills:

```go
func (r *Registry) Allow(key string) Decision {
	lim := r.limiterFor(key)
	now := r.now()
	res := lim.ReserveN(now, 1)
	if !res.OK() {
		return Decision{Allowed: false, RetryAfter: r.window()}
	}
	delay := res.DelayFrom(now)
	if delay > 0 {
		// Not enough tokens right now: cancel the reservation (do not consume a
		// future token) and deny with the wait as the Retry-After hint.
		res.CancelAt(now)
		return Decision{Allowed: false, RetryAfter: delay}
	}
	return Decision{Allowed: true}
}
```

Note `CancelAt`: a denied request does **not** consume a future token, so a
rejected caller is not also penalised on its next attempt.

**Bounded registry (DoS defence).** A naïve per-key map grows unbounded under a
key-spraying attack. The `Registry` is capped at `maxKeys`
(`GATEWAY_RATELIMIT_MAX_KEYS`, default `100_000`) with two reclamation paths:

- A **hard cap on insert**: when at capacity, `limiterFor` evicts the
  least-recently-used entry before inserting a new key (`evictLRULocked`).
- A **background idle sweep** every `sweepInterval` (default 5 min) that drops any
  limiter whose bucket is full — a full bucket means the key is idle, so dropping
  it is safe:

```go
func (r *Registry) sweepIdle() {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.limiters {
		if e.lim.TokensAt(now) >= float64(r.burst) {
			delete(r.limiters, k)
		}
	}
}
```

The sweep goroutine is started in `NewRegistry` and stopped by `Close`, which
`cmd/gateway` defers for graceful shutdown (FR-012).

## 4. Tier 2 — the distributed token bucket (Redis Lua)

The distributed tier must be **atomic** across instances: N gateways sharing one
Redis must enforce a single shared bucket, with no read-modify-write race. The
`RedisRepository` achieves this by doing the entire read → refill → decrement →
persist in one Lua script (`tokenBucketScript` in `redisrepo.go`), keyed by a
Redis hash `{tokens, ts}`:

```lua
local rate    = tonumber(ARGV[1])
local burst   = tonumber(ARGV[2])
local now_ms  = tonumber(ARGV[3])
local ttl_ms  = tonumber(ARGV[4])

local data    = redis.call("HMGET", KEYS[1], "tokens", "ts")
local tokens  = tonumber(data[1])
local ts      = tonumber(data[2])

if tokens == nil or ts == nil then
	-- First sighting of this key: start with a full bucket.
	tokens = burst
	ts     = now_ms
end

-- Refill based on elapsed wall-clock since the last update (never negative).
local elapsed_ms = now_ms - ts
if elapsed_ms < 0 then
	elapsed_ms = 0
end
tokens = math.min(burst, tokens + (elapsed_ms / 1000.0) * rate)

local allowed = 0
local retry_after_ms = 0
if tokens >= 1 then
	allowed = 1
	tokens = tokens - 1
else
	if rate > 0 then
		retry_after_ms = math.ceil((1 - tokens) / rate * 1000.0)
	end
end

redis.call("HSET", KEYS[1], "tokens", tokens, "ts", now_ms)
redis.call("PEXPIRE", KEYS[1], ttl_ms)

return {allowed, retry_after_ms}
```

Design points visible in the code:

- **Refill** is `min(burst, tokens + elapsed_seconds * rate)`; **allow** iff
  `tokens >= 1`, decrementing one token. `retry_after_ms` is the time until one
  whole token is back.
- **`now` is passed from Go (ARGV[3])**, not read via Redis `TIME`, so the
  algorithm is deterministic and unit-testable with an injected clock. The
  documented trade-off: refill follows the caller's clock, which is fine for an
  NTP-synced fleet.
- **TTL** (`ttlMS = window.Milliseconds() * 5`) lets idle keys self-evict via
  `PEXPIRE` while an active key is never reclaimed mid-use.
- The Go side computes `rate = limit / window.Seconds()` and defaults the bucket
  `burst` to `limit` when unset, then maps the Lua reply into the same `Decision`
  struct the local tier returns.

The adapter depends on the narrow `redis.Scripter` interface (so
`*redis.Client`, `*redis.Ring`, `*redis.ClusterClient` all satisfy it), keeping
the ACL boundary (ADR-0010) explicit.

## 5. Fail-open: a Redis blip must not 503 the fleet

The distributed tier can fail (Redis unreachable). The middleware treats a
repository **error** as fail-open — it falls back to the local verdict rather
than rejecting:

```go
if m.repo != nil {
	d, err := m.repo.Allow(r.Context(), key, m.globalRPS, m.window)
	if err != nil {
		m.logger.LogAttrs(r.Context(), slog.LevelWarn,
			"rate-limit distributed check failed; failing open to local limiter",
			slog.String("error", err.Error()),
		)
	} else if !d.Allowed {
		m.reject(w, r, d.RetryAfter, "distributed")
		return
	}
}
```

The rationale (from the type doc): for a proxy whose job is availability,
fail-*closed* would amplify a dependency outage into a total outage — the worse
failure mode. Per-instance burst is still bounded by the local limiter.

## 6. Rejection: 429 + Retry-After

Both tiers reject through one path, `reject`, which increments the metric *before*
writing the response (so every rejection is counted regardless of tier), clamps
the `Retry-After` to at least 1 second, and writes a stable JSON error:

```go
func (m *RateLimiter) reject(w http.ResponseWriter, r *http.Request, retryAfter time.Duration, tier string) {
	m.metrics.IncRateLimitRejected()

	secs := int(retryAfter.Round(time.Second) / time.Second)
	if retryAfter <= 0 {
		secs = int(defaultRetryAfter / time.Second)
	}
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":"rate_limited","message":"rate limit exceeded; retry later"}`))
	...
}
```

`IncRateLimitRejected` is reached through a one-method `rejectRecorder` port —
the middleware never imports Prometheus (ADR-0008). The counter surfaced is
`ratelimit_rejected_total` (see the Observability aspect).

---

## 7. Backpressure: the bounded worker pool

Rate limiting bounds *per client*. The worker pool (`internal/pool/pool.go`,
COMP-010) bounds the *instance's* concurrent upstream calls, so that under
overload goroutines waiting on a provider are strictly capped (NFR-006) and
excess load is shed cleanly rather than queued until memory runs out.

The pool is a buffered-channel semaphore whose capacity is the concurrency limit
(`GATEWAY_WORKER_POOL_SIZE`, default 100 — ADR-0003):

```go
type Pool struct {
	sem        chan struct{}
	retryAfter time.Duration
}
```

The defining property is that **acquire is non-blocking** — when every slot is
taken, the pool rejects *immediately* without blocking the caller and without
spawning a goroutine:

```go
func (p *Pool) tryAcquire() bool {
	select {
	case p.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (p *Pool) release() { <-p.sem }
```

### 7.1 Unary guard — reject before work

`Guard` wraps the composed provider seam (`server.InferFunc`). On a free slot it
holds the slot for the whole wrapped call and releases it exactly once via
`defer` (even on a panic); on a full pool it returns a typed sentinel and starts
nothing:

```go
func (p *Pool) Guard(next server.InferFunc) server.InferFunc {
	return func(ctx context.Context, prov provider.Provider, req provider.Request) (provider.Response, error) {
		if !p.tryAcquire() {
			// Reject-before-work: shed load without spawning a goroutine.
			return provider.Response{}, &saturatedError{retryAfter: p.retryAfter}
		}
		defer p.release()
		return next(ctx, prov, req)
	}
}
```

`saturatedError` wraps `ErrPoolSaturated` and additionally matches
`server.ErrServiceUnavailable`, so the server maps it to `503` + `Retry-After`
through the *same* classification path as the resilience open-breaker fast-fail —
no string matching, no import of the pool package by the server:

```go
func (e *saturatedError) Unwrap() error { return ErrPoolSaturated }
func (e *saturatedError) Is(target error) bool {
	return target == server.ErrServiceUnavailable
}
func (e *saturatedError) RetryAfter() time.Duration { return e.retryAfter }
```

### 7.2 Streaming guard — slot held for the stream lifetime, released once

Streaming is harder: the slot must be held for the *entire* stream, not just its
initiation, and released **exactly once** when the stream ends — whether it ends
by the source channel closing, by client cancellation, or by error. `GuardStream`
handles each case. On an initiation error or a contract-violating `nil` channel
it releases immediately; on success it hands forwarding to a goroutine guarded by
a `sync.Once`:

```go
var releaseOnce sync.Once
releaseSlot := func() { releaseOnce.Do(p.release) }

out := make(chan provider.Chunk)
go func() {
	defer close(out)
	defer releaseSlot()
	for {
		select {
		case chunk, ok := <-src:
			if !ok {
				return // source drained/closed: slot released by defer
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				// Consumer gone: stop forwarding but keep draining src so the
				// provider goroutine can finish and close it — else it leaks.
				drain(src)
				return
			}
		case <-ctx.Done():
			drain(src)
			return
		}
	}
}()
return out, nil
```

The `drain(src)` calls on cancellation are the key to NFR-003 (zero leaked
goroutines): a client disconnect stops forwarding, but the source is still read
to completion so the provider's blocked send can finish and the slot's `defer`
fires. A single shared `*Pool` backs both `Guard` and `GuardStream` in
`cmd/gateway`, so streams and unary calls count against the same cap.

---

## 8. Where the two boundaries sit in the request path

Rate limiting is an **outer** middleware; backpressure is **inner**, at the
provider seam. From `cmd/gateway/main.go`, the middleware chain (outermost first)
is:

```
recover → logging → tracing → metrics → rate-limit → counting → cache → routes
```

and the provider seam the routes call is `pool.Guard(retry(breaker.Execute(provider.Infer)))`.
So a request is rate-limited *before* it can ever reach the pool, and shed by the
pool *before* any retry/breaker/provider work begins. The composition order is
the subject of [02-circuit-breaking.md](02-circuit-breaking.md) §4 and ADR-0006.

> Not determinable from code: the *production* numeric values of
> `GATEWAY_RATELIMIT_RPS` / `BURST` / `WINDOW` and `GATEWAY_WORKER_POOL_SIZE` are
> deployment configuration; the code only fixes the structural defaults cited
> above (`MaxKeys` 100 000, pool size 100, sweep 5 min).
