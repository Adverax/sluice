package ratelimit

import (
	"context"
	"sync"
	"time"
)

// MemoryRepository is an in-memory RateLimitRepository. It implements the same
// fixed-window global counter semantics as the Redis adapter, but in process
// memory.
//
// Its primary use is testing: a SINGLE MemoryRepository instance shared by two
// Middleware instances simulates two gateway processes pointing at one Redis,
// so the shared global cap (AC-013) can be exercised without a live Redis (the
// real go-redis adapter is integration-tested in CARD-011 via testcontainers).
//
// It is also a safe default when no distributed store is configured: a single
// instance then enforces the global cap locally. It is safe for concurrent use.
type MemoryRepository struct {
	now func() time.Time

	mu      sync.Mutex
	windows map[string]*memWindow
}

type memWindow struct {
	start time.Time
	count int
}

// MemoryOption configures a MemoryRepository.
type MemoryOption func(*MemoryRepository)

// WithMemoryClock injects the clock (test seam for deterministic windows).
func WithMemoryClock(now func() time.Time) MemoryOption {
	return func(m *MemoryRepository) {
		if now != nil {
			m.now = now
		}
	}
}

// NewMemoryRepository builds an in-memory distributed-limit repository.
func NewMemoryRepository(opts ...MemoryOption) *MemoryRepository {
	m := &MemoryRepository{
		now:     time.Now,
		windows: make(map[string]*memWindow),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Allow atomically increments the fixed-window counter for key and reports
// whether the count is within limit for the current window. It never returns an
// error (the in-memory store cannot fail), satisfying RateLimitRepository.
func (m *MemoryRepository) Allow(_ context.Context, key string, limit int, window time.Duration) (Decision, error) {
	now := m.now()

	m.mu.Lock()
	defer m.mu.Unlock()

	w, ok := m.windows[key]
	if !ok || now.Sub(w.start) >= window {
		w = &memWindow{start: now}
		m.windows[key] = w
	}
	w.count++
	if w.count > limit {
		// Deny: suggest waiting until the current window rolls over.
		retryAfter := window - now.Sub(w.start)
		if retryAfter <= 0 {
			retryAfter = window
		}
		return Decision{Allowed: false, RetryAfter: retryAfter}, nil
	}
	return Decision{Allowed: true}, nil
}

var _ RateLimitRepository = (*MemoryRepository)(nil)
