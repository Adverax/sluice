# Streaming

When you set `"stream": true` in the request body, the gateway responds with a
Server-Sent Events (SSE) stream instead of a buffered JSON response. You receive
content tokens as they arrive from the provider.

## Making a streaming request

```sh
curl -N http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "mock",
    "stream": true,
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

The `-N` flag disables curl's output buffering so you see each event immediately.

## Response headers

On a successful stream the gateway sets:

```
HTTP/1.1 200 OK
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
```

## SSE wire format

The stream is the **real OpenAI streaming format**: each event is a line
starting with `data: ` followed by a `chat.completion.chunk` JSON object, then a
blank line. The stream ends with a literal `data: [DONE]`. The `id`, `created`
and `model` are stable across every chunk of one stream.

```
data: {"id":"chatcmpl-…","object":"chat.completion.chunk","created":1718000000,"model":"mock","choices":[{"index":0,"delta":{"content":"this "}}]}

data: {"id":"chatcmpl-…","object":"chat.completion.chunk","created":1718000000,"model":"mock","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
```

### Event shapes

**Content delta** — carries a fragment of the assistant's reply in
`choices[0].delta.content`:

```json
{"id":"chatcmpl-…","object":"chat.completion.chunk","created":1718000000,"model":"mock","choices":[{"index":0,"delta":{"content":"<text fragment>"}}]}
```

**Final chunk** — an empty delta with a `finish_reason`, emitted just before the
terminator:

```json
{"id":"chatcmpl-…","object":"chat.completion.chunk","created":1718000000,"model":"mock","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}
```

**Stream terminator** — the final line, always present at the end of a successful stream:

```
data: [DONE]
```

Stop reading after you see `data: [DONE]`. Token usage for a streamed completion
is recorded by the gateway for metering; it is not echoed in the SSE chunks.

## Cancelling a stream

Close the HTTP connection at any time to cancel the in-flight request. The
gateway detects the client disconnect and immediately cancels the upstream
call. You do not need to send any special signal — just close the connection.

## Caching and streaming

Streaming responses are **never cached**. The `X-Cache-TTL` override header has no
effect on streaming requests. See [Caching](caching.md).

## Error handling before the stream starts

If the gateway cannot initiate the stream (e.g. the worker pool is full, or the
circuit breaker is open), it returns a normal JSON error response with the
appropriate status code — **before** writing any SSE bytes. You will see a
`Content-Type: application/json` response, not `text/event-stream`.

| Status | Cause |
|--------|-------|
| 400 | Invalid request body. |
| 404 | Unknown model. |
| 429 | Rate limit exceeded. |
| 503 | Gateway overloaded or circuit breaker open. Includes `Retry-After`. |
| 502 | Provider error at stream initiation. |

These pre-stream errors use the OpenAI error envelope
`{"error": {"message", "type", "code"}}`.

Once the `200` header and `Content-Type: text/event-stream` are written, a
mid-stream transport error causes the stream to end with `data: [DONE]` and no
further content, rather than a new status code.
