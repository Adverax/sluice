package config

import (
	"testing"
	"time"
)

// TestConfig_AllBoundariesHaveTimeouts covers AC-045 (NFR-004): every boundary
// timeout in a freshly-loaded (default) configuration is strictly > 0.
func TestConfig_AllBoundariesHaveTimeouts(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	boundaries := []struct {
		name  string
		value time.Duration
	}{
		{"http.Server ReadTimeout", cfg.Server.ReadTimeout},
		{"http.Server WriteTimeout", cfg.Server.WriteTimeout},
		{"http.Server IdleTimeout", cfg.Server.IdleTimeout},
		{"upstream http.Client Timeout", cfg.Upstream.Timeout},
		{"Redis DialTimeout", cfg.Redis.DialTimeout},
		{"Redis ReadTimeout", cfg.Redis.ReadTimeout},
		{"Postgres AcquireTimeout", cfg.Postgres.AcquireTimeout},
	}
	for _, b := range boundaries {
		if b.value <= 0 {
			t.Errorf("%s must be > 0, got %s", b.name, b.value)
		}
	}
}

func TestConfig_ValidateRejectsZeroTimeout(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{
			name:    "valid defaults",
			mutate:  func(*Config) {},
			wantErr: false,
		},
		{
			name:    "zero read timeout",
			mutate:  func(c *Config) { c.Server.ReadTimeout = 0 },
			wantErr: true,
		},
		{
			name:    "negative upstream timeout",
			mutate:  func(c *Config) { c.Upstream.Timeout = -1 },
			wantErr: true,
		},
		{
			name:    "zero redis dial timeout",
			mutate:  func(c *Config) { c.Redis.DialTimeout = 0 },
			wantErr: true,
		},
		{
			name:    "zero postgres acquire timeout",
			mutate:  func(c *Config) { c.Postgres.AcquireTimeout = 0 },
			wantErr: true,
		},
		{
			name:    "empty addr",
			mutate:  func(c *Config) { c.Server.Addr = "" },
			wantErr: true,
		},
		{
			name:    "bad log format",
			mutate:  func(c *Config) { c.Logging.Format = "xml" },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() returned error: %v", err)
			}
			tt.mutate(cfg)
			err = cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestConfig_EnvOverride(t *testing.T) {
	t.Setenv("GATEWAY_ADDR", ":9999")
	t.Setenv("GATEWAY_READ_TIMEOUT", "7s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.Server.Addr != ":9999" {
		t.Errorf("Addr = %q, want :9999", cfg.Server.Addr)
	}
	if cfg.Server.ReadTimeout != 7*time.Second {
		t.Errorf("ReadTimeout = %s, want 7s", cfg.Server.ReadTimeout)
	}
}
