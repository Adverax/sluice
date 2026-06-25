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

// InstrumentStreamFunc wraps a server.StreamFunc so each streaming provider call
// is timed into provider_request_duration_seconds{provider} — giving streams the
// SAME provider-latency coverage as the unary path (CARD-014, NFR-007 parity).
//
// Unlike the unary variant, the duration here is the FULL STREAM LIFETIME: timing
// starts at initiation and the observation is recorded when the returned channel
// CLOSES (normal end, ctx cancel, or error), not when InferStream returns the
// channel. An initiation error is observed immediately (a zero-length stream).
//
// It wraps the channel with a pass-through forwarding goroutine that records the
// observation on close; the goroutine always terminates when the source closes,
// so it adds no leak (the pool guard upstream guarantees the source is fully
// drained to close on cancellation).
func (m *Metrics) InstrumentStreamFunc(next server.StreamFunc) server.StreamFunc {
	return func(ctx context.Context, p provider.Provider, req provider.Request) (<-chan provider.Chunk, error) {
		start := time.Now()
		src, err := next(ctx, p, req)
		if err != nil {
			// Failed initiation: observe the (near-zero) duration immediately so
			// failed stream attempts still show in the provider latency histogram.
			m.ProviderRequestDuration.WithLabelValues(req.Model).Observe(time.Since(start).Seconds())
			return nil, err
		}

		out := make(chan provider.Chunk)
		go func() {
			defer close(out)
			defer func() {
				m.ProviderRequestDuration.WithLabelValues(req.Model).Observe(time.Since(start).Seconds())
			}()
			for {
				select {
				case chunk, ok := <-src:
					if !ok {
						return // source drained: record duration via defer.
					}
					select {
					case out <- chunk:
					case <-ctx.Done():
						// Consumer abandoned out: keep draining src to its close so no
						// upstream goroutine/slot leaks, then record duration via defer.
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
