# Integrator / API Consumer

You are building an application that calls the sluice LLM gateway to run chat
completions. This documentation tells you everything you need to call the API
correctly, handle errors, and keep your client reliable.

## What you can do

- Send chat completion requests and receive a full JSON response.
- Stream completions token-by-token over Server-Sent Events (SSE).
- Cancel an in-flight request by closing the connection.
- Understand how the gateway rate-limits your calls and how to stay within limits
  without manual key management.
- Handle transient errors with the gateway's own retry hints.
- Cache-bust or influence caching TTL on a per-request basis.

## Current model

The v1 gateway ships with a **mock** model named `mock`. It returns a fixed
completion string and zero token counts. This lets you integrate against the full
wire contract before real provider adapters ship.

## Guides

| Guide | What it covers |
|-------|----------------|
| [Getting started](getting-started.md) | First request with curl; base URL; Content-Type |
| [Chat completions](chat-completions.md) | Full request/response fields; models |
| [Streaming](streaming.md) | SSE wire format; `stream: true`; cancellation |
| [Rate limits and keys](rate-limits-and-keys.md) | Ephemeral keys; 429; `Retry-After`; `X-Sluice-Api-Key` |
| [Errors and resilience](errors-and-resilience.md) | All status codes; retry guidance; backpressure; cancellation |
| [Caching](caching.md) | `X-Cache` header; `X-Cache-TTL` override; streaming exclusion |
| [Health endpoints](health-endpoints.md) | `/healthz`, `/readyz`; when to check them |
| [API reference](api-reference.md) | Complete endpoint, schema, and status-code reference |
