// Package cache provides an in-memory cache with JSON file persistence.
//
// CacheManager maintains a map[string]cacheEntry protected by sync.RWMutex.
// On startup it loads from a JSON file; on every write it flushes to a temp
// file then os.Rename (atomic).
//
// Get uses RLock for the common hit path.  When an entry is found to be expired
// it re-acquires a full Lock, re-checks (a concurrent Set may have refreshed it),
// then deletes.  Set/ClearProvider/Close always hold a full Lock.
package cache

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TTL defaults — these match the Python implementation.
// The env-var overrides (OCC_CACHE_TTL_HOURS, OCC_METADATA_TTL_DAYS,
// OCC_EFFECTIVE_PRICE_TTL_HOURS) are owned by the config package (Agent 1A);
// callers pass the resolved TTL to Set/SetMetadata so this package stays
// independent of env parsing.
const (
	DefaultPriceTTL          = 24 * time.Hour
	DefaultMetadataTTL       = 7 * 24 * time.Hour
	DefaultEffectivePriceTTL = 1 * time.Hour
)

// cacheEntry holds a cached value alongside its provenance.
type cacheEntry struct {
	Value     []byte        `json:"value"`
	FetchedAt time.Time     `json:"fetched_at"`
	TTL       time.Duration `json:"ttl"`
}

// isExpired reports whether this entry's TTL has elapsed.
func (e cacheEntry) isExpired() bool {
	return time.Since(e.FetchedAt) > e.TTL
}

// persistedMap is the on-disk representation of the full cache.
type persistedMap struct {
	Entries map[string]cacheEntry `json:"entries"`
}

// ProviderServiceStats holds the entry count and most recent write time for
// one provider/service bucket, as surfaced in CacheStats.ByProvider.
type ProviderServiceStats struct {
	EntryCount  int       `json:"entry_count"`
	LastWriteAt time.Time `json:"last_write_at"`
}

// CacheStats is returned by Stats().
type CacheStats struct {
	EntryCount    int    `json:"entry_count"`
	FilePath      string `json:"file_path"`
	FileSizeBytes int64  `json:"file_size_bytes"`

	// AsOf is the most recent FetchedAt across every entry in the cache
	// (the zero time if the cache is empty). Callers can derive an age by
	// comparing against time.Now().
	AsOf time.Time

	// ByProvider breaks entry counts and last-write timestamps down by
	// provider, then by service. The service key is the second colon-
	// separated segment of the cache key (e.g. "aws:compute:..." buckets
	// under provider "aws", service "compute"); keys that do not follow
	// this convention bucket under service "unknown".
	ByProvider map[string]map[string]ProviderServiceStats
}

// splitCacheKey extracts the provider and service buckets from a cache key.
// Every cache key written by the provider packages follows the convention
// "<provider>:<service>:...", e.g. "aws:compute:us-east-1:m5.xlarge:Linux:on_demand"
// or "azure:sql:eastus:tier=GeneralPurpose". Keys that do not have at least
// two colon-separated segments fall back to "unknown" for the missing part(s)
// so Stats() never panics or drops an entry on an unexpected key shape.
func splitCacheKey(key string) (provider, service string) {
	parts := strings.SplitN(key, ":", 3)
	provider = "unknown"
	service = "unknown"
	if len(parts) > 0 && parts[0] != "" {
		provider = parts[0]
	}
	if len(parts) > 1 && parts[1] != "" {
		service = parts[1]
	}
	return provider, service
}

// CacheManager is an in-memory key/value store with TTL and JSON persistence.
// All exported methods are safe for concurrent use.
type CacheManager struct {
	mu       sync.RWMutex
	entries  map[string]cacheEntry
	filePath string
}

// DefaultCacheDir returns the OS-appropriate user cache directory for opencloudcosts.
// On Linux this is ~/.cache/opencloudcosts; on macOS ~/Library/Caches/opencloudcosts.
func DefaultCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("cache: cannot determine user cache directory: %w", err)
	}
	return filepath.Join(base, "opencloudcosts"), nil
}

// New creates a CacheManager rooted at cacheDir/cache.json.
// If the file exists and is valid JSON it is loaded; corrupt or missing files
// start with an empty cache (no error is returned in either case — the server
// must not crash due to a cold or corrupt cache).
func New(cacheDir string) (*CacheManager, error) {
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("cache: cannot create cache directory %s: %w", cacheDir, err)
	}

	filePath := filepath.Join(cacheDir, "cache.json")

	cm := &CacheManager{
		entries:  make(map[string]cacheEntry),
		filePath: filePath,
	}

	if err := cm.loadFromDisk(); err != nil {
		// Corrupt or unreadable — log and start empty. Do not propagate.
		slog.Error("cache: failed to load from disk, starting empty", "path", filePath, "error", err)
	}

	return cm, nil
}

