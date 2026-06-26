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

// HTTPProvider is a Provider adapter that speaks the REAL OpenAI
// /v1/chat/completions wire format to an OpenAI-compatible upstream backend
// (CARD-016, ADR-0013). It is the production wiring of the ADR-0009
// anti-corruption layer: it translates the canonical Request into an OpenAI
// chat-completion request, POSTs it through an INJECTED *http.Client (so
// process-wide connection pooling — ADR-0010, NFR-004 — is genuinely
// exercised), and maps the OpenAI JSON / SSE response back into the canonical
// Response / Chunk types. No OpenAI wire type ever crosses the Provider
// boundary.
//
// The backend is configured by base URL + optional bearer key + model:
//   - Ollama (primary showcase): http://localhost:11434/v1, no key.
//   - OpenAI / vLLM / LM Studio: same wire, configured by base URL + key.
//
// The base URL already includes the /v1 segment (e.g.
// http://localhost:11434/v1); the adapter POSTs to <baseURL>/chat/completions.
//
// Every call builds its request with http.NewRequestWithContext, so a cancelled
// or deadline-exceeded ctx aborts the in-flight upstream HTTP call and surfaces
// a ctx-wrapping error (FR-003). Upstream non-2xx statuses are mapped to
// *StatusError so the resilience layer (FR-006/FR-007) keeps its classification:
// 5xx retryable, 4xx not. The OpenAI error body {error:{message,type,code}} is
// parsed into the error message when present.
type HTTPProvider struct {
	client  *http.Client
	baseURL string
	apiKey  string
	model   string
}

// HTTPOption configures an HTTPProvider via the functional-options pattern
// (CON-001).
type HTTPOption func(*HTTPProvider)

// WithAPIKey sets the upstream bearer key. When non-empty, the adapter sends
// `Authorization: Bearer <key>`; when empty (Ollama), no Authorization header
// is sent (ADR-0013 §3). The key is upstream-only and never the client edge key.
func WithAPIKey(key string) HTTPOption {
	return func(p *HTTPProvider) { p.apiKey = key }
}

// WithModel sets a fallback upstream model used when a Request carries no Model.
// The Request.Model, when set, always takes precedence (passed through as-is,
// ADR-0013 §4).
func WithModel(model string) HTTPOption {
	return func(p *HTTPProvider) { p.model = model }
}

// NewHTTP constructs an HTTPProvider that POSTs OpenAI chat-completion requests
// to baseURL using the injected client. baseURL must already include the /v1
// segment (e.g. http://localhost:11434/v1). The client is REQUIRED — pooling and
// the explicit total timeout are owned by the caller (cmd/gateway), never by a
// package-global transport. A nil client falls back to http.DefaultClient so the
// zero-config path still works, but production wiring always injects the tuned
// client.
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

// OpenAI wire types (ADR-0009 ACL boundary).
//
// These are DELIBERATELY private to this adapter so no OpenAI shape leaks across
// the Provider interface. They model the real OpenAI /v1/chat/completions API
// served by Ollama / OpenAI / vLLM / LM Studio.

type oaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// oaiStreamOptions carries the OpenAI stream_options object. include_usage asks
// the backend to emit a trailing usage chunk on the SSE path (FR-019).
type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiRequest struct {
	Model         string            `json:"model"`
	Messages      []oaiMessage      `json:"messages"`
	Stream        bool              `json:"stream,omitempty"`
	StreamOptions *oaiStreamOptions `json:"stream_options,omitempty"`
	Temperature   *float64          `json:"temperature,omitempty"`
	TopP          *float64          `json:"top_p,omitempty"`
	MaxTokens     int               `json:"max_tokens,omitempty"`
	Stop          []string          `json:"stop,omitempty"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// oaiChoice is one unary choice: choices[i].message.content + finish_reason.
type oaiChoice struct {
	Index        int        `json:"index"`
	Message      oaiMessage `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

type oaiResponse struct {
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   oaiUsage    `json:"usage"`
}

// oaiStreamChoice is one streamed choice: choices[i].delta.{role,content} +
// finish_reason. finish_reason is null on intermediate chunks and set on the
// final one; we only consume delta.content (finish_reason is not relayed to the
// canonical Chunk today).
type oaiStreamChoice struct {
	Index        int      `json:"index"`
	Delta        oaiDelta `json:"delta"`
	FinishReason string   `json:"finish_reason"`
}

type oaiDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// oaiStreamEvent is one SSE `data:` payload on the streaming path. A delta
// carries choices[0].delta.content; the trailing usage chunk carries an empty
// choices list and a populated usage (FR-019).
type oaiStreamEvent struct {
	Choices []oaiStreamChoice `json:"choices"`
	Usage   *oaiUsage         `json:"usage,omitempty"`
}

// oaiError is the OpenAI error envelope {error:{message,type,code}} parsed off a
// non-2xx body to enrich the canonical *StatusError message.
type oaiErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// toOAIRequest maps the canonical Request to the OpenAI request (inbound half of
// the ACL). Only modeled canonical fields are forwarded (CON-007/CON-008). When
// stream is set, stream_options.include_usage is requested so the backend emits
// the trailing usage chunk (FR-019).
func (p *HTTPProvider) toOAIRequest(req Request, stream bool) oaiRequest {
	msgs := make([]oaiMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = oaiMessage{Role: string(m.Role), Content: m.Content}
	}
	model := req.Model
	if model == "" {
		model = p.model
	}
	out := oaiRequest{
		Model:       model,
		Messages:    msgs,
		Stream:      stream,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
		Stop:        req.Stop,
	}
	if stream {
		out.StreamOptions = &oaiStreamOptions{IncludeUsage: true}
	}
	return out
}

// toCanonicalUsage maps OpenAI usage to canonical Usage (outbound half of the
// ACL). A nil pointer (no usage reported) maps to the zero Usage — the graceful
// uncounted path (FR-019).
func toCanonicalUsage(u *oaiUsage) Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
}

