package middleware

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adverax/sluice/internal/ratelimit"
)

// discardLogger returns a logger that drops all output (tests assert behaviour,
// not log lines).
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// spyHandler records whether the wrapped (downstream) handler was invoked. The
// 429 path must never reach it (AC-010, INV-004).
type spyHandler struct {
	called atomic.Int64
}

func (s *spyHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.called.Add(1)
	w.WriteHeader(http.StatusOK)
}

// fixedClock is a controllable clock for deterministic token accounting.
type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func newRequest(authKey string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if authKey != "" {
		r.Header.Set("Authorization", "Bearer "+authKey)
	}
	return r
}

// TestRateLimit_ExceedLimit_Returns429WithRetryAfter covers AC-010: with a local
// burst of 10, the first 10 requests for key-A are served and the 11th is
// rejected with 429 + Retry-After; the downstream handler is NOT called on the
// 11th. The clock is frozen so no tokens refill mid-test (deterministic).
func TestRateLimit_ExceedLimit_Returns429WithRetryAfter(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1_700_000_000, 0)}
	reg := ratelimit.NewRegistry(10, 10, ratelimit.WithClock(clk.Now))
	spy := &spyHandler{}
	mw := NewRateLimiter(reg, nil, 10, time.Second, discardLogger()).Middleware(spy)

	// 10 served (burst capacity), clock frozen so no refill.
	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, newRequest("key-A"))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: got status %d, want 200", i+1, rec.Code)
		}
	}
	if got := spy.called.Load(); got != 10 {
		t.Fatalf("downstream called %d times, want 10", got)
	}

	// 11th: rejected.
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest("key-A"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("11th request: got status %d, want 429", rec.Code)
	}
	ra := rec.Header().Get("Retry-After")
	if ra == "" {
		t.Fatal("11th request: missing Retry-After header")
	}
	if secs, err := strconv.Atoi(ra); err != nil || secs < 1 {
		t.Fatalf("11th request: Retry-After=%q, want positive integer seconds", ra)
	}
	if got := spy.called.Load(); got != 10 {
		t.Fatalf("downstream called %d times after 429, want 10 (provider must NOT be contacted)", got)
	}
}

// TestRateLimit_WithinLimit_Passes covers AC-011: with 5 of 10 tokens used, the
// next request passes through to the downstream handler with no 429.
func TestRateLimit_WithinLimit_Passes(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1_700_000_000, 0)}
	reg := ratelimit.NewRegistry(10, 10, ratelimit.WithClock(clk.Now))
	spy := &spyHandler{}
	mw := NewRateLimiter(reg, nil, 10, time.Second, discardLogger()).Middleware(spy)

	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, newRequest("key-A"))
		if rec.Code != http.StatusOK {
			t.Fatalf("warmup request %d: got %d, want 200", i+1, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest("key-A"))
	if rec.Code != http.StatusOK {
		t.Fatalf("6th request: got status %d, want 200", rec.Code)
	}
	if got := spy.called.Load(); got != 6 {
		t.Fatalf("downstream called %d times, want 6", got)
	}
}

// TestRateLimit_MissingApiKey_HandledGracefully covers AC-012: with no
// Authorization header the middleware mints an ephemeral key, advertises it via
// the X-Sluice-Api-Key response header AND a Set-Cookie, and rate-limits the
// request under that minted key (a fresh per-key bucket is applied).
func TestRateLimit_MissingApiKey_HandledGracefully(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1_700_000_000, 0)}
	reg := ratelimit.NewRegistry(10, 10, ratelimit.WithClock(clk.Now))
	spy := &spyHandler{}

	// Deterministic minter so we can assert the exact key flows into the bucket.
	var counter atomic.Int64
	minter := func() string { return "minted-" + strconv.FormatInt(counter.Add(1), 10) }
	mw := NewRateLimiter(reg, nil, 10, time.Second, discardLogger(), WithKeyMinter(minter)).Middleware(spy)

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest("")) // no Authorization

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200 (handled gracefully, not 401)", rec.Code)
	}
	gotKey := rec.Header().Get(apiKeyHeader)
	if gotKey == "" {
		t.Fatalf("missing %s response header", apiKeyHeader)
	}
	if gotKey != "minted-1" {
		t.Fatalf("X-Sluice-Api-Key=%q, want minted-1", gotKey)
	}

	// Set-Cookie carries the same key, HttpOnly.
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == apiKeyCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("missing %s Set-Cookie", apiKeyCookie)
	}
	if cookie.Value != gotKey {
		t.Fatalf("cookie value=%q, want %q (same minted key)", cookie.Value, gotKey)
	}
	if !cookie.HttpOnly {
		t.Fatal("ephemeral key cookie must be HttpOnly")
	}
	if got := spy.called.Load(); got != 1 {
		t.Fatalf("downstream called %d times, want 1", got)
	}

	// A fresh bucket WAS applied under the minted key: drain it (10 burst) using
	// the same key and confirm the 11th is 429 — proving the minted key is the
	// rate-limit subject, not a passthrough.
	for i := 0; i < 9; i++ { // 1 already consumed above → 9 more fill the burst
		rec := httptest.NewRecorder()
		r := newRequest("")
		r.Header.Set("Authorization", "Bearer minted-1") // reuse the minted key
		mw.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("reuse request %d under minted key: got %d, want 200", i+1, rec.Code)
		}
	}
	rec2 := httptest.NewRecorder()
	r2 := newRequest("")
	r2.Header.Set("Authorization", "Bearer minted-1")
	mw.ServeHTTP(rec2, r2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("11th request under minted key: got %d, want 429 (fresh bucket applied)", rec2.Code)
	}
}

