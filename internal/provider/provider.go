// Package provider defines COMP-005: the single Provider interface that is the
// anti-corruption layer (ADR-0009) between the gateway core (CTX-001) and any
// external LLM provider (EXT-001).
//
// The Provider interface and the canonical types Request, Response and Chunk
// are owned by the gateway. Real provider adapters (OpenAI, Anthropic, ...)
// translate their wire format (HTTP JSON / SSE) to and from these canonical
// types, and normalise provider-specific usage into the canonical Usage fields.
// Because of this, downstream contexts — notably CTX-004 metering (CARD-010) —
// read canonical usage from Response/Chunk and never import a provider package.
//
// This package ships exactly one implementation in v1: Mock (see mock.go), a
// configurable test double. Real adapters are deferred to later cards; see the
// "Writing a real adapter" note at the bottom of this file for the contract
// they must satisfy.
package provider

import "context"

// Provider is the single port through which the gateway executes inference.
// Per ADR-0009 it exposes exactly two methods — one per request path — and no
// provider-specific type ever crosses this boundary.
//
// Implementations MUST honour ctx cancellation and deadlines on every call: a
// cancelled ctx aborts the in-flight upstream call and surfaces ctx.Err()
// (FR-003). The per-provider circuit breaker (FR-007) wraps these two methods.
type Provider interface {
	// Infer executes the unary (non-streaming) path: one Request maps to one
	// Response (FR-001). It returns ctx.Err() if ctx is cancelled or its
	// deadline elapses before the Response is ready.
	Infer(ctx context.Context, req Request) (Response, error)

	// InferStream executes the streaming path (SSE, FR-002): it returns a
	// receive-only channel of Chunks. The returned error reports a failure to
	// *initialise* the stream; per-chunk transport errors are delivered as a
	// terminal Chunk on the channel (Chunk.Err). The implementation owns the
	// channel and MUST close it when the stream ends — on success, on error, or
	// when ctx is cancelled — so callers can range over it safely without
	// leaking goroutines.
	InferStream(ctx context.Context, req Request) (<-chan Chunk, error)
}

// Role identifies the author of a Message in the canonical chat format. It is
// provider-agnostic; adapters map provider-specific role strings onto these
// values.
type Role string

// Canonical message roles.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one canonical chat message. Adapters translate provider-specific
// message shapes to and from this type.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

// Request is the canonical inference request used everywhere inside the
// gateway. Adapters normalise an incoming client/provider wire request into a
// Request on the way in. It deliberately carries only provider-agnostic fields;
// provider-specific tuning is the adapter's concern.
type Request struct {
	// Model selects the upstream model (FR-002 routing key).
	Model string `json:"model"`

	// Messages is the ordered chat history for the completion.
	Messages []Message `json:"messages"`

	// Stream requests the streaming path. Routing between Infer and InferStream
	// is the caller's decision (CARD-003/004); this flag carries the client's
	// intent through the canonical request.
	Stream bool `json:"stream,omitempty"`

	// MaxTokens optionally bounds the completion length. Zero means "unset"
	// (let the provider apply its default).
	MaxTokens int `json:"max_tokens,omitempty"`

	// Temperature optionally controls sampling. Nil means "unset".
	Temperature *float64 `json:"temperature,omitempty"`
}

// Usage is the canonical, provider-agnostic token accounting for a completion.
// Adapters MUST populate these fields from whatever shape the upstream provider
// reports, so that CTX-004 metering (CARD-010) reads usage from here and never
// from a provider-specific field (ADR-0009 ACL guarantee).
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Response is the canonical unary inference result. Adapters normalise the
// upstream provider's response body into this type, including normalised Usage.
type Response struct {
	// Model is the model that produced the completion (may differ from the
	// requested alias once the adapter resolves it).
	Model string `json:"model"`

	// Content is the assistant completion text.
	Content string `json:"content"`

	// FinishReason is the canonical reason the completion ended (e.g. "stop",
	// "length"). Adapters map provider-specific reasons onto a common set.
	FinishReason string `json:"finish_reason,omitempty"`

	// Usage is the normalised token accounting (see Usage).
	Usage Usage `json:"usage"`
}

// Chunk is one streamed delta on the streaming path. A stream is a sequence of
// Chunks terminated by the channel closing.
//
// Exactly one terminal condition ends the stream:
//   - the final content Chunk may carry Done = true and a populated Usage, or
//   - a Chunk may carry a non-nil Err describing a mid-stream transport failure;
//
// in both cases the implementation then closes the channel.
type Chunk struct {
	// Content is the incremental text delta for this chunk. Empty on a terminal
	// usage-only or error chunk.
	Content string `json:"content,omitempty"`

	// Done marks the final, successful chunk of a stream.
	Done bool `json:"done,omitempty"`

	// Usage carries the normalised token accounting and is populated only on the
	// terminal chunk (Done == true), mirroring Response.Usage for metering.
	Usage Usage `json:"usage,omitempty"`

	// Err is non-nil when the stream failed mid-flight. It is provider-agnostic:
	// adapters map provider-specific errors to canonical errors here.
	Err error `json:"-"`
}

// Writing a real adapter (deferred — not implemented in CARD-002)
//
// A real provider adapter (e.g. internal/provider/openai, internal/provider/
// anthropic) implements Provider and acts as the anti-corruption layer:
//
//   - Inbound: translate the canonical Request into the provider's wire request
//     (auth headers, body shape, model alias resolution).
//   - Outbound (unary): parse the provider's HTTP JSON response into Response,
//     normalising token usage into the canonical Usage fields.
//   - Outbound (streaming): consume the provider's SSE stream, emit one Chunk
//     per delta, and close the channel when "[DONE]"/EOF/ctx-cancel is reached.
//   - Errors: map provider-specific HTTP errors (e.g. 429/5xx with differing
//     bodies across OpenAI and Anthropic) to canonical errors, so the gateway
//     core and the circuit breaker (FR-007) stay provider-agnostic.
//
// No provider package may leak its own types across the Provider boundary.
