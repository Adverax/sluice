package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/adverax/sluice/internal/api"
	"github.com/adverax/sluice/internal/health"
	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/proxy"
	"github.com/adverax/sluice/internal/server"
)

// edgeServer wires a spyProvider behind the full generated boundary for the
// OpenAI-edge ACs (CARD-017). It returns the handler and the spy so a test can
// assert both the wire response and what the edge forwarded canonically.
func edgeServer(t *testing.T, resp provider.Response, err error) (http.Handler, *spyProvider) {
	t.Helper()
	spy := &spyProvider{resp: resp, err: err}
	router := proxy.NewRouter()
	router.Register("gpt-4", spy)
	hh := health.New(discardLogger(), 0)
	srv := server.New(router, hh, discardLogger())
	return srv.Handler(http.NewServeMux()), spy
}

// AC-053 — a valid OpenAI request is accepted and the modeled fields (model,
// messages, stream, temperature, top_p, max_tokens, stop) are forwarded as
// canonical fields.
func TestEdge_OpenAIRequest_Accepted(t *testing.T) {
	h, spy := edgeServer(t, provider.Response{Model: "gpt-4", Content: "hi", FinishReason: "stop"}, nil)

	body := `{
		"model":"gpt-4",
		"messages":[{"role":"system","content":"be terse"},{"role":"user","content":"hi"}],
		"temperature":0.5,
		"top_p":0.9,
		"max_tokens":42,
		"stop":["\n","END"]
	}`
	rec := doJSON(t, h, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !spy.called {
		t.Fatal("provider was not contacted")
	}
	got := spy.lastReq
	if got.Model != "gpt-4" {
		t.Errorf("model = %q, want gpt-4", got.Model)
	}
	if len(got.Messages) != 2 || got.Messages[0].Role != provider.RoleSystem || got.Messages[1].Content != "hi" {
		t.Errorf("messages not forwarded: %+v", got.Messages)
	}
	if got.MaxTokens != 42 {
		t.Errorf("max_tokens = %d, want 42", got.MaxTokens)
	}
	if got.Temperature == nil || *got.Temperature < 0.49 || *got.Temperature > 0.51 {
		t.Errorf("temperature = %v, want ~0.5", got.Temperature)
	}
	if got.TopP == nil || *got.TopP < 0.89 || *got.TopP > 0.91 {
		t.Errorf("top_p = %v, want ~0.9", got.TopP)
	}
	if len(got.Stop) != 2 || got.Stop[1] != "END" {
		t.Errorf("stop = %v, want [\\n END]", got.Stop)
	}
}

// AC-053b — the OpenAI `stop` field accepts a scalar string (not just an array).
// A request with `"stop":"\n"` must return 200 (not 400) and the scalar is
// forwarded as a single-element stop list to the canonical provider.Request.
func TestEdge_ScalarStop_AcceptedAndNormalized(t *testing.T) {
	h, spy := edgeServer(t, provider.Response{Model: "gpt-4", Content: "hi", FinishReason: "stop"}, nil)

	rec := doJSON(t, h, `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stop":"\n"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("scalar stop: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !spy.called {
		t.Fatal("provider was not contacted")
	}
	got := spy.lastReq
	if len(got.Stop) != 1 || got.Stop[0] != "\n" {
		t.Errorf("scalar stop not normalized: got %v, want [\"\\n\"]", got.Stop)
	}
}

// AC-054 — unknown OpenAI fields are accepted (200, not 400) and NOT forwarded
// to the provider (liberal-accept, ADR-0012 §3).
func TestEdge_UnknownFields_IgnoredNot400(t *testing.T) {
	h, spy := edgeServer(t, provider.Response{Model: "gpt-4", Content: "ok", FinishReason: "stop"}, nil)

	body := `{
		"model":"gpt-4",
		"messages":[{"role":"user","content":"hi","name":"bob"}],
		"seed":7,
		"user":"u-1",
		"presence_penalty":0.1,
		"frequency_penalty":0.2,
		"logit_bias":{"50256":-100},
		"response_format":{"type":"json_object"},
		"n":1,
		"logprobs":true
	}`
	rec := doJSON(t, h, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unknown fields must not 400); body=%s", rec.Code, rec.Body.String())
	}
	if !spy.called {
		t.Fatal("provider was not contacted")
	}
	// The canonical request carries only the modeled subset; nothing reflects the
	// ignored fields (the canonical Request has no seed/user/penalty fields).
	if spy.lastReq.Model != "gpt-4" || len(spy.lastReq.Messages) != 1 {
		t.Errorf("unexpected forwarded request: %+v", spy.lastReq)
	}
}

// AC-055 — an unsupported shape (n>1, or multimodal/array content) is rejected
// with an OpenAI-shaped 400 WITHOUT contacting the provider.
func TestEdge_UnsupportedContent_Returns400(t *testing.T) {
	t.Run("n>1", func(t *testing.T) {
		h, spy := edgeServer(t, provider.Response{}, nil)
		rec := doJSON(t, h, `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"n":2}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
		}
		if spy.called {
			t.Error("provider must not be contacted for n>1")
		}
		assertOpenAIError(t, rec.Body.Bytes())
	})

	t.Run("array content", func(t *testing.T) {
		h, spy := edgeServer(t, provider.Response{}, nil)
		// Multimodal/array content fails the content:string schema at the
		// validator BEFORE the handler runs.
		rec := doJSON(t, h, `{"model":"gpt-4","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
		}
		if spy.called {
			t.Error("provider must not be contacted for array content")
		}
		assertOpenAIError(t, rec.Body.Bytes())
	})
}

// AC-056 — the unary response has the real OpenAI chat.completion shape.
func TestEdge_UnaryResponse_OpenAIShape(t *testing.T) {
	h, _ := edgeServer(t, provider.Response{
		Model:        "gpt-4",
		Content:      "hello world",
		FinishReason: "stop",
		Usage:        provider.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
	}, nil)

	rec := doJSON(t, h, `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	var got api.ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", got.Object)
	}
	if !strings.HasPrefix(got.Id, "chatcmpl-") {
		t.Errorf("id = %q, want chatcmpl- prefix", got.Id)
	}
	if got.Created == 0 {
		t.Error("created timestamp is zero")
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d, want exactly 1", len(got.Choices))
	}
	c := got.Choices[0]
	if c.Index != 0 || c.Message.Role != "assistant" || c.Message.Content != "hello world" {
		t.Errorf("unexpected choice: %+v", c)
	}
	if c.FinishReason == nil || *c.FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop", c.FinishReason)
	}
	if got.Usage.TotalTokens != 5 || got.Usage.PromptTokens != 3 || got.Usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v, want 3/2/5", got.Usage)
	}
}

