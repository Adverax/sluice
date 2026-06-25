// Package load holds CARD-011's IN-PROCESS load/overhead measurements. Unlike
// the k6 scenario in load/scenario.js (which drives a deployed `make up` stack
// over the network), these tests compose the FULL server handler chain in
// memory via httptest with a 0-latency mock provider, so the wall-clock time of
// each request IS the gateway's own overhead — yielding a REAL, runnable p95
// overhead figure without a deployed stack (AC-042 / NFR-001).
//
// They also carry the full-chain proxies for the LOAD acceptance criteria so the
// composed gateway (not just the pool in isolation) is exercised:
//
//   - BenchGateway_p95OverheadUnder20ms  (AC-042 / NFR-001) — p95 overhead bound.
//   - BenchGateway_GoroutineLeakCheck    (AC-044 / NFR-003) — no goroutine leak.
//   - BenchGateway_GoroutineCountBounded (AC-047 / NFR-006) — bounded upstream.
//   - BenchGateway_Overload3x_...        (AC-043 / NFR-002) — graceful overload.
//   - TestSuite_RaceFree                 (AC-049 / NFR-008) — race-free marker.
//
// Everything here is fast (O(seconds)) and runs under `go test -race ./...`. The
// strict 20ms p95 bound is machine-dependent, so under `-short` we assert only a
// lenient ceiling to keep CI non-flaky; the full bound runs in a normal `go test`.
package load

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adverax/sluice/internal/health"
	"github.com/adverax/sluice/internal/pool"
	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/proxy"
	"github.com/adverax/sluice/internal/server"
)

// chatBody is a minimal valid POST /v1/chat/completions body.
var chatBody = mustJSON(map[string]any{
	"model": "mock",
	"messages": []map[string]string{
		{"role": "user", "content": "ping"},
	},
})

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// newGateway builds the composed gateway handler over a 0-latency mock provider
// and a bounded worker pool, mirroring the production composition order closely
// enough that the measured per-request time is the gateway's real overhead. The
// pool wraps the (instant) infer hook so AC-047's bounded-upstream invariant is
// live. limit is the worker-pool size.
func newGateway(limit int) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	router := proxy.NewRouter()
	router.Register("mock", provider.New(provider.WithResponse(provider.Response{
		Model:        "mock",
		Content:      "pong",
		FinishReason: "stop",
		Usage:        provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}))) // zero latency by construction.

	guarded := pool.Guard(limit, time.Second, func(ctx context.Context, p provider.Provider, req provider.Request) (provider.Response, error) {
		return p.Infer(ctx, req)
	})

	hh := health.New(logger, time.Second)
	srv := server.New(router, hh, logger, server.WithInferFunc(guarded))
	return srv.Handler(http.NewServeMux())
}

// doRequest issues one POST /v1/chat/completions and returns the status code.
func doRequest(h http.Handler) int {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// BenchGateway_p95OverheadUnder20ms is AC-042 (NFR-001): with a 0ms-latency mock
// provider, measure the gateway's own per-request overhead over N requests and
// assert p95 <= the bound. Because the mock adds no latency, the full in-process
// request time is the overhead. The strict 20ms bound is machine-dependent: in
// -short mode we assert only a generous ceiling so CI never flakes; a full run
// asserts the real 20ms NFR target.
func TestBenchGateway_p95OverheadUnder20ms(t *testing.T) {
	h := newGateway(100)

	// Warm up the handler (spec parse, JIT of allocations) so the measured window
	// reflects steady-state overhead, not first-call costs.
	for i := 0; i < 200; i++ {
		if code := doRequest(h); code != http.StatusOK {
			t.Fatalf("warmup request returned %d, want 200", code)
		}
	}

	const samples = 5000
	latencies := make([]time.Duration, samples)
	for i := 0; i < samples; i++ {
		start := time.Now()
		code := doRequest(h)
		latencies[i] = time.Since(start)
		if code != http.StatusOK {
			t.Fatalf("request %d returned %d, want 200", i, code)
		}
	}

	p50 := percentile(latencies, 50)
	p95 := percentile(latencies, 95)
	p99 := percentile(latencies, 99)
	t.Logf("in-process gateway overhead over %d requests: p50=%s p95=%s p99=%s", samples, p50, p95, p99)

	bound := 20 * time.Millisecond // NFR-001 target.
	if testing.Short() {
		// Machine-dependent under shared CI runners: assert only a lenient ceiling.
		bound = 200 * time.Millisecond
	}
	if p95 > bound {
		t.Fatalf("p95 gateway overhead = %s, exceeds bound %s (NFR-001 / AC-042)", p95, bound)
	}
}

// percentile returns the p-th percentile of d (0<p<100) using nearest-rank on a
// sorted copy, so the input slice is not mutated.
func percentile(d []time.Duration, p int) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(d))
	copy(sorted, d)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := (p * len(sorted)) / 100
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// BenchGateway_Overload3x_NocrashGracefulDegradation is AC-043 (NFR-002) at the
// FULL-CHAIN level: drive ~3× the worker-pool limit in concurrent requests and
// assert no panic and every response is 200/429/503 (never any other status). A
// composed gateway under overload must shed load (503) rather than crash.
func TestBenchGateway_Overload3x_NocrashGracefulDegradation(t *testing.T) {
	const limit = 50
	h := newGateway(limit)

	const callers = limit * 3
	var ok, shed int64
	var wg sync.WaitGroup
	wg.Add(callers)
	for c := 0; c < callers; c++ {
		go func() {
			defer wg.Done()
			code := doRequest(h)
			switch code {
			case http.StatusOK:
				atomic.AddInt64(&ok, 1)
			case http.StatusTooManyRequests, http.StatusServiceUnavailable:
				atomic.AddInt64(&shed, 1)
			default:
				t.Errorf("unexpected status under overload: %d (want 200/429/503)", code)
			}
		}()
	}
	wg.Wait()

	if ok+shed != callers {
		t.Fatalf("ok+shed = %d, want %d", ok+shed, callers)
	}
	// Post-load: the gateway accepts again (graceful recovery).
	if code := doRequest(h); code != http.StatusOK {
		t.Fatalf("post-overload request returned %d, want 200 (no recovery)", code)
	}
}

