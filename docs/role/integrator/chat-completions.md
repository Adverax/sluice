# Chat completions

`POST /v1/chat/completions` is the core endpoint. It accepts a conversation
history and returns a completion from the selected model.

## Request

```
POST /v1/chat/completions
Content-Type: application/json
```

### Body fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `model` | string | yes | The model to use. v1 ships `"mock"`. |
| `messages` | array of Message | yes | Ordered conversation history. Must contain at least one message. |
| `stream` | boolean | no (default `false`) | Set to `true` to receive an SSE stream instead of a buffered JSON response. See [Streaming](streaming.md). |
| `max_tokens` | integer | no | Upper bound on the completion length. |
| `temperature` | number | no | Sampling temperature. |

### Message object

Each entry in `messages` must have:

| Field | Type | Values |
|-------|------|--------|
| `role` | string | `"system"`, `"user"`, or `"assistant"` |
| `content` | string | The message text. |

### Example request

```json
{
  "model": "mock",
  "messages": [
    {"role": "system", "content": "You are a helpful assistant."},
    {"role": "user",   "content": "What is the capital of France?"}
  ],
  "temperature": 0.7
}
```

## Non-streaming response (HTTP 200)

When `stream` is `false` (or omitted), the gateway returns a single JSON object:

```json
{
  "model": "mock",
  "content": "this is a mock completion",
  "finish_reason": "stop",
  "usage": {
    "prompt_tokens": 0,
    "completion_tokens": 0,
    "total_tokens": 0
  }
}
```

### Response fields

| Field | Type | Description |
|-------|------|-------------|
| `model` | string | The model that produced the completion. |
| `content` | string | The assistant's reply text. |
| `finish_reason` | string | Why the completion ended, e.g. `"stop"` or `"length"`. |
| `usage.prompt_tokens` | integer | Tokens in the input. |
| `usage.completion_tokens` | integer | Tokens in the completion. |
| `usage.total_tokens` | integer | Sum of prompt + completion tokens. |

## Error responses

| Status | Meaning | When |
|--------|---------|------|
| 400 | Bad request | Body is missing, not valid JSON, `model` field is absent, or `messages` is empty. |
| 404 | Model not found | `model` value is not registered on this gateway. |
| 429 | Rate limited | Your key has exceeded its request quota. See [Rate limits and keys](rate-limits-and-keys.md). |
| 502 | Bad gateway | The upstream provider returned an error or retries were exhausted. |
| 503 | Service unavailable | The gateway is overloaded or the circuit breaker is open. See [Errors and resilience](errors-and-resilience.md). |

All error bodies follow this shape:

```json
{"error": "<code>", "message": "<human-readable description>"}
```

## Models available in v1

| Model name | Behaviour |
|------------|-----------|
| `mock` | Returns a fixed completion string. Useful for integration testing. |

Real provider adapters (OpenAI, Anthropic, etc.) are planned for a future release.
Sending an unknown `model` value returns HTTP 404.
