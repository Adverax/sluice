package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// HTTPProvider is a Provider adapter that reaches a mock LLM upstream over REAL
// HTTP (CARD-013). It is the production wiring of the ADR-0009 anti-corruption
// layer: it translates the canonical Request into the upstream wire request,
// POSTs it through an INJECTED *http.Client (so process-wide connection pooling
// — ADR-0010, NFR-004 — is genuinely exercised), and maps the upstream JSON /
// SSE response back into the canonical Response / Chunk types. No upstream wire
// type ever crosses the Provider boundary.
//
// Every call builds its request with http.NewRequestWithContext, so a cancelled
// or deadline-exceeded ctx aborts the in-flight upstream HTTP call and surfaces
// a ctx-wrapping error (FR-003). Upstream non-2xx statuses are mapped to
// *StatusError so the resilience layer (FR-006/FR-007) keeps its classification:
// 5xx retryable, 4xx not.
//
// This is still a MOCK upstream in v1 (no real OpenAI/Anthropic — a deliberate
// non-goal); see MockUpstreamHandler for the server side.
type HTTPProvider struct {
	client  *http.Client
	baseURL string
}

// HTTPOption configures an HTTPProvider via the functional-options pattern
// (CON-001).
type HTTPOption func(*HTTPProvider)

// NewHTTP constructs an HTTPProvider that POSTs to baseURL using the injected
// client. The client is REQUIRED — pooling and the explicit total timeout are
// owned by the caller (cmd/gateway), never by a package-global transport. A nil
// client falls back to http.DefaultClient so the zero-config path still works,
// but production wiring always injects the tuned client.
func NewHTTP(client *http.Client, baseURL string, opts ...HTTPOption) *HTTPProvider {
	if client == nil {
		client = http.DefaultClient
	}
	p := &HTTPProvider{
		client:  client,
		baseURL: strings.TrimRight(baseURL, "/"),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Upstream wire types (ADR-0009 ACL boundary).
//
// These are DELIBERATELY distinct from the canonical Request/Response/Chunk so
// the mapping is explicit and no upstream shape leaks across the Provider
// interface. They model an OpenAI-ish chat-completion API served by our mock
// upstream; a real adapter would shape these to its provider's actual wire
// format.

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type wireRequest struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Stream      bool          `json:"stream,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
}

type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type wireResponse struct {
	Model        string    `json:"model"`
	Content      string    `json:"content"`
	FinishReason string    `json:"finish_reason"`
	Usage        wireUsage `json:"usage"`
}

// wireStreamEvent is one SSE `data:` payload on the streaming path. A delta
// carries Content; the terminal event carries Done=true and the final Usage.
type wireStreamEvent struct {
	Content string    `json:"content,omitempty"`
	Done    bool      `json:"done,omitempty"`
	Usage   wireUsage `json:"usage,omitempty"`
}

// toWireRequest maps the canonical Request to the upstream wire request
// (inbound half of the ACL).
func toWireRequest(req Request, stream bool) wireRequest {
	msgs := make([]wireMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = wireMessage{Role: string(m.Role), Content: m.Content}
	}
	return wireRequest{
		Model:       req.Model,
		Messages:    msgs,
		Stream:      stream,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
}

// toCanonicalUsage maps wire usage to canonical Usage (outbound half of the
// ACL). The two types share a field set today, so a direct conversion is the
// mapping; should either shape diverge, this becomes explicit field assignment.
func toCanonicalUsage(u wireUsage) Usage {
	return Usage(u)
}

// toWireUsage maps canonical Usage to wire usage (inbound half of the ACL).
func toWireUsage(u Usage) wireUsage {
	return wireUsage(u)
}

// Infer implements Provider on the unary path. It POSTs the canonical request as
// upstream-wire JSON and maps the upstream JSON response into a canonical
// Response. ctx cancellation aborts the in-flight HTTP call and is surfaced as a
// ctx-wrapping error; an upstream non-2xx status becomes a *StatusError.
func (p *HTTPProvider) Infer(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(toWireRequest(req, false))
	if err != nil {
		return Response{}, fmt.Errorf("provider: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("provider: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return Response{}, mapTransportError(err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Response{}, statusErrorFromResponse(resp)
	}

	var wr wireResponse
	if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
		return Response{}, fmt.Errorf("provider: decode response: %w", err)
	}

	return Response{
		Model:        wr.Model,
		Content:      wr.Content,
		FinishReason: wr.FinishReason,
		Usage:        toCanonicalUsage(wr.Usage),
	}, nil
}

// InferStream implements Provider on the streaming path. It requests the SSE
// variant and, on a 2xx, returns a channel onto which a background goroutine
// reads the response body incrementally, emitting one canonical Chunk per SSE
// `data:` event and a terminal Done chunk carrying the normalised Usage. A
// failure to INITIALISE the stream (transport error, non-2xx status) is returned
// synchronously as an error and no channel is produced.
//
// The reader goroutine selects on ctx.Done() at every send and closes the
// response body on exit, so a cancelled ctx (client disconnect) aborts the
// upstream stream promptly, stops emission, and closes the channel — no
// goroutine leak.
func (p *HTTPProvider) InferStream(ctx context.Context, req Request) (<-chan Chunk, error) {
	body, err := json.Marshal(toWireRequest(req, true))
	if err != nil {
		return nil, fmt.Errorf("provider: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("provider: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, mapTransportError(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := statusErrorFromResponse(resp)
		drainAndClose(resp.Body)
		return nil, err
	}

	out := make(chan Chunk)
	go p.streamLoop(ctx, resp.Body, out)
	return out, nil
}

// streamLoop reads SSE events from body and forwards canonical Chunks on out.
// It owns body (closes it on exit) and out (closes it on exit). It stops on EOF,
// a terminal Done event, ctx cancellation, or a read/decode error (delivered as
// a terminal error Chunk).
func (p *HTTPProvider) streamLoop(ctx context.Context, body io.ReadCloser, out chan<- Chunk) {
	defer close(out)
	defer drainAndClose(body)

	scanner := bufio.NewScanner(body)
	// Allow generous SSE lines without an unbounded buffer.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for scanner.Scan() {
		// Honour cancellation between events: a client disconnect aborts the
		// upstream read (closing body on return) and stops emitting.
		if err := ctx.Err(); err != nil {
			return
		}

		line := scanner.Text()
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue // SSE comment, blank separator, or event/id line — skip.
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var ev wireStreamEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			sendChunk(ctx, out, Chunk{Err: fmt.Errorf("provider: decode stream event: %w", err)})
			return
		}

		if ev.Done {
			sendChunk(ctx, out, Chunk{Done: true, Usage: toCanonicalUsage(ev.Usage)})
			return
		}
		if !sendChunk(ctx, out, Chunk{Content: ev.Content}) {
			return // ctx cancelled mid-send.
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		// A genuine read error (not a ctx-driven abort) is surfaced as a terminal
		// chunk. mapTransportError unwraps a ctx cause if the transport wrapped one.
		sendChunk(ctx, out, Chunk{Err: mapTransportError(err)})
	}
}

// sendChunk delivers c on out while honouring ctx. It reports whether the send
// succeeded; a false return means ctx was cancelled and the caller must stop.
func sendChunk(ctx context.Context, out chan<- Chunk, c Chunk) bool {
	select {
	case out <- c:
		return true
	case <-ctx.Done():
		return false
	}
}

// statusErrorFromResponse builds a canonical *StatusError from a non-2xx
// upstream response, so retry/breaker classification (5xx retryable, 4xx not)
// works without string-matching. The upstream body is read for a short message
// but its shape never crosses the boundary.
func statusErrorFromResponse(resp *http.Response) error {
	msg := http.StatusText(resp.StatusCode)
	if b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<10)); err == nil && len(b) > 0 {
		msg = strings.TrimSpace(string(b))
	}
	return NewStatusError(resp.StatusCode, msg)
}

// mapTransportError normalises a *http.Client.Do transport error. When the
// failure is caused by ctx cancellation/deadline, the ctx error is surfaced
// (unwrapped) so callers and tests can match it with errors.Is(err,
// context.Canceled / context.DeadlineExceeded) (FR-003).
func mapTransportError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return fmt.Errorf("provider: upstream request failed: %w", err)
}

// drainAndClose drains and closes an HTTP response body so the underlying
// connection can be returned to the pool for reuse (ADR-0010). A small drain cap
// avoids reading an unbounded body on the error path.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4*1024))
	_ = body.Close()
}
