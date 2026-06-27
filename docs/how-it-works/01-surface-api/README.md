# surface-api — what the gateway exposes

The **OpenAI-compatible** HTTP surface clients interact with, and the request hot path behind
it. This group covers how a drop-in `/v1/chat/completions` request is accepted (liberal-accept),
mapped to the canonical model, routed, proxied (`chat.completion` JSON or `chat.completion.chunk`
SSE), cached, and cancelled — plus the process lifecycle that keeps the surface stable.

| Aspect | Topics |
|--------|--------|
| [Proxy](proxy/) | [01 · Inference proxying](proxy/01-inference-proxying.md) — request path, routing, JSON/SSE, cache, cancellation · [02 · Runtime lifecycle](proxy/02-runtime-lifecycle.md) — graceful shutdown, panic recovery |

Bounded context: **CTX-001 Proxy** (CAP-001 Inference proxying, CAP-005 Runtime lifecycle).
The protective machinery this path calls into lives in [resilience](../02-resilience/),
and the usage it records flows to [metering](../04-integrations/).
