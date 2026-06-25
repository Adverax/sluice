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
		{"HealthCheckTimeout", cfg.HealthCheckTimeout},
		{"shutdown.HookTimeout", cfg.Shutdown.HookTimeout},
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

// TestConfig_Load_RejectsBadEnvTimeout covers NFR-004 (fail-loud): if a
// duration env var is SET but malformed or <= 0, Load must return an error
// rather than silently falling back to the default.
func TestConfig_Load_RejectsBadEnvTimeout(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr bool
	}{
		{
			name:    "malformed read timeout",
			key:     "GATEWAY_READ_TIMEOUT",
			value:   "garbage",
			wantErr: true,
		},
		{
			name:    "zero read timeout",
			key:     "GATEWAY_READ_TIMEOUT",
			value:   "0s",
			wantErr: true,
		},
		{
			name:    "negative write timeout",
			key:     "GATEWAY_WRITE_TIMEOUT",
			value:   "-5s",
			wantErr: true,
		},
		{
			name:    "malformed shutdown timeout",
			key:     "GATEWAY_SHUTDOWN_TIMEOUT",
			value:   "30x",
			wantErr: true,
		},
		{
			name:    "valid read timeout still loads",
			key:     "GATEWAY_READ_TIMEOUT",
			value:   "3s",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			_, err := Load()
			if (err != nil) != tt.wantErr {
				t.Errorf("Load() error = %v, wantErr = %v", err, tt.wantErr)
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

// TestConfig_ResilienceDefaults asserts the FR-006/FR-007 (ADR-0002) defaults.
func TestConfig_ResilienceDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.Retry.MaxAttempts != 3 {
		t.Errorf("Retry.MaxAttempts = %d, want 3", cfg.Retry.MaxAttempts)
	}
	if cfg.Breaker.Interval != 10*time.Second {
		t.Errorf("Breaker.Interval = %s, want 10s", cfg.Breaker.Interval)
	}
	if cfg.Breaker.Timeout != 60*time.Second {
		t.Errorf("Breaker.Timeout = %s, want 60s", cfg.Breaker.Timeout)
	}
	if cfg.Breaker.MaxRequests != 5 {
		t.Errorf("Breaker.MaxRequests = %d, want 5", cfg.Breaker.MaxRequests)
	}
	if cfg.Breaker.MinRequests != 10 {
		t.Errorf("Breaker.MinRequests = %d, want 10", cfg.Breaker.MinRequests)
	}
	if cfg.Breaker.FailureRatio != 0.5 {
		t.Errorf("Breaker.FailureRatio = %g, want 0.5", cfg.Breaker.FailureRatio)
	}
}

// TestConfig_ResilienceEnvOverride covers the new GATEWAY_RETRY_*/GATEWAY_BREAKER_*
// knobs.
func TestConfig_ResilienceEnvOverride(t *testing.T) {
	t.Setenv("GATEWAY_RETRY_MAX_ATTEMPTS", "5")
	t.Setenv("GATEWAY_RETRY_BASE_DELAY", "100ms")
	t.Setenv("GATEWAY_RETRY_JITTER", "0.25")
	t.Setenv("GATEWAY_BREAKER_TIMEOUT", "30s")
	t.Setenv("GATEWAY_BREAKER_FAILURE_RATIO", "0.75")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.Retry.MaxAttempts != 5 {
		t.Errorf("Retry.MaxAttempts = %d, want 5", cfg.Retry.MaxAttempts)
	}
	if cfg.Retry.BaseDelay != 100*time.Millisecond {
		t.Errorf("Retry.BaseDelay = %s, want 100ms", cfg.Retry.BaseDelay)
	}
	if cfg.Retry.Jitter != 0.25 {
		t.Errorf("Retry.Jitter = %g, want 0.25", cfg.Retry.Jitter)
	}
	if cfg.Breaker.Timeout != 30*time.Second {
		t.Errorf("Breaker.Timeout = %s, want 30s", cfg.Breaker.Timeout)
	}
	if cfg.Breaker.FailureRatio != 0.75 {
		t.Errorf("Breaker.FailureRatio = %g, want 0.75", cfg.Breaker.FailureRatio)
	}
}

// TestConfig_Load_RejectsBadResilienceEnv covers NFR-004 (fail-loud) for the
// integer/float resilience knobs: if a value is SET but malformed or negative,
// Load must return an error rather than silently falling back.
func TestConfig_Load_RejectsBadResilienceEnv(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr bool
	}{
		// --- malformed (unparseable) values ---
		{
			name:    "malformed GATEWAY_RETRY_MAX_ATTEMPTS",
			key:     "GATEWAY_RETRY_MAX_ATTEMPTS",
			value:   "three",
			wantErr: true,
		},
		{
			name:    "malformed GATEWAY_RETRY_JITTER",
			key:     "GATEWAY_RETRY_JITTER",
			value:   "half",
			wantErr: true,
		},
		{
			name:    "malformed GATEWAY_BREAKER_FAILURE_RATIO",
			key:     "GATEWAY_BREAKER_FAILURE_RATIO",
			value:   "fifty_percent",
			wantErr: true,
		},
		{
			name:    "malformed GATEWAY_BREAKER_MAX_REQUESTS",
			key:     "GATEWAY_BREAKER_MAX_REQUESTS",
			value:   "five",
			wantErr: true,
		},
		{
			name:    "malformed GATEWAY_BREAKER_MIN_REQUESTS",
			key:     "GATEWAY_BREAKER_MIN_REQUESTS",
			value:   "ten",
			wantErr: true,
		},
		// --- negative / non-positive values for count fields (would wrap to ~4e9) ---
		{
			name:    "negative GATEWAY_RETRY_MAX_ATTEMPTS",
			key:     "GATEWAY_RETRY_MAX_ATTEMPTS",
			value:   "-1",
			wantErr: true,
		},
		{
			name:    "zero GATEWAY_RETRY_MAX_ATTEMPTS",
			key:     "GATEWAY_RETRY_MAX_ATTEMPTS",
			value:   "0",
			wantErr: true,
		},
		{
			name:    "negative GATEWAY_BREAKER_MAX_REQUESTS",
			key:     "GATEWAY_BREAKER_MAX_REQUESTS",
			value:   "-5",
			wantErr: true,
		},
		{
			name:    "negative GATEWAY_BREAKER_MIN_REQUESTS",
			key:     "GATEWAY_BREAKER_MIN_REQUESTS",
			value:   "-10",
			wantErr: true,
		},
		// --- valid overrides must still load cleanly ---
		{
			name:    "valid GATEWAY_RETRY_MAX_ATTEMPTS",
			key:     "GATEWAY_RETRY_MAX_ATTEMPTS",
			value:   "5",
			wantErr: false,
		},
		{
			name:    "valid GATEWAY_BREAKER_FAILURE_RATIO",
			key:     "GATEWAY_BREAKER_FAILURE_RATIO",
			value:   "0.75",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			_, err := Load()
			if (err != nil) != tt.wantErr {
				t.Errorf("Load() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

// TestConfig_ResilienceValidation covers the new validation rules.
func TestConfig_ResilienceValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"zero retry attempts", func(c *Config) { c.Retry.MaxAttempts = 0 }, true},
		{"maxdelay below basedelay", func(c *Config) { c.Retry.MaxDelay = time.Nanosecond }, true},
		{"jitter out of range", func(c *Config) { c.Retry.Jitter = 1.5 }, true},
		{"failure ratio zero", func(c *Config) { c.Breaker.FailureRatio = 0 }, true},
		{"failure ratio above one", func(c *Config) { c.Breaker.FailureRatio = 2 }, true},
		{"zero min requests", func(c *Config) { c.Breaker.MinRequests = 0 }, true},
		{"zero max requests", func(c *Config) { c.Breaker.MaxRequests = 0 }, true},
		{"zero breaker timeout", func(c *Config) { c.Breaker.Timeout = 0 }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() returned error: %v", err)
			}
			tt.mutate(cfg)
			if (cfg.Validate() != nil) != tt.wantErr {
				t.Errorf("Validate() wantErr = %v", tt.wantErr)
			}
		})
	}
}
