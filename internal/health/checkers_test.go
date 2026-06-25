package health

import (
	"context"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"
)

// fakeRedis implements RedisPinger so the checker mapping is unit-testable
// without a live Redis (live behaviour is integration-tested in CARD-011).
type fakeRedis struct{ err error }

func (f fakeRedis) Ping(ctx context.Context) *redis.StatusCmd {
	cmd := redis.NewStatusCmd(ctx)
	cmd.SetErr(f.err)
	return cmd
}

// fakePostgres implements PostgresPinger.
type fakePostgres struct{ err error }

func (f fakePostgres) Ping(context.Context) error { return f.err }

func TestRedisChecker(t *testing.T) {
	tests := []struct {
		name    string
		pingErr error
		wantErr bool
	}{
		{name: "healthy", pingErr: nil, wantErr: false},
		{name: "down", pingErr: errors.New("connection refused"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewRedisChecker(fakeRedis{err: tt.pingErr})
			if c.Name() != "redis" {
				t.Errorf("Name() = %q, want redis", c.Name())
			}
			err := c.Check(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("Check() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestPostgresChecker(t *testing.T) {
	tests := []struct {
		name    string
		pingErr error
		wantErr bool
	}{
		{name: "healthy", pingErr: nil, wantErr: false},
		{name: "down", pingErr: errors.New("connection refused"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewPostgresChecker(fakePostgres{err: tt.pingErr})
			if c.Name() != "postgres" {
				t.Errorf("Name() = %q, want postgres", c.Name())
			}
			err := c.Check(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("Check() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}
