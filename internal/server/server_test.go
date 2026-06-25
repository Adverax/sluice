package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adverax/sluice/internal/api"
	"github.com/adverax/sluice/internal/health"
	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/proxy"
	"github.com/adverax/sluice/internal/server"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// spyProvider records whether Infer was called so negative-path tests can prove
// the provider is NOT contacted (AC-004/006/007). It can also be configured to
// return a canonical response or an error.
type spyProvider struct {
	resp   provider.Response
	err    error
	called bool
}

func (s *spyProvider) Infer(_ context.Context, _ provider.Request) (provider.Response, error) {
	s.called = true
	if s.err != nil {
		return provider.Response{}, s.err
	}
	return s.resp, nil
}

func (s *spyProvider) InferStream(context.Context, provider.Request) (<-chan provider.Chunk, error) {
	return nil, errors.New("not implemented in test")
}

// newTestServer builds the full generated HTTP boundary (api.HandlerFromMux +
// strict handler) around a Server, so tests exercise decode/route/map exactly
// as production does (ADR-0011). Extra checkers are registered on the health
// handler for readiness tests.
func newTestServer(t *testing.T, router *proxy.Router, checkers ...health.Checker) http.Handler {
	t.Helper()
	hh := health.New(discardLogger(), 0)
	hh.Register(checkers...)
	srv := server.New(router, hh, discardLogger())
	return srv.Handler(http.NewServeMux())
}

