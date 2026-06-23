package config

import (
	"os"
	"testing"
	"time"
)

// setenv sets an env var and returns a cleanup function that restores the original.
func setenv(t *testing.T, key, value string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	if err := os.Setenv(key, value); err != nil {
		t.Fatalf("setenv %s: %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			os.Setenv(key, old) //nolint:errcheck
		} else {
			os.Unsetenv(key) //nolint:errcheck
		}
	})
}

// unsetenv unsets an env var for the duration of the test.
func unsetenv(t *testing.T, key string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	os.Unsetenv(key) //nolint:errcheck
	t.Cleanup(func() {
		if had {
			os.Setenv(key, old) //nolint:errcheck
		}
	})
}

// ---- Load defaults ----------------------------------------------------------

func TestLoad_Defaults(t *testing.T) {
	// Unset all OCC_ vars so defaults are exercised.
	occKeys := []string{
		"OCC_CACHE_DIR", "OCC_CACHE_TTL_HOURS", "OCC_METADATA_TTL_DAYS",
		"OCC_EFFECTIVE_PRICE_TTL_HOURS", "OCC_SPOT_CACHE_TTL_MINUTES",
		"OCC_DEFAULT_CURRENCY", "OCC_DEFAULT_REGIONS", "OCC_MAX_RESULTS",
		"OCC_AWS_PROFILE", "OCC_AWS_REGION", "OCC_AWS_ENABLE_COST_EXPLORER",
		"OCC_GCP_PROJECT_ID", "OCC_GCP_BILLING_DATASET", "OCC_GCP_API_KEY",
		"OCC_GCP_BILLING_ACCOUNT_ID", "OCC_GCP_ACCESS_TOKEN",
		"OCC_GCP_ACCESS_TOKEN_EXPIRES_AT", "OCC_GCP_SERVICE_ACCOUNT_JSON_B64",
		"OCC_GCP_SERVICE_ACCOUNT_JSON", "OCC_GCP_EXTERNAL_ACCOUNT_JSON_B64",
		"OCC_GCP_EXTERNAL_ACCOUNT_JSON",
		"OCC_HTTP_PORT", "OCC_HTTP_HOST", "OCC_API_KEY",
	}
	for _, k := range occKeys {
		unsetenv(t, k)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	home, _ := os.UserHomeDir()

	tests := []struct {
		name string
		got  interface{}
		want interface{}
	}{
		{"CacheTTLHours", cfg.CacheTTLHours, 24},
		{"MetadataTTLDays", cfg.MetadataTTLDays, 7},
		{"EffectivePriceTTLHours", cfg.EffectivePriceTTLHours, 1},
		{"SpotCacheTTLMinutes", cfg.SpotCacheTTLMinutes, 5},
		{"DefaultCurrency", cfg.DefaultCurrency, "USD"},
		{"MaxResults", cfg.MaxResults, 20},
		{"AWSProfile", cfg.AWSProfile, ""},
		{"AWSRegion", cfg.AWSRegion, "us-east-1"},
		{"AWSEnableCostExplorer", cfg.AWSEnableCostExplorer, false},
		{"GCPProjectID", cfg.GCPProjectID, ""},
		{"GCPAPIKey", cfg.GCPAPIKey, ""},
		{"HTTPPort", cfg.HTTPPort, 8080},
		{"HTTPHost", cfg.HTTPHost, "127.0.0.1"},
		{"APIKey", cfg.APIKey, ""},
		{"CacheDir contains home", cfg.CacheDir, home + "/.cache/opencloudcosts"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %v, want %v", tc.got, tc.want)
			}
		})
	}

	// DefaultRegions slice check
	t.Run("DefaultRegions", func(t *testing.T) {
		want := []string{"us-east-1", "us-west-2"}
		if len(cfg.DefaultRegions) != len(want) {
			t.Fatalf("len %d, want %d", len(cfg.DefaultRegions), len(want))
		}
		for i, r := range want {
			if cfg.DefaultRegions[i] != r {
				t.Errorf("[%d] got %q, want %q", i, cfg.DefaultRegions[i], r)
			}
		}
	})
}

// ---- Load from env ----------------------------------------------------------

