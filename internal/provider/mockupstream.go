package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// MockUpstreamOptions tunes the reproducible mock LLM upstream served over real
// HTTP (CARD-013). The zero value is a usable, instantaneous, always-success
// upstream that echoes a fixed completion.
type MockUpstreamOptions struct {
	// Content is the completion text returned by the unary endpoint and split
	// across deltas on the streaming endpoint. Empty falls back to a default.
	Content string

	// FinishReason is the canonical finish reason reported on the unary path.
	// Empty falls back to "stop".
	FinishReason string

	// Usage is the token accounting reported on both paths (echoed back through
	// the wire response / terminal stream event).
	Usage Usage

	// Latency is a per-request delay applied BEFORE the response is produced
	// (unary) and before each stream delta. It is honoured against the request
	// context so a client disconnect aborts it.
	Latency time.Duration

	// FailStatus, when >= 400, makes EVERY request fail with that HTTP status
	// (used to exercise the 5xx-retryable / 4xx-not classification). Zero means
	// "always succeed".
	FailStatus int

	// StreamChunks is the number of content deltas the streaming endpoint emits
	// before the terminal event. <= 0 defaults to 1.
	StreamChunks int
}

const (
	defaultMockUpstreamContent = "this is a mock completion"
	defaultMockFinishReason    = "stop"
)

// MockUpstreamHandler returns an http.Handler implementing the mock LLM upstream
// wire API consumed by HTTPProvider. It is the reproducible mock served over
// REAL HTTP: usable both by tests (wrapped in httptest.NewServer) and in-process
// by cmd/gateway (mounted on a side http.Server) so connection pooling and
// real-upstream ctx cancellation are genuinely exercised.
//
// It serves POST /v1/chat/completions, branching on the request body's `stream`
// flag between a JSON response and an SSE (text/event-stream) response. Latency,
// error-rate (FailStatus) and stream-chunk count are controllable via opts.
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

		var req wireRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		// Honour configured latency against the client context BEFORE producing
		// any output, so a cancelled request aborts promptly.
		if err := sleepCtx(ctx, opts.Latency); err != nil {
			return // client gone: write nothing.
		}

		if opts.FailStatus >= 400 {
			http.Error(w, http.StatusText(opts.FailStatus), opts.FailStatus)
			return
		}

		if req.Stream {
			writeStream(ctx, w, content, finish, chunks, opts)
			return
		}

		writeUnary(w, req, content, finish, opts.Usage)
	})
	return mux
}

// writeUnary maps the canonical-derived data to the upstream wire JSON response.
func writeUnary(w http.ResponseWriter, req wireRequest, content, finish string, usage Usage) {
	resp := wireResponse{
		Model:        req.Model,
		Content:      content,
		FinishReason: finish,
		Usage:        toWireUsage(usage),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// writeStream emits the content split into `chunks` SSE delta events (each
// preceded by a latency delay), then a terminal Done event carrying usage, and
// finally a `data: [DONE]` sentinel. It flushes after every event and stops
// promptly when the client disconnects (ctx cancelled), aborting the upstream
// stream.
func writeStream(ctx context.Context, w http.ResponseWriter, content, finish string, chunks int, opts MockUpstreamOptions) {
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
		if !writeEvent(w, wireStreamEvent{Content: delta}) {
			return
		}
		flush()
	}

	_ = finish // finish reason is unary-only in this mock wire shape.
	writeEvent(w, wireStreamEvent{Done: true, Usage: toWireUsage(opts.Usage)})
	flush()

	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flush()
}

// writeEvent serialises ev as a single SSE `data:` line. It reports whether the
// write succeeded (a failed write means the client is gone).
func writeEvent(w http.ResponseWriter, ev wireStreamEvent) bool {
	b, err := json.Marshal(ev)
	if err != nil {
		return false
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err == nil
}
