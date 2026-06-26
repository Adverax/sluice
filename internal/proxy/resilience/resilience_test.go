package resilience_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker"

	"github.com/adverax/sluice/internal/breaker"
	"github.com/adverax/sluice/internal/config"
	"github.com/adverax/sluice/internal/health"
	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/proxy"
	"github.com/adverax/sluice/internal/proxy/resilience"
	"github.com/adverax/sluice/internal/proxy/retry"
	"github.com/adverax/sluice/internal/server"
)

// fakeProvider is a scriptable provider for composition tests: it fails the
// first failFirst calls with failErr, then returns resp.
type fakeProvider struct {
	failFirst int
	failErr   error
	resp      provider.Response
	calls     atomic.Int32
}

func (p *fakeProvider) Infer(ctx context.Context, _ provider.Request) (provider.Response, error) {
	if err := ctx.Err(); err != nil {
		return provider.Response{}, err
	}
	n := p.calls.Add(1)
	if int(n) <= p.failFirst {
		return provider.Response{}, p.failErr
	}
	return p.resp, nil
}

func (p *fakeProvider) InferStream(context.Context, provider.Request) (<-chan provider.Chunk, error) {
	return nil, errors.New("not implemented")
}

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func retryCfg() config.Retry {
	return config.Retry{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond, Jitter: 0.5}
}

func breakerCfg() config.Breaker {
	return config.Breaker{
		Interval: 10 * time.Second, Timeout: 60 * time.Second,
		MaxRequests: 5, MinRequests: 10, FailureRatio: 0.5, RetryAfter: 30 * time.Second,
	}
}

func noSleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

// buildServer wires the composed retry+breaker InferFunc into the real
// generated HTTP boundary, registering p under "mock".
func buildServer(t *testing.T, p provider.Provider, opts ...retry.Option) http.Handler {
	t.Helper()
	router := proxy.NewRouter()
	router.Register("mock", p)

	retrier := retry.New(retryCfg(), append([]retry.Option{
		retry.WithSleep(noSleep),
		retry.WithNonRetryable(resilience.IsOpenState),
	}, opts...)...)
	breakers := breaker.NewRegistry(breakerCfg())
	composer := resilience.New(retrier, breakers, breakerCfg().RetryAfter)

	hh := health.New(discardLogger(), 0)
	srv := server.New(router, hh, discardLogger(), server.WithInferFunc(composer.InferFunc()))
	return srv.Handler(http.NewServeMux())
}

func post(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"mock","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// AC-018 (integration): transient 503 twice then success → 200 through the
// composed retry+breaker path.
func TestComposition_TransientThenSuccess_200(t *testing.T) {
	p := &fakeProvider{failFirst: 2, failErr: provider.NewStatusError(503, "down"),
		resp: provider.Response{Model: "mock", Content: "ok"}}
	h := buildServer(t, p)

	rec := post(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if p.calls.Load() != 3 {
		t.Errorf("expected 3 provider calls, got %d", p.calls.Load())
	}
}

// AC-019 (integration): persistent 503 → retries exhausted → 502.
func TestComposition_ExhaustedRetries_502(t *testing.T) {
	p := &fakeProvider{failFirst: 100, failErr: provider.NewStatusError(503, "down")}
	h := buildServer(t, p)

	rec := post(t, h)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// AC-021 (integration): 4xx client error → no retry → 502, single provider call.
func TestComposition_ClientError_NoRetry_502(t *testing.T) {
	p := &fakeProvider{failFirst: 100, failErr: provider.NewStatusError(400, "bad request")}
	h := buildServer(t, p)

	rec := post(t, h)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
	if p.calls.Load() != 1 {
		t.Errorf("4xx must not be retried; expected 1 call, got %d", p.calls.Load())
	}
}

// AC-022 (integration): once the breaker opens, requests fast-fail with 503 +
// Retry-After and the provider is not contacted.
func TestComposition_BreakerOpen_503_RetryAfter(t *testing.T) {
	p := &fakeProvider{failFirst: 1000, failErr: provider.NewStatusError(503, "down")}
	h := buildServer(t, p)

	// Drive enough failing requests to open the breaker. Each HTTP request makes
	// up to MaxAttempts(3) provider calls; the breaker counts each. >=4 requests
	// guarantees >=10 breaker observations at 100% failure.
	for i := 0; i < 6; i++ {
		_ = post(t, h)
	}

	before := p.calls.Load()
	rec := post(t, h)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 from open breaker, got %d (%s)", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("expected Retry-After header on 503 fast-fail")
	}
	if p.calls.Load() != before {
		t.Errorf("open breaker must not contact provider; calls went %d → %d", before, p.calls.Load())
	}

	// OpenAI-shaped error envelope {error:{message,type,code}} (ADR-0012 §7).
	var body struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error.Type != "service_unavailable" {
		t.Errorf("expected service_unavailable error type, got %q", body.Error.Type)
	}
}

// AC-020 (integration): an already-cancelled deadline → 503 with cancellation,
// no provider call.
func TestComposition_DeadlineExpired_503(t *testing.T) {
	p := &fakeProvider{resp: provider.Response{Content: "ok"}}

	retrier := retry.New(retryCfg(), retry.WithSleep(noSleep), retry.WithNonRetryable(resilience.IsOpenState))
	breakers := breaker.NewRegistry(breakerCfg())
	composer := resilience.New(retrier, breakers, breakerCfg().RetryAfter)
	infer := composer.InferFunc()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := infer(ctx, p, provider.Request{Model: "mock"})
	if err == nil {
		t.Fatal("expected an error from an expired deadline")
	}
	if !errors.Is(err, server.ErrServiceUnavailable) {
		t.Errorf("expected ErrServiceUnavailable, got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected wrapped DeadlineExceeded, got %v", err)
	}
	if p.calls.Load() != 0 {
		t.Errorf("expired deadline must not contact provider, got %d calls", p.calls.Load())
	}
}

// Sanity: IsOpenState classifier matches gobreaker.ErrOpenState only.
func TestIsOpenState(t *testing.T) {
	if !resilience.IsOpenState(gobreaker.ErrOpenState) {
		t.Error("IsOpenState should match ErrOpenState")
	}
	if resilience.IsOpenState(errors.New("other")) {
		t.Error("IsOpenState should not match arbitrary errors")
	}
}
