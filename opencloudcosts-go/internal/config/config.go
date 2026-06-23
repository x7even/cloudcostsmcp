// Package config parses all OCC_* environment variables into a typed Config
// struct. No external library is used — each field is read with os.Getenv and
// converted by a typed helper function.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds every OCC_* setting. Field names mirror the snake_case names
// in config.py; values are the same types and defaults.
type Config struct {
	// General

	// CacheDir is the directory where the cache file is stored.
	// Default: ~/.cache/opencloudcosts  (OCC_CACHE_DIR)
	CacheDir string

	// CacheTTLHours is the TTL for price cache entries.
	// Default: 24  (OCC_CACHE_TTL_HOURS)
	CacheTTLHours int

	// MetadataTTLDays is the TTL for metadata cache entries.
	// Default: 7  (OCC_METADATA_TTL_DAYS)
	MetadataTTLDays int

	// EffectivePriceTTLHours is the TTL for effective-price cache entries.
	// Default: 1  (OCC_EFFECTIVE_PRICE_TTL_HOURS)
	EffectivePriceTTLHours int

	// SpotCacheTTLMinutes is the TTL for spot price cache entries.
	// Default: 5  (OCC_SPOT_CACHE_TTL_MINUTES)
	SpotCacheTTLMinutes int

	// DefaultCurrency is the currency code for pricing output.
	// Default: "USD"  (OCC_DEFAULT_CURRENCY)
	DefaultCurrency string

	// DefaultRegions is the comma-separated list of regions used when none is specified.
	// Default: ["us-east-1","us-west-2"]  (OCC_DEFAULT_REGIONS)
	DefaultRegions []string

	// MaxResults caps the number of results returned by search tools.
	// Default: 20  (OCC_MAX_RESULTS)
	MaxResults int

	// AWS

	// AWSProfile is the AWS CLI profile name. Empty means default chain.
	// Default: ""  (OCC_AWS_PROFILE)
	AWSProfile string

	// AWSRegion is the AWS region used for pricing API calls.
	// Default: "us-east-1"  (OCC_AWS_REGION)
	AWSRegion string

	// AWSEnableCostExplorer opts in to Cost Explorer API calls ($0.01/call).
	// Default: false  (OCC_AWS_ENABLE_COST_EXPLORER)
	AWSEnableCostExplorer bool

	// GCP

	// GCPProjectID is the GCP project ID. Empty means not configured.
	// Default: ""  (OCC_GCP_PROJECT_ID)
	GCPProjectID string

	// GCPBillingDataset is the BigQuery billing dataset name.
	// Default: ""  (OCC_GCP_BILLING_DATASET)
	GCPBillingDataset string

	// GCPAPIKey is the GCP API key for unauthenticated Billing Catalog access.
	// Default: ""  (OCC_GCP_API_KEY)
	GCPAPIKey string

	// GCPBillingAccountID is the GCP billing account ID for contract pricing.
	// Default: ""  (OCC_GCP_BILLING_ACCOUNT_ID)
	GCPBillingAccountID string

	// GCPAccessToken is a short-lived Bearer token (debug / escape hatch only).
	// Default: ""  (OCC_GCP_ACCESS_TOKEN)
	GCPAccessToken string

	// GCPAccessTokenExpiresAt is the ISO-8601 expiry of GCPAccessToken.
	// Default: ""  (OCC_GCP_ACCESS_TOKEN_EXPIRES_AT)
	GCPAccessTokenExpiresAt string

	// GCPServiceAccountJSONB64 is the base64-encoded service account key JSON.
	// Default: ""  (OCC_GCP_SERVICE_ACCOUNT_JSON_B64)
	GCPServiceAccountJSONB64 string

	// GCPServiceAccountJSON is the raw service account key JSON.
	// Default: ""  (OCC_GCP_SERVICE_ACCOUNT_JSON)
	GCPServiceAccountJSON string

	// GCPExternalAccountJSONB64 is the base64-encoded WIF external account JSON.
	// Default: ""  (OCC_GCP_EXTERNAL_ACCOUNT_JSON_B64)
	GCPExternalAccountJSONB64 string

	// GCPExternalAccountJSON is the raw WIF external account JSON.
	// Default: ""  (OCC_GCP_EXTERNAL_ACCOUNT_JSON)
	GCPExternalAccountJSON string

	// HTTP transport

	// HTTPPort is the port for the HTTP transport listener.
	// Default: 8080  (OCC_HTTP_PORT)
	HTTPPort int

	// HTTPHost is the bind address for the HTTP transport listener.
	// Default: "127.0.0.1"  (OCC_HTTP_HOST)
	HTTPHost string

	// APIKey is the optional Bearer token for HTTP transport authentication.
	// Default: ""  (OCC_API_KEY)
	APIKey string

	// Enterprise / operational

	// LogLevel controls the slog log level (debug, info, warn, error).
	// Default: "info"  (OCC_LOG_LEVEL)
	LogLevel string

	// RequestTimeout is the maximum duration for a complete tool call
	// (cache lookup + provider fetch + response serialisation).
	// Default: 60s  (OCC_REQUEST_TIMEOUT)
	RequestTimeout time.Duration

	// ProviderTimeout is the maximum duration for a single outbound
	// provider API call (AWS Pricing API, GCP Billing Catalog, Azure REST).
	// Default: 30s  (OCC_PROVIDER_TIMEOUT)
	ProviderTimeout time.Duration

	// ShutdownTimeout is the maximum duration to wait for in-flight tool
	// calls to complete after SIGTERM is received.
	// Default: 15s  (OCC_SHUTDOWN_TIMEOUT)
	ShutdownTimeout time.Duration

	// RateLimit is the maximum number of HTTP-transport requests per second
	// per client IP. 0 disables rate limiting.
	// Default: 10  (OCC_RATE_LIMIT)
	RateLimit float64
}

