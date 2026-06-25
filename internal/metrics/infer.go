package metrics

import (
	"context"
	"time"

	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/server"
)

// InstrumentInferFunc wraps a server.InferFunc so each upstream provider call is
// timed into provider_request_duration_seconds{provider}. The provider label is
// the request model (the per-provider key, matching the breaker keying in
// internal/proxy/resilience). The duration is observed regardless of whether the
// call succeeds or fails, so latency reflects both outcomes.
//
// This decorator is composed OUTSIDE the resilience layer in cmd/gateway:
// metrics.InstrumentInferFunc(pool → retry → breaker → provider). Timing here
// therefore covers the whole resilient call (including retries), which is the
// latency a client actually experiences.
func (m *Metrics) InstrumentInferFunc(next server.InferFunc) server.InferFunc {
	return func(ctx context.Context, p provider.Provider, req provider.Request) (provider.Response, error) {
		start := time.Now()
		resp, err := next(ctx, p, req)
		m.ProviderRequestDuration.WithLabelValues(req.Model).Observe(time.Since(start).Seconds())
		return resp, err
	}
}
