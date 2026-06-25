package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/adverax/sluice/internal/cache"
)

const (
	// cacheRoute is the only route the cache acts on: the chat-completions
	// endpoint (POST). Every other route/method passes straight through.
	cacheRoute  = "/v1/chat/completions"
	cacheMethod = http.MethodPost

	// cacheHeader carries the cache outcome to the client (AC-014 / AC-015).
	cacheHeader   = "X-Cache"
	cacheHitValue = "HIT"
	cacheMissVal  = "MISS"

	// ttlOverrideHeader lets a client override the cache TTL for a single
	// request, in whole seconds (ADR-0004). Invalid values are ignored and the
	// configured default applies.
	ttlOverrideHeader = "X-Cache-TTL"
)

// streamProbe is a tolerant minimal view of the chat-completion request body:
// it extracts only the "stream" flag and ignores every other field. A
// streaming request (stream:true) bypasses the cache entirely — no key is
// computed and no provider response is stored (AC-016).
type streamProbe struct {
	Stream bool `json:"stream"`
}

// CacheMiddleware caches successful POST /v1/chat/completions responses in a
// CacheRepository (COMP-004, FR-005), keyed by a sha256 hash of the request
// identity (method + path + raw body bytes).
//
// Canonicalization limitation (DOCUMENTED, acceptable for v1): the key hashes
// the RAW body bytes, so two semantically-identical requests that differ only in
// JSON whitespace or key ordering are treated as DISTINCT cache entries. Proper
// JSON canonicalization is deferred — over-caching is never a correctness risk,
// only a hit-rate one.
//
// The middleware depends only on the cache.CacheRepository port (ports &
// adapters, ADR-0010), never on go-redis. A repository error on Get OR Set is
// logged and the request falls through to the live handler, so a Redis outage
// never becomes a client error (AC-017, resilience).
type CacheMiddleware struct {
	repo       cache.CacheRepository
	defaultTTL time.Duration
	logger     *slog.Logger
}

// NewCacheMiddleware builds the cache middleware. repo is the injected
// repository port (a Redis adapter in production, a fake in tests); defaultTTL
// is the configured fallback TTL (GATEWAY_CACHE_TTL, default 5m per ADR-0004);
// logger is injected (ADR-0008). A nil repo disables caching (every request
// passes through untouched), and a non-positive defaultTTL falls back to 5m so
// the middleware can never store a non-expiring entry.
func NewCacheMiddleware(repo cache.CacheRepository, defaultTTL time.Duration, logger *slog.Logger) *CacheMiddleware {
	if defaultTTL <= 0 {
		defaultTTL = 5 * time.Minute
	}
	return &CacheMiddleware{
		repo:       repo,
		defaultTTL: defaultTTL,
		logger:     logger,
	}
}

// Middleware returns the http middleware function (the chain link). It is
// intended to sit INNERMOST (just before the generated routes), after the
// counting middleware, so a cache HIT short-circuits the provider path while
// still being covered by the outer logging/metrics/tracing instrumentation.
func (m *CacheMiddleware) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Pass through anything that is not POST /v1/chat/completions, and
		// disable caching entirely when no repository is wired.
		if m.repo == nil || r.Method != cacheMethod || r.URL.Path != cacheRoute {
			next.ServeHTTP(w, r)
			return
		}

		// Read + RESTORE the body so the downstream handler can re-read it. On a
		// read error fall through to the handler with whatever remains; the cache
		// simply does not engage for this request.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			m.logger.LogAttrs(r.Context(), slog.LevelWarn,
				"cache: failed to read request body; bypassing cache",
				slog.String("error", err.Error()),
			)
			r.Body = io.NopCloser(bytes.NewReader(body))
			next.ServeHTTP(w, r)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		// Streaming requests bypass the cache entirely — no key is computed and
		// no response is stored (AC-016). Parse tolerantly: a malformed body is
		// treated as non-streaming and left for the downstream handler to reject.
		var probe streamProbe
		if jsonErr := json.Unmarshal(body, &probe); jsonErr == nil && probe.Stream {
			next.ServeHTTP(w, r)
			return
		}

		key := cacheKey(r.Method, r.URL.Path, body)

		// Cache GET. On a HIT, replay the cached status+body and return WITHOUT
		// calling next — the provider is never contacted (AC-014). On a backend
		// error, log and fall through to the live handler (AC-017).
		if cached, found, getErr := m.repo.Get(r.Context(), key); getErr != nil {
			m.logger.LogAttrs(r.Context(), slog.LevelWarn,
				"cache: get failed; serving live",
				slog.String("error", getErr.Error()),
			)
		} else if found {
			w.Header().Set(cacheHeader, cacheHitValue)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(cached)
			return
		}

		// MISS: capture the downstream response so a cacheable result can be
		// stored, then advertise the miss. X-Cache is set BEFORE WriteHeader so it
		// is flushed with the response headers.
		rec := &cacheRecorder{ResponseWriter: w, status: http.StatusOK}
		w.Header().Set(cacheHeader, cacheMissVal)
		next.ServeHTTP(rec, r)

		// Only cache a successful 200 response. Use the per-request TTL override
		// when present and valid, else the configured default (ADR-0004).
		if rec.status == http.StatusOK {
			ttl := m.resolveTTL(r)
			// Detach from the request context so cancellation of the (completed)
			// request does not abort the best-effort store, but bound it so a slow
			// Redis cannot leak goroutines indefinitely.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
			defer cancel()
			if setErr := m.repo.Set(ctx, key, rec.body.Bytes(), ttl); setErr != nil {
				m.logger.LogAttrs(r.Context(), slog.LevelWarn,
					"cache: set failed; response served but not cached",
					slog.String("error", setErr.Error()),
				)
			}
		}
	})
}

// resolveTTL returns the TTL for this response: the X-Cache-TTL header value (in
// seconds) when present and a valid positive integer, otherwise the configured
// default (ADR-0004 per-request override). Invalid/non-positive values are
// ignored.
func (m *CacheMiddleware) resolveTTL(r *http.Request) time.Duration {
	if v := r.Header.Get(ttlOverrideHeader); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return m.defaultTTL
}

// cacheKey hashes the canonical request identity (method + path + raw body
// bytes) into a hex sha256 string. See the CacheMiddleware doc comment for the
// whitespace/key-order canonicalization limitation.
func cacheKey(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// cacheRecorder wraps the ResponseWriter on a cache MISS to capture the status
// code and body bytes so a cacheable response can be stored. Writes are still
// streamed to the underlying writer immediately (the client is not blocked on
// the cache store).
//
// Unwrap exposes the underlying ResponseWriter so http.ResponseController and
// net/http's interface-capability detection (Flusher, Hijacker) can reach the
// base writer — keeping the chain clean even though the streaming path bypasses
// this wrapper.
type cacheRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (r *cacheRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *cacheRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

// Flush implements http.Flusher so streaming responses pass through.
func (r *cacheRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying ResponseWriter so http.ResponseController and
// net/http's interface-capability detection can reach the base writer.
func (r *cacheRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
