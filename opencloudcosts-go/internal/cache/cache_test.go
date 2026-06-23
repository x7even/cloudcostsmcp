package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newTestCache creates a CacheManager rooted at a test-scoped temp directory.
// Using t.TempDir() ensures isolation between tests and cleanup after each run.
func newTestCache(t *testing.T) *CacheManager {
	t.Helper()
	cm, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	return cm
}

// jsonBytes is a helper that marshals v to JSON or fails the test.
func jsonBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

// TestGetMiss verifies that Get returns a miss on an empty cache.
func TestGetMiss(t *testing.T) {
	cm := newTestCache(t)
	val, ok := cm.Get("aws:compute:us-east-1")
	if ok {
		t.Errorf("Get() on empty cache: got ok=true, want false")
	}
	if val != nil {
		t.Errorf("Get() on empty cache: got non-nil value %v", val)
	}
}

// TestSetAndGetHit verifies a stored value is retrievable.
func TestSetAndGetHit(t *testing.T) {
	cm := newTestCache(t)
	payload := jsonBytes(t, map[string]string{"price": "0.192", "unit": "per_hour"})

	cm.Set("aws:compute:us-east-1", payload, DefaultPriceTTL)

	got, ok := cm.Get("aws:compute:us-east-1")
	if !ok {
		t.Fatal("Get() after Set(): got ok=false, want true")
	}
	if string(got) != string(payload) {
		t.Errorf("Get() value mismatch:\n  got  %s\n  want %s", got, payload)
	}
}

// TestTTLExpiry verifies that an entry with a very short TTL becomes a miss
// once the TTL has elapsed.
func TestTTLExpiry(t *testing.T) {
	cm := newTestCache(t)
	payload := jsonBytes(t, "some value")

	// Set with a 1ms TTL.
	cm.Set("aws:compute:expire-me", payload, 1*time.Millisecond)

	// Sleep well past the TTL.
	time.Sleep(20 * time.Millisecond)

	_, ok := cm.Get("aws:compute:expire-me")
	if ok {
		t.Error("Get() after TTL expiry: got ok=true, want false")
	}
}

// TestClearProviderIsolation verifies that ClearProvider removes only entries
// with the matching prefix and leaves other providers untouched.
func TestClearProviderIsolation(t *testing.T) {
	cm := newTestCache(t)

	awsVal := jsonBytes(t, "aws-price")
	gcpVal := jsonBytes(t, "gcp-price")
	azureVal := jsonBytes(t, "azure-price")

	cm.Set("aws:compute:us-east-1", awsVal, DefaultPriceTTL)
	cm.Set("aws:storage:us-east-1", awsVal, DefaultPriceTTL)
	cm.Set("gcp:compute:us-east1", gcpVal, DefaultPriceTTL)
	cm.Set("azure:compute:eastus", azureVal, DefaultPriceTTL)

	deleted := cm.ClearProvider("aws")
	if deleted != 2 {
		t.Errorf("ClearProvider(aws) deleted %d entries, want 2", deleted)
	}

	// AWS entries must be gone.
	if _, ok := cm.Get("aws:compute:us-east-1"); ok {
		t.Error("aws:compute:us-east-1 still present after ClearProvider(aws)")
	}
	if _, ok := cm.Get("aws:storage:us-east-1"); ok {
		t.Error("aws:storage:us-east-1 still present after ClearProvider(aws)")
	}

	// GCP and Azure entries must survive.
	if _, ok := cm.Get("gcp:compute:us-east1"); !ok {
		t.Error("gcp:compute:us-east1 missing after ClearProvider(aws)")
	}
	if _, ok := cm.Get("azure:compute:eastus"); !ok {
		t.Error("azure:compute:eastus missing after ClearProvider(aws)")
	}
}

