package tracing_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/adverax/sluice/internal/health"
	"github.com/adverax/sluice/internal/middleware"
	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/proxy"
	"github.com/adverax/sluice/internal/server"
	"github.com/adverax/sluice/internal/tracing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func mockRouter() *proxy.Router {
	r := proxy.NewRouter()
	r.Register("mock", provider.New(provider.WithResponse(provider.Response{
		Model:        "mock",
		Content:      "ok",
		FinishReason: "stop",
	})))
	return r
}

func chatRequest() *http.Request {
	body := `{"model":"mock","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestTracing_EndToEndSpanCreated covers AC-030: a request produces a trace with
// at least two spans — the incoming HTTP root span and the nested upstream
// provider-call span — using an in-memory SpanRecorder (deterministic, no
// network, SyncSpanProcessor for immediate capture).
func TestTracing_EndToEndSpanCreated(t *testing.T) {
	t.Parallel()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prov := tracing.NewWithProvider(tp, tp.Shutdown)

	defaultInfer := func(ctx context.Context, p provider.Provider, req provider.Request) (provider.Response, error) {
		return p.Infer(ctx, req)
	}
	srv := server.New(mockRouter(), health.New(discardLogger(), 0), discardLogger(),
		server.WithInferFunc(prov.InstrumentInferFunc(defaultInfer)),
	)
	handler := middleware.Tracing(prov.Tracer())(srv.Handler(http.NewServeMux()))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, chatRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	spans := sr.Ended()
	if len(spans) < 2 {
		t.Fatalf("got %d spans, want >= 2 (HTTP root + provider call)", len(spans))
	}

	var haveHTTP, haveProvider bool
	for _, s := range spans {
		switch {
		case strings.HasPrefix(s.Name(), "HTTP "):
			haveHTTP = true
		case s.Name() == "provider.infer":
			haveProvider = true
		}
	}
	if !haveHTTP {
		t.Errorf("missing incoming HTTP root span")
	}
	if !haveProvider {
		t.Errorf("missing nested provider.infer span")
	}

	// The provider span must be a child of the HTTP root span (same trace).
	traceIDs := make(map[string]struct{})
	for _, s := range spans {
		traceIDs[s.SpanContext().TraceID().String()] = struct{}{}
	}
	if len(traceIDs) != 1 {
		t.Errorf("spans span %d traces, want 1 (provider span must be a child of the HTTP span)", len(traceIDs))
	}
}

// errExporter is a span exporter that always fails, simulating an unreachable /
// down collector (AC-050).
type errExporter struct{}

func (errExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return errors.New("collector unreachable")
}

func (errExporter) Shutdown(context.Context) error { return nil }

// TestTracing_CollectorDown_DoesNotBreakRequest covers AC-050: with an exporter
// that always errors, wired through a BATCH (async) processor exactly as
// production, a request still returns 200 and does not hang. The batch processor
// exports off the request path, so the export error never reaches the handler.
func TestTracing_CollectorDown_DoesNotBreakRequest(t *testing.T) {
	t.Parallel()

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(errExporter{}, sdktrace.WithExportTimeout(50*time.Millisecond)),
	)
	prov := tracing.NewWithProvider(tp, tp.Shutdown)

	defaultInfer := func(ctx context.Context, p provider.Provider, req provider.Request) (provider.Response, error) {
		return p.Infer(ctx, req)
	}
	srv := server.New(mockRouter(), health.New(discardLogger(), 0), discardLogger(),
		server.WithInferFunc(prov.InstrumentInferFunc(defaultInfer)),
	)
	handler := middleware.Tracing(prov.Tracer())(srv.Handler(http.NewServeMux()))

	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, chatRequest())
		done <- rec.Code
	}()

	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("got status %d, want 200 despite collector being down", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request hung with collector down (AC-050 violated)")
	}

	// Shutdown must also tolerate the failing exporter without panicking; the
	// error is acceptable (it never affects request processing).
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = prov.Shutdown(ctx)
}

// TestTracing_DisabledWhenNoEndpoint verifies New returns a usable no-op
// provider (never nil) when no OTLP endpoint is configured, so the gateway runs
// un-traced rather than failing to boot.
func TestTracing_DisabledWhenNoEndpoint(t *testing.T) {
	t.Parallel()

	prov := tracing.New(context.Background(), tracing.Config{}, discardLogger())
	if prov == nil {
		t.Fatal("New returned nil provider")
	}
	// Tracer and Shutdown must be safe to call on the disabled provider.
	_, span := prov.Tracer().Start(context.Background(), "noop")
	span.End()
	if err := prov.Shutdown(context.Background()); err != nil {
		t.Errorf("disabled provider Shutdown returned error: %v", err)
	}
}