// BenchGateway_GoroutineLeakCheck is AC-044 (NFR-003) at the full-chain level:
// snapshot a goroutine baseline, run repeated overload bursts against the
// composed gateway, let load subside, and assert the goroutine count returns to
// baseline within ±5.
func TestBenchGateway_GoroutineLeakCheck(t *testing.T) {
	h := newGateway(50)

	// Prime once so lazily-started background goroutines are in the baseline.
	doRequest(h)
	runtime.GC()
	baseline := runtime.NumGoroutine()

	for i := 0; i < 20; i++ {
		const callers = 150
		var wg sync.WaitGroup
		wg.Add(callers)
		for c := 0; c < callers; c++ {
			go func() { defer wg.Done(); doRequest(h) }()
		}
		wg.Wait()
	}

	// Allow stragglers to unwind.
	for i := 0; i < 5; i++ {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(20 * time.Millisecond)
	}
	after := runtime.NumGoroutine()
	if after > baseline+5 {
		t.Fatalf("goroutine leak: baseline=%d after=%d (tolerance 5, AC-044)", baseline, after)
	}
}

// BenchGateway_GoroutineCountBounded is AC-047 (NFR-006): under sustained
// overload the number of requests concurrently inside the upstream (provider)
// call never exceeds the worker-pool limit. A sampler in the infer hook records
// the max concurrency it ever observes; it must stay <= limit.
func TestBenchGateway_GoroutineCountBounded(t *testing.T) {
	const limit = 50
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var current, maxObserved int64
	infer := func(ctx context.Context, p provider.Provider, req provider.Request) (provider.Response, error) {
		cur := atomic.AddInt64(&current, 1)
		for {
			m := atomic.LoadInt64(&maxObserved)
			if cur <= m || atomic.CompareAndSwapInt64(&maxObserved, m, cur) {
				break
			}
		}
		runtime.Gosched() // encourage overlap so calls contend for slots.
		atomic.AddInt64(&current, -1)
		return p.Infer(ctx, req)
	}

	router := proxy.NewRouter()
	router.Register("mock", provider.New(provider.WithResponse(provider.Response{Model: "mock"})))
	guarded := pool.Guard(limit, time.Second, infer)
	hh := health.New(logger, time.Second)
	srv := server.New(router, hh, logger, server.WithInferFunc(guarded))
	h := srv.Handler(http.NewServeMux())

	for i := 0; i < 10; i++ {
		const callers = limit * 3
		var wg sync.WaitGroup
		wg.Add(callers)
		for c := 0; c < callers; c++ {
			go func() { defer wg.Done(); doRequest(h) }()
		}
		wg.Wait()
	}

	if got := atomic.LoadInt64(&maxObserved); got > int64(limit) {
		t.Fatalf("max concurrent upstream calls = %d, exceeds pool limit %d (NFR-006 / AC-047)", got, limit)
	}
}

// TestSuite_RaceFree is the AC-049 (NFR-008) marker: it composes the gateway and
// hammers it from many goroutines so the race detector has a wide surface to
// observe. The whole suite — this plus every package's table-driven tests AND
// the //go:build integration suite — is expected to run clean under
// `go test -race ./...` (and `-tags=integration -race -p 1 ./...`) with zero
// "DATA RACE" reports. There is no assertion to make beyond "the race detector
// found nothing"; a detected race fails the test process with exit code != 0.
func TestSuite_RaceFree(t *testing.T) {
	h := newGateway(64)
	const workers = 32
	const perWorker = 50
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				code := doRequest(h)
				if code != http.StatusOK && code != http.StatusServiceUnavailable && code != http.StatusTooManyRequests {
					t.Errorf("unexpected status %d (want 200/429/503)", code)
					return
				}
			}
		}()
	}
	wg.Wait()
}