// Load reads all OCC_* environment variables and returns a populated Config.
// ~ in OCC_CACHE_DIR is expanded to the user's home directory.
func Load() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("config: cannot determine home directory: %w", err)
	}
	defaultCacheDir := filepath.Join(home, ".cache", "opencloudcosts")

	cfg := &Config{
		CacheDir:               envString("OCC_CACHE_DIR", defaultCacheDir),
		CacheTTLHours:          envInt("OCC_CACHE_TTL_HOURS", 24),
		MetadataTTLDays:        envInt("OCC_METADATA_TTL_DAYS", 7),
		EffectivePriceTTLHours: envInt("OCC_EFFECTIVE_PRICE_TTL_HOURS", 1),
		SpotCacheTTLMinutes:    envInt("OCC_SPOT_CACHE_TTL_MINUTES", 5),
		DefaultCurrency:        envString("OCC_DEFAULT_CURRENCY", "USD"),
		DefaultRegions:         envStringList("OCC_DEFAULT_REGIONS", []string{"us-east-1", "us-west-2"}),
		MaxResults:             envInt("OCC_MAX_RESULTS", 20),

		AWSProfile:            envString("OCC_AWS_PROFILE", ""),
		AWSRegion:             envString("OCC_AWS_REGION", "us-east-1"),
		AWSEnableCostExplorer: envBool("OCC_AWS_ENABLE_COST_EXPLORER", false),

		GCPProjectID:              envString("OCC_GCP_PROJECT_ID", ""),
		GCPBillingDataset:         envString("OCC_GCP_BILLING_DATASET", ""),
		GCPAPIKey:                 envString("OCC_GCP_API_KEY", ""),
		GCPBillingAccountID:       envString("OCC_GCP_BILLING_ACCOUNT_ID", ""),
		GCPAccessToken:            envString("OCC_GCP_ACCESS_TOKEN", ""),
		GCPAccessTokenExpiresAt:   envString("OCC_GCP_ACCESS_TOKEN_EXPIRES_AT", ""),
		GCPServiceAccountJSONB64:  envString("OCC_GCP_SERVICE_ACCOUNT_JSON_B64", ""),
		GCPServiceAccountJSON:     envString("OCC_GCP_SERVICE_ACCOUNT_JSON", ""),
		GCPExternalAccountJSONB64: envString("OCC_GCP_EXTERNAL_ACCOUNT_JSON_B64", ""),
		GCPExternalAccountJSON:    envString("OCC_GCP_EXTERNAL_ACCOUNT_JSON", ""),

		HTTPPort: envInt("OCC_HTTP_PORT", 8080),
		HTTPHost: envString("OCC_HTTP_HOST", "127.0.0.1"),
		APIKey:   envString("OCC_API_KEY", ""),

		LogLevel:        envString("OCC_LOG_LEVEL", "info"),
		RequestTimeout:  envDuration("OCC_REQUEST_TIMEOUT", 60*time.Second),
		ProviderTimeout: envDuration("OCC_PROVIDER_TIMEOUT", 30*time.Second),
		ShutdownTimeout: envDuration("OCC_SHUTDOWN_TIMEOUT", 15*time.Second),
		RateLimit:       float64(envInt("OCC_RATE_LIMIT", 10)),
	}

	// Expand ~ in CacheDir (mirrors Python's expanduser).
	cfg.CacheDir = expandUser(cfg.CacheDir, home)

	return cfg, nil
}

