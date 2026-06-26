# Chat completions

`POST /v1/chat/completions` is the core endpoint. It speaks the **real OpenAI
chat-completions wire format**, so unmodified OpenAI SDKs and `curl` examples
work against sluice by changing only the base URL (drop-in compatibility).

## Request

```
POST /v1/chat/completions
Content-Type: application/json
```

### Body fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `model` | string | yes | The model to use (also the routing key). |
| `messages` | array of Message | yes | Ordered conversation history. Must contain at least one message. |
| `stream` | boolean | no (default `false`) | Set to `true` to receive an SSE stream instead of a buffered JSON response. See [Streaming](streaming.md). |
| `max_tokens` | integer | no | Upper bound on the completion length. |
| `temperature` | number | no | Sampling temperature. |
| `top_p` | number | no | Nucleus-sampling probability mass. |
| `stop` | array of string | no | Stop sequences that end generation. |

**Liberal accept.** Any other OpenAI field — `seed`, `user`,
`presence_penalty`, `frequency_penalty`, `logit_bias`, `response_format`, `n`,
`logprobs`, … — is **accepted and silently ignored**, never a 400. This is what
lets real OpenAI SDK payloads (which send these extras) round-trip unmodified.
Only the fields in the table above are forwarded upstream.

**Non-goals (return 400).** `n > 1`, multimodal/array `content`, and function
calling (`tools`/`functions`/`tool_choice`) are out of scope (CON-008): the
gateway models a single string-content choice and rejects these with an
OpenAI-shaped 400.

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

When `stream` is `false` (or omitted), the gateway returns a single OpenAI
`chat.completion` object. `id`, `object` and `created` are generated at the edge;
`system_fingerprint` is omitted.

```json
{
  "id": "chatcmpl-9f8c1a2b3c4d5e6f70819a2b",
  "object": "chat.completion",
  "created": 1718000000,
  "model": "mock",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "this is a mock completion"},
      "finish_reason": "stop"
    }
  ],
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
| `id` | string | Edge-generated completion id, prefixed `chatcmpl-`. |
| `object` | string | Always `"chat.completion"`. |
| `created` | integer | Unix timestamp (seconds) the gateway built the response. |
| `model` | string | The model that produced the completion. |
| `choices` | array | Always exactly one choice (`index: 0`). |
| `choices[0].message.role` | string | Always `"assistant"`. |
| `choices[0].message.content` | string | The assistant's reply text. |
| `choices[0].finish_reason` | string | Why the completion ended, e.g. `"stop"` or `"length"`. |
| `usage.prompt_tokens` | integer | Tokens in the input. |
| `usage.completion_tokens` | integer | Tokens in the completion. |
| `usage.total_tokens` | integer | Sum of prompt + completion tokens. |

## Error responses

| Status | Meaning | When |
|--------|---------|------|
| 400 | Bad request | Body is missing/invalid, `model` absent, `messages` empty, or an unsupported shape (`n>1`, array content). |
| 404 | Model not found | `model` value is not registered on this gateway. |
| 429 | Rate limited | Your key has exceeded its request quota. See [Rate limits and keys](rate-limits-and-keys.md). |
| 502 | Bad gateway | The upstream provider returned an error or retries were exhausted. |
| 503 | Service unavailable | The gateway is overloaded or the circuit breaker is open. See [Errors and resilience](errors-and-resilience.md). |

All error bodies follow the OpenAI error envelope:

```json
{"error": {"message": "<human-readable description>", "type": "<error type>", "code": "<code or null>"}}
```

## Models

The `model` value is both the routing key and the upstream model. The default
demo upstream is an in-process `mock`; pointing the gateway at a real
OpenAI-compatible backend (Ollama, OpenAI, vLLM, LM Studio) lets you use that
backend's model names (e.g. `llama3.2`, `gpt-4o-mini`). Sending an unregistered
`model` returns HTTP 404.