// TestCorruptJSONOnStartup verifies that a corrupt JSON file at startup causes
// the cache to start empty and does not crash.
func TestCorruptJSONOnStartup(t *testing.T) {
	dir := t.TempDir()

	// Write garbage bytes to the cache file before creating the manager.
	cachePath := filepath.Join(dir, "cache.json")
	if err := os.WriteFile(cachePath, []byte("NOT VALID JSON {{{"), 0o600); err != nil {
		t.Fatalf("failed to write corrupt cache file: %v", err)
	}

	// New() must not return an error — it logs and starts empty.
	cm, err := New(dir)
	if err != nil {
		t.Fatalf("New() with corrupt file returned error: %v", err)
	}

	// Cache should be empty.
	if _, ok := cm.Get("any:key"); ok {
		t.Error("Get() on corrupt-start cache: got ok=true, want false")
	}

	// Should be fully usable after the bad start.
	payload := jsonBytes(t, "recovered value")
	cm.Set("aws:test", payload, DefaultPriceTTL)
	if _, ok := cm.Get("aws:test"); !ok {
		t.Error("Set+Get after corrupt start: got miss, want hit")
	}
}

// TestStats verifies that Stats returns a non-empty file path and a correct
// entry count reflecting the current state of the cache.
func TestStats(t *testing.T) {
	cm := newTestCache(t)

	// Empty cache.
	stats := cm.Stats()
	if stats.FilePath == "" {
		t.Error("Stats().FilePath is empty")
	}
	if stats.EntryCount != 0 {
		t.Errorf("Stats().EntryCount = %d on empty cache, want 0", stats.EntryCount)
	}

	// After setting two entries.
	cm.Set("aws:compute:us-east-1", jsonBytes(t, "v1"), DefaultPriceTTL)
	cm.Set("gcp:compute:us-east1", jsonBytes(t, "v2"), DefaultPriceTTL)

	stats = cm.Stats()
	if stats.EntryCount != 2 {
		t.Errorf("Stats().EntryCount = %d after two Sets, want 2", stats.EntryCount)
	}
	// File should have been flushed so size is non-zero.
	if stats.FileSizeBytes == 0 {
		t.Error("Stats().FileSizeBytes = 0 after write, expected non-zero")
	}
}

// TestConcurrentReads verifies that multiple goroutines calling Get
// simultaneously do not trigger the race detector.
func TestConcurrentReads(t *testing.T) {
	cm := newTestCache(t)
	payload := jsonBytes(t, "shared value")
	cm.Set("shared:key", payload, DefaultPriceTTL)

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()
			val, ok := cm.Get("shared:key")
			if !ok {
				t.Errorf("concurrent Get: got miss, want hit")
			}
			if string(val) != string(payload) {
				t.Errorf("concurrent Get: value mismatch: got %s", val)
			}
		}()
	}

	wg.Wait()
}

// TestPersistenceRoundTrip verifies that entries written by one CacheManager
// instance are visible to a second instance pointed at the same directory.
func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()

	cm1, err := New(dir)
	if err != nil {
		t.Fatalf("New(dir) first instance: %v", err)
	}
	payload := jsonBytes(t, map[string]string{"price": "0.192"})
	cm1.Set("aws:compute:us-east-1", payload, DefaultPriceTTL)
	cm1.Close()

	// Create a new CacheManager pointing at the same directory — it must load
	// the previously flushed entry.
	cm2, err := New(dir)
	if err != nil {
		t.Fatalf("New(dir) second instance: %v", err)
	}
	got, ok := cm2.Get("aws:compute:us-east-1")
	if !ok {
		t.Fatal("persistence round-trip: Get() on second instance: miss, want hit")
	}
	if string(got) != string(payload) {
		t.Errorf("persistence round-trip: value mismatch:\n  got  %s\n  want %s", got, payload)
	}
}

// TestGetMetadataAndSetMetadata verifies the metadata-flavoured helpers.
func TestGetMetadataAndSetMetadata(t *testing.T) {
	cm := newTestCache(t)

	// Miss on empty.
	if _, ok := cm.GetMetadata("gcp:regions"); ok {
		t.Error("GetMetadata() on empty cache: got hit, want miss")
	}

	regions := jsonBytes(t, []string{"us-east1", "us-west1"})
	cm.SetMetadata("gcp:regions", regions, DefaultMetadataTTL)

	got, ok := cm.GetMetadata("gcp:regions")
	if !ok {
		t.Fatal("GetMetadata() after SetMetadata(): got miss, want hit")
	}
	if string(got) != string(regions) {
		t.Errorf("GetMetadata() value mismatch:\n  got  %s\n  want %s", got, regions)
	}
}

