// edge.go holds the OpenAI-compatible edge adapter (ADR-0012): the mapping
// between the generated OpenAI DTOs (api.ChatCompletionRequest/Response and the
// SSE chunk shape) and the canonical provider.Request/Response/Chunk that cross
// the Provider boundary (ADR-0009 ACL). The OpenAI wire shape — `chat.completion`
// objects, `chat.completion.chunk` SSE events, the `{error:{message,type,code}}`
// envelope, and the edge-generated id/created/object — lives ONLY here, so the
// proxy core and the resilience/metering/observability layers stay
// provider-agnostic.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/adverax/sluice/internal/api"
	"github.com/adverax/sluice/internal/provider"
)

// OpenAI object discriminators emitted on the edge.
const (
	objectChatCompletion      = "chat.completion"
	objectChatCompletionChunk = "chat.completion.chunk"
	roleAssistant             = "assistant"
)

// edgeError is the gateway's internal representation of an OpenAI-shaped error
// before it is rendered to a concrete generated response. It carries the OpenAI
// `type`/`code`/`message` triple (FR-020) and a human-readable message.
type edgeError struct {
	httpStatus int
	message    string
	typ        string
	code       string
}

// newID generates an edge-side completion id prefixed "chatcmpl-" (ADR-0012 §5).
// The id is generated at the edge — never passed through from upstream — so the
// contract is stable and backend-independent. crypto/rand is used so concurrent
// requests never collide; a failure falls back to a timestamp so an id is always
// produced (the id is informational, not a security token).
func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

// toCanonicalRequest maps the generated OpenAI request DTO onto the canonical
// provider.Request (ADR-0009 inbound ACL). Only the modeled v1-subset fields are
// forwarded (ADR-0012 §2): model, messages, stream, temperature, top_p,
// max_tokens, stop. Unknown fields carried via AdditionalProperties are accepted
// but NOT forwarded (liberal-accept, §3) — except the documented non-goal
// `n > 1`, which is detected here and rejected as an OpenAI-shaped 400 (AC-055,
// §4). Multimodal/array `content` cannot reach this point: the request validator
// rejects it against the `content: string` schema before the handler runs.
func toCanonicalRequest(body api.ChatCompletionRequest) (provider.Request, *edgeError) {
	if eerr := rejectUnsupported(body); eerr != nil {
		return provider.Request{}, eerr
	}

	req := provider.Request{
		Model:    body.Model,
		Messages: make([]provider.Message, 0, len(body.Messages)),
	}
	if body.Stream != nil {
		req.Stream = *body.Stream
	}
	if body.MaxTokens != nil {
		req.MaxTokens = *body.MaxTokens
	}
	if body.Temperature != nil {
		t := float64(*body.Temperature)
		req.Temperature = &t
	}
	if body.TopP != nil {
		p := float64(*body.TopP)
		req.TopP = &p
	}
	if body.Stop != nil {
		req.Stop = normalizeStop(body.Stop)
	}
	for _, m := range body.Messages {
		req.Messages = append(req.Messages, provider.Message{
			Role:    provider.Role(m.Role),
			Content: m.Content,
		})
	}
	return req, nil
}

// rejectUnsupported detects the documented non-goal `n > 1` (ADR-0012 §4,
// CON-008) carried as a liberal-accept additional property and maps it to an
// OpenAI-shaped 400 (AC-055). Any other shape (e.g. multimodal content) is
// already rejected by the OpenAPI request validator, so it never reaches here.
func rejectUnsupported(body api.ChatCompletionRequest) *edgeError {
	if v, ok := body.AdditionalProperties["n"]; ok {
		if n, isNum := v.(float64); isNum && n > 1 {
			return &edgeError{
				httpStatus: 400,
				message:    "n>1 is not supported: the gateway returns a single choice",
				typ:        "invalid_request_error",
				code:       "unsupported_value",
			}
		}
	}
	return nil
}

// normalizeStop converts the generated oneOf Stop union (string | []string) into
// the canonical []string used by provider.Request. A scalar string is wrapped in
// a single-element slice; an array is returned as-is; an empty array returns nil
// so the upstream adapter treats it as absent (ADR-0009).
func normalizeStop(s *api.ChatCompletionRequest_Stop) []string {
	if s == nil {
		return nil
	}
	// Try array form first (Stop1).
	if arr, err := s.AsChatCompletionRequestStop1(); err == nil && len(arr) > 0 {
		return append([]string(nil), arr...)
	}
	// Try scalar string form (Stop0).
	if str, err := s.AsChatCompletionRequestStop0(); err == nil && str != "" {
		return []string{str}
	}
	return nil
}

// toUnaryResponse maps the canonical provider.Response onto a real OpenAI
// `chat.completion` object (ADR-0012 §5, AC-056/AC-057). id/created/object are
// generated at the edge (NOT passed through from upstream); exactly one
// choices[0] with message.role "assistant" is returned; system_fingerprint is
// omitted.
func toUnaryResponse(resp provider.Response) api.ChatCompletionResponse {
	finish := resp.FinishReason
	return api.ChatCompletionResponse{
		Id:      newID(),
		Object:  objectChatCompletion,
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []api.Choice{{
			Index: 0,
			Message: api.ResponseMessage{
				Role:    roleAssistant,
				Content: resp.Content,
			},
			FinishReason: &finish,
		}},
		Usage: api.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}
}

// chunkDelta is the `choices[0].delta` of a streaming chat.completion.chunk.
// Fields are omitempty so a content delta carries only `content` and the
// terminal chunk can carry an empty delta `{}`.
type chunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// chunkChoice is one choices[] element of a streaming chunk.
type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

// streamChunk is the wire shape of one `chat.completion.chunk` SSE `data:`
// event (ADR-0012 §6). id/created/model are stable across a single stream's
// chunks (set once by the handler); object is always "chat.completion.chunk".
type streamChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
}

// streamShaper builds OpenAI streaming chunks for one stream, holding the id /
// created / model stable across every emitted chunk (ADR-0012 §6).
type streamShaper struct {
	id      string
	created int64
	model   string
}

// newStreamShaper seeds a shaper with a single edge-generated id + created for
// the whole stream.
func newStreamShaper(model string) streamShaper {
	return streamShaper{
		id:      newID(),
		created: time.Now().Unix(),
		model:   model,
	}
}

// contentChunk shapes a content delta into an OpenAI chat.completion.chunk.
func (s streamShaper) contentChunk(content string) streamChunk {
	return streamChunk{
		ID:      s.id,
		Object:  objectChatCompletionChunk,
		Created: s.created,
		Model:   s.model,
		Choices: []chunkChoice{{
			Index: 0,
			Delta: chunkDelta{Content: content},
		}},
	}
}

// finalChunk shapes the terminal chunk (empty delta, finish_reason "stop") that
// precedes the literal `data: [DONE]` sentinel.
func (s streamShaper) finalChunk() streamChunk {
	stop := "stop"
	return streamChunk{
		ID:      s.id,
		Object:  objectChatCompletionChunk,
		Created: s.created,
		Model:   s.model,
		Choices: []chunkChoice{{
			Index:        0,
			Delta:        chunkDelta{},
			FinishReason: &stop,
		}},
	}
}

// openAIError renders an edgeError as the generated OpenAI error envelope
// `{error:{message,type,code}}` (ADR-0012 §7, FR-020).
func openAIError(message, typ, code string) api.Error {
	var codePtr *string
	if code != "" {
		codePtr = &code
	}
	return api.Error{
		Error: api.ErrorDetail{
			Message: message,
			Type:    typ,
			Code:    codePtr,
		},
	}
}
