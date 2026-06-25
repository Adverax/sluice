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
