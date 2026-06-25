package breaker_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker"

	"github.com/adverax/sluice/internal/breaker"
	"github.com/adverax/sluice/internal/config"
	"github.com/adverax/sluice/internal/provider"
)

// adrCfg returns the ADR-0002 (volume_based_50pct) breaker tuning. Timeout is
// left short via WithSettings in the half-open test so we don't wait 60s.
func adrCfg() config.Breaker {
	return config.Breaker{
		Interval:     10 * time.Second,
		Timeout:      60 * time.Second,
		MaxRequests:  5,
		MinRequests:  10,
		FailureRatio: 0.5,
		RetryAfter:   60 * time.Second,
	}
}

const key = "mock"

// failCall always fails with a transient 503; recording calls.
func failCall(counter *atomic.Int32) breaker.Call {
	return func(_ context.Context) (provider.Response, error) {
		counter.Add(1)
		return provider.Response{}, provider.NewStatusError(503, "down")
	}
}

func okCall(counter *atomic.Int32) breaker.Call {
	return func(_ context.Context) (provider.Response, error) {
		counter.Add(1)
		return provider.Response{Content: "ok"}, nil
	}
}

// AC-022: breaker is open → Execute returns ErrOpenState WITHOUT invoking the
// call (no provider contact) and does so fast (< 1ms).
func TestCircuitBreaker_OpenState_FastFail(t *testing.T) {
	reg := breaker.NewRegistry(adrCfg())

	// Drive the breaker open: 10 requests, all failures (ratio 1.0 ≥ 0.5).
	var calls atomic.Int32
	for i := 0; i < 10; i++ {
		_, _ = reg.Execute(context.Background(), key, failCall(&calls))
	}
	if reg.State(key) != gobreaker.StateOpen {
		t.Fatalf("expected breaker open after threshold, got %v", reg.State(key))
	}

	// Now a request must fast-fail without invoking the call.
	var probe atomic.Int32
	start := time.Now()
	_, err := reg.Execute(context.Background(), key, okCall(&probe))
	elapsed := time.Since(start)

	if !errors.Is(err, breaker.ErrOpenState) {
		t.Fatalf("expected ErrOpenState, got %v", err)
	}
	if probe.Load() != 0 {
		t.Errorf("provider must NOT be contacted in open state, got %d calls", probe.Load())
	}
	if elapsed >= time.Millisecond {
		t.Errorf("fast-fail must be < 1ms, took %s", elapsed)
	}
}

// AC-023: errors exceed the threshold → breaker transitions to open; subsequent
// requests receive ErrOpenState (EVT-004). Also asserts the OnStateChange hook
// fires with the open transition.
func TestCircuitBreaker_ThresholdExceeded_Opens(t *testing.T) {
	var opened atomic.Bool
	reg := breaker.NewRegistry(adrCfg(),
		breaker.WithOnStateChange(func(_ string, _, to gobreaker.State) {
			if to == gobreaker.StateOpen {
				opened.Store(true)
			}
		}),
	)

	var calls atomic.Int32
	// Below the minimum volume (10) the breaker must NOT trip even at 100% errors.
	for i := 0; i < 9; i++ {
		_, _ = reg.Execute(context.Background(), key, failCall(&calls))
	}
	if reg.State(key) != gobreaker.StateClosed {
		t.Fatalf("breaker must stay closed below min volume, got %v", reg.State(key))
	}

	// The 10th failure reaches the volume threshold with ratio ≥ 0.5 → open.
	_, _ = reg.Execute(context.Background(), key, failCall(&calls))
	if reg.State(key) != gobreaker.StateOpen {
		t.Fatalf("expected open after threshold exceeded, got %v", reg.State(key))
	}
	if !opened.Load() {
		t.Error("OnStateChange hook did not observe the open transition (EVT-004)")
	}

	// Subsequent request receives ErrOpenState.
	_, err := reg.Execute(context.Background(), key, okCall(new(atomic.Int32)))
	if !errors.Is(err, breaker.ErrOpenState) {
		t.Errorf("subsequent request must receive ErrOpenState, got %v", err)
	}
}

