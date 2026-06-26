package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// MockUpstreamOptions tunes the reproducible mock LLM upstream served over real
// HTTP (CARD-013). It now emits the REAL OpenAI /v1/chat/completions wire shape
// (CARD-016, ADR-0013) so unit and load tests exercise the real adapter
// mapping. The zero value is a usable, instantaneous, always-success upstream
// that echoes a fixed completion.
type MockUpstreamOptions struct {
	// Content is the completion text returned by the unary endpoint and split
	// across deltas on the streaming endpoint. Empty falls back to a default.
	Content string

	// FinishReason is the OpenAI finish_reason reported on the unary path and on
	// the final stream chunk. Empty falls back to "stop".
	FinishReason string

	// Usage is the token accounting reported on both paths (the unary `usage`
	// object and the trailing SSE usage chunk).
	Usage Usage

	// Latency is a per-request delay applied BEFORE the response is produced
	// (unary) and before each stream delta. It is honoured against the request
	// context so a client disconnect aborts it.
	Latency time.Duration

	// FailStatus, when >= 400, makes EVERY request fail with that HTTP status
	// (used to exercise the 5xx-retryable / 4xx-not classification). The body is
	// the OpenAI error envelope so the adapter's error parsing is exercised. Zero
	// means "always succeed".
	FailStatus int

	// StreamChunks is the number of content deltas the streaming endpoint emits
	// before the terminal chunk. <= 0 defaults to 1.
	StreamChunks int

	// OmitStreamUsage, when true, makes the SSE endpoint NOT emit a trailing
	// usage chunk even when the request asks for stream_options.include_usage.
	// It models a backend (e.g. some Ollama builds) that ignores include_usage,
	// exercising the adapter's graceful zero-usage path (FR-019, AC-059).
	OmitStreamUsage bool
}

const (
	defaultMockUpstreamContent = "this is a mock completion"
	defaultMockFinishReason    = "stop"
)

// MockUpstreamHandler returns an http.Handler implementing the REAL OpenAI
// /v1/chat/completions wire API consumed by HTTPProvider (CARD-016). It is the
// reproducible mock served over REAL HTTP: usable both by tests (wrapped in
// httptest.NewServer) and in-process by cmd/gateway (mounted on a side
// http.Server) so connection pooling and real-upstream ctx cancellation are
// genuinely exercised.
//
// It serves POST /v1/chat/completions, branching on the request body's `stream`
// flag between an OpenAI `chat.completion` JSON response and an OpenAI SSE
// stream of `chat.completion.chunk` events terminated by `data: [DONE]`.
// Latency, error-rate (FailStatus) and stream-chunk count are controllable via
// opts.
func MockUpstreamHandler(opts MockUpstreamOptions) http.Handler {
	content := opts.Content
	if content == "" {
		content = defaultMockUpstreamContent
	}
	finish := opts.FinishReason
	if finish == "" {
		finish = defaultMockFinishReason
	}
	chunks := opts.StreamChunks
	if chunks <= 0 {
		chunks = 1
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		var req oaiRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeOAIError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		// Honour configured latency against the client context BEFORE producing
		// any output, so a cancelled request aborts promptly.
		if err := sleepCtx(ctx, opts.Latency); err != nil {
			return // client gone: write nothing.
		}

		if opts.FailStatus >= 400 {
			writeOAIError(w, opts.FailStatus, http.StatusText(opts.FailStatus))
			return
		}

		if req.Stream {
			writeStream(ctx, w, req, content, finish, chunks, opts)
			return
		}

		writeUnary(w, req, content, finish, opts.Usage)
	})
	return mux
}

// writeUnary emits the OpenAI unary `chat.completion` JSON response.
func writeUnary(w http.ResponseWriter, req oaiRequest, content, finish string, usage Usage) {
	resp := map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": finish,
			},
		},
		"usage": toOAIUsageMap(usage),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// writeStream emits the content split into `chunks` OpenAI `chat.completion.chunk`
// delta events (each preceded by a latency delay), a final chunk carrying the
// finish_reason, then — when the request asked for stream_options.include_usage
// and OmitStreamUsage is not set — a trailing usage chunk (empty choices,
// populated usage), and finally a `data: [DONE]` sentinel. It flushes after
// every event and stops promptly when the client disconnects (ctx cancelled),
// aborting the upstream stream.
func writeStream(ctx context.Context, w http.ResponseWriter, req oaiRequest, content, finish string, chunks int, opts MockUpstreamOptions) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	deltas := splitContent(content, chunks)
	for _, delta := range deltas {
		if err := sleepCtx(ctx, opts.Latency); err != nil {
			return // client disconnected mid-stream.
		}
		ev := map[string]any{
			"object": "chat.completion.chunk",
			"model":  req.Model,
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]any{"content": delta}, "finish_reason": nil},
			},
		}
		if !writeEvent(w, ev) {
			return
		}
		flush()
	}

	// Final chunk: empty delta + finish_reason.
	finalChunk := map[string]any{
		"object": "chat.completion.chunk",
		"model":  req.Model,
		"choices": []map[string]any{
			{"index": 0, "delta": map[string]any{}, "finish_reason": finish},
		},
	}
	if !writeEvent(w, finalChunk) {
		return
	}
	flush()

	// Trailing usage chunk (FR-019): emitted only when the request asked for
	// include_usage and OmitStreamUsage is not set. Empty choices, populated
	// usage — exactly the OpenAI shape the adapter parses for terminal metering.
	includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
	if includeUsage && !opts.OmitStreamUsage {
		usageChunk := map[string]any{
			"object":  "chat.completion.chunk",
			"model":   req.Model,
			"choices": []map[string]any{},
			"usage":   toOAIUsageMap(opts.Usage),
		}
		if !writeEvent(w, usageChunk) {
			return
		}
		flush()
	}

	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flush()
}

// writeEvent serialises v as a single SSE `data:` line. It reports whether the
// write succeeded (a failed write means the client is gone).
func writeEvent(w http.ResponseWriter, v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err == nil
}

// writeOAIError emits the OpenAI error envelope {error:{message,type,code}} with
// the given HTTP status, so the adapter's error-body parsing is exercised.
func writeOAIError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "mock_error",
			"code":    http.StatusText(status),
		},
	})
}

// toOAIUsageMap renders canonical Usage as the OpenAI usage object.
func toOAIUsageMap(u Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
}
