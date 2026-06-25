package pool

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/server"
)

// gatedInfer returns an InferFunc whose every call blocks on entered/release so
// tests can hold a deterministic number of calls in flight WITHOUT sleeping.
// Each call signals entry on `entered` (capacity must cover the held calls),
// then blocks reading from `release` until the test frees it, then returns ok.
func gatedInfer(entered chan<- struct{}, release <-chan struct{}) server.InferFunc {
	return func(ctx context.Context, _ provider.Provider, _ provider.Request) (provider.Response, error) {
		entered <- struct{}{}
		<-release
		return provider.Response{Model: "mock"}, nil
	}
}

// TestWorkerPool_Saturated_Returns503WithRetryAfter covers AC-038: with N=10
// slots and 10 calls held in flight, the 11th acquire fails fast with a
// sentinel that maps to 503 + Retry-After, starts NO extra goroutine, and
// returns immediately.
func TestWorkerPool_Saturated_Returns503WithRetryAfter(t *testing.T) {
	t.Parallel()

	const n = 10
	const retryAfter = 60 * time.Second

	enteredCh := make(chan struct{}, n)
	releaseCh := make(chan struct{})
	p := New(n, retryAfter)
	guard := p.Guard(gatedInfer(enteredCh, releaseCh))

	// Saturate: launch n calls and wait until all are parked in-flight.
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = guard(context.Background(), nil, provider.Request{Model: "mock"})
		}()
	}
	for i := 0; i < n; i++ {
		<-enteredCh
	}
	defer func() {
		close(releaseCh)
		wg.Wait()
	}()

	if got := p.InFlight(); got != n {
		t.Fatalf("InFlight = %d, want %d (pool not fully saturated)", got, n)
	}

	// Capture goroutine count, then issue the 11th call on this goroutine.
	runtime.GC()
	before := runtime.NumGoroutine()

	start := time.Now()
	resp, err := guard(context.Background(), nil, provider.Request{Model: "mock"})
	elapsed := time.Since(start)

	// Returns immediately (fast-fail, no blocking on the held calls).
	if elapsed > 100*time.Millisecond {
		t.Errorf("saturated acquire took %s, expected immediate reject", elapsed)
	}
	if resp != (provider.Response{}) {
		t.Errorf("saturated call returned non-zero response: %+v", resp)
	}

	// Typed sentinel, classified without string-matching.
	if !errors.Is(err, ErrPoolSaturated) {
		t.Errorf("err = %v, want errors.Is(ErrPoolSaturated)", err)
	}
	// Maps to the server's 503 sentinel.
	if !errors.Is(err, server.ErrServiceUnavailable) {
		t.Errorf("err = %v, want errors.Is(server.ErrServiceUnavailable) for 503 mapping", err)
	}
	// Carries the Retry-After hint via the retryAfterer interface.
	var ra interface{ RetryAfter() time.Duration }
	if !errors.As(err, &ra) {
		t.Fatalf("err = %v, want it to expose RetryAfter()", err)
	}
	if got := ra.RetryAfter(); got != retryAfter {
		t.Errorf("RetryAfter = %s, want %s", got, retryAfter)
	}

	// No extra goroutine started for the rejected call (allow small tolerance
	// for runtime/background scheduling noise).
	runtime.GC()
	after := runtime.NumGoroutine()
	if after > before+2 {
		t.Errorf("goroutine count grew on reject: before=%d after=%d", before, after)
	}
}

// TestWorkerPool_RecoveryAfterSaturation covers AC-039: saturate the pool,
// release 5 slots, and assert the next 5 acquires succeed without a 503.
func TestWorkerPool_RecoveryAfterSaturation(t *testing.T) {
	t.Parallel()

	const n = 10
	const free = 5

	// First fill: n calls parked in-flight on a gate we can release selectively.
	enteredCh := make(chan struct{}, n)
	releaseCh := make(chan struct{}, n) // buffered: send `free` tokens to free that many
	released := make(chan struct{}, n)

	inner := func(ctx context.Context, _ provider.Provider, _ provider.Request) (provider.Response, error) {
		enteredCh <- struct{}{}
		<-releaseCh
		released <- struct{}{}
		return provider.Response{Model: "mock"}, nil
	}

	p := New(n, time.Second)
	guard := p.Guard(inner)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = guard(context.Background(), nil, provider.Request{Model: "mock"})
		}()
	}
	for i := 0; i < n; i++ {
		<-enteredCh
	}

	// Pool is full: an acquire now must be rejected.
	if _, err := guard(context.Background(), nil, provider.Request{Model: "mock"}); !errors.Is(err, ErrPoolSaturated) {
		t.Fatalf("expected saturation before recovery, got err=%v", err)
	}

	// Free exactly `free` slots and wait for those calls to fully return so the
	// slots are released back to the semaphore.
	for i := 0; i < free; i++ {
		releaseCh <- struct{}{}
	}
	for i := 0; i < free; i++ {
		<-released
	}
	// Drain freed slots back to empty deterministically: poll InFlight.
	waitFor(t, func() bool { return p.InFlight() == n-free }, time.Second)

	// The next `free` acquires must succeed (no 503). Park them in-flight too.
	var wg2 sync.WaitGroup
	wg2.Add(free)
	for i := 0; i < free; i++ {
		go func() {
			defer wg2.Done()
			if _, err := guard(context.Background(), nil, provider.Request{Model: "mock"}); err != nil {
				t.Errorf("post-recovery acquire failed: %v", err)
			}
		}()
	}
	for i := 0; i < free; i++ {
		<-enteredCh
	}
	if got := p.InFlight(); got != n {
		t.Errorf("InFlight after recovery = %d, want %d", got, n)
	}

	// Release everything and let all goroutines finish.
	for i := 0; i < n; i++ {
		releaseCh <- struct{}{}
	}
	wg.Wait()
	wg2.Wait()
}