// loadFromDisk reads the JSON file and populates cm.entries.
// Errors are returned so the caller can decide to log and continue.
func (cm *CacheManager) loadFromDisk() error {
	data, err := os.ReadFile(cm.filePath)
	if os.IsNotExist(err) {
		// First run — not an error.
		return nil
	}
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	var pm persistedMap
	if err := json.Unmarshal(data, &pm); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	if pm.Entries != nil {
		cm.entries = pm.Entries
	}
	return nil
}

// flushLocked writes the current in-memory map to disk atomically.
// The caller MUST hold cm.mu before calling this function.
func (cm *CacheManager) flushLocked() {
	pm := persistedMap{Entries: cm.entries}
	data, err := json.Marshal(pm)
	if err != nil {
		slog.Error("cache: failed to marshal cache", "error", err)
		return
	}

	// Write to a temp file in the same directory to ensure same filesystem for Rename.
	cacheDir := filepath.Dir(cm.filePath)
	tmp, err := os.CreateTemp(cacheDir, "cache-*.json.tmp")
	if err != nil {
		slog.Error("cache: failed to create temp file", "error", err)
		return
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		slog.Error("cache: failed to write temp file", "error", err)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		slog.Error("cache: failed to close temp file", "error", err)
		return
	}
	if err := os.Rename(tmpName, cm.filePath); err != nil {
		_ = os.Remove(tmpName)
		slog.Error("cache: failed to rename temp file", "path", cm.filePath, "error", err)
		return
	}
}

// Get retrieves the value for key. Returns (value, true) on hit,
// or (nil, false) on miss or expiry. Expired entries are lazily deleted.
//
// The common hit path holds only RLock (allowing concurrent readers).
// When expiry is detected the lock is upgraded: RLock is released, Lock is
// acquired, and the entry is re-checked (a concurrent Set may have refreshed
// it) before deletion.  No lock is ever held while the other is also held.
func (cm *CacheManager) Get(key string) ([]byte, bool) {
	cm.mu.RLock()
	entry, ok := cm.entries[key]
	cm.mu.RUnlock()

	if !ok {
		return nil, false
	}
	if !entry.isExpired() {
		return entry.Value, true
	}

	// Expired: acquire a full write lock, re-check, then delete.
	// Do not flush — lazy expiry is not a write that needs persistence.
	cm.mu.Lock()
	if e, still := cm.entries[key]; still && e.isExpired() {
		delete(cm.entries, key)
	}
	cm.mu.Unlock()
	return nil, false
}

// Set stores value under key with the given TTL and flushes to disk.
// Callers must NOT hold any lock before calling Set.
func (cm *CacheManager) Set(key string, value []byte, ttl time.Duration) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	cm.entries[key] = cacheEntry{
		Value:     value,
		FetchedAt: time.Now(),
		TTL:       ttl,
	}
	cm.flushLocked()
}

// GetMetadata retrieves a metadata value. Semantically identical to Get but
// documented separately to match the Python API surface.
func (cm *CacheManager) GetMetadata(key string) ([]byte, bool) {
	return cm.Get(key)
}

// SetMetadata stores a metadata value. Semantically identical to Set.
func (cm *CacheManager) SetMetadata(key string, value []byte, ttl time.Duration) {
	cm.Set(key, value, ttl)
}

// ClearProvider deletes all entries whose key starts with provider+":".
// Returns the number of entries deleted.
func (cm *CacheManager) ClearProvider(provider string) int {
	prefix := provider + ":"

	cm.mu.Lock()
	defer cm.mu.Unlock()

	deleted := 0
	for k := range cm.entries {
		if strings.HasPrefix(k, prefix) {
			delete(cm.entries, k)
			deleted++
		}
	}
	if deleted > 0 {
		cm.flushLocked()
	}
	return deleted
}

// Stats returns a snapshot of cache metrics, including a per-provider/service
// breakdown of entry counts and last-write timestamps.
func (cm *CacheManager) Stats() CacheStats {
	cm.mu.RLock()
	count := len(cm.entries)
	byProvider := make(map[string]map[string]ProviderServiceStats)
	var asOf time.Time
	for k, e := range cm.entries {
		provider, service := splitCacheKey(k)
		svcMap, ok := byProvider[provider]
		if !ok {
			svcMap = make(map[string]ProviderServiceStats)
			byProvider[provider] = svcMap
		}
		bucket := svcMap[service]
		bucket.EntryCount++
		if e.FetchedAt.After(bucket.LastWriteAt) {
			bucket.LastWriteAt = e.FetchedAt
		}
		svcMap[service] = bucket
		if e.FetchedAt.After(asOf) {
			asOf = e.FetchedAt
		}
	}
	cm.mu.RUnlock()

	var fileSize int64
	if fi, err := os.Stat(cm.filePath); err == nil {
		fileSize = fi.Size()
	}

	return CacheStats{
		EntryCount:    count,
		FilePath:      cm.filePath,
		FileSizeBytes: fileSize,
		AsOf:          asOf,
		ByProvider:    byProvider,
	}
}

// Close performs a final flush to disk and is idempotent.
// Safe to call more than once.
func (cm *CacheManager) Close() {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.flushLocked()
}
