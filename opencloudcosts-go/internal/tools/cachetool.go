// cachetool.go implements the cache MCP tools:
//   - refresh_cache
//   - cache_stats
//
// Output shapes mirror the Python implementation in
// src/opencloudcosts/tools/lookup.py (refresh_cache) and
// src/opencloudcosts/tools/availability.py (cache_stats).
package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --------------------------------------------------------------------------
// Input types
// --------------------------------------------------------------------------

// RefreshCacheInput is the typed input for the refresh_cache tool.
type RefreshCacheInput struct {
	Provider string `json:"provider"`
}

// CacheStatsInput is the (empty) typed input for the cache_stats tool.
type CacheStatsInput struct{}

// --------------------------------------------------------------------------
// HandleRefreshCache — refresh_cache tool handler
// --------------------------------------------------------------------------

// HandleRefreshCache implements the refresh_cache MCP tool.
//
// When provider is non-empty, it clears all cache entries for that provider
// and returns the count of deleted entries.
//
// When provider is empty, Go's in-memory cache uses lazy expiry (entries are
// evicted on the next Get after their TTL elapses). There is no bulk "purge
// expired" operation. We return a message describing this behaviour so the
// LLM knows what happened — this matches the spirit of the Python purge path
// which returns the count of expired entries removed.
func (h *Handler) HandleRefreshCache(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in RefreshCacheInput,
) (*mcp.CallToolResult, any, error) {
	if h.cm == nil {
		return jsonText(map[string]any{
			"error": "cache not configured",
		}), nil, nil
	}

	if in.Provider != "" {
		deleted := h.cm.ClearProvider(in.Provider)
		return jsonText(map[string]any{
			"message":          fmt.Sprintf("Cache cleared for provider: %s", in.Provider),
			"prices_deleted":   deleted,
			"metadata_deleted": 0,
		}), nil, nil
	}

	// Empty provider: no bulk purge available in the Go cache (lazy expiry).
	// Return the current stats so the caller has context.
	stats := h.cm.Stats()
	return jsonText(map[string]any{
		"message": "Purged 0 expired entries (Go cache uses lazy expiry; entries expire on next access)",
		"cache_stats": map[string]any{
			"price_entries":    stats.EntryCount,
			"metadata_entries": 0,
			"db_size_mb":       roundToTwoDecimal(float64(stats.FileSizeBytes) / (1024 * 1024)),
			"db_path":          stats.FilePath,
		},
	}), nil, nil
}

// --------------------------------------------------------------------------
// HandleCacheStats — cache_stats tool handler
// --------------------------------------------------------------------------

// HandleCacheStats implements the cache_stats MCP tool.
// Returns statistics about the local pricing cache, with field names matching
// the Python implementation (price_entries, metadata_entries, db_size_mb, db_path).
func (h *Handler) HandleCacheStats(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	_ CacheStatsInput,
) (*mcp.CallToolResult, any, error) {
	if h.cm == nil {
		return jsonText(map[string]any{
			"error": "cache not configured",
		}), nil, nil
	}

	stats := h.cm.Stats()
	return jsonText(map[string]any{
		"price_entries":    stats.EntryCount,
		"metadata_entries": 0,
		"db_size_mb":       roundToTwoDecimal(float64(stats.FileSizeBytes) / (1024 * 1024)),
		"db_path":          stats.FilePath,
	}), nil, nil
}

// --------------------------------------------------------------------------
// Helper
// --------------------------------------------------------------------------

// roundToTwoDecimal rounds a float64 to two decimal places.
// Mirrors Python's round(x, 2).
func roundToTwoDecimal(x float64) float64 {
	// Use integer arithmetic to avoid floating-point drift.
	// math.Round is not needed: multiplying by 100, rounding, dividing by 100
	// is accurate for the magnitudes involved (0–1000 MB).
	scaled := x * 100
	if scaled < 0 {
		scaled -= 0.5
	} else {
		scaled += 0.5
	}
	return float64(int64(scaled)) / 100
}
