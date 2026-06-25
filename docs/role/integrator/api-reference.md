# API reference

Derived from `api/openapi.yaml` — the authoritative wire contract.

---

## POST /v1/chat/completions

Create a chat completion.

### Request

```
POST /v1/chat/completions
Content-Type: application/json
Authorization: Bearer <key>   (optional; see rate-limits-and-keys.md)
X-Cache-TTL: <seconds>        (optional; see caching.md)
```

#### Request body (`ChatCompletionRequest`)

```json
{
  "model":       "<string, required>",
  "messages":    [<Message>, ...],
  "stream":      false,
  "max_tokens":  1024,
  "temperature": 0.7
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `model` | string | yes | Model to run the completion against. v1: `"mock"`. |
| `messages` | array of [Message](#message) | yes | Ordered conversation history. At least one entry required. |
| `stream` | boolean | no (default `false`) | `true` → SSE stream response. |
| `max_tokens` | integer | no | Maximum completion length. |
| `temperature` | number | no | Sampling temperature. |

#### Message

| Field | Type | Values |
|-------|------|--------|
| `role` | string | `"system"` \| `"user"` \| `"assistant"` |
| `content` | string | Message text. |

---

### Responses

#### 200 OK — non-streaming (`stream: false`)

```
Content-Type: application/json
X-Cache: HIT | MISS
```

Body (`ChatCompletionResponse`):

```json
{
  "model":         "mock",
  "content":       "this is a mock completion",
  "finish_reason": "stop",
  "usage": {
    "prompt_tokens":     0,
    "completion_tokens": 0,
    "total_tokens":      0
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `model` | string | Model that produced the completion. |
| `content` | string | Assistant reply text. |
| `finish_reason` | string | Why the completion ended, e.g. `"stop"` or `"length"`. |
| `usage.prompt_tokens` | integer | Tokens in the prompt. |
| `usage.completion_tokens` | integer | Tokens in the completion. |
| `usage.total_tokens` | integer | Sum of prompt + completion tokens. |

#### 200 OK — streaming (`stream: true`)

```
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
```

Body: a sequence of SSE events.

**Content delta:**
```
data: {"content":"<text fragment>"}

```

**Terminal event (usage):**
```
data: {"done":true,"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}

```

**Stream terminator:**
```
data: [DONE]
```

Streaming responses never include `X-Cache`.

---

#### 400 Bad Request

Body: [Error](#error)

Returned when: body is missing or invalid JSON; `model` field is absent;
`messages` is empty; any required field fails schema validation.

#### 404 Not Found

Body: [Error](#error)

Returned when: `model` value has no registered provider.

#### 429 Too Many Requests

Headers: `Retry-After: <seconds>`

Body: [Error](#error)

```json
{"error": "rate_limited", "message": "rate limit exceeded; retry later"}
```

Returned when: the API key has exceeded its request quota.

#### 500 Internal Server Error

Body: [Error](#error)

Returned when: an unexpected error occurred inside the gateway.

#### 502 Bad Gateway

Body: [Error](#error)

```json
{"error": "provider_error", "message": "upstream provider request failed"}
```

Returned when: the upstream provider returned an error, or all retries were
exhausted.

#### 503 Service Unavailable

Headers: `Retry-After: <seconds>`

Body: [Error](#error)

```json
{"error": "service_unavailable", "message": "upstream temporarily unavailable; retry later"}
```

Returned when: the worker pool is saturated (gateway is overloaded), or the
circuit breaker is open for the selected provider.

---

## GET /healthz

Liveness probe.

### Responses

#### 200 OK

```json
{"status": "ok"}
```

Always returned while the process is running. Does not check dependencies.

---

## GET /readyz

Readiness probe. Checks all registered dependencies (Redis, Postgres).

### Responses

#### 200 OK

All dependencies are healthy.

```json
{
  "status": "ok",
  "dependencies": {
    "redis":    "ok",
    "postgres": "ok"
  }
}
```

#### 503 Service Unavailable

At least one dependency is down.

```json
{
  "status": "unavailable",
  "dependencies": {
    "redis":    "connection refused",
    "postgres": "ok"
  }
}
```

The `dependencies` map contains one entry per dependency. The value is `"ok"`
when healthy, or an error description when not.

---

## GET /metrics

Prometheus metrics in text exposition format.

### Responses

#### 200 OK

```
Content-Type: text/plain
```

Body: Prometheus text exposition format. Includes metrics:
`http_requests_total`, `http_request_duration_seconds`,
`gateway_inflight_requests`, `provider_request_duration_seconds`,
`ratelimit_rejected_total`, `breaker_state`.

---

## Common schemas

### Error

```json
{
  "error":   "<machine-readable code>",
  "message": "<human-readable description>"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `error` | string | Short, machine-readable error code. |
| `message` | string | Human-readable description. |

### Usage

```json
{
  "prompt_tokens":     0,
  "completion_tokens": 0,
  "total_tokens":      0
}
```

---

## Client-visible response headers

| Header | Endpoint | Description |
|--------|----------|-------------|
| `X-Cache` | `POST /v1/chat/completions` (non-streaming) | `HIT` or `MISS` — whether the response was served from cache. |
| `X-Sluice-Api-Key` | `POST /v1/chat/completions` | Present on responses to keyless callers. Contains the minted ephemeral key to reuse in subsequent requests. |
| `Retry-After` | Any 429 or 503 response | Number of seconds to wait before retrying. |
| `Set-Cookie` | `POST /v1/chat/completions` (keyless callers) | Sets `sluice_api_key=<ephemeral key>` so browser clients reuse the key automatically. |