// AC-024: breaker open, recovery timeout elapsed → a half-open probe succeeds →
// breaker closes and the response is returned. Timeout is injected short so the
// test does not wait 60s.
func TestCircuitBreaker_HalfOpen_SuccessClosesCircuit(t *testing.T) {
	cfg := adrCfg()
	reg := breaker.NewRegistry(cfg,
		breaker.WithSettings(func(name string) gobreaker.Settings {
			return gobreaker.Settings{
				Name:        name,
				Interval:    cfg.Interval,
				Timeout:     20 * time.Millisecond, // short open→half-open for tests
				MaxRequests: cfg.MaxRequests,
				ReadyToTrip: func(c gobreaker.Counts) bool {
					return c.Requests >= cfg.MinRequests &&
						float64(c.TotalFailures)/float64(c.Requests) >= cfg.FailureRatio
				},
			}
		}),
	)

	// Trip it open.
	var calls atomic.Int32
	for i := 0; i < 10; i++ {
		_, _ = reg.Execute(context.Background(), key, failCall(&calls))
	}
	if reg.State(key) != gobreaker.StateOpen {
		t.Fatalf("expected open, got %v", reg.State(key))
	}

	// Wait for the (short) recovery timeout to elapse → half-open.
	waitFor(t, 2*time.Second, func() bool { return reg.State(key) == gobreaker.StateHalfOpen })

	// A successful probe in half-open closes the circuit and returns the response.
	var probe atomic.Int32
	resp, err := reg.Execute(context.Background(), key, okCall(&probe))
	if err != nil {
		t.Fatalf("half-open probe should succeed, got %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("expected the probe response returned, got %q", resp.Content)
	}
	if probe.Load() != 1 {
		t.Errorf("expected exactly 1 probe call, got %d", probe.Load())
	}

	// With MaxRequests probes succeeding the breaker closes. One success may be
	// enough or require all probes; drive enough successes and assert closed.
	for i := 0; i < int(cfg.MaxRequests); i++ {
		_, _ = reg.Execute(context.Background(), key, okCall(new(atomic.Int32)))
		if reg.State(key) == gobreaker.StateClosed {
			break
		}
	}
	if reg.State(key) != gobreaker.StateClosed {
		t.Errorf("expected breaker closed after successful probes, got %v", reg.State(key))
	}
}

// TestCircuitBreaker_CtxCancel_DoesNotTripBreaker asserts that a burst of
// client-cancelled calls does NOT open the breaker (NFR: a client hanging up
// is not a provider fault). Even after enough cancellations to exceed
// MinRequests, the breaker must remain closed.
func TestCircuitBreaker_CtxCancel_DoesNotTripBreaker(t *testing.T) {
	cfg := adrCfg() // MinRequests=10, FailureRatio=0.5
	reg := breaker.NewRegistry(cfg)

	// Send 15 calls — more than MinRequests — each with an already-cancelled
	// context. These should NOT count as failures.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately so it's already done

	for i := 0; i < 15; i++ {
		_, err := reg.Execute(cancelledCtx, key, func(ctx context.Context) (provider.Response, error) {
			// The ctx is cancelled; a real provider would return ctx.Err().
			return provider.Response{}, ctx.Err()
		})
		// The ctx error must propagate to the caller.
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("call %d: expected context.Canceled, got %v", i+1, err)
		}
	}

	// The breaker must remain closed — ctx cancellations are not provider failures.
	if state := reg.State(key); state != gobreaker.StateClosed {
		t.Errorf("breaker must stay closed after client cancellations, got %v", state)
	}
}

// TestCircuitBreaker_CtxDeadline_DoesNotTripBreaker mirrors the cancellation
// test for context.DeadlineExceeded.
func TestCircuitBreaker_CtxDeadline_DoesNotTripBreaker(t *testing.T) {
	cfg := adrCfg()
	reg := breaker.NewRegistry(cfg)

	for i := 0; i < 15; i++ {
		// Create a context that has already expired.
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()

		_, err := reg.Execute(ctx, key, func(ctx context.Context) (provider.Response, error) {
			return provider.Response{}, ctx.Err()
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("call %d: expected context.DeadlineExceeded, got %v", i+1, err)
		}
	}

	if state := reg.State(key); state != gobreaker.StateClosed {
		t.Errorf("breaker must stay closed after client deadline expiry, got %v", state)
	}
}

// waitFor polls cond until it returns true or the timeout elapses. Used to
// observe the open→half-open transition deterministically without sleeping the
// full recovery timeout.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
