package tracing

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/server"
)

// InstrumentInferFunc wraps a server.InferFunc so the upstream provider call
// runs inside a nested span (named "provider.infer"), a CHILD of the request's
// root span created by the HTTP tracing middleware (AC-030: a trace with at
// least two spans — incoming HTTP + upstream call). The child relationship is
// established through the context propagated from the middleware; this decorator
// must therefore be composed so the ctx it receives carries the root span (i.e.
// the request handler runs under the tracing middleware).
//
// The span records the provider model and ends in a defer regardless of
// outcome; a call error is reflected on the span status but is returned to the
// caller unchanged.
func (p *Provider) InstrumentInferFunc(next server.InferFunc) server.InferFunc {
	tracer := p.Tracer()
	return func(ctx context.Context, prov provider.Provider, req provider.Request) (provider.Response, error) {
		ctx, span := tracer.Start(ctx, "provider.infer")
		span.SetAttributes(attribute.String("provider.model", req.Model))
		defer span.End()

		resp, err := next(ctx, prov, req)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
		}
		return resp, err
	}
}

// InstrumentStreamFunc wraps a server.StreamFunc so the streaming provider call
// runs inside a nested span (named "provider.stream"), giving streams the SAME
// trace coverage as the unary path (CARD-014, NFR-007 parity). The span is a
// CHILD of the request's root span via the propagated ctx, exactly like the
// unary variant.
//
// The span covers the FULL STREAM LIFETIME: it starts at initiation and ends
// when the returned channel CLOSES (normal end, ctx cancel, or error), so trace
// duration reflects the whole stream. An initiation error ends the span
// immediately with an error status. On success the span ends from the forwarding
// goroutine when the source drains; that goroutine always terminates (it drains
// the source to close even if the consumer abandons out), so no goroutine leaks.
func (p *Provider) InstrumentStreamFunc(next server.StreamFunc) server.StreamFunc {
	tracer := p.Tracer()
	return func(ctx context.Context, prov provider.Provider, req provider.Request) (<-chan provider.Chunk, error) {
		ctx, span := tracer.Start(ctx, "provider.stream")
		span.SetAttributes(attribute.String("provider.model", req.Model))

		src, err := next(ctx, prov, req)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			span.End()
			return nil, err
		}

		out := make(chan provider.Chunk)
		go func() {
			defer close(out)
			defer span.End()
			for {
				select {
				case chunk, ok := <-src:
					if !ok {
						return // source drained: span ends via defer.
					}
					select {
					case out <- chunk:
					case <-ctx.Done():
						// Consumer abandoned out: drain src to its close so no upstream
						// goroutine/slot leaks, then end the span via defer.
						for range src {
						}
						return
					}
				case <-ctx.Done():
					for range src {
					}
					return
				}
			}
		}()
		return out, nil
	}
}