// sharedAllowRepo wraps a single MemoryRepository so two middleware instances
// share one distributed store, simulating two gateway processes pointing at one
// Redis (AC-013).
//
// TestRateLimit_DistributedRedis_GlobalLimit covers AC-013: a SHARED in-memory
// RateLimitRepository (the port) is consulted by two Middleware instances. With
// a global cap of 100/window and 60 concurrent requests on each instance (120
// total), no more than ~100 pass; the rest get 429. The real go-redis adapter is
// integration-tested via testcontainers in CARD-011 (not required here).
func TestRateLimit_DistributedRedis_GlobalLimit(t *testing.T) {
	const globalLimit = 100
	const perInstance = 60
	window := time.Hour // single fixed window for the whole test (deterministic)

	// One shared distributed repo; local limiters are sized large so they never
	// bind — the test isolates the DISTRIBUTED cap.
	repo := ratelimit.NewMemoryRepository()
	mk := func() http.Handler {
		reg := ratelimit.NewRegistry(1_000_000, 1_000_000)
		return NewRateLimiter(reg, repo, globalLimit, window, discardLogger()).Middleware(
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		)
	}
	inst1, inst2 := mk(), mk()

	var passed, limited atomic.Int64
	var wg sync.WaitGroup
	fire := func(h http.Handler) {
		defer wg.Done()
		rec := httptest.NewRecorder()
		// Same key across both instances → one shared global bucket.
		r := newRequest("shared-key").WithContext(context.Background())
		h.ServeHTTP(rec, r)
		switch rec.Code {
		case http.StatusOK:
			passed.Add(1)
		case http.StatusTooManyRequests:
			limited.Add(1)
		default:
			t.Errorf("unexpected status %d", rec.Code)
		}
	}

	wg.Add(perInstance * 2)
	for i := 0; i < perInstance; i++ {
		go fire(inst1)
		go fire(inst2)
	}
	wg.Wait()

	if got := passed.Load(); got != globalLimit {
		t.Fatalf("passed=%d, want exactly %d (global cap enforced across instances)", got, globalLimit)
	}
	if got := limited.Load(); got != perInstance*2-globalLimit {
		t.Fatalf("limited=%d, want %d", got, perInstance*2-globalLimit)
	}
}

// TestRateLimit_DistributedFailOpen documents the resilience choice: when the
// distributed repository returns an ERROR (Redis down), the middleware fails
// OPEN to the local limiter rather than rejecting the request.
func TestRateLimit_DistributedFailOpen(t *testing.T) {
	reg := ratelimit.NewRegistry(10, 10)
	spy := &spyHandler{}
	mw := NewRateLimiter(reg, errRepo{}, 10, time.Second, discardLogger()).Middleware(spy)

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest("key-A"))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (fail open on repo error)", rec.Code)
	}
	if got := spy.called.Load(); got != 1 {
		t.Fatalf("downstream called %d times, want 1", got)
	}
}

type errRepo struct{}

func (errRepo) Allow(context.Context, string, int, time.Duration) (ratelimit.Decision, error) {
	return ratelimit.Decision{}, context.DeadlineExceeded
}

