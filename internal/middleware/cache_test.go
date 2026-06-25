package middleware

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCacheRepo is an in-memory CacheRepository for the middleware tests. It
// records the number of Get/Set calls and the last TTL passed to Set, and can be
// forced to return errors to exercise the fall-through path (AC-017). It is
// concurrency-safe so -race stays clean.
type fakeCacheRepo struct {
	mu       sync.Mutex
	store    map[string][]byte
	getCalls int
	setCalls int
	lastTTL  time.Duration
	failGet  bool
	failSet  bool
}

func newFakeCacheRepo() *fakeCacheRepo {
	return &fakeCacheRepo{store: make(map[string][]byte)}
}

func (f *fakeCacheRepo) Get(_ context.Context, key string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.failGet {
		return nil, false, errors.New("redis: connection refused")
	}
	v, ok := f.store[key]
	return v, ok, nil
}

func (f *fakeCacheRepo) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	f.lastTTL = ttl
	if f.failSet {
		return errors.New("redis: connection refused")
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	f.store[key] = cp
	return nil
}

func (f *fakeCacheRepo) counts() (get, set int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCalls, f.setCalls
}

// cacheSpyHandler counts how many times it is invoked and returns a fixed 200 body.
type cacheSpyHandler struct {
	mu    sync.Mutex
	calls int
	body  string
}

func (s *cacheSpyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, s.body)
}

