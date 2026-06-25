# Errors and resilience

The gateway returns structured JSON error bodies for every non-2xx response.
This page covers each status code you can receive, why it happens, and what your
client should do.

## Error body shape

All error responses use this JSON structure:

```json
{"error": "<machine-readable code>", "message": "<human-readable description>"}
```

## Status codes

### 400 Bad Request

**Cause:** The request body is missing, is not valid JSON, the `model` field is
absent, or `messages` is empty.

**What to do:** Fix the request before retrying. A 400 is not a transient error
and will not resolve itself on retry.

---

### 404 Not Found

**Cause:** The `model` value in the request body is not registered on this
gateway instance.

**What to do:** Check the `model` field. In v1 the only available model is
`"mock"`. Do not retry without changing the request.

---

### 429 Too Many Requests

**Cause:** Your API key has exceeded its rate-limit quota.

**Response includes:** `Retry-After: <seconds>` header.

**What to do:** Wait the number of seconds given in `Retry-After` before sending
another request with the same key. See [Rate limits and keys](rate-limits-and-keys.md)
for details.

---

### 500 Internal Server Error

**Cause:** An unexpected error occurred inside the gateway (e.g. a panic was
recovered). This is not a client error.

**What to do:** Retry with exponential backoff. If the error persists, contact
the operator.

---

### 502 Bad Gateway

**Cause:** The upstream provider returned an error, or all retry attempts were
exhausted (the provider consistently failed).

**What to do:** Retry with exponential backoff. The upstream provider may be
temporarily unavailable. If retries continue to fail, treat it as an upstream
outage.

---

### 503 Service Unavailable

**Cause:** One of two conditions:
- The gateway's worker pool is saturated (too many concurrent upstream calls).
- The circuit breaker for the selected provider is open (the provider has been
  failing repeatedly and the gateway is protecting it).

**Response includes:** `Retry-After: <seconds>` header with a retry hint.

**What to do:** Wait the number of seconds in `Retry-After`, then retry. The
gateway sheds load immediately rather than queuing, so a 503 with a short
`Retry-After` is expected and recoverable under high load.

---

## Retry guidance

| Status | Retry? | How |
|--------|--------|-----|
| 400 | No | Fix the request. |
| 404 | No | Fix the model name. |
| 429 | Yes, after waiting | Honour `Retry-After`. |
| 500 | Yes | Exponential backoff. |
| 502 | Yes | Exponential backoff. |
| 503 | Yes, after waiting | Honour `Retry-After`. |

**Always implement a retry budget.** Do not retry indefinitely. Three to five
attempts with exponential backoff and full jitter is a reasonable starting point.

## Backpressure: what the gateway does for you

The gateway itself retries transient upstream errors (provider 5xx) before
returning 502. By the time you see a 502, the gateway has already tried and
failed. You may still retry from your client, but back off before doing so.

The gateway also has a circuit breaker per provider. When a provider fails
repeatedly, the circuit opens and the gateway returns 503 immediately without
contacting the provider. The circuit closes again after a recovery period.
The `Retry-After` hint on a 503 indicates how long to wait.

## Cancellation

Close the HTTP connection at any time to cancel your request. The gateway
propagates the cancellation to the upstream provider call immediately — you
are not billed for compute that was cancelled. This works for both regular
(buffered) and streaming requests.