// TestRateLimit_CookieRoundTrip covers the cookie-based bucket reuse path
// (ADR-0001 / MANDATORY FIX 1):
//
//  1. A first keyless request (no Authorization header, no cookie) → middleware
//     mints a fresh key, sets X-Sluice-Api-Key + Set-Cookie, and the request
//     counts against the new bucket.
//  2. A follow-up request carrying ONLY the cookie (no header) → middleware
//     must reuse the SAME bucket (no new registry entry is created, the counter
//     continues from where it left off).
//  3. A request with neither header nor cookie creates a NEW key (and bucket).
//
// The test also verifies that header-omission cannot bypass the limit once the
// registry has a cookie for the key: draining the bucket via cookie-only
// requests should produce 429 just like direct key usage.
func TestRateLimit_CookieRoundTrip(t *testing.T) {
	clk := &fixedClock{now: time.Unix(1_700_000_000, 0)}
	// Burst of 3 so we can drain it quickly.
	reg := ratelimit.NewRegistry(3, 3, ratelimit.WithClock(clk.Now), ratelimit.WithSweepInterval(time.Hour))
	defer reg.Close()
	spy := &spyHandler{}

	var mintCount atomic.Int64
	// Produce valid eph_ keys: prefix + 32 hex chars (zero-padded counter).
	minter := func() string {
		n := mintCount.Add(1)
		return fmt.Sprintf("%s%032x", ephemeralKeyPrefix, n)
	}
	mw := NewRateLimiter(reg, nil, 3, time.Second, discardLogger(), WithKeyMinter(minter)).Middleware(spy)

	// --- Step 1: first keyless request mints a key and sets the cookie. ---
	rec1 := httptest.NewRecorder()
	mw.ServeHTTP(rec1, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first keyless request: got %d, want 200", rec1.Code)
	}
	mintedKey := rec1.Header().Get(apiKeyHeader)
	if mintedKey == "" {
		t.Fatalf("first keyless request: missing %s header", apiKeyHeader)
	}

	var mintedCookie *http.Cookie
	for _, c := range rec1.Result().Cookies() {
		if c.Name == apiKeyCookie {
			mintedCookie = c
		}
	}
	if mintedCookie == nil {
		t.Fatalf("first keyless request: missing %s Set-Cookie", apiKeyCookie)
	}
	if mintedCookie.Value != mintedKey {
		t.Fatalf("cookie value %q != header value %q", mintedCookie.Value, mintedKey)
	}

	sizeBefore := reg.Len() // should be 1 (only the minted key)

	// --- Step 2: follow-up with the cookie only (no Authorization header). ---
	// Bucket already consumed 1 token; 2 remain (burst=3).
	for i := 0; i < 2; i++ {
		r2 := httptest.NewRequest(http.MethodPost, "/", nil)
		r2.AddCookie(mintedCookie) // carry the issued cookie
		rec2 := httptest.NewRecorder()
		mw.ServeHTTP(rec2, r2)
		if rec2.Code != http.StatusOK {
			t.Fatalf("cookie-only request %d: got %d, want 200", i+1, rec2.Code)
		}
	}

	// Registry must NOT have grown — cookie reused the existing bucket.
	if got := reg.Len(); got != sizeBefore {
		t.Fatalf("registry size after cookie-only requests: got %d, want %d (same bucket reused)", got, sizeBefore)
	}

	// Bucket is now empty (burst=3, three tokens consumed). Next cookie-only
	// request must get 429 — proving the cookie kept accumulating against the
	// same bucket and that header-omission does NOT reset it.
	r3 := httptest.NewRequest(http.MethodPost, "/", nil)
	r3.AddCookie(mintedCookie)
	rec3 := httptest.NewRecorder()
	mw.ServeHTTP(rec3, r3)
	if rec3.Code != http.StatusTooManyRequests {
		t.Fatalf("4th request with exhausted bucket (cookie-only): got %d, want 429", rec3.Code)
	}

	// --- Step 3: a brand new request with neither header nor cookie mints a
	// DIFFERENT key (separate bucket). It does not inherit the exhausted one.
	mintCount.Store(99) // force a distinct minted key
	r4 := httptest.NewRequest(http.MethodPost, "/", nil)
	rec4 := httptest.NewRecorder()
	mw.ServeHTTP(rec4, r4)
	if rec4.Code != http.StatusOK {
		t.Fatalf("fresh keyless request (no header, no cookie): got %d, want 200", rec4.Code)
	}
	freshKey := rec4.Header().Get(apiKeyHeader)
	if freshKey == mintedKey {
		t.Fatalf("fresh request got same key as exhausted bucket (%q); want a new key", freshKey)
	}
}

// TestRateLimit_CryptoRandFailClosed verifies that a crypto/rand failure
// (minter returns "") results in a 500 Internal Server Error, not a passthrough
// or a request counted against an empty-string bucket (Minor fix / fail-closed).
func TestRateLimit_CryptoRandFailClosed(t *testing.T) {
	reg := ratelimit.NewRegistry(10, 10, ratelimit.WithSweepInterval(time.Hour))
	defer reg.Close()
	spy := &spyHandler{}

	// Simulate a crypto/rand failure by returning "".
	mw := NewRateLimiter(reg, nil, 10, time.Second, discardLogger(),
		WithKeyMinter(func() string { return "" }),
	).Middleware(spy)

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("crypto/rand failure: got %d, want 500 (fail-closed)", rec.Code)
	}
	if got := spy.called.Load(); got != 0 {
		t.Fatalf("downstream called %d times, want 0 (request must not reach handler)", got)
	}
	// Registry must be empty — the empty key must not have been inserted.
	if got := reg.Len(); got != 0 {
		t.Fatalf("registry size after fail-closed: got %d, want 0", got)
	}
}