func TestLoad_FromEnv(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		check func(*Config) interface{}
		want  interface{}
	}{
		{
			name:  "CacheTTLHours",
			key:   "OCC_CACHE_TTL_HOURS",
			value: "48",
			check: func(c *Config) interface{} { return c.CacheTTLHours },
			want:  48,
		},
		{
			name:  "MetadataTTLDays",
			key:   "OCC_METADATA_TTL_DAYS",
			value: "14",
			check: func(c *Config) interface{} { return c.MetadataTTLDays },
			want:  14,
		},
		{
			name:  "SpotCacheTTLMinutes",
			key:   "OCC_SPOT_CACHE_TTL_MINUTES",
			value: "10",
			check: func(c *Config) interface{} { return c.SpotCacheTTLMinutes },
			want:  10,
		},
		{
			name:  "DefaultCurrency",
			key:   "OCC_DEFAULT_CURRENCY",
			value: "EUR",
			check: func(c *Config) interface{} { return c.DefaultCurrency },
			want:  "EUR",
		},
		{
			name:  "MaxResults",
			key:   "OCC_MAX_RESULTS",
			value: "50",
			check: func(c *Config) interface{} { return c.MaxResults },
			want:  50,
		},
		{
			name:  "AWSRegion",
			key:   "OCC_AWS_REGION",
			value: "eu-west-1",
			check: func(c *Config) interface{} { return c.AWSRegion },
			want:  "eu-west-1",
		},
		{
			name:  "AWSEnableCostExplorer true",
			key:   "OCC_AWS_ENABLE_COST_EXPLORER",
			value: "true",
			check: func(c *Config) interface{} { return c.AWSEnableCostExplorer },
			want:  true,
		},
		{
			name:  "AWSEnableCostExplorer 1",
			key:   "OCC_AWS_ENABLE_COST_EXPLORER",
			value: "1",
			check: func(c *Config) interface{} { return c.AWSEnableCostExplorer },
			want:  true,
		},
		{
			name:  "AWSEnableCostExplorer false",
			key:   "OCC_AWS_ENABLE_COST_EXPLORER",
			value: "false",
			check: func(c *Config) interface{} { return c.AWSEnableCostExplorer },
			want:  false,
		},
		{
			name:  "GCPProjectID",
			key:   "OCC_GCP_PROJECT_ID",
			value: "my-project",
			check: func(c *Config) interface{} { return c.GCPProjectID },
			want:  "my-project",
		},
		{
			name:  "GCPAPIKey",
			key:   "OCC_GCP_API_KEY",
			value: "AIza...",
			check: func(c *Config) interface{} { return c.GCPAPIKey },
			want:  "AIza...",
		},
		{
			name:  "HTTPPort",
			key:   "OCC_HTTP_PORT",
			value: "9090",
			check: func(c *Config) interface{} { return c.HTTPPort },
			want:  9090,
		},
		{
			name:  "HTTPHost",
			key:   "OCC_HTTP_HOST",
			value: "0.0.0.0",
			check: func(c *Config) interface{} { return c.HTTPHost },
			want:  "0.0.0.0",
		},
		{
			name:  "APIKey",
			key:   "OCC_API_KEY",
			value: "secret-token",
			check: func(c *Config) interface{} { return c.APIKey },
			want:  "secret-token",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			setenv(t, tc.key, tc.value)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			got := tc.check(cfg)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// ---- DefaultRegions parsing --------------------------------------------------

func TestLoad_DefaultRegions(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{
			name:  "single region",
			value: "eu-west-1",
			want:  []string{"eu-west-1"},
		},
		{
			name:  "multiple regions",
			value: "us-east-1,eu-west-1,ap-southeast-1",
			want:  []string{"us-east-1", "eu-west-1", "ap-southeast-1"},
		},
		{
			name:  "whitespace trimmed",
			value: " us-east-1 , eu-west-1 ",
			want:  []string{"us-east-1", "eu-west-1"},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			setenv(t, "OCC_DEFAULT_REGIONS", tc.value)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			if len(cfg.DefaultRegions) != len(tc.want) {
				t.Fatalf("len %d, want %d: %v", len(cfg.DefaultRegions), len(tc.want), cfg.DefaultRegions)
			}
			for i, r := range tc.want {
				if cfg.DefaultRegions[i] != r {
					t.Errorf("[%d] got %q, want %q", i, cfg.DefaultRegions[i], r)
				}
			}
		})
	}
}

// ---- CacheDir expansion -----------------------------------------------------

func TestLoad_CacheDirExpansion(t *testing.T) {
	home, _ := os.UserHomeDir()

	t.Run("tilde expanded", func(t *testing.T) {
		setenv(t, "OCC_CACHE_DIR", "~/.cache/myapp")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		want := home + "/.cache/myapp"
		if cfg.CacheDir != want {
			t.Errorf("got %q, want %q", cfg.CacheDir, want)
		}
	})

	t.Run("absolute path unchanged", func(t *testing.T) {
		setenv(t, "OCC_CACHE_DIR", "/tmp/occ-cache")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if cfg.CacheDir != "/tmp/occ-cache" {
			t.Errorf("got %q, want %q", cfg.CacheDir, "/tmp/occ-cache")
		}
	})
}

