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

Real OpenAI `/v1/chat/completions` wire format. **Liberal accept:** any field
not listed below (`seed`, `user`, `presence_penalty`, `frequency_penalty`,
`logit_bias`, `response_format`, `n`, `logprobs`, …) is accepted and ignored,
never a 400. `n>1`, multimodal/array `content`, and function calling are
non-goals and return a 400.

```json
{
  "model":       "<string, required>",
  "messages":    [<Message>, ...],
  "stream":      false,
  "max_tokens":  1024,
  "temperature": 0.7,
  "top_p":       0.9,
  "stop":        ["\n"]
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `model` | string | yes | Model to run the completion against (also the routing key). |
| `messages` | array of [Message](#message) | yes | Ordered conversation history. At least one entry required. |
| `stream` | boolean | no (default `false`) | `true` → SSE stream response. |
| `max_tokens` | integer | no | Maximum completion length. |
| `temperature` | number | no | Sampling temperature. |
| `top_p` | number | no | Nucleus-sampling probability mass. |
| `stop` | array of string | no | Stop sequences that end generation. |

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

Body (`ChatCompletionResponse`) — a real OpenAI `chat.completion` object.
`id`/`object`/`created` are generated at the edge; `system_fingerprint` is omitted.

```json
{
  "id":      "chatcmpl-9f8c1a2b3c4d5e6f70819a2b",
  "object":  "chat.completion",
  "created": 1718000000,
  "model":   "mock",
  "choices": [
    {
      "index":   0,
      "message": {"role": "assistant", "content": "this is a mock completion"},
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens":     0,
    "completion_tokens": 0,
    "total_tokens":      0
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Edge-generated completion id, prefixed `chatcmpl-`. |
| `object` | string | Always `"chat.completion"`. |
| `created` | integer | Unix timestamp (seconds). |
| `model` | string | Model that produced the completion. |
| `choices` | array | Always exactly one choice (`index: 0`). |
| `choices[0].message.role` | string | Always `"assistant"`. |
| `choices[0].message.content` | string | Assistant reply text. |
| `choices[0].finish_reason` | string | Why the completion ended, e.g. `"stop"` or `"length"`. |
| `usage.prompt_tokens` | integer | Tokens in the prompt. |
| `usage.completion_tokens` | integer | Tokens in the completion. |
| `usage.total_tokens` | integer | Sum of prompt + completion tokens. |

#### 200 OK — streaming (`stream: true`)

```
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
```

Body: a sequence of OpenAI `chat.completion.chunk` SSE events, terminated by
`data: [DONE]`. `id`/`created`/`model` are stable across the stream.

**Content delta:**
```
data: {"id":"chatcmpl-…","object":"chat.completion.chunk","created":1718000000,"model":"mock","choices":[{"index":0,"delta":{"content":"<text fragment>"}}]}

```

**Final chunk (finish_reason):**
```
data: {"id":"chatcmpl-…","object":"chat.completion.chunk","created":1718000000,"model":"mock","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

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
{"error": {"message": "rate limit exceeded; retry later", "type": "rate_limit_error", "code": "rate_limited"}}
```

Returned when: the API key has exceeded its request quota.

#### 500 Internal Server Error

Body: [Error](#error)

Returned when: an unexpected error occurred inside the gateway.

#### 502 Bad Gateway

Body: [Error](#error)

```json
{"error": {"message": "upstream provider request failed", "type": "upstream_error", "code": "bad_gateway"}}
```

Returned when: the upstream provider returned an error, or all retries were
exhausted.

#### 503 Service Unavailable

Headers: `Retry-After: <seconds>`

Body: [Error](#error)

```json
{"error": {"message": "upstream temporarily unavailable; retry later", "type": "service_unavailable", "code": "service_unavailable"}}
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

The OpenAI error envelope, so unmodified OpenAI SDKs parse gateway and
mapped-upstream errors.

```json
{
  "error": {
    "message": "<human-readable description>",
    "type":    "<error type, e.g. invalid_request_error>",
    "code":    "<machine-readable code, or null>"
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `error.message` | string | Human-readable description. |
| `error.type` | string | OpenAI error type (e.g. `invalid_request_error`, `rate_limit_error`, `server_error`). |
| `error.code` | string \| null | Short, machine-readable code (may be null). |

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