func (s *cacheSpyHandler) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func doPost(t *testing.T, h http.Handler, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestCache_Hit_ReturnsCachedResponse (AC-014): the first request stores; an
// identical second request returns X-Cache: HIT with the same body WITHOUT
// invoking the downstream handler (provider not contacted). The HIT response
// must carry the same Content-Type as the MISS response (header parity).
func TestCache_Hit_ReturnsCachedResponse(t *testing.T) {
	repo := newFakeCacheRepo()
	spy := &cacheSpyHandler{body: `{"id":"resp-1","choices":[]}`}
	h := NewCacheMiddleware(repo, time.Minute, testLogger()).Middleware(spy)

	body := `{"model":"mock","messages":[{"role":"user","content":"hi"}]}`

	// First request: MISS, populates the cache, handler called once.
	rr1 := doPost(t, h, body, nil)
	if got := rr1.Header().Get(cacheHeader); got != cacheMissVal {
		t.Fatalf("first request X-Cache = %q, want %q", got, cacheMissVal)
	}
	if spy.count() != 1 {
		t.Fatalf("handler calls after first request = %d, want 1", spy.count())
	}
	missContentType := rr1.Header().Get("Content-Type")

	// Second identical request: HIT, served from cache, handler NOT called again.
	rr2 := doPost(t, h, body, nil)
	if got := rr2.Header().Get(cacheHeader); got != cacheHitValue {
		t.Fatalf("second request X-Cache = %q, want %q", got, cacheHitValue)
	}
	if rr2.Body.String() != spy.body {
		t.Fatalf("cached body = %q, want %q", rr2.Body.String(), spy.body)
	}
	if rr2.Code != http.StatusOK {
		t.Fatalf("cached status = %d, want 200", rr2.Code)
	}
	if spy.count() != 1 {
		t.Fatalf("handler calls after cache hit = %d, want 1 (provider must not be contacted)", spy.count())
	}

	// HIT Content-Type must equal MISS Content-Type (header parity assertion).
	hitContentType := rr2.Header().Get("Content-Type")
	if hitContentType != missContentType {
		t.Fatalf("HIT Content-Type = %q, MISS Content-Type = %q: header parity violated", hitContentType, missContentType)
	}
	if hitContentType != "application/json" {
		t.Fatalf("HIT Content-Type = %q, want %q", hitContentType, "application/json")
	}
}

// TestCache_Miss_FetchesAndCaches (AC-015): an empty cache results in the handler
// being called, the response stored via Set with the expected (default) TTL, and
// X-Cache: MISS returned.
func TestCache_Miss_FetchesAndCaches(t *testing.T) {
	repo := newFakeCacheRepo()
	spy := &cacheSpyHandler{body: `{"id":"resp-2"}`}
	const ttl = 5 * time.Minute
	h := NewCacheMiddleware(repo, ttl, testLogger()).Middleware(spy)

	rr := doPost(t, h, `{"model":"mock"}`, nil)

	if got := rr.Header().Get(cacheHeader); got != cacheMissVal {
		t.Fatalf("X-Cache = %q, want %q", got, cacheMissVal)
	}
	if spy.count() != 1 {
		t.Fatalf("handler calls = %d, want 1", spy.count())
	}
	_, setCalls := repo.counts()
	if setCalls != 1 {
		t.Fatalf("Set calls = %d, want 1", setCalls)
	}
	if repo.lastTTL != ttl {
		t.Fatalf("Set TTL = %s, want %s", repo.lastTTL, ttl)
	}
}

// TestCache_StreamingNotCached (AC-016): a stream:true body bypasses the cache
// entirely — no key is computed (Get not called), nothing is stored (Set not
// called), and the handler is invoked.
func TestCache_StreamingNotCached(t *testing.T) {
	repo := newFakeCacheRepo()
	spy := &cacheSpyHandler{body: "data: ...\n\n"}
	h := NewCacheMiddleware(repo, time.Minute, testLogger()).Middleware(spy)

	rr := doPost(t, h, `{"model":"mock","stream":true,"messages":[]}`, nil)

	getCalls, setCalls := repo.counts()
	if getCalls != 0 {
		t.Fatalf("Get calls = %d, want 0 (no key computed for streaming)", getCalls)
	}
	if setCalls != 0 {
		t.Fatalf("Set calls = %d, want 0 (streaming not cached)", setCalls)
	}
	if spy.count() != 1 {
		t.Fatalf("handler calls = %d, want 1", spy.count())
	}
	if got := rr.Header().Get(cacheHeader); got != "" {
		t.Fatalf("X-Cache = %q, want empty for bypassed streaming request", got)
	}
}

// TestCache_RedisDown_FallsThrough (AC-017): when the repository errors on Get
// AND Set, the request still succeeds via the handler with the handler's status
// and body; no Redis error is propagated to the client.
func TestCache_RedisDown_FallsThrough(t *testing.T) {
	repo := newFakeCacheRepo()
	repo.failGet = true
	repo.failSet = true
	spy := &cacheSpyHandler{body: `{"id":"resp-live"}`}
	h := NewCacheMiddleware(repo, time.Minute, testLogger()).Middleware(spy)

	rr := doPost(t, h, `{"model":"mock"}`, nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (must serve live on Redis error)", rr.Code)
	}
	if rr.Body.String() != spy.body {
		t.Fatalf("body = %q, want live handler body %q", rr.Body.String(), spy.body)
	}
	if spy.count() != 1 {
		t.Fatalf("handler calls = %d, want 1", spy.count())
	}
	// X-Cache: MISS is set before the live response is served.
	if got := rr.Header().Get(cacheHeader); got != cacheMissVal {
		t.Fatalf("X-Cache = %q, want %q", got, cacheMissVal)
	}
}

// TestCache_PerRequestTTLOverride: a valid X-Cache-TTL header overrides the
// default TTL for that response's Set call.
func TestCache_PerRequestTTLOverride(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		wantTTL time.Duration
	}{
		{name: "valid override", header: "30", wantTTL: 30 * time.Second},
		{name: "invalid non-numeric falls back to default", header: "abc", wantTTL: time.Minute},
		{name: "non-positive falls back to default", header: "0", wantTTL: time.Minute},
		{name: "negative falls back to default", header: "-5", wantTTL: time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeCacheRepo()
			spy := &cacheSpyHandler{body: `{"ok":true}`}
			h := NewCacheMiddleware(repo, time.Minute, testLogger()).Middleware(spy)

			doPost(t, h, `{"model":"mock"}`, map[string]string{ttlOverrideHeader: tc.header})

			if _, setCalls := repo.counts(); setCalls != 1 {
				t.Fatalf("Set calls = %d, want 1", setCalls)
			}
			if repo.lastTTL != tc.wantTTL {
				t.Fatalf("Set TTL = %s, want %s", repo.lastTTL, tc.wantTTL)
			}
		})
	}
}

// TestCache_NonTargetRoutePassesThrough verifies that requests that are not
// POST /v1/chat/completions never touch the cache.
func TestCache_NonTargetRoutePassesThrough(t *testing.T) {
	repo := newFakeCacheRepo()
	spy := &cacheSpyHandler{body: "ok"}
	h := NewCacheMiddleware(repo, time.Minute, testLogger()).Middleware(spy)

	// Wrong method on the cache route.
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// Wrong path with POST.
	req2 := httptest.NewRequest(http.MethodPost, "/healthz", strings.NewReader("{}"))
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)

	getCalls, setCalls := repo.counts()
	if getCalls != 0 || setCalls != 0 {
		t.Fatalf("repo touched for non-target routes: get=%d set=%d, want 0/0", getCalls, setCalls)
	}
	if spy.count() != 2 {
		t.Fatalf("handler calls = %d, want 2", spy.count())
	}
}

