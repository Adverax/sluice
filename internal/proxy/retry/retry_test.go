package retry_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker"

	"github.com/adverax/sluice/internal/config"
	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/proxy/retry"
)

// testCfg returns a retry config with deterministic, tiny delays. Tests inject a
// no-op sleep so even these never block, but the values keep backoff() valid.
func testCfg(maxAttempts int) config.Retry {
	return config.Retry{
		MaxAttempts: maxAttempts,
		BaseDelay:   time.Millisecond,
		MaxDelay:    10 * time.Millisecond,
		Jitter:      0.5,
	}
}

// noSleep is an injected sleep that never blocks but still honours an
// already-cancelled ctx (so deadline-during-backoff paths stay testable without
// real time passing).
func noSleep(ctx context.Context, _ time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// scriptedProvider returns the configured error for the first n calls, then a
// success. It records the number of calls so tests can assert attempt counts.
type scriptedProvider struct {
	failFirst int
	failErr   error
	resp      provider.Response
	calls     atomic.Int32
}

func (p *scriptedProvider) call(_ context.Context) (provider.Response, error) {
	n := p.calls.Add(1)
	if int(n) <= p.failFirst {
		return provider.Response{}, p.failErr
	}
	return p.resp, nil
}

func transient() error { return provider.NewStatusError(503, "service unavailable") }
func clientErr() error { return provider.NewStatusError(400, "bad request") }

// AC-018: provider returns 503 on the first 2 requests, then 200 → engine
// performs 2 retries and returns the successful response.
func TestRetry_TransientError_SucceedsOnThirdAttempt(t *testing.T) {
	p := &scriptedProvider{
		failFirst: 2,
		failErr:   transient(),
		resp:      provider.Response{Model: "mock", Content: "ok"},
	}
	eng := retry.New(testCfg(3), retry.WithSleep(noSleep))

	resp, err := eng.Do(context.Background(), p.call)
	if err != nil {
		t.Fatalf("expected success after retries, got error: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("unexpected response content %q", resp.Content)
	}
	if got := p.calls.Load(); got != 3 {
		t.Errorf("expected 3 attempts (initial + 2 retries), got %d", got)
	}
}

// AC-019: provider consistently returns 503, max attempts = 3 → exhausted,
// wrapped in ErrExhausted (server maps to 502).
func TestRetry_ExhaustedAttempts_Returns502(t *testing.T) {
	p := &scriptedProvider{failFirst: 100, failErr: transient()}
	eng := retry.New(testCfg(3), retry.WithSleep(noSleep))

	_, err := eng.Do(context.Background(), p.call)
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if !errors.Is(err, retry.ErrExhausted) {
		t.Errorf("expected ErrExhausted, got %v", err)
	}
	// The underlying transient cause must remain inspectable.
	var se *provider.StatusError
	if !errors.As(err, &se) || se.Code != 503 {
		t.Errorf("expected wrapped 503 StatusError, got %v", err)
	}
	if got := p.calls.Load(); got != 3 {
		t.Errorf("expected exactly 3 attempts, got %d", got)
	}
}

// AC-020: the context deadline is already exceeded → no attempt is started and
// a cancellation error is returned promptly.
func TestRetry_ContextDeadlineExpired_NoRetry(t *testing.T) {
	p := &scriptedProvider{failFirst: 100, failErr: transient()}
	eng := retry.New(testCfg(3), retry.WithSleep(noSleep))

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := eng.Do(ctx, p.call)
	if err == nil {
		t.Fatal("expected a cancellation error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
	if got := p.calls.Load(); got != 0 {
		t.Errorf("expected 0 attempts against an expired deadline, got %d", got)
	}
}

// AC-020 (variant): the deadline expires DURING a retry backoff → the retry
// does not start; cancellation information is returned.
func TestRetry_DeadlineDuringBackoff_NoRetry(t *testing.T) {
	p := &scriptedProvider{failFirst: 100, failErr: transient()}
	// Sleep simulates the deadline elapsing mid-backoff by cancelling.
	ctx, cancel := context.WithCancel(context.Background())
	sleep := func(c context.Context, _ time.Duration) error {
		cancel() // deadline elapses during the wait
		return context.DeadlineExceeded
	}
	eng := retry.New(testCfg(3), retry.WithSleep(sleep))

	_, err := eng.Do(ctx, p.call)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded during backoff, got %v", err)
	}
	if got := p.calls.Load(); got != 1 {
		t.Errorf("expected exactly 1 attempt before the aborted backoff, got %d", got)
	}
}

// AC-021: provider returns 400 (client error) → no retry, error returned
// immediately and unwrapped so the server can passthrough.
func TestRetry_ClientError_NoRetry(t *testing.T) {
	p := &scriptedProvider{failFirst: 100, failErr: clientErr()}
	eng := retry.New(testCfg(3), retry.WithSleep(noSleep))

	_, err := eng.Do(context.Background(), p.call)
	if err == nil {
		t.Fatal("expected the client error to be returned")
	}
	if errors.Is(err, retry.ErrExhausted) {
		t.Error("4xx client error must NOT be wrapped as exhausted")
	}
	var se *provider.StatusError
	if !errors.As(err, &se) || se.Code != 400 {
		t.Errorf("expected the original 400 StatusError, got %v", err)
	}
	if got := p.calls.Load(); got != 1 {
		t.Errorf("expected exactly 1 attempt for a 4xx, got %d", got)
	}
}

// ErrOpenState must be treated as non-retryable via the injected predicate
// (ADR-0006) and propagated immediately and unwrapped.
func TestRetry_OpenState_NotRetried(t *testing.T) {
	p := &scriptedProvider{failFirst: 100, failErr: gobreaker.ErrOpenState}
	eng := retry.New(testCfg(3),
		retry.WithSleep(noSleep),
		retry.WithNonRetryable(func(err error) bool { return errors.Is(err, gobreaker.ErrOpenState) }),
	)

	_, err := eng.Do(context.Background(), p.call)
	if !errors.Is(err, gobreaker.ErrOpenState) {
		t.Fatalf("expected ErrOpenState propagated, got %v", err)
	}
	if got := p.calls.Load(); got != 1 {
		t.Errorf("expected exactly 1 attempt against an open breaker, got %d", got)
	}
}

// A bare (untyped) provider error is treated as a transient network failure and
// retried until success.
func TestRetry_UntypedError_TreatedAsTransient(t *testing.T) {
	p := &scriptedProvider{
		failFirst: 1,
		failErr:   errors.New("connection reset"),
		resp:      provider.Response{Content: "ok"},
	}
	eng := retry.New(testCfg(3), retry.WithSleep(noSleep))

	resp, err := eng.Do(context.Background(), p.call)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("unexpected content %q", resp.Content)
	}
}