func doJSON(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestProxy_HappyPath_NonStreaming covers AC-001: a valid request to a
// registered provider returns 200 with the mapped provider response body.
func TestProxy_HappyPath_NonStreaming(t *testing.T) {
	spy := &spyProvider{resp: provider.Response{
		Model:        "gpt-4",
		Content:      "hello world",
		FinishReason: "stop",
		Usage:        provider.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
	}}
	router := proxy.NewRouter()
	router.Register("gpt-4", spy)
	h := newTestServer(t, router)

	rec := doJSON(t, h, `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !spy.called {
		t.Error("provider.Infer was not called")
	}
	var got api.ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Content != "hello world" || got.Model != "gpt-4" || got.FinishReason != "stop" {
		t.Errorf("unexpected response: %+v", got)
	}
	if got.Usage.TotalTokens != 5 {
		t.Errorf("usage = %+v, want total 5", got.Usage)
	}
}

// TestProxy_ProviderError_Returns502 covers AC-003: a provider failure maps to
// 502 with a JSON error body.
func TestProxy_ProviderError_Returns502(t *testing.T) {
	spy := &spyProvider{err: errors.New("upstream 500")}
	router := proxy.NewRouter()
	router.Register("gpt-4", spy)
	h := newTestServer(t, router)

	rec := doJSON(t, h, `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	var apiErr api.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &apiErr); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if apiErr.Error == "" {
		t.Errorf("expected non-empty error code, got %+v", apiErr)
	}
}

// TestProxy_InvalidBody_Returns400 covers AC-004: a malformed JSON body yields
// 400 without contacting the provider. The strict server handles the decode.
func TestProxy_InvalidBody_Returns400(t *testing.T) {
	spy := &spyProvider{}
	router := proxy.NewRouter()
	router.Register("gpt-4", spy)
	h := newTestServer(t, router)

	rec := doJSON(t, h, `{"model":"gpt-4", not valid json`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if spy.called {
		t.Error("provider.Infer must not be called on a malformed body")
	}
}

// TestRouter_MissingModel_Returns400 covers AC-006: an absent model yields 400
// without contacting the provider.
func TestRouter_MissingModel_Returns400(t *testing.T) {
	spy := &spyProvider{}
	router := proxy.NewRouter()
	router.Register("gpt-4", spy)
	h := newTestServer(t, router)

	rec := doJSON(t, h, `{"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if spy.called {
		t.Error("provider.Infer must not be called when model is absent")
	}
}

// TestRouter_UnknownModel_Returns404 covers AC-007: an unregistered model
// yields 404 without contacting any provider.
func TestRouter_UnknownModel_Returns404(t *testing.T) {
	spy := &spyProvider{}
	router := proxy.NewRouter()
	router.Register("gpt-4", spy)
	h := newTestServer(t, router)

	rec := doJSON(t, h, `{"model":"unregistered","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if spy.called {
		t.Error("provider.Infer must not be called for an unknown model")
	}
}

// TestRouter_RoutesToCorrectProvider covers AC-005 at the HTTP boundary: two
// providers registered for two models; the request routes to the one matching
// the model field.
func TestRouter_RoutesToCorrectProvider(t *testing.T) {
	gpt4 := &spyProvider{resp: provider.Response{Model: "gpt-4", Content: "from gpt-4"}}
	claude := &spyProvider{resp: provider.Response{Model: "claude-3", Content: "from claude"}}
	router := proxy.NewRouter()
	router.Register("gpt-4", gpt4)
	router.Register("claude-3", claude)
	h := newTestServer(t, router)

	rec := doJSON(t, h, `{"model":"claude-3","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if gpt4.called {
		t.Error("gpt-4 provider must not be called for a claude-3 request")
	}
	if !claude.called {
		t.Error("claude-3 provider was not called")
	}
	var got api.ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Content != "from claude" {
		t.Errorf("routed to wrong provider: content = %q", got.Content)
	}
}

// --- Health & readiness (FR-008/FR-009) -----------------------------------

func getReadyz(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec
}

func okChecker(name string) health.Checker {
	return health.CheckerFunc{CheckerName: name, CheckFunc: func(context.Context) error { return nil }}
}

func downChecker(name string) health.Checker {
	return health.CheckerFunc{CheckerName: name, CheckFunc: func(context.Context) error {
		return errors.New("down")
	}}
}

// TestHealthz_ReturnsOK covers AC-025: /healthz returns 200 with status ok.
func TestHealthz_ReturnsOK(t *testing.T) {
	h := newTestServer(t, proxy.NewRouter())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var hs api.HealthStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &hs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if hs.Status != "ok" {
		t.Errorf("status field = %q, want ok", hs.Status)
	}
}

// TestReadyz_AllDepsUp_Returns200 covers AC-026: both deps healthy → 200 with a
// per-dependency body.
func TestReadyz_AllDepsUp_Returns200(t *testing.T) {
	h := newTestServer(t, proxy.NewRouter(), okChecker("redis"), okChecker("postgres"))
	rec := getReadyz(t, h)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var rs api.ReadinessStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &rs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rs.Dependencies["redis"] != "ok" || rs.Dependencies["postgres"] != "ok" {
		t.Errorf("dependencies = %+v, want both ok", rs.Dependencies)
	}
}

// TestReadyz_RedisDown_Returns503 covers AC-027: redis down → 503 with redis
// reported as not "ok" in the body.
func TestReadyz_RedisDown_Returns503(t *testing.T) {
	h := newTestServer(t, proxy.NewRouter(), downChecker("redis"), okChecker("postgres"))
	rec := getReadyz(t, h)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	var rs api.ReadinessStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &rs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rs.Dependencies["redis"] == "ok" || rs.Dependencies["redis"] == "" {
		t.Errorf("redis dependency = %q, want a down reason", rs.Dependencies["redis"])
	}
	if rs.Dependencies["postgres"] != "ok" {
		t.Errorf("postgres dependency = %q, want ok", rs.Dependencies["postgres"])
	}
}

// TestReadyz_PostgresDown_Returns503 covers AC-028: postgres down → 503 with
// postgres reported as not "ok" in the body.
func TestReadyz_PostgresDown_Returns503(t *testing.T) {
	h := newTestServer(t, proxy.NewRouter(), okChecker("redis"), downChecker("postgres"))
	rec := getReadyz(t, h)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	var rs api.ReadinessStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &rs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rs.Dependencies["postgres"] == "ok" || rs.Dependencies["postgres"] == "" {
		t.Errorf("postgres dependency = %q, want a down reason", rs.Dependencies["postgres"])
	}
}