// Validate checks post-load constraints. Returns a multi-error if any are violated.
func (c *Config) Validate() error {
	var errs []string

	if c.CacheTTLHours < 0 {
		errs = append(errs, "OCC_CACHE_TTL_HOURS must be non-negative")
	}
	if c.MetadataTTLDays < 0 {
		errs = append(errs, "OCC_METADATA_TTL_DAYS must be non-negative")
	}
	if c.EffectivePriceTTLHours < 0 {
		errs = append(errs, "OCC_EFFECTIVE_PRICE_TTL_HOURS must be non-negative")
	}
	if c.SpotCacheTTLMinutes < 0 {
		errs = append(errs, "OCC_SPOT_CACHE_TTL_MINUTES must be non-negative")
	}
	if c.MaxResults <= 0 {
		errs = append(errs, "OCC_MAX_RESULTS must be positive")
	}
	if c.DefaultCurrency == "" {
		errs = append(errs, "OCC_DEFAULT_CURRENCY must not be empty")
	}
	if len(c.DefaultRegions) == 0 {
		errs = append(errs, "OCC_DEFAULT_REGIONS must contain at least one region")
	}
	if c.HTTPPort < 1 || c.HTTPPort > 65535 {
		errs = append(errs, fmt.Sprintf("OCC_HTTP_PORT must be 1–65535, got %d", c.HTTPPort))
	}

	if len(errs) == 0 {
		return nil
	}
	return errors.New(strings.Join(errs, "; "))
}

// CacheTTL returns CacheTTLHours as a time.Duration.
func (c *Config) CacheTTL() time.Duration {
	return time.Duration(c.CacheTTLHours) * time.Hour
}

// MetadataTTL returns MetadataTTLDays as a time.Duration.
func (c *Config) MetadataTTL() time.Duration {
	return time.Duration(c.MetadataTTLDays) * 24 * time.Hour
}

// EffectivePriceTTL returns EffectivePriceTTLHours as a time.Duration.
func (c *Config) EffectivePriceTTL() time.Duration {
	return time.Duration(c.EffectivePriceTTLHours) * time.Hour
}

// SpotCacheTTL returns SpotCacheTTLMinutes as a time.Duration.
func (c *Config) SpotCacheTTL() time.Duration {
	return time.Duration(c.SpotCacheTTLMinutes) * time.Minute
}

// --------------------------------------------------------------------------
// Typed env-var helpers — no external library.
// --------------------------------------------------------------------------

// envString returns the value of the named env var, or def if unset/empty.
func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt returns the integer value of the named env var, or def if unset/invalid.
func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// envBool returns the boolean value of the named env var, or def if unset/invalid.
// Accepted true values: "1", "true", "yes" (case-insensitive).
// Accepted false values: "0", "false", "no" (case-insensitive).
func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	}
	return def
}

// envDuration returns the time.Duration value of the named env var (e.g. "30s",
// "5m"), or def if unset/invalid.
func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// envStringList returns the comma-separated values of the named env var as a
// slice, or def if unset/empty. Leading/trailing whitespace around each item is
// trimmed.
func envStringList(key string, def []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	if len(result) == 0 {
		return def
	}
	return result
}

// expandUser replaces a leading "~/" with the home directory path.
func expandUser(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}
