// cachetool_test.go tests the refresh_cache and cache_stats tool handlers.
package tools_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/tools"
)

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// newTestCache creates a real *cache.CacheManager backed by a temp directory.
// The caller is responsible for removing the temp dir after the test.
func newTestCache(t *testing.T) (*cache.CacheManager, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "occ-cache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	cm, err := cache.New(dir)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("failed to create cache: %v", err)
	}
	return cm, func() { os.RemoveAll(dir) }
}

// newHandlerWithCache creates a Handler wired with the given CacheManager.
func newHandlerWithCache(cm *cache.CacheManager) *tools.Handler {
	h := tools.New(nil)
	h.SetCache(cm)
	return h
}

// callRefreshCache invokes HandleRefreshCache and decodes the response.
func callRefreshCache(t *testing.T, h *tools.Handler, provider string) map[string]any {
	t.Helper()
	in := tools.RefreshCacheInput{Provider: provider}
	result, _, err := h.HandleRefreshCache(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleRefreshCache returned err: %v", err)
	}
	return decodeResult(t, result)
}

// callCacheStats invokes HandleCacheStats and decodes the response.
func callCacheStats(t *testing.T, h *tools.Handler) map[string]any {
	t.Helper()
	result, _, err := h.HandleCacheStats(context.Background(), nil, tools.CacheStatsInput{})
	if err != nil {
		t.Fatalf("HandleCacheStats returned err: %v", err)
	}
	return decodeResult(t, result)
}

// --------------------------------------------------------------------------
// cache_stats tests
// --------------------------------------------------------------------------

// TestCacheStats_RequiredFields verifies that cache_stats returns the required
// fields: price_entries and db_size_mb (matching the Python output shape).
func TestCacheStats_RequiredFields(t *testing.T) {
	cm, cleanup := newTestCache(t)
	defer cleanup()

	// Pre-populate some entries so price_entries > 0 after seeding.
	cm.Set("aws:compute:us-east-1:test", []byte(`{"test":true}`), 24*time.Hour)
	cm.Set("aws:storage:us-east-1:test", []byte(`{"test":true}`), 24*time.Hour)

	h := newHandlerWithCache(cm)
	resp := callCacheStats(t, h)

	// Must have price_entries field.
	priceEntries, ok := resp["price_entries"]
	if !ok {
		t.Error("cache_stats missing 'price_entries' field")
	}
	pe, _ := priceEntries.(float64) // JSON numbers decode as float64
	if pe < 2 {
		t.Errorf("expected price_entries >= 2, got %v", pe)
	}

	// Must have db_size_mb field.
	dbSizeMB, ok := resp["db_size_mb"]
	if !ok {
		t.Error("cache_stats missing 'db_size_mb' field")
	}
	mb, _ := dbSizeMB.(float64)
	if mb < 0 {
		t.Errorf("expected db_size_mb >= 0, got %v", mb)
	}

	// Must have db_path field.
	dbPath, ok := resp["db_path"]
	if !ok {
		t.Error("cache_stats missing 'db_path' field")
	}
	if dbPath == "" || dbPath == nil {
		t.Error("expected non-empty db_path")
	}
}

// TestCacheStats_ByProviderBreakdown verifies that cache_stats includes a
// per-provider/service breakdown and an as_of age for the most recent write
// (RC3-017).
func TestCacheStats_ByProviderBreakdown(t *testing.T) {
	cm, cleanup := newTestCache(t)
	defer cleanup()

	cm.Set("aws:compute:us-east-1:m5.xlarge", []byte(`{"test":true}`), 24*time.Hour)
	cm.Set("aws:compute:eu-west-1:m5.xlarge", []byte(`{"test":true}`), 24*time.Hour)
	cm.Set("gcp:storage:us-central1:pd-ssd", []byte(`{"test":true}`), 24*time.Hour)

	h := newHandlerWithCache(cm)
	resp := callCacheStats(t, h)

	byProvider, ok := resp["by_provider"].(map[string]any)
	if !ok {
		t.Fatalf("cache_stats missing 'by_provider' map, got %v (%T)", resp["by_provider"], resp["by_provider"])
	}

	aws, ok := byProvider["aws"].(map[string]any)
	if !ok {
		t.Fatalf("by_provider missing 'aws' bucket, got %v", byProvider)
	}
	awsCompute, ok := aws["compute"].(map[string]any)
	if !ok {
		t.Fatalf("by_provider.aws missing 'compute' bucket, got %v", aws)
	}
	if count, _ := awsCompute["entry_count"].(float64); count != 2 {
		t.Errorf("by_provider.aws.compute.entry_count = %v, want 2", awsCompute["entry_count"])
	}
	if _, ok := awsCompute["last_write_at"]; !ok {
		t.Error("by_provider.aws.compute missing 'last_write_at'")
	}
	if _, ok := awsCompute["age_seconds"]; !ok {
		t.Error("by_provider.aws.compute missing 'age_seconds'")
	}

	gcp, ok := byProvider["gcp"].(map[string]any)
	if !ok {
		t.Fatalf("by_provider missing 'gcp' bucket, got %v", byProvider)
	}
	gcpStorage, ok := gcp["storage"].(map[string]any)
	if !ok {
		t.Fatalf("by_provider.gcp missing 'storage' bucket, got %v", gcp)
	}
	if count, _ := gcpStorage["entry_count"].(float64); count != 1 {
		t.Errorf("by_provider.gcp.storage.entry_count = %v, want 1", gcpStorage["entry_count"])
	}

	// Top-level as_of must be present once the cache has entries.
	if _, ok := resp["as_of"]; !ok {
		t.Error("cache_stats missing top-level 'as_of' field once cache is non-empty")
	}
	if _, ok := resp["as_of_age_seconds"]; !ok {
		t.Error("cache_stats missing top-level 'as_of_age_seconds' field once cache is non-empty")
	}
}