// TestWorkerPool_NeverExceedsLimit drives many more concurrent calls than the
// limit and asserts the maximum observed concurrency equals the limit exactly,
// while every excess call gets the saturated sentinel (NFR-006 / AC-047).
func TestWorkerPool_NeverExceedsLimit(t *testing.T) {
	t.Parallel()

	const limit = 8
	const callers = 64

	var current int64 // currently-running wrapped calls
	var maxObserved int64
	var accepted int64
	var rejected int64

	gate := make(chan struct{}) // hold every accepted call until we say go

	inner := func(ctx context.Context, _ provider.Provider, _ provider.Request) (provider.Response, error) {
		cur := atomic.AddInt64(&current, 1)
		for {
			m := atomic.LoadInt64(&maxObserved)
			if cur <= m || atomic.CompareAndSwapInt64(&maxObserved, m, cur) {
				break
			}
		}
		<-gate
		atomic.AddInt64(&current, -1)
		return provider.Response{}, nil
	}

	p := New(limit, time.Second)
	guard := p.Guard(inner)

	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			_, err := guard(context.Background(), nil, provider.Request{Model: "mock"})
			if err == nil {
				atomic.AddInt64(&accepted, 1)
			} else if errors.Is(err, ErrPoolSaturated) {
				atomic.AddInt64(&rejected, 1)
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}

	// Wait until the pool is saturated (limit calls parked at the gate), then
	// release them so accepted callers can finish.
	waitFor(t, func() bool { return atomic.LoadInt64(&current) == int64(limit) }, time.Second)
	close(gate)
	wg.Wait()

	if got := atomic.LoadInt64(&maxObserved); got != int64(limit) {
		t.Errorf("max observed concurrency = %d, want exactly %d", got, limit)
	}
	if got := atomic.LoadInt64(&accepted) + atomic.LoadInt64(&rejected); got != int64(callers) {
		t.Errorf("accepted+rejected = %d, want %d", got, callers)
	}
	if atomic.LoadInt64(&rejected) == 0 {
		t.Errorf("expected some rejections under %d callers vs limit %d", callers, limit)
	}
}

// TestPool_New_PanicsOnNonPositiveSize asserts the fail-loud guard on size.
func TestPool_New_PanicsOnNonPositiveSize(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, -1} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("New(%d) did not panic", size)
				}
			}()
			_ = New(size, time.Second)
		}()
	}
}

// TestPool_Guard_ReleasesOnError confirms a slot is freed when the wrapped call
// returns an error, so an errored call does not permanently consume capacity.
func TestPool_Guard_ReleasesOnError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	p := New(1, time.Second)
	guard := p.Guard(func(context.Context, provider.Provider, provider.Request) (provider.Response, error) {
		return provider.Response{}, wantErr
	})

	if _, err := guard(context.Background(), nil, provider.Request{}); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if got := p.InFlight(); got != 0 {
		t.Fatalf("InFlight = %d after errored call, want 0 (slot leaked)", got)
	}
	// Capacity recovered: a subsequent call must not be rejected.
	if _, err := guard(context.Background(), nil, provider.Request{}); !errors.Is(err, wantErr) {
		t.Fatalf("second call err = %v, want %v (slot not recovered)", err, wantErr)
	}
}

// waitFor polls cond until it is true or the deadline elapses. Used to await a
// concurrent state transition deterministically without a fixed sleep.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("condition not met within %s", timeout)
}

// TestGuardStream_NilSourceChannel_SlotReleasedAndChannelClosed verifies the
// defensive nil-channel guard (FIX 2): if the inner StreamFunc returns (nil,
// nil) — a contract violation — GuardStream must NOT leak the slot and must
// return a closed channel (zero-length stream) rather than spawning a goroutine
// that blocks forever on a nil channel read.
func TestGuardStream_NilSourceChannel_SlotReleasedAndChannelClosed(t *testing.T) {
	t.Parallel()

	const limit = 1
	p := New(limit, time.Second)

	// nilInner simulates a misbehaving inner layer that returns (nil, nil).
	nilInner := func(context.Context, provider.Provider, provider.Request) (<-chan provider.Chunk, error) {
		return nil, nil
	}
	guarded := p.GuardStream(nilInner)

	// The call must succeed (no error) and return a non-nil channel.
	out, err := guarded(context.Background(), nil, provider.Request{Model: "mock"})
	if err != nil {
		t.Fatalf("nil-source guard returned unexpected error: %v", err)
	}
	if out == nil {
		t.Fatal("nil-source guard returned nil channel; want a closed channel")
	}

	// The channel must be closed immediately (zero-length stream).
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("channel sent a value; want it closed immediately")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel not closed after 100ms; possible goroutine block on nil channel")
	}

	// Slot must be released (InFlight back to 0) so subsequent streams are not
	// refused (proves no leak).
	waitFor(t, func() bool { return p.InFlight() == 0 }, time.Second)

	// A subsequent stream must acquire the slot (would 503 if the slot leaked).
	out2, err := guarded(context.Background(), nil, provider.Request{Model: "mock"})
	if err != nil {
		t.Fatalf("post-nil stream should acquire slot, got %v (slot leaked on nil source)", err)
	}
	for range out2 { // drain to close.
	}
	waitFor(t, func() bool { return p.InFlight() == 0 }, time.Second)
}