// TestConcurrentMixedAccess stress-tests the RWMutex upgrade path by running
// concurrent Get (hits), Get (expiring entries), and Set calls simultaneously.
// This test is designed to trigger the race detector if the lock-upgrade logic
// in Get is implemented incorrectly.
func TestConcurrentMixedAccess(t *testing.T) {
	cm := newTestCache(t)
	payload := jsonBytes(t, "value")

	// Seed a key that expires very quickly.
	cm.Set("expiring:key", payload, 5*time.Millisecond)
	// Seed a key that lives long.
	cm.Set("stable:key", payload, DefaultPriceTTL)

	var wg sync.WaitGroup
	const goroutines = 40

	// Goroutines that read the stable key (hit path — exercises RLock).
	for i := range goroutines / 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cm.Get("stable:key")
		}(i)
	}

	// Goroutines that read the expiring key (may trigger lock-upgrade path).
	for i := range goroutines / 4 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			time.Sleep(10 * time.Millisecond) // let TTL elapse first
			cm.Get("expiring:key")
		}(i)
	}

	// Goroutines that write (exercises full Lock + flush).
	for i := range goroutines / 4 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cm.Set("dynamic:key", jsonBytes(t, i), DefaultPriceTTL)
		}(i)
	}

	wg.Wait()
}

// TestCacheManager_ExpiredEntryReturnsNotFound verifies that after a TTL expires
// Get returns (nil, false) — i.e. not stale data, just a clean not-found.
func TestCacheManager_ExpiredEntryReturnsNotFound(t *testing.T) {
	cm := newTestCache(t)
	payload := jsonBytes(t, map[string]string{"price": "1.234", "unit": "per_hour"})

	// Set with a 1 ms TTL so it expires almost immediately.
	cm.Set("aws:compute:expire-check", payload, 1*time.Millisecond)

	// Confirm the entry is present before it expires.
	if _, ok := cm.Get("aws:compute:expire-check"); !ok {
		t.Fatal("Get() immediately after Set(): got miss, want hit")
	}

	// Wait for the TTL to elapse.
	time.Sleep(20 * time.Millisecond)

	got, ok := cm.Get("aws:compute:expire-check")
	if ok {
		t.Error("Get() after TTL expiry: got ok=true, want false")
	}
	if got != nil {
		t.Errorf("Get() after TTL expiry: got non-nil stale value %s, want nil", got)
	}
}

// TestClearProviderNoFlushOnZeroDeletes verifies that ClearProvider on a prefix
// with no matching keys returns 0 and does not panic.
func TestClearProviderNoMatchingKeys(t *testing.T) {
	cm := newTestCache(t)
	cm.Set("aws:compute:us-east-1", jsonBytes(t, "v"), DefaultPriceTTL)

	deleted := cm.ClearProvider("azure") // no azure keys exist
	if deleted != 0 {
		t.Errorf("ClearProvider(azure) on cache with only aws keys: deleted %d, want 0", deleted)
	}

	// AWS key must still be present.
	if _, ok := cm.Get("aws:compute:us-east-1"); !ok {
		t.Error("aws key missing after ClearProvider for unrelated provider")
	}
}

// TestCacheManager_ConcurrentWrites verifies that concurrent writes to different
// keys do not corrupt either entry — each key must hold exactly the value that
// was written to it, with no data from another writer leaking through.
func TestCacheManager_ConcurrentWrites(t *testing.T) {
	cm := newTestCache(t)

	type kv struct {
		key   string
		value []byte
	}

	// Build a set of distinct key/value pairs — one per goroutine.
	const writers = 40
	pairs := make([]kv, writers)
	for i := range writers {
		pairs[i] = kv{
			key:   fmt.Sprintf("provider%d:compute:us-east-1", i),
			value: jsonBytes(t, map[string]any{"writer": i, "price": float64(i) * 0.01}),
		}
	}

	// Launch all writes concurrently.
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := range writers {
		go func(i int) {
			defer wg.Done()
			cm.Set(pairs[i].key, pairs[i].value, DefaultPriceTTL)
		}(i)
	}
	wg.Wait()

	// Every key must be readable and hold exactly its written value.
	for i, p := range pairs {
		got, ok := cm.Get(p.key)
		if !ok {
			t.Errorf("writer %d: Get(%q) after concurrent Set: miss, want hit", i, p.key)
			continue
		}
		if string(got) != string(p.value) {
			t.Errorf("writer %d: value corruption:\n  got  %s\n  want %s", i, got, p.value)
		}
	}
}