// AC-057 — id/object/created are generated at the edge (not passed through from
// upstream), and system_fingerprint is omitted.
func TestEdge_UnaryResponse_EdgeGeneratedFields(t *testing.T) {
	// The canonical Response carries NO id/created/object/system_fingerprint, so
	// the edge must synthesise them. Two requests must yield distinct ids.
	h, _ := edgeServer(t, provider.Response{Model: "gpt-4", Content: "x", FinishReason: "stop"}, nil)

	dec := func() api.ChatCompletionResponse {
		rec := doJSON(t, h, `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var r api.ChatCompletionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// system_fingerprint must be absent from the raw JSON.
		if strings.Contains(rec.Body.String(), "system_fingerprint") {
			t.Error("system_fingerprint must be omitted")
		}
		return r
	}

	r1, r2 := dec(), dec()
	if r1.Id == "" || r2.Id == "" {
		t.Fatal("edge did not generate an id")
	}
	if r1.Id == r2.Id {
		t.Errorf("ids must be unique per request, got %q twice", r1.Id)
	}
	if r1.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", r1.Object)
	}
	if r1.Created == 0 {
		t.Error("edge did not generate a created timestamp")
	}
}

// AC-058 — a streaming request emits OpenAI chat.completion.chunk events
// terminated by a literal `data: [DONE]`.
func TestEdge_Streaming_OpenAIChunksAndDone(t *testing.T) {
	mock := provider.New(
		provider.WithResponse(provider.Response{Model: "gpt-4", Content: "hello world", Usage: provider.Usage{TotalTokens: 7}}),
		provider.WithStreamChunks(3),
	)
	router := proxy.NewRouter()
	router.Register("gpt-4", mock)
	hh := health.New(discardLogger(), 0)
	h := server.New(router, hh, discardLogger()).Handler(http.NewServeMux())

	rec := doJSON(t, h, `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	body := rec.Body.String()
	if !strings.HasSuffix(strings.TrimRight(body, "\n"), "data: [DONE]") {
		t.Errorf("stream must terminate with data: [DONE], body=%q", body)
	}

	var sawChunk, sawDelta bool
	var streamID string
	for _, line := range strings.Split(body, "\n") {
		const p = "data: "
		if !strings.HasPrefix(line, p) {
			continue
		}
		payload := strings.TrimPrefix(line, p)
		if payload == "[DONE]" {
			continue
		}
		var ev struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			Model   string `json:"model"`
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("chunk is not valid JSON: %v (payload=%q)", err, payload)
		}
		if ev.Object != "chat.completion.chunk" {
			t.Errorf("chunk object = %q, want chat.completion.chunk", ev.Object)
		}
		sawChunk = true
		if streamID == "" {
			streamID = ev.ID
		} else if ev.ID != streamID {
			t.Errorf("chunk id changed mid-stream: %q != %q", ev.ID, streamID)
		}
		if !strings.HasPrefix(ev.ID, "chatcmpl-") {
			t.Errorf("chunk id = %q, want chatcmpl- prefix", ev.ID)
		}
		if len(ev.Choices) > 0 && ev.Choices[0].Delta.Content != "" {
			sawDelta = true
		}
	}
	if !sawChunk {
		t.Error("no chat.completion.chunk events seen")
	}
	if !sawDelta {
		t.Error("no content delta seen in stream")
	}
}

// AC-060 — a gateway-originated error (503 fast-fail) is rendered in the OpenAI
// error envelope with the right status.
func TestEdge_GatewayError_OpenAIShape(t *testing.T) {
	spy := &spyProvider{}
	router := proxy.NewRouter()
	router.Register("gpt-4", spy)
	hh := health.New(discardLogger(), 0)
	// Inject an infer hook that fast-fails like an open breaker (server-owned
	// sentinel → 503), so we exercise the gateway-error mapping without a real
	// breaker.
	srv := server.New(router, hh, discardLogger(),
		server.WithInferFunc(func(context.Context, provider.Provider, provider.Request) (provider.Response, error) {
			return provider.Response{}, server.ErrServiceUnavailable
		}),
	)
	h := srv.Handler(http.NewServeMux())

	rec := doJSON(t, h, `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	d := assertOpenAIError(t, rec.Body.Bytes())
	if d.Type != "service_unavailable" {
		t.Errorf("error type = %q, want service_unavailable", d.Type)
	}
}

// AC-061 — an upstream provider error (non-2xx, retries exhausted) maps to a 502
// in the OpenAI error envelope.
func TestEdge_UpstreamError_MappedToOpenAIShape(t *testing.T) {
	h, spy := edgeServer(t, provider.Response{}, provider.NewStatusError(http.StatusInternalServerError, "boom"))

	rec := doJSON(t, h, `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	if !spy.called {
		t.Error("provider should have been contacted")
	}
	d := assertOpenAIError(t, rec.Body.Bytes())
	if d.Message == "" || d.Type == "" {
		t.Errorf("incomplete error detail: %+v", d)
	}
}

// errDetail mirrors the OpenAI error detail for assertions.
type errDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Code    *string `json:"code"`
}

// assertOpenAIError decodes an OpenAI error envelope {error:{message,type,code}}
// and fails the test if it is not well-formed; it returns the detail for further
// field assertions.
func assertOpenAIError(t *testing.T, body []byte) errDetail {
	t.Helper()
	var env struct {
		Error errDetail `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("error body is not valid JSON: %v (body=%s)", err, body)
	}
	if env.Error.Message == "" {
		t.Errorf("error envelope missing message: %s", body)
	}
	if env.Error.Type == "" {
		t.Errorf("error envelope missing type: %s", body)
	}
	return env.Error
}
