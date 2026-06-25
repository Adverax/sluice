package cache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeCmdable is a minimal redis.Cmdable that emulates GET/SET in memory so the
// RedisRepository adapter can be unit-tested without a live Redis (the real
// client is integration-tested in CARD-011). It embeds redis.Cmdable so the
// large interface is satisfied; only Get and Set — the methods the adapter uses
// — are overridden. Any other method would panic on the nil embedded interface,
// which is intentional: it flags accidental use of an un-faked command.
type fakeCmdable struct {
	redis.Cmdable
	store   map[string][]byte
	lastTTL time.Duration
	failGet bool
	failSet bool
	missKey bool // when true, GET returns redis.Nil (clean miss)
}

func newFakeCmdable() *fakeCmdable {
	return &fakeCmdable{store: make(map[string][]byte)}
}

func (f *fakeCmdable) Get(ctx context.Context, key string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx, "get", key)
	if f.failGet {
		cmd.SetErr(errors.New("redis: connection refused"))
		return cmd
	}
	if f.missKey {
		cmd.SetErr(redis.Nil)
		return cmd
	}
	v, ok := f.store[key]
	if !ok {
		cmd.SetErr(redis.Nil)
		return cmd
	}
	cmd.SetVal(string(v))
	return cmd
}

func (f *fakeCmdable) Set(ctx context.Context, key string, value any, ttl time.Duration) *redis.StatusCmd {
	cmd := redis.NewStatusCmd(ctx, "set", key)
	if f.failSet {
		cmd.SetErr(errors.New("redis: connection refused"))
		return cmd
	}
	f.lastTTL = ttl
	switch v := value.(type) {
	case []byte:
		cp := make([]byte, len(v))
		copy(cp, v)
		f.store[key] = cp
	case string:
		f.store[key] = []byte(v)
	}
	cmd.SetVal("OK")
	return cmd
}

func TestRedisRepository_SetThenGet(t *testing.T) {
	fc := newFakeCmdable()
	repo := NewRedisRepository(fc)
	ctx := context.Background()

	const ttl = 5 * time.Minute
	if err := repo.Set(ctx, "k1", []byte("payload"), ttl); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if fc.lastTTL != ttl {
		t.Fatalf("stored TTL = %s, want %s", fc.lastTTL, ttl)
	}

	val, found, err := repo.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("Get found = false, want true")
	}
	if string(val) != "payload" {
		t.Fatalf("Get value = %q, want %q", val, "payload")
	}
}

func TestRedisRepository_GetMiss(t *testing.T) {
	repo := NewRedisRepository(newFakeCmdable())
	val, found, err := repo.Get(context.Background(), "absent")
	if err != nil {
		t.Fatalf("Get returned error on miss: %v", err)
	}
	if found {
		t.Fatal("Get found = true, want false on miss")
	}
	if val != nil {
		t.Fatalf("Get value = %q, want nil on miss", val)
	}
}

func TestRedisRepository_GetBackendError(t *testing.T) {
	fc := newFakeCmdable()
	fc.failGet = true
	repo := NewRedisRepository(fc)

	_, found, err := repo.Get(context.Background(), "k")
	if err == nil {
		t.Fatal("expected error on backend failure, got nil")
	}
	if found {
		t.Fatal("found must be false on backend error")
	}
}

func TestRedisRepository_SetBackendError(t *testing.T) {
	fc := newFakeCmdable()
	fc.failSet = true
	repo := NewRedisRepository(fc)

	if err := repo.Set(context.Background(), "k", []byte("v"), time.Minute); err == nil {
		t.Fatal("expected error on backend failure, got nil")
	}
}

func TestRedisRepository_SetRejectsNonPositiveTTL(t *testing.T) {
	repo := NewRedisRepository(newFakeCmdable())
	if err := repo.Set(context.Background(), "k", []byte("v"), 0); err == nil {
		t.Fatal("expected error for non-positive TTL, got nil")
	}
}

func TestRedisRepository_KeyPrefix(t *testing.T) {
	fc := newFakeCmdable()
	repo := NewRedisRepository(fc, WithKeyPrefix("resp:"))
	if err := repo.Set(context.Background(), "abc", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, ok := fc.store["resp:abc"]; !ok {
		t.Fatalf("expected key %q in store, have %v", "resp:abc", keys(fc.store))
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