// Infer implements Provider on the unary path. It POSTs the canonical request as
// an OpenAI chat-completion request and maps choices[0].message.content +
// usage into a canonical Response. ctx cancellation aborts the in-flight HTTP
// call and is surfaced as a ctx-wrapping error; an upstream non-2xx status
// becomes a *StatusError.
func (p *HTTPProvider) Infer(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(p.toOAIRequest(req, false))
	if err != nil {
		return Response{}, fmt.Errorf("provider: marshal request: %w", err)
	}

	httpReq, err := p.newRequest(ctx, body, "application/json")
	if err != nil {
		return Response{}, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return Response{}, mapTransportError(err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Response{}, statusErrorFromResponse(resp)
	}

	var or oaiResponse
	if err := json.NewDecoder(resp.Body).Decode(&or); err != nil {
		return Response{}, fmt.Errorf("provider: decode response: %w", err)
	}

	// Defensive: a well-formed OpenAI response always has at least one choice,
	// but a degenerate backend might return none. Map to an empty completion
	// rather than panic; usage still flows through for metering.
	var content, finish string
	if len(or.Choices) > 0 {
		content = or.Choices[0].Message.Content
		finish = or.Choices[0].FinishReason
	}

	return Response{
		Model:        or.Model,
		Content:      content,
		FinishReason: finish,
		Usage:        toCanonicalUsage(&or.Usage),
	}, nil
}

// InferStream implements Provider on the streaming path. It requests the SSE
// variant (with stream_options.include_usage) and, on a 2xx, returns a channel
// onto which a background goroutine reads the response body incrementally,
// emitting one canonical Chunk per OpenAI delta and a terminal Done chunk
// carrying the normalised Usage. A failure to INITIALISE the stream (transport
// error, non-2xx status) is returned synchronously as an error and no channel is
// produced.
//
// The reader goroutine selects on ctx.Done() at every send and closes the
// response body on exit, so a cancelled ctx (client disconnect) aborts the
// upstream stream promptly, stops emission, and closes the channel — no
// goroutine leak.
func (p *HTTPProvider) InferStream(ctx context.Context, req Request) (<-chan Chunk, error) {
	body, err := json.Marshal(p.toOAIRequest(req, true))
	if err != nil {
		return nil, fmt.Errorf("provider: marshal request: %w", err)
	}

	httpReq, err := p.newRequest(ctx, body, "text/event-stream")
	if err != nil {
		return nil, err
	}

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

// newRequest builds the upstream POST with ctx, the JSON body, and the OpenAI
// headers. The Authorization header is attached only when an API key is
// configured (ADR-0013 §3 — Ollama needs none).
func (p *HTTPProvider) newRequest(ctx context.Context, body []byte, accept string) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("provider: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", accept)
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	return httpReq, nil
}

// streamLoop reads SSE events from body and forwards canonical Chunks on out.
// It owns body (closes it on exit) and out (closes it on exit). It stops on EOF,
// the literal `data: [DONE]` sentinel, ctx cancellation, or a read/decode error
// (delivered as a terminal error Chunk).
//
// Usage handling (FR-019): the trailing OpenAI usage chunk (empty choices,
// populated usage) is captured and emitted as the terminal Done chunk's Usage.
// If no usage chunk arrives (a backend that ignores include_usage), the stream
// still ends normally with a Done chunk carrying zero/uncounted usage — no
// error, no stream break.
func (p *HTTPProvider) streamLoop(ctx context.Context, body io.ReadCloser, out chan<- Chunk) {
	defer close(out)
	defer drainAndClose(body)

	scanner := bufio.NewScanner(body)
	// Allow generous SSE lines without an unbounded buffer.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var usage Usage // accumulated from the trailing usage chunk, if any.

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
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			// Graceful terminal: emit the Done chunk with whatever usage we saw
			// (zero/uncounted if the backend omitted the usage chunk).
			sendChunk(ctx, out, Chunk{Done: true, Usage: usage})
			return
		}

		var ev oaiStreamEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			sendChunk(ctx, out, Chunk{Err: fmt.Errorf("provider: decode stream event: %w", err)})
			return
		}

		// Trailing usage chunk: choices empty, usage present. Capture it for the
		// terminal Done chunk and continue (the [DONE] sentinel ends the stream).
		if ev.Usage != nil {
			usage = toCanonicalUsage(ev.Usage)
		}
		if len(ev.Choices) == 0 {
			continue
		}

		// Forward the content delta. A role-only or empty-content delta carries no
		// text; emit it as an empty content chunk only if it has content to avoid
		// noise — but we forward any non-empty delta.content.
		if delta := ev.Choices[0].Delta.Content; delta != "" {
			if !sendChunk(ctx, out, Chunk{Content: delta}) {
				return // ctx cancelled mid-send.
			}
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		// A genuine read error (not a ctx-driven abort) is surfaced as a terminal
		// chunk. mapTransportError unwraps a ctx cause if the transport wrapped one.
		sendChunk(ctx, out, Chunk{Err: mapTransportError(err)})
		return
	}

	// EOF without a [DONE] sentinel (some backends just close the stream): end
	// normally with the Done chunk and whatever usage we captured (graceful).
	if ctx.Err() == nil {
		sendChunk(ctx, out, Chunk{Done: true, Usage: usage})
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
// works without string-matching. The OpenAI error envelope
// {error:{message,type,code}} is parsed for a useful message when present;
// otherwise the raw body (capped) or the status text is used. The upstream body
// shape never crosses the boundary.
func statusErrorFromResponse(resp *http.Response) error {
	msg := http.StatusText(resp.StatusCode)
	if b, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024)); err == nil && len(b) > 0 {
		var env oaiErrorEnvelope
		if json.Unmarshal(b, &env) == nil && env.Error.Message != "" {
			msg = env.Error.Message
		} else {
			msg = strings.TrimSpace(string(b))
		}
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