// TestCacheStats_EmptyCache verifies that cache_stats works on an empty cache.
func TestCacheStats_EmptyCache(t *testing.T) {
	cm, cleanup := newTestCache(t)
	defer cleanup()

	h := newHandlerWithCache(cm)
	resp := callCacheStats(t, h)

	priceEntries, ok := resp["price_entries"]
	if !ok {
		t.Fatal("cache_stats missing 'price_entries'")
	}
	pe, _ := priceEntries.(float64)
	if pe != 0 {
		t.Errorf("expected 0 entries in empty cache, got %v", pe)
	}
}

// TestCacheStats_NoCache verifies the error when no cache is configured.
func TestCacheStats_NoCache(t *testing.T) {
	h := tools.New(nil) // no cache
	resp := callCacheStats(t, h)

	if _, ok := resp["error"]; !ok {
		t.Error("expected error key when cache is not configured")
	}
}

// --------------------------------------------------------------------------
// refresh_cache tests
// --------------------------------------------------------------------------

// TestRefreshCache_ClearsProvider verifies that refresh_cache(provider="aws")
// deletes all AWS entries and reports prices_deleted > 0.
func TestRefreshCache_ClearsProvider(t *testing.T) {
	cm, cleanup := newTestCache(t)
	defer cleanup()

	// Seed AWS entries.
	cm.Set("aws:compute:us-east-1:t3.small", []byte(`{}`), 24*time.Hour)
	cm.Set("aws:storage:us-east-1:gp3", []byte(`{}`), 24*time.Hour)
	cm.Set("aws:database:us-east-1:rds", []byte(`{}`), 24*time.Hour)

	h := newHandlerWithCache(cm)
	resp := callRefreshCache(t, h, "aws")

	if _, ok := resp["error"]; ok {
		t.Fatalf("expected success, got error: %v", resp["error"])
	}

	deleted, _ := resp["prices_deleted"].(float64)
	if deleted < 1 {
		t.Errorf("expected prices_deleted >= 1, got %v", deleted)
	}

	msg, _ := resp["message"].(string)
	if msg == "" {
		t.Error("expected non-empty message")
	}
}

// TestRefreshCache_DoesNotDeleteOtherProvider verifies that clearing "aws"
// does not remove GCP entries.
func TestRefreshCache_DoesNotDeleteOtherProvider(t *testing.T) {
	cm, cleanup := newTestCache(t)
	defer cleanup()

	// Seed both AWS and GCP entries.
	cm.Set("aws:compute:us-east-1:m5.xlarge", []byte(`{}`), 24*time.Hour)
	cm.Set("gcp:compute:us-central1:n1-standard-4", []byte(`{}`), 24*time.Hour)

	h := newHandlerWithCache(cm)
	callRefreshCache(t, h, "aws")

	// GCP entry must still be present.
	stats := cm.Stats()
	if stats.EntryCount == 0 {
		t.Error("expected GCP entry to survive after clearing AWS")
	}

	// Specifically: verify the GCP key survives.
	val, ok := cm.Get("gcp:compute:us-central1:n1-standard-4")
	if !ok || val == nil {
		t.Error("GCP cache entry was deleted when only AWS was cleared")
	}
}

// TestRefreshCache_EmptyProvider verifies behavior when provider is empty.
// The Go cache uses lazy expiry, so we expect a graceful response (no error).
func TestRefreshCache_EmptyProvider(t *testing.T) {
	cm, cleanup := newTestCache(t)
	defer cleanup()

	h := newHandlerWithCache(cm)
	resp := callRefreshCache(t, h, "")

	if errVal, ok := resp["error"]; ok && errVal != nil {
		t.Errorf("expected no error for empty provider, got: %v", errVal)
	}

	// Response should mention something about the purge / stats.
	msg, _ := resp["message"].(string)
	if msg == "" {
		t.Error("expected non-empty message for empty provider refresh")
	}
}

// TestRefreshCache_NoCache verifies the error when no cache is configured.
func TestRefreshCache_NoCache(t *testing.T) {
	h := tools.New(nil)
	resp := callRefreshCache(t, h, "aws")

	if _, ok := resp["error"]; !ok {
		t.Error("expected error key when cache is not configured")
	}
}