// ---- TTL helpers ------------------------------------------------------------

func TestConfig_TTLHelpers(t *testing.T) {
	cfg := &Config{
		CacheTTLHours:          24,
		MetadataTTLDays:        7,
		EffectivePriceTTLHours: 1,
		SpotCacheTTLMinutes:    5,
	}

	tests := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"CacheTTL", cfg.CacheTTL(), 24 * time.Hour},
		{"MetadataTTL", cfg.MetadataTTL(), 7 * 24 * time.Hour},
		{"EffectivePriceTTL", cfg.EffectivePriceTTL(), time.Hour},
		{"SpotCacheTTL", cfg.SpotCacheTTL(), 5 * time.Minute},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %v, want %v", tc.got, tc.want)
			}
		})
	}
}

// ---- Validate ---------------------------------------------------------------

func TestConfig_Validate(t *testing.T) {
	good := func() *Config {
		return &Config{
			CacheTTLHours:          24,
			MetadataTTLDays:        7,
			EffectivePriceTTLHours: 1,
			SpotCacheTTLMinutes:    5,
			DefaultCurrency:        "USD",
			DefaultRegions:         []string{"us-east-1"},
			MaxResults:             20,
			HTTPPort:               8080,
		}
	}

	t.Run("valid config passes", func(t *testing.T) {
		if err := good().Validate(); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"negative CacheTTLHours", func(c *Config) { c.CacheTTLHours = -1 }},
		{"negative MetadataTTLDays", func(c *Config) { c.MetadataTTLDays = -1 }},
		{"negative EffectivePriceTTLHours", func(c *Config) { c.EffectivePriceTTLHours = -1 }},
		{"negative SpotCacheTTLMinutes", func(c *Config) { c.SpotCacheTTLMinutes = -1 }},
		{"zero MaxResults", func(c *Config) { c.MaxResults = 0 }},
		{"negative MaxResults", func(c *Config) { c.MaxResults = -1 }},
		{"empty DefaultCurrency", func(c *Config) { c.DefaultCurrency = "" }},
		{"empty DefaultRegions", func(c *Config) { c.DefaultRegions = nil }},
		{"port zero", func(c *Config) { c.HTTPPort = 0 }},
		{"port too high", func(c *Config) { c.HTTPPort = 65536 }},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := good()
			tc.mutate(c)
			if err := c.Validate(); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

// ---- envBool helper ---------------------------------------------------------

func TestEnvBool(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"1", true},
		{"true", true},
		{"True", true},
		{"TRUE", true},
		{"yes", true},
		{"YES", true},
		{"0", false},
		{"false", false},
		{"False", false},
		{"no", false},
		{"NO", false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.value, func(t *testing.T) {
			setenv(t, "OCC_TEST_BOOL", tc.value)
			got := envBool("OCC_TEST_BOOL", !tc.want) // default is opposite to catch pass-through
			if got != tc.want {
				t.Errorf("envBool(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// ---- envInt invalid ---------------------------------------------------------

func TestEnvInt_Invalid(t *testing.T) {
	setenv(t, "OCC_TEST_INT", "not-a-number")
	got := envInt("OCC_TEST_INT", 42)
	if got != 42 {
		t.Errorf("expected default 42, got %d", got)
	}
}

// ---- envDuration helper -----------------------------------------------------

func TestEnvDuration(t *testing.T) {
	tests := []struct {
		name  string
		value string
		def   time.Duration
		want  time.Duration
	}{
		{"30s parsed", "30s", time.Minute, 30 * time.Second},
		{"5m parsed", "5m", time.Second, 5 * time.Minute},
		{"2h parsed", "2h", time.Second, 2 * time.Hour},
		{"invalid falls back to default", "not-a-duration", time.Minute, time.Minute},
		{"empty falls back to default", "", 10 * time.Second, 10 * time.Second},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.value == "" {
				unsetenv(t, "OCC_TEST_DUR")
			} else {
				setenv(t, "OCC_TEST_DUR", tc.value)
			}
			got := envDuration("OCC_TEST_DUR", tc.def)
			if got != tc.want {
				t.Errorf("envDuration(%q, %v) = %v, want %v", tc.value, tc.def, got, tc.want)
			}
		})
	}
}