// TestCache_NonOKNotCached verifies that a non-200 downstream response is not
// stored (only successful responses are cacheable).
func TestCache_NonOKNotCached(t *testing.T) {
	repo := newFakeCacheRepo()
	errHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":"upstream"}`)
	})
	h := NewCacheMiddleware(repo, time.Minute, testLogger()).Middleware(errHandler)

	rr := doPost(t, h, `{"model":"mock"}`, nil)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	if _, setCalls := repo.counts(); setCalls != 0 {
		t.Fatalf("Set calls = %d, want 0 (non-200 not cacheable)", setCalls)
	}
}

// TestCache_BodyRestoredForDownstream verifies the downstream handler can still
// read the request body after the middleware consumes it for keying.
func TestCache_BodyRestoredForDownstream(t *testing.T) {
	repo := newFakeCacheRepo()
	const body = `{"model":"mock","messages":[{"role":"user","content":"hello"}]}`
	var seen string
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	})
	h := NewCacheMiddleware(repo, time.Minute, testLogger()).Middleware(echo)

	doPost(t, h, body, nil)

	if seen != body {
		t.Fatalf("downstream read body = %q, want %q (body must be restored)", seen, body)
	}
}

// TestCache_CorruptEnvelopeTreatedAsMiss verifies that if the stored value in
// the repository cannot be decoded as a valid cacheEnvelope (e.g. it was
// written in a previous format), the middleware falls through to the live
// handler rather than 500-ing.
func TestCache_CorruptEnvelopeTreatedAsMiss(t *testing.T) {
	repo := newFakeCacheRepo()
	spy := &cacheSpyHandler{body: `{"id":"fresh"}`}
	h := NewCacheMiddleware(repo, time.Minute, testLogger()).Middleware(spy)

	reqBody := `{"model":"mock","messages":[]}`
	key := cacheKey(http.MethodPost, cacheRoute, []byte(reqBody))

	// Inject a corrupt (non-envelope) value directly into the repo.
	_ = repo.Set(context.Background(), key, []byte("not-valid-json-envelope"), time.Minute)

	rr := doPost(t, h, reqBody, nil)

	// Should have fallen through to the handler (live response).
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if spy.count() != 1 {
		t.Fatalf("handler calls = %d, want 1 (corrupt entry must be treated as MISS)", spy.count())
	}
	if rr.Body.String() != spy.body {
		t.Fatalf("body = %q, want %q", rr.Body.String(), spy.body)
	}
}

// TestCache_OversizedBodyFallsThrough verifies that a request body exceeding
// the configured cap bypasses the cache entirely (no Get/Set) and the downstream
// handler receives the COMPLETE, untruncated body (not a 413 from the cache layer,
// no corruption from partial reads).
func TestCache_OversizedBodyFallsThrough(t *testing.T) {
	const cap = 16 // tiny cap so the test body easily exceeds it

	repo := newFakeCacheRepo()

	// Echo handler: reads the full request body and writes it back as the response,
	// so we can assert byte-for-byte completeness on the response.
	var handlerCalls int
	var mu sync.Mutex
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		handlerCalls++
		mu.Unlock()
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "body read error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	})

	h := NewCacheMiddleware(repo, time.Minute, testLogger(), WithMaxBodyBytes(cap)).Middleware(echo)

	// bigBody is definitely >cap bytes (well over 16 bytes) plus a large extra payload
	// to ensure the remainder-of-stream path is exercised.
	bigBody := `{"model":"mock","messages":[{"role":"user","content":"this is a long message that exceeds the cap"}]}`
	if len(bigBody) <= cap {
		t.Fatalf("test setup: bigBody length %d must exceed cap %d", len(bigBody), cap)
	}

	rr := doPost(t, h, bigBody, nil)

	// Must succeed — no 413 from the cache layer.
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (oversized body must not produce 413 from cache layer)", rr.Code)
	}

	// Handler must be invoked exactly once.
	mu.Lock()
	calls := handlerCalls
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}

	// Cache must not be consulted or written.
	getCalls, setCalls := repo.counts()
	if getCalls != 0 || setCalls != 0 {
		t.Fatalf("repo touched for oversized body: get=%d set=%d, want 0/0", getCalls, setCalls)
	}

	// CRITICAL: the downstream handler must receive the COMPLETE, untruncated body.
	// The echo handler writes back exactly what it read, so response body == input.
	got := rr.Body.String()
	if got != bigBody {
		t.Fatalf("downstream body mismatch:\n  got  len=%d %q\n  want len=%d %q\n(body must not be truncated by the cache layer)", len(got), got, len(bigBody), bigBody)
	}
}
