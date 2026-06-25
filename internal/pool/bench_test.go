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
)

// These benchmarks are the IN-PACKAGE proxies for the NFR LOAD acceptance
// criteria (AC-043/AC-044/AC-047). They prove the SAME invariants — no crash
// under overload, no goroutine leak, bounded upstream goroutines — at a small,
// fast scale (a few thousand iterations on a fast in-memory "provider"), so the
// unit suite stays quick and deterministic under `go test -race ./...`.
//
// The FULL-scale load run (3× nominal RPS / 500 RPS for 2 minutes, with a pprof
// goroutine baseline captured before/after) is executed by CARD-011's k6/load
// harness against the real gateway. These functions assert the property; they
// do NOT reproduce the 2-minute soak. Keep them O(seconds).

// fastInfer is an instantaneous, always-success wrapped call: it does the
// minimum so the benchmark stresses the pool's acquire/release path, not a
// simulated upstream latency.
func fastInfer(_ context.Context, _ provider.Provider, _ provider.Request) (provider.Response, error) {
	return provider.Response{Model: "mock"}, nil
}

// BenchGateway_Overload3x_NocrashGracefulDegradation is the in-package proxy for
// AC-043 (NFR-002): drive ~3× the pool limit in concurrent callers and assert
// the process does not crash/panic and every request resolves to either a
// success or the saturated 503 sentinel (never blocks, never errors otherwise).
// After the burst the pool returns to a fully-free state (post-load recovery).
func BenchGateway_Overload3x_NocrashGracefulDegradation(b *testing.B) {
	const limit = 50
	p := New(limit, time.Second)
	guard := p.Guard(fastInfer)

	for i := 0; i < b.N; i++ {
		const callers = limit * 3 // 3× overload
		var accepted, rejected int64
		var wg sync.WaitGroup
		wg.Add(callers)
		for c := 0; c < callers; c++ {
			go func() {
				defer wg.Done()
				_, err := guard(context.Background(), nil, provider.Request{Model: "mock"})
				if err == nil {
					atomic.AddInt64(&accepted, 1)
				} else if errors.Is(err, ErrPoolSaturated) {
					atomic.AddInt64(&rejected, 1)
				} else {
					b.Errorf("unexpected error under overload: %v", err)
				}
			}()
		}
		wg.Wait()

		if accepted+rejected != int64(callers) {
			b.Fatalf("accepted+rejected=%d, want %d (a request neither succeeded nor was shed)", accepted+rejected, callers)
		}
		// Post-load: every slot freed, gateway accepts again (graceful recovery).
		if got := p.InFlight(); got != 0 {
			b.Fatalf("InFlight=%d after burst, want 0 (no recovery)", got)
		}
	}
}

// BenchGateway_GoroutineLeakCheck is the in-package proxy for AC-044 (NFR-003):
// record a goroutine baseline, run repeated overload bursts, let load subside,
// and assert the goroutine count returns to the baseline (±tolerance). The
// guard must never leak a goroutine — rejected calls spawn none and accepted
// calls return their goroutine on completion.
func BenchGateway_GoroutineLeakCheck(b *testing.B) {
	const limit = 50
	p := New(limit, time.Second)
	guard := p.Guard(fastInfer)

	runtime.GC()
	baseline := runtime.NumGoroutine()

	for i := 0; i < b.N; i++ {
		const callers = limit * 3
		var wg sync.WaitGroup
		wg.Add(callers)
		for c := 0; c < callers; c++ {
			go func() {
				defer wg.Done()
				_, _ = guard(context.Background(), nil, provider.Request{Model: "mock"})
			}()
		}
		wg.Wait()
	}

	// Let any straggler scheduler goroutines settle, then compare to baseline.
	runtime.GC()
	runtime.Gosched()
	after := runtime.NumGoroutine()
	if after > baseline+5 { // tolerance ±5 background goroutines (AC-044)
		b.Fatalf("goroutine leak: baseline=%d after=%d (tolerance 5)", baseline, after)
	}
}

// BenchGateway_GoroutineCountBounded is the in-package proxy for AC-047
// (NFR-006): under sustained overload the number of calls concurrently inside
// the wrapped (upstream) func never exceeds the pool limit. A sampler observes
// the live in-flight count; the wrapped func also records the max it ever sees.
func BenchGateway_GoroutineCountBounded(b *testing.B) {
	const limit = 50
	p := New(limit, time.Second)

	var current int64
	var maxObserved int64
	inner := func(context.Context, provider.Provider, provider.Request) (provider.Response, error) {
		cur := atomic.AddInt64(&current, 1)
		for {
			m := atomic.LoadInt64(&maxObserved)
			if cur <= m || atomic.CompareAndSwapInt64(&maxObserved, m, cur) {
				break
			}
		}
		// Tiny spin so calls actually overlap and contend for slots.
		runtime.Gosched()
		atomic.AddInt64(&current, -1)
		return provider.Response{}, nil
	}
	guard := p.Guard(inner)

	for i := 0; i < b.N; i++ {
		const callers = limit * 3
		var wg sync.WaitGroup
		wg.Add(callers)
		for c := 0; c < callers; c++ {
			go func() {
				defer wg.Done()
				_, _ = guard(context.Background(), nil, provider.Request{Model: "mock"})
			}()
		}
		wg.Wait()
	}

	if got := atomic.LoadInt64(&maxObserved); got > int64(limit) {
		b.Fatalf("max concurrent upstream calls = %d, exceeds limit %d (NFR-006 violated)", got, limit)
	}
	// In-flight bound must also hold as a hard invariant via the semaphore cap.
	if p.InFlight() != 0 {
		b.Fatalf("InFlight=%d after run, want 0", p.InFlight())
	}
}
