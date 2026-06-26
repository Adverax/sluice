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

This is the **real OpenAI `chat.completion` shape**, so an unmodified OpenAI SDK
pointed at sluice's base URL works as a drop-in.

## What the default model does

The default demo upstream is an in-process `mock` model: it always returns
`"this is a mock completion"` with zero token counts. This lets you test the full
integration contract — request validation, rate limiting, caching, error
handling — without a real backend. Point the gateway at a real OpenAI-compatible
backend (Ollama, OpenAI, vLLM, LM Studio) to use that backend's model names.

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
