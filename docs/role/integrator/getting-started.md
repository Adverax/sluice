# Getting started

Send your first request to the sluice gateway in under a minute.

## Prerequisites

- A running gateway. The default listen address is `http://localhost:8080`.
  See the project [README](../../../../README.md) for how to start it with
  `make up` (full stack) or `make run` (host process).

## Your first request

```sh
curl -sS http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "mock",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

Expected response (HTTP 200):

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

## What the v1 model does

The only model available in v1 is `mock`. It always returns the string
`"this is a mock completion"` with zero token counts. This lets you test
the full integration contract — request validation, rate limiting, caching,
error handling — before real provider adapters are added.

## Required header

Every request to `POST /v1/chat/completions` must include:

```
Content-Type: application/json
```

Omitting it, or sending a malformed body, returns HTTP 400.

## Base URL

All gateway paths are relative to its listen address. There is no path prefix
beyond what is shown in each endpoint (e.g. `/v1/chat/completions`).

## Next steps

- [Chat completions](chat-completions.md) — full request and response field reference.
- [Streaming](streaming.md) — receive tokens as they arrive via SSE.
- [Rate limits and keys](rate-limits-and-keys.md) — understand how requests are counted.
