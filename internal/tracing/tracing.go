// Package tracing implements COMP-014, the OpenTelemetry tracing setup (FR-011).
// It initialises the OTel SDK with an OTLP/HTTP span exporter whose endpoint is
// read from the environment, and exposes the resulting trace.TracerProvider plus
// a Shutdown hook. Per ADR-0008 nothing is global: the caller injects the
// provider into the HTTP middleware; this package never installs
// otel.SetTracerProvider unless explicitly asked.
//
// Collector-down tolerance (AC-050): the SDK uses a BATCH span processor, which
// exports asynchronously on a background goroutine. Export failures (collector
// unreachable, network errors) are therefore decoupled from the request path —
// a span is enqueued in-memory and the request returns immediately, regardless
// of whether the collector ever acknowledges the batch. Exporter/initialisation
// errors are logged and NEVER propagated into request handling. If the endpoint
// is unset, a no-op provider is returned so the gateway runs un-traced rather
// than failing to boot.
package tracing

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// serviceName is the resource service.name attached to every span.
const serviceName = "sluice-gateway"

// Provider bundles the configured TracerProvider with a Shutdown hook. The
// Tracer is what the HTTP middleware uses to start spans; Shutdown flushes the
// batch processor on graceful shutdown. Both are always non-nil — when tracing
// is disabled they are backed by a no-op provider so call sites need no nil
// checks.
type Provider struct {
	tp       trace.TracerProvider
	shutdown func(context.Context) error
}

// Tracer returns a named tracer from the provider. The middleware uses it to
// create the per-request root span and the nested provider-call span.
func (p *Provider) Tracer() trace.Tracer {
	return p.tp.Tracer("github.com/adverax/sluice/internal/tracing")
}

// Shutdown flushes and stops the exporter. It is safe to call on a disabled
// provider (no-op). Errors are returned so the caller can log them, but they
// never affect request processing (AC-050).
func (p *Provider) Shutdown(ctx context.Context) error {
	if p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

// Config controls tracing initialisation.
type Config struct {
	// Endpoint is the OTLP/HTTP collector endpoint (host:port, e.g.
	// "otel-collector:4318"). Empty disables tracing (no-op provider).
	Endpoint string
	// Insecure sends over plain HTTP (no TLS). Typical for in-cluster collectors.
	Insecure bool
	// ExportTimeout bounds a single batch export attempt so a wedged collector
	// cannot pin the background exporter goroutine forever. Defaults to 5s.
	ExportTimeout time.Duration
}

// New initialises the tracer provider from cfg. On any error (bad resource,
// exporter construction failure) it logs at WARN and returns a DISABLED no-op
// provider together with a nil error: tracing must never block the gateway from
// starting or serving (AC-050). When cfg.Endpoint is empty it returns the no-op
// provider directly.
func New(ctx context.Context, cfg Config, logger *slog.Logger) *Provider {
	if cfg.Endpoint == "" {
		logger.LogAttrs(ctx, slog.LevelInfo, "tracing disabled (no OTLP endpoint configured)")
		return disabled()
	}

	exportTimeout := cfg.ExportTimeout
	if exportTimeout <= 0 {
		exportTimeout = 5 * time.Second
	}

	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(cfg.Endpoint),
		otlptracehttp.WithTimeout(exportTimeout),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}

	// otlptracehttp.New does NOT dial the collector here; it only builds the
	// client. The first actual export happens lazily on the batch processor's
	// background goroutine, so an unreachable collector cannot fail startup.
	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "tracing exporter init failed; continuing without tracing",
			slog.String("error", err.Error()))
		return disabled()
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "tracing resource init failed; continuing without tracing",
			slog.String("error", err.Error()))
		return disabled()
	}

	tp := sdktrace.NewTracerProvider(
		// Batch (async) processor: export failures happen off the request path so
		// a down collector can never interrupt request processing (AC-050).
		sdktrace.WithBatcher(exporter, sdktrace.WithExportTimeout(exportTimeout)),
		sdktrace.WithResource(res),
	)

	return &Provider{tp: tp, shutdown: tp.Shutdown}
}

// NewWithProvider wraps an already-built TracerProvider (e.g. the OTel
// tracetest SDK provider used in tests with a SpanRecorder). The shutdown hook
// is used as-is. This keeps the middleware fully injectable without the test
// touching real OTLP wiring.
func NewWithProvider(tp trace.TracerProvider, shutdown func(context.Context) error) *Provider {
	return &Provider{tp: tp, shutdown: shutdown}
}

// disabled returns a no-op provider so call sites never need nil checks.
func disabled() *Provider {
	return &Provider{tp: noop.NewTracerProvider()}
}
