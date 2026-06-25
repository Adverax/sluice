package ratelimit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeScripter is a minimal redis.Scripter that emulates the atomic
// INCR+PEXPIRE+PTTL fixed-window script in memory, so the RedisRepository
// adapter can be unit-tested without a live Redis (the real client is
// integration-tested via testcontainers in CARD-011). It supports forcing an
// error to exercise the fail-open path.
type fakeScripter struct {
	mu     sync.Mutex
	counts map[string]int64
	failed bool
}

func newFakeScripter() *fakeScripter {
	return &fakeScripter{counts: make(map[string]int64)}
}

func (f *fakeScripter) Eval(ctx context.Context, _ string, keys []string, args ...any) *redis.Cmd {
	cmd := redis.NewCmd(ctx)
	if f.failed {
		cmd.SetErr(errors.New("redis: connection refused"))
		return cmd
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := keys[0]
	f.counts[key]++
	current := f.counts[key]
	// Return [count, ttl_ms] mirroring the real script's reply shape.
	cmd.SetVal([]any{current, int64(1000)})
	return cmd
}

func (f *fakeScripter) EvalRO(ctx context.Context, s string, keys []string, args ...any) *redis.Cmd {
	return f.Eval(ctx, s, keys, args...)
}

func (f *fakeScripter) EvalSha(ctx context.Context, _ string, keys []string, args ...any) *redis.Cmd {
	// Force the Script.Run fallback to plain EVAL by reporting the sha missing.
	cmd := redis.NewCmd(ctx)
	cmd.SetErr(redis.ErrNoScript)
	return cmd
}

func (f *fakeScripter) EvalShaRO(ctx context.Context, s string, keys []string, args ...any) *redis.Cmd {
	return f.EvalSha(ctx, s, keys, args...)
}

func (f *fakeScripter) ScriptExists(ctx context.Context, _ ...string) *redis.BoolSliceCmd {
	cmd := redis.NewBoolSliceCmd(ctx)
	cmd.SetVal([]bool{false})
	return cmd
}

func (f *fakeScripter) ScriptLoad(ctx context.Context, _ string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx)
	cmd.SetVal("fakesha")
	return cmd
}

var _ redis.Scripter = (*fakeScripter)(nil)

// TestRedisRepository_FixedWindow verifies the adapter parses the Lua reply and
// caps at limit (the distributed global cap, AC-013), using a fake scripter.
func TestRedisRepository_FixedWindow(t *testing.T) {
	repo := NewRedisRepository(newFakeScripter())
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		d, err := repo.Allow(ctx, "k", 5, time.Second)
		if err != nil {
			t.Fatalf("request %d: err %v", i+1, err)
		}
		if !d.Allowed {
			t.Fatalf("request %d: denied, want allowed", i+1)
		}
	}
	d, err := repo.Allow(ctx, "k", 5, time.Second)
	if err != nil {
		t.Fatalf("6th: err %v", err)
	}
	if d.Allowed {
		t.Fatal("6th request: allowed, want denied")
	}
	if d.RetryAfter <= 0 {
		t.Fatal("denied decision must carry a positive RetryAfter")
	}
}

// TestRedisRepository_BackendError verifies a backend failure surfaces as an
// error (which the middleware treats as fail-open).
func TestRedisRepository_BackendError(t *testing.T) {
	fs := newFakeScripter()
	fs.failed = true
	repo := NewRedisRepository(fs)

	_, err := repo.Allow(context.Background(), "k", 5, time.Second)
	if err == nil {
		t.Fatal("expected error on backend failure, got nil")
	}
}
