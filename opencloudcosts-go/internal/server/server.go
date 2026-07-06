// Package server wires the modelcontextprotocol/go-sdk MCP server, registers
// all 18 tools, and exposes RunStdio / RunHTTP transports.
//
// Tool InputSchemas are taken verbatim from schemas/tools-snapshot.json (the
// Phase 0 Python snapshot) rather than being auto-generated from Go struct
// reflection, to guarantee byte-level parity with the Python implementation.
//
// Enterprise features implemented here:
//   - Structured JSON logging via log/slog on every tool call (latency, error).
//   - Per-tool-call context.WithTimeout governed by cfg.RequestTimeout.
//   - In-process token-bucket rate limiting via golang.org/x/time/rate (HTTP transport).
//   - panic recovery at the MCP handler boundary: no panic reaches the MCP client.
//   - Graceful degradation: missing providers return structured errors, no crash.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/time/rate"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/config"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/tools"
)

// Provider is the cloud pricing provider interface, aliased from the providers
// package so callers can reference it from this package.
type Provider = providers.Provider

// Version is the build-time version string. Set it from main() before calling
// New() so /healthz reports the real version instead of "dev".
// Defaults to "dev" when not set (e.g. in tests).
var Version = "dev"

// AppServer holds the runtime state shared across all tool handlers.
type AppServer struct {
	cfg       *config.Config
	cache     *cache.CacheManager
	providers map[string]Provider

	// handler holds the wired tools.Handler (tools package).
	handler *tools.Handler

	// limiter is the rate limiter for the HTTP transport (nil = disabled).
	limiter *rate.Limiter

	// cacheReady is set to 1 once cache and providers are initialised.
	// /readyz returns 200 only when this is non-zero.
	cacheReady atomic.Int32
}

// New creates an AppServer from the given config, cache manager, and provider
// map. It does NOT start any transport — call RunStdio or RunHTTP for that.
//
// Providers may be nil or empty — the server starts successfully and returns
// structured errors for provider-specific tool calls.
func New(cfg *config.Config, cm *cache.CacheManager, provs map[string]Provider) *AppServer {
	if provs == nil {
		provs = make(map[string]Provider)
	}

	h := tools.New(provs)
	h.SetCache(cm)

	var lim *rate.Limiter
	if cfg != nil && cfg.RateLimit > 0 {
		// Burst = rps + 5 so brief spikes don't immediately get throttled.
		lim = rate.NewLimiter(rate.Limit(cfg.RateLimit), int(cfg.RateLimit)+5)
	}

	s := &AppServer{
		cfg:       cfg,
		cache:     cm,
		providers: provs,
		handler:   h,
		limiter:   lim,
	}
	// Providers and cache were supplied by the caller; mark as ready.
	s.cacheReady.Store(1)
	return s
}

// requestTimeout returns the configured request timeout, or 60s as the default
// when cfg is nil (e.g. in unit tests that pass a zero Config).
func (s *AppServer) requestTimeout() time.Duration {
	if s.cfg != nil && s.cfg.RequestTimeout > 0 {
		return s.cfg.RequestTimeout
	}
	return 60 * time.Second
}

// callTool wraps a tool call with:
//  1. context.WithTimeout from cfg.RequestTimeout
//  2. slog structured logging (tool name, latency, error)
//  3. panic recovery → structured error response
//
// The fn parameter is the actual tool implementation. It receives a
// timeout-bounded context. callTool must be called at the top of every
// tool handler.
func (s *AppServer) callTool(
	ctx context.Context,
	toolName string,
	fn func(ctx context.Context) (*mcp.CallToolResult, any, error),
) (res *mcp.CallToolResult, _ any, retErr error) {
	start := time.Now()

	// Panic recovery — catches any unhandled panic in the tool logic and
	// returns a structured error instead of crashing the server.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("tool panic recovered",
				"tool", toolName,
				"panic", fmt.Sprintf("%v", r),
				"latency_ms", time.Since(start).Milliseconds(),
			)
			b, _ := json.Marshal(map[string]any{
				"error":   "internal_error",
				"message": "An unexpected error occurred. Please try again.",
			})
			res = &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: string(b)},
				},
			}
			retErr = nil
		}
	}()

	// Apply per-tool-call timeout.
	timeout := s.requestTimeout()
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, extra, err := fn(callCtx)

	latency := time.Since(start)
	if err != nil {
		slog.Warn("tool call error",
			"tool", toolName,
			"latency_ms", latency.Milliseconds(),
			"error", err.Error(),
		)
	} else {
		slog.Debug("tool call complete",
			"tool", toolName,
			"latency_ms", latency.Milliseconds(),
		)
	}

	return result, extra, err
}

// buildMCPServer constructs the mcp.Server with all 18 tools registered.
func (s *AppServer) buildMCPServer() *mcp.Server {
	mcpSrv := mcp.NewServer(&mcp.Implementation{
		Name:    "OpenCloudCosts MCP",
		Version: "dev",
	}, &mcp.ServerOptions{
		Instructions: "OpenCloudCosts MCP provides accurate public and effective cloud pricing data. " +
			"Use it to look up compute, storage, and database pricing on AWS, GCP, and Azure; " +
			"compare prices across regions and providers; estimate TCO from a Bill of Materials; " +
			"and calculate unit economics. For effective/bespoke pricing (post-discount), " +
			"ensure provider credentials are configured. Azure pricing requires no credentials.",
	})
	s.registerTools(mcpSrv)
	return mcpSrv
}

// RunStdio runs the MCP server over stdin/stdout (the default transport for
// MCP clients such as Claude Code).
func (s *AppServer) RunStdio() error {
	ctx := context.Background()
	return s.buildMCPServer().Run(ctx, &mcp.StdioTransport{})
}

// RunHTTP runs the MCP server over streamable-HTTP on addr (host:port).
// Health probes (/healthz, /readyz) are served on the same listener on a
// separate ServeMux path, never through the MCP handler.
func (s *AppServer) RunHTTP(host, port string) error {
	addr := host + ":" + port

	mcpSrv := s.buildMCPServer()
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcpSrv
	}, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.Handle("/", s.withRateLimit(s.withAPIKey(mcpHandler)))

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return srv.ListenAndServe()
}

// handleHealthz is the liveness probe — always 200 while the process is alive.
func (s *AppServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"status":"ok","version":%q}`, Version)
}

// handleReadyz is the readiness probe — 503 until cache+providers are ready.
func (s *AppServer) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.cacheReady.Load() == 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, `{"status":"not_ready","reason":"cache not initialised"}`)
		return
	}
	if len(s.providers) == 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, `{"status":"not_ready","reason":"no providers available"}`)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"status":"ok"}`)
}

// withAPIKey is a middleware that enforces the Bearer token when cfg.APIKey is set.
// Uses constant-time comparison to prevent timing attacks.
func (s *AppServer) withAPIKey(next http.Handler) http.Handler {
	if s.cfg == nil || s.cfg.APIKey == "" {
		return next
	}
	expected := []byte(s.cfg.APIKey)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		var token string
		if len(auth) > 7 && auth[:7] == "Bearer " {
			token = auth[7:]
		}
		if subtle.ConstantTimeCompare([]byte(token), expected) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprintf(w, `{"error":"unauthorized"}`)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withRateLimit is a middleware that enforces the in-process rate limiter.
// Returns 429 with a Retry-After header when the limit is exceeded.
func (s *AppServer) withRateLimit(next http.Handler) http.Handler {
	if s.limiter == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.Allow() {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprintf(w, `{"error":"rate_limit_exceeded","retry_after_seconds":1}`)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- Schema helpers ----

// rawSchema parses a JSON literal into a json.RawMessage and panics on error.
// All schemas are constants derived from schemas/tools-snapshot.json.
func rawSchema(s string) json.RawMessage {
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		panic(fmt.Sprintf("server: invalid tool schema JSON: %v", err))
	}
	return raw
}

// ---- Tool schema constants (verbatim from tools-snapshot.json) ----
//
// These are the inputSchema values for each tool, taken byte-for-byte from
// the Phase 0 Python snapshot. Using them as json.RawMessage prevents any
// schema drift introduced by Go struct reflection.

const (
	schemaGetPrice = `{
		"properties": {
			"spec": {
				"additionalProperties": true,
				"title": "Spec",
				"type": "object"
			}
		},
		"required": ["spec"],
		"title": "get_priceArguments",
		"type": "object"
	}`

	schemaGetPricesBatch = `{
		"properties": {
			"provider": {"title": "Provider", "type": "string"},
			"instance_types": {
				"items": {"type": "string"},
				"title": "Instance Types",
				"type": "array"
			},
			"region": {"title": "Region", "type": "string"},
			"os": {"default": "Linux", "title": "Os", "type": "string"},
			"term": {"default": "on_demand", "title": "Term", "type": "string"}
		},
		"required": ["provider", "instance_types", "region"],
		"title": "get_prices_batchArguments",
		"type": "object"
	}`

	schemaComparePrices = `{
		"properties": {
			"spec": {
				"additionalProperties": true,
				"title": "Spec",
				"type": "object"
			},
			"regions": {
				"items": {"type": "string"},
				"title": "Regions",
				"type": "array"
			},
			"baseline_region": {
				"default": "",
				"title": "Baseline Region",
				"type": "string"
			}
		},
		"required": ["spec", "regions"],
		"title": "compare_pricesArguments",
		"type": "object"
	}`

	schemaGetPriceBySKU = `{
		"properties": {
			"provider": {"default": "aws", "title": "Provider", "type": "string"},
			"sku": {"title": "Sku", "type": "string"},
			"service": {"default": "", "title": "Service", "type": "string"},
			"regions": {
				"items": {"type": "string"},
				"title": "Regions",
				"type": "array"
			},
			"baseline_region": {
				"default": "",
				"title": "Baseline Region",
				"type": "string"
			},
			"operation": {
				"default": "",
				"title": "Operation",
				"type": "string",
				"description": "Optional disambiguating hint: the AWS product 'operation' attribute (e.g. 'CreateDBInstance:0021' for RDS Aurora PostgreSQL), matched case-insensitively."
			},
			"product_family": {
				"default": "",
				"title": "Product Family",
				"type": "string",
				"description": "Optional disambiguating hint: the AWS top-level 'productFamily' (e.g. 'Load Balancer-Application' for an ALB), matched case-insensitively."
			}
		},
		"required": ["sku", "regions"],
		"title": "get_price_by_skuArguments",
		"type": "object"
	}`

	schemaGetPricesBySKU = `{
		"properties": {
			"provider": {"default": "aws", "title": "Provider", "type": "string"},
			"skus": {
				"items": {"type": "string"},
				"title": "Skus",
				"type": "array"
			},
			"regions": {
				"items": {"type": "string"},
				"title": "Regions",
				"type": "array"
			},
			"baseline_region": {
				"default": "",
				"title": "Baseline Region",
				"type": "string"
			}
		},
		"required": ["skus", "regions"],
		"title": "get_prices_by_skuArguments",
		"type": "object"
	}`

	schemaSearchPricing = `{
		"additionalProperties": true,
		"properties": {},
		"title": "search_pricingArguments",
		"type": "object"
	}`

	schemaGetDiscountSummary = `{
		"properties": {
			"provider": {"default": "aws", "title": "Provider", "type": "string"}
		},
		"title": "get_discount_summaryArguments",
		"type": "object"
	}`

	schemaGetSpotHistory = `{
		"additionalProperties": true,
		"properties": {},
		"title": "get_spot_historyArguments",
		"type": "object"
	}`

	schemaRefreshCache = `{
		"properties": {
			"provider": {"default": "", "title": "Provider", "type": "string"}
		},
		"title": "refresh_cacheArguments",
		"type": "object"
	}`

	schemaListRegions = `{
		"properties": {
			"provider": {"title": "Provider", "type": "string"},
			"domain": {"default": "compute", "title": "Domain", "type": "string"}
		},
		"required": ["provider"],
		"title": "list_regionsArguments",
		"type": "object"
	}`

	schemaListInstanceTypes = `{
		"properties": {
			"provider": {"title": "Provider", "type": "string"},
			"region": {"title": "Region", "type": "string"},
			"family": {"default": "", "title": "Family", "type": "string"},
			"min_vcpu": {
				"anyOf": [{"type": "integer"}, {"type": "string"}, {"type": "null"}],
				"default": null,
				"title": "Min Vcpu"
			},
			"min_memory_gb": {
				"anyOf": [{"type": "number"}, {"type": "string"}, {"type": "null"}],
				"default": null,
				"title": "Min Memory Gb"
			},
			"gpu": {"default": false, "title": "Gpu", "type": "boolean"}
		},
		"required": ["provider", "region"],
		"title": "list_instance_typesArguments",
		"type": "object"
	}`

	schemaDescribeCatalog = `{
		"properties": {
			"provider": {"default": "", "title": "Provider", "type": "string"},
			"domain": {"default": "", "title": "Domain", "type": "string"},
			"service": {"default": "", "title": "Service", "type": "string"}
		},
		"title": "describe_catalogArguments",
		"type": "object"
	}`

	schemaCompareBOMRegions = `{
		"properties": {
			"items": {
				"items": {
					"additionalProperties": true,
					"type": "object"
				},
				"title": "Items",
				"type": "array"
			},
			"regions": {
				"items": {"type": "string"},
				"title": "Regions",
				"type": "array"
			},
			"baseline_region": {
				"default": "",
				"title": "Baseline Region",
				"type": "string"
			}
		},
		"required": ["items", "regions"],
		"title": "compare_bom_regionsArguments",
		"type": "object"
	}`

	schemaGetCoverage = `{
		"properties": {
			"provider": {"default": "", "title": "Provider", "type": "string"}
		},
		"title": "get_coverageArguments",
		"type": "object"
	}`

	schemaFindCheapestRegion = `{
		"properties": {
			"spec": {
				"additionalProperties": true,
				"title": "Spec",
				"type": "object"
			},
			"regions": {
				"anyOf": [
					{"items": {"type": "string"}, "type": "array"},
					{"type": "null"}
				],
				"default": null,
				"title": "Regions"
			},
			"baseline_region": {
				"default": "",
				"title": "Baseline Region",
				"type": "string"
			}
		},
		"required": ["spec"],
		"title": "find_cheapest_regionArguments",
		"type": "object"
	}`

	schemaFindAvailableRegions = `{
		"properties": {
			"spec": {
				"additionalProperties": true,
				"title": "Spec",
				"type": "object"
			},
			"regions": {
				"anyOf": [
					{"items": {"type": "string"}, "type": "array"},
					{"type": "null"}
				],
				"default": null,
				"title": "Regions"
			},
			"baseline_region": {
				"default": "",
				"title": "Baseline Region",
				"type": "string"
			}
		},
		"required": ["spec"],
		"title": "find_available_regionsArguments",
		"type": "object"
	}`

	schemaCacheStats = `{
		"properties": {},
		"title": "cache_statsArguments",
		"type": "object"
	}`

	schemaWarmCache = `{
		"properties": {
			"provider": {"title": "Provider", "type": "string"},
			"regions": {
				"items": {"type": "string"},
				"title": "Regions",
				"type": "array"
			},
			"services": {
				"items": {"type": "string"},
				"title": "Services",
				"type": "array"
			}
		},
		"required": ["provider", "regions"],
		"title": "warm_cacheArguments",
		"type": "object"
	}`

	schemaEstimateBOM = `{
		"properties": {
			"items": {
				"items": {
					"additionalProperties": true,
					"type": "object"
				},
				"title": "Items",
				"type": "array"
			}
		},
		"required": ["items"],
		"title": "estimate_bomArguments",
		"type": "object"
	}`

	schemaEstimateUnitEconomics = `{
		"properties": {
			"items": {
				"items": {
					"additionalProperties": true,
					"type": "object"
				},
				"title": "Items",
				"type": "array"
			},
			"units_per_month": {"title": "Units Per Month", "type": "number"},
			"unit_label": {"default": "user", "title": "Unit Label", "type": "string"}
		},
		"required": ["items", "units_per_month"],
		"title": "estimate_unit_economicsArguments",
		"type": "object"
	}`

	schemaCompareBOM = `{
		"properties": {
			"providers": {
				"items": {"type": "string", "enum": ["aws", "gcp", "azure"]},
				"description": "Which providers to compare. Defaults to all three.",
				"default": ["aws", "gcp", "azure"],
				"title": "Providers",
				"type": "array"
			},
			"region_preference": {
				"description": "Preferred region tier: 'us' (US regions), 'eu' (Europe), 'apac' (Asia-Pacific).",
				"enum": ["us", "eu", "apac"],
				"default": "us",
				"title": "Region Preference",
				"type": "string"
			},
			"workload": {
				"description": "Cloud-agnostic workload description. Each key is a logical service name.",
				"additionalProperties": {
					"type": "object",
					"properties": {
						"type": {"type": "string", "description": "Resource type: 'compute', 'database', 'storage', 'cache'"},
						"vcpus": {"type": "number", "description": "Number of virtual CPUs"},
						"memory_gb": {"type": "number", "description": "Memory in GB"},
						"quantity": {"type": "number", "description": "Number of instances", "default": 1},
						"engine": {"type": "string", "description": "For databases: mysql, postgres, etc."},
						"storage_gb": {"type": "number", "description": "Storage size in GB (for storage/database)"},
						"storage_type": {"type": "string", "description": "ssd (default), hdd"},
						"os": {"type": "string", "default": "linux"}
					},
					"required": ["type"]
				},
				"title": "Workload",
				"type": "object"
			},
			"terms": {
				"items": {"type": "string"},
				"description": "Pricing terms to include. Common values: on_demand, reserved_1yr, cud_1yr. Default: ['on_demand', 'reserved_1yr'].",
				"default": ["on_demand", "reserved_1yr"],
				"title": "Terms",
				"type": "array"
			}
		},
		"required": ["workload"],
		"title": "compare_bomArguments",
		"type": "object"
	}`
)

// ---- Tool output schema constants (generated; mirrors tools-output-snapshot.json) ----
//
// These are the outputSchema values for each tool. Every field from the tool's
// success shape AND its structured-error shape must appear here (permissive types,
// no "required", no "additionalProperties":false) because the go-sdk validates
// actual tool output against this schema at call time, and a tool may legitimately
// return either shape depending on runtime state.

const (
	schemaGetPriceOutput = `{
	"type": "object",
	"properties": {
		"public_prices": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"provider": {
						"type": "string"
					},
					"description": {
						"type": "string"
					},
					"region": {
						"type": "string"
					},
					"region_name": {
						"type": "string"
					},
					"term": {
						"type": "string"
					},
					"price_per_unit": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"unit": {
								"type": "string"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"monthly_estimate": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"instanceType": {
						"type": "string"
					},
					"vcpu": {
						"type": "string"
					},
					"memory": {
						"type": "string"
					},
					"operatingSystem": {
						"type": "string"
					},
					"storage": {
						"type": "string"
					},
					"storage_type": {
						"type": "string"
					},
					"volumeType": {
						"type": "string"
					},
					"fallback": {
						"type": "string"
					},
					"fallback_note": {
						"type": "string"
					},
					"note": {
						"type": "string"
					},
					"fromRegionCode": {
						"type": "string"
					},
					"toRegionCode": {
						"type": "string"
					},
					"as_of": {
						"type": "string"
					},
					"cache_age_seconds": {
						"type": "number"
					},
					"price_effective_date": {
						"type": "string"
					},
					"source_url": {
						"type": "string"
					}
				}
			}
		},
		"auth_available": {
			"type": "boolean"
		},
		"source": {
			"type": "string"
		},
		"contracted_prices": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"provider": {
						"type": "string"
					},
					"description": {
						"type": "string"
					},
					"region": {
						"type": "string"
					},
					"region_name": {
						"type": "string"
					},
					"term": {
						"type": "string"
					},
					"price_per_unit": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"unit": {
								"type": "string"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"monthly_estimate": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"instanceType": {
						"type": "string"
					},
					"vcpu": {
						"type": "string"
					},
					"memory": {
						"type": "string"
					},
					"operatingSystem": {
						"type": "string"
					},
					"storage": {
						"type": "string"
					},
					"storage_type": {
						"type": "string"
					},
					"volumeType": {
						"type": "string"
					},
					"fallback": {
						"type": "string"
					},
					"fallback_note": {
						"type": "string"
					},
					"note": {
						"type": "string"
					},
					"fromRegionCode": {
						"type": "string"
					},
					"toRegionCode": {
						"type": "string"
					},
					"as_of": {
						"type": "string"
					},
					"cache_age_seconds": {
						"type": "number"
					},
					"price_effective_date": {
						"type": "string"
					},
					"source_url": {
						"type": "string"
					}
				}
			}
		},
		"effective_price": {
			"type": "object",
			"properties": {
				"price_per_unit": {
					"type": "object",
					"properties": {
						"amount": {
							"type": "number"
						},
						"unit": {
							"type": "string"
						},
						"currency": {
							"type": "string"
						},
						"display": {
							"type": "string"
						}
					}
				},
				"discount_type": {
					"type": "string"
				},
				"discount_pct": {
					"type": "number"
				},
				"savings_vs_on_demand": {
					"type": "object",
					"properties": {
						"amount": {
							"type": "number"
						},
						"unit": {
							"type": "string"
						},
						"currency": {
							"type": "string"
						},
						"display": {
							"type": "string"
						}
					}
				}
			}
		},
		"breakdown": {
			"type": "object"
		},
		"note": {
			"type": "string"
		},
		"warnings": {
			"type": "array",
			"items": {
				"type": "string"
			}
		},
		"error": {
			"type": "string"
		},
		"region": {
			"type": "string"
		},
		"reason": {
			"type": "string"
		},
		"hint": {
			"type": "string"
		},
		"fix": {
			"type": "string"
		},
		"provider": {
			"type": "string"
		},
		"domain": {
			"type": "string"
		},
		"service": {
			"type": "string"
		},
		"alternatives": {
			"type": "array",
			"items": {
				"type": "string"
			}
		},
		"message": {
			"type": "string"
		},
		"retryable": {
			"type": "boolean"
		},
		"result": {
			"type": "string"
		},
		"filters_applied": {
			"type": "object"
		},
		"tip": {
			"type": "string"
		},
		"not_available_in": {
			"type": "array",
			"items": {
				"type": "string"
			}
		}
	}
}`
	schemaGetPricesBatchOutput = `{
	"type": "object",
	"properties": {
		"provider": {
			"type": "string"
		},
		"region": {
			"type": "string"
		},
		"os": {
			"type": "string"
		},
		"term": {
			"type": "string"
		},
		"count": {
			"type": "number"
		},
		"results": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"instance_type": {
						"type": "string"
					},
					"price_per_unit": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"unit": {
								"type": "string"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"monthly_estimate": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"vcpu": {
						"type": "string"
					},
					"memory": {
						"type": "string"
					},
					"description": {
						"type": "string"
					},
					"fallback": {
						"type": "string"
					}
				}
			}
		},
		"errors": {
			"type": "object"
		},
		"not_available_in": {
			"type": "array",
			"items": {
				"type": "string"
			}
		},
		"error": {
			"type": "string"
		}
	}
}`
	schemaComparePricesOutput = `{
	"type": "object",
	"properties": {
		"provider": {
			"type": "string"
		},
		"domain": {
			"type": "string"
		},
		"service": {
			"type": "string"
		},
		"cheapest_region": {
			"type": "string"
		},
		"cheapest_price": {
			"type": "object",
			"properties": {
				"amount": {
					"type": "number"
				},
				"unit": {
					"type": "string"
				},
				"currency": {
					"type": "string"
				},
				"display": {
					"type": "string"
				}
			}
		},
		"most_expensive_region": {
			"type": "string"
		},
		"most_expensive_price": {
			"type": "object",
			"properties": {
				"amount": {
					"type": "number"
				},
				"unit": {
					"type": "string"
				},
				"currency": {
					"type": "string"
				},
				"display": {
					"type": "string"
				}
			}
		},
		"price_delta_pct": {
			"type": [
				"number",
				"null"
			]
		},
		"all_regions_sorted": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"region": {
						"type": "string"
					},
					"region_name": {
						"type": "string"
					},
					"price_per_unit": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"unit": {
								"type": "string"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"monthly_estimate": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"fallback": {
						"type": "string"
					},
					"sku_description": {
						"type": "string"
					},
					"delta_per_hour": {
						"type": [
							"string",
							"null"
						]
					},
					"delta_monthly": {
						"type": [
							"string",
							"null"
						]
					},
					"delta_pct": {
						"type": [
							"string",
							"null"
						]
					}
				}
			}
		},
		"warnings": {
			"type": "array",
			"items": {
				"type": "string"
			}
		},
		"ranking_low_confidence": {
			"type": "boolean"
		},
		"multi_sku": {
			"type": "boolean"
		},
		"sku_count": {
			"type": "number"
		},
		"multi_sku_message": {
			"type": "string"
		},
		"not_available_in": {
			"type": "array",
			"items": {
				"type": "string"
			}
		},
		"transient_errors": {
			"type": "object"
		},
		"baseline_region": {
			"type": "string"
		},
		"baseline_missing": {
			"type": "boolean"
		},
		"baseline_missing_message": {
			"type": "string"
		},
		"error": {
			"type": "string"
		},
		"regions": {
			"type": "array",
			"items": {
				"type": "string"
			}
		},
		"message": {
			"type": "string"
		},
		"retryable": {
			"type": "boolean"
		},
		"result": {
			"type": "string"
		}
	}
}`
	schemaGetPriceBySKUOutput = `{
	"type": "object",
	"properties": {
		"sku": {
			"type": "string"
		},
		"usage_type_prefix": {
			"type": "string"
		},
		"usage_type_suffix": {
			"type": "string"
		},
		"service_source": {
			"type": "string"
		},
		"all_regions_sorted": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"region": {
						"type": "string"
					},
					"region_name": {
						"type": "string"
					},
					"price_per_unit": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"unit": {
								"type": "string"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"service_used": {
						"type": "string"
					},
					"hint_status": {
						"type": "string"
					},
					"description": {
						"type": "string"
					},
					"product_family": {
						"type": "string"
					},
					"attributes": {
						"type": "object"
					},
					"sku_id": {
						"type": "string"
					},
					"monthly_estimate": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"service_mismatch": {
						"type": "boolean"
					},
					"delta_per_hour": {
						"type": "string"
					},
					"delta_monthly": {
						"type": "string"
					},
					"delta_pct": {
						"type": "string"
					}
				}
			}
		},
		"service_hint": {
			"type": "string"
		},
		"inferred_service": {
			"type": "string"
		},
		"warnings": {
			"type": "array",
			"items": {
				"type": "string"
			}
		},
		"cheapest_region": {
			"type": "string"
		},
		"cheapest_price": {
			"type": "object",
			"properties": {
				"amount": {
					"type": "number"
				},
				"unit": {
					"type": "string"
				},
				"currency": {
					"type": "string"
				},
				"display": {
					"type": "string"
				}
			}
		},
		"most_expensive_region": {
			"type": "string"
		},
		"most_expensive_price": {
			"type": "object",
			"properties": {
				"amount": {
					"type": "number"
				},
				"unit": {
					"type": "string"
				},
				"currency": {
					"type": "string"
				},
				"display": {
					"type": "string"
				}
			}
		},
		"price_delta_pct": {
			"type": "number"
		},
		"ambiguous_in": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"region": {
						"type": "string"
					},
					"service_used": {
						"type": "string"
					},
					"hint_status": {
						"type": "string"
					},
					"alternate_match_count": {
						"type": "number"
					},
					"alternate_matches": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"price_per_unit": {
									"type": "object",
									"properties": {
										"amount": {
											"type": "number"
										},
										"unit": {
											"type": "string"
										},
										"currency": {
											"type": "string"
										},
										"display": {
											"type": "string"
										}
									}
								},
								"description": {
									"type": "string"
								},
								"product_family": {
									"type": "string"
								},
								"attributes": {
									"type": "object"
								},
								"sku_id": {
									"type": "string"
								}
							}
						}
					},
					"service_mismatch": {
						"type": "boolean"
					}
				}
			}
		},
		"no_mapping_in": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"region": {
						"type": "string"
					},
					"attempted_services": {
						"type": "array",
						"items": {
							"type": "string"
						}
					}
				}
			}
		},
		"errors_in": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"region": {
						"type": "string"
					},
					"error": {
						"type": "string"
					}
				}
			}
		},
		"baseline_region": {
			"type": "string"
		},
		"result": {
			"type": "string"
		},
		"message": {
			"type": "string"
		},
		"error": {
			"type": "string"
		},
		"retryable": {
			"type": "boolean"
		},
		"regions": {
			"type": "array",
			"items": {
				"type": "string"
			}
		}
	}
}`
	schemaGetPricesBySKUOutput = `{
	"type": "object",
	"properties": {
		"provider": {
			"type": "string"
		},
		"skus": {
			"type": "array",
			"items": {
				"type": "string"
			}
		},
		"regions": {
			"type": "array",
			"items": {
				"type": "string"
			}
		},
		"count": {
			"type": "number"
		},
		"results": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"sku": {
						"type": "string"
					},
					"usage_type_prefix": {
						"type": "string"
					},
					"usage_type_suffix": {
						"type": "string"
					},
					"service_source": {
						"type": "string"
					},
					"all_regions_sorted": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"region": {
									"type": "string"
								},
								"region_name": {
									"type": "string"
								},
								"price_per_unit": {
									"type": "object",
									"properties": {
										"amount": {
											"type": "number"
										},
										"unit": {
											"type": "string"
										},
										"currency": {
											"type": "string"
										},
										"display": {
											"type": "string"
										}
									}
								},
								"service_used": {
									"type": "string"
								},
								"hint_status": {
									"type": "string"
								},
								"description": {
									"type": "string"
								},
								"product_family": {
									"type": "string"
								},
								"attributes": {
									"type": "object"
								},
								"sku_id": {
									"type": "string"
								},
								"monthly_estimate": {
									"type": "object",
									"properties": {
										"amount": {
											"type": "number"
										},
										"currency": {
											"type": "string"
										},
										"display": {
											"type": "string"
										}
									}
								},
								"service_mismatch": {
									"type": "boolean"
								},
								"delta_per_hour": {
									"type": "string"
								},
								"delta_monthly": {
									"type": "string"
								},
								"delta_pct": {
									"type": "string"
								}
							}
						}
					},
					"service_hint": {
						"type": "string"
					},
					"inferred_service": {
						"type": "string"
					},
					"warnings": {
						"type": "array",
						"items": {
							"type": "string"
						}
					},
					"cheapest_region": {
						"type": "string"
					},
					"cheapest_price": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"unit": {
								"type": "string"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"most_expensive_region": {
						"type": "string"
					},
					"most_expensive_price": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"unit": {
								"type": "string"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"price_delta_pct": {
						"type": "number"
					},
					"ambiguous_in": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"region": {
									"type": "string"
								},
								"service_used": {
									"type": "string"
								},
								"hint_status": {
									"type": "string"
								},
								"alternate_match_count": {
									"type": "number"
								},
								"alternate_matches": {
									"type": "array",
									"items": {
										"type": "object",
										"properties": {
											"price_per_unit": {
												"type": "object",
												"properties": {
													"amount": {
														"type": "number"
													},
													"unit": {
														"type": "string"
													},
													"currency": {
														"type": "string"
													},
													"display": {
														"type": "string"
													}
												}
											},
											"description": {
												"type": "string"
											},
											"product_family": {
												"type": "string"
											},
											"attributes": {
												"type": "object"
											},
											"sku_id": {
												"type": "string"
											}
										}
									}
								},
								"service_mismatch": {
									"type": "boolean"
								}
							}
						}
					},
					"no_mapping_in": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"region": {
									"type": "string"
								},
								"attempted_services": {
									"type": "array",
									"items": {
										"type": "string"
									}
								}
							}
						}
					},
					"errors_in": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"region": {
									"type": "string"
								},
								"error": {
									"type": "string"
								}
							}
						}
					},
					"baseline_region": {
						"type": "string"
					},
					"result": {
						"type": "string"
					},
					"message": {
						"type": "string"
					},
					"error": {
						"type": "string"
					},
					"retryable": {
						"type": "boolean"
					},
					"regions": {
						"type": "array",
						"items": {
							"type": "string"
						}
					}
				}
			}
		},
		"baseline_region": {
			"type": "string"
		},
		"errors": {
			"type": "object"
		},
		"error": {
			"type": "string"
		},
		"message": {
			"type": "string"
		}
	}
}`
	schemaSearchPricingOutput = `{
	"type": "object",
	"properties": {
		"error": {
			"type": "string"
		},
		"message": {
			"type": "string"
		},
		"alternatives": {
			"type": "object",
			"properties": {
				"browse_catalog": {
					"type": "string"
				},
				"price_known_service": {
					"type": "string"
				},
				"estimate_workload": {
					"type": "string"
				}
			}
		}
	}
}`
	schemaDescribeCatalogOutput = `{
	"type": "object",
	"properties": {
		"support_matrix": {
			"type": "object"
		},
		"tip": {
			"type": "string"
		},
		"provider": {
			"type": "string"
		},
		"domain": {
			"type": "string"
		},
		"service": {
			"type": [
				"string",
				"null"
			]
		},
		"redirect_notice": {
			"type": "string"
		},
		"supported_terms": {
			"type": [
				"array",
				"null"
			],
			"items": {
				"type": "string"
			}
		},
		"filter_hints": {
			"type": [
				"object",
				"null"
			]
		},
		"example_invocation": {
			"type": [
				"object",
				"null"
			]
		},
		"usage": {
			"type": "string"
		},
		"available_services": {
			"type": [
				"array",
				"null"
			],
			"items": {
				"type": "string"
			}
		},
		"auto_resolved": {
			"type": "string"
		},
		"error": {
			"type": "string"
		},
		"message": {
			"type": "string"
		}
	}
}`
	schemaGetCoverageOutput = `{
	"type": "object",
	"properties": {
		"as_of": {
			"type": "string"
		},
		"note": {
			"type": "string"
		},
		"provider": {
			"type": "string"
		},
		"domains": {
			"type": "object"
		},
		"coverage": {
			"type": "object"
		},
		"error": {
			"type": "string"
		},
		"message": {
			"type": "string"
		}
	}
}`
	schemaGetSpotHistoryOutput = `{
	"type": "object",
	"properties": {
		"error": {
			"type": "string"
		},
		"retryable": {
			"type": "boolean"
		},
		"message": {
			"type": "string"
		},
		"alternatives": {
			"type": "object",
			"properties": {
				"spot_price": {
					"type": "string"
				},
				"browse_instances": {
					"type": "string"
				},
				"compare_spot": {
					"type": "string"
				}
			}
		}
	}
}`
	schemaGetDiscountSummaryOutput = `{
	"type": "object",
	"properties": {
		"error": {
			"type": "string"
		},
		"savings_plans": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"id": {
						"type": "string"
					},
					"type": {
						"type": "string"
					},
					"payment_option": {
						"type": "string"
					},
					"commitment_usd_per_hour": {
						"type": "string"
					},
					"term_years": {
						"type": "string"
					},
					"start": {
						"type": "string"
					},
					"end": {
						"type": "string"
					},
					"state": {
						"type": "string"
					}
				}
			}
		},
		"reserved_instances": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"instance_type": {
						"type": "string"
					},
					"region": {
						"type": "string"
					},
					"count": {
						"type": "number"
					},
					"offering_type": {
						"type": "string"
					},
					"duration_years": {
						"type": "string"
					},
					"days_remaining": {
						"type": "number"
					},
					"fixed_price": {
						"type": "number"
					},
					"usage_price": {
						"type": "number"
					},
					"product_description": {
						"type": "string"
					},
					"state": {
						"type": "string"
					}
				}
			}
		},
		"utilisation": {
			"type": "object",
			"properties": {
				"savings_plans": {
					"type": "object",
					"properties": {
						"total_commitment": {
							"type": "string"
						},
						"unused_commitment": {
							"type": "string"
						},
						"utilization_pct": {
							"type": "string"
						},
						"net_savings": {
							"type": "string"
						}
					}
				},
				"savings_plans_error": {
					"type": "string"
				},
				"reserved_instances": {
					"type": "object",
					"properties": {
						"utilization_pct": {
							"type": "string"
						},
						"on_demand_cost_of_ri_hours": {
							"type": "string"
						},
						"net_ri_savings": {
							"type": "string"
						},
						"purchased_hours": {
							"type": "string"
						}
					}
				},
				"reserved_instances_error": {
					"type": "string"
				}
			}
		},
		"sp_count": {
			"type": "number"
		},
		"ri_count": {
			"type": "number"
		},
		"provider": {
			"type": "string"
		},
		"domain": {
			"type": "string"
		},
		"service": {
			"type": [
				"string",
				"null"
			]
		},
		"reason": {
			"type": "string"
		},
		"alternatives": {
			"type": "array",
			"items": {
				"type": "string"
			}
		},
		"message": {
			"type": "string"
		},
		"retryable": {
			"type": "boolean"
		}
	}
}`
	schemaEstimateBOMOutput = `{
	"type": "object",
	"properties": {
		"line_items": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"description": {
						"type": "string"
					},
					"provider": {
						"type": "string"
					},
					"service": {
						"type": "string"
					},
					"region": {
						"type": "string"
					},
					"quantity": {
						"type": "number"
					},
					"price_per_unit": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"unit": {
								"type": "string"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"monthly_cost": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"fallback": {
						"type": "string"
					},
					"fallback_note": {
						"type": "string"
					}
				}
			}
		},
		"totals": {
			"type": "object",
			"properties": {
				"monthly": {
					"type": "object",
					"properties": {
						"amount": {
							"type": "number"
						},
						"currency": {
							"type": "string"
						},
						"display": {
							"type": "string"
						}
					}
				},
				"annual": {
					"type": "object",
					"properties": {
						"amount": {
							"type": "number"
						},
						"currency": {
							"type": "string"
						},
						"display": {
							"type": "string"
						}
					}
				}
			}
		},
		"not_included": {
			"type": [
				"array",
				"null"
			],
			"items": {
				"type": "object"
			}
		},
		"not_included_action": {
			"type": [
				"string",
				"null"
			]
		},
		"errors": {
			"type": [
				"array",
				"null"
			],
			"items": {
				"type": "string"
			}
		},
		"error": {
			"type": "string"
		}
	}
}`
	schemaEstimateUnitEconomicsOutput = `{
	"type": "object",
	"properties": {
		"pricing_region": {
			"type": "string"
		},
		"infrastructure_monthly": {
			"type": "object",
			"properties": {
				"amount": {
					"type": "number"
				},
				"currency": {
					"type": "string"
				},
				"display": {
					"type": "string"
				}
			}
		},
		"infrastructure_annual": {
			"type": "object",
			"properties": {
				"amount": {
					"type": "number"
				},
				"currency": {
					"type": "string"
				},
				"display": {
					"type": "string"
				}
			}
		},
		"volume": {
			"type": "string"
		},
		"cost_per_unit": {
			"type": "object",
			"properties": {
				"amount": {
					"type": "number"
				},
				"currency": {
					"type": "string"
				},
				"display": {
					"type": "string"
				}
			}
		},
		"cost_per_unit_annual": {
			"type": "object",
			"properties": {
				"amount": {
					"type": "number"
				},
				"currency": {
					"type": "string"
				},
				"display": {
					"type": "string"
				}
			}
		},
		"errors": {
			"type": [
				"array",
				"null"
			],
			"items": {
				"type": "string"
			}
		},
		"important": {
			"type": "string"
		},
		"error": {
			"type": "string"
		}
	}
}`
	schemaCompareBOMOutput = `{
	"type": "object",
	"properties": {
		"comparison": {
			"type": "object"
		},
		"summary": {
			"type": "string"
		},
		"note": {
			"type": "string"
		},
		"error": {
			"type": "string"
		},
		"message": {
			"type": "string"
		}
	}
}`
	schemaCompareBOMRegionsOutput = `{
	"type": "object",
	"properties": {
		"regions": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"region": {
						"type": "string"
					},
					"region_name": {
						"type": "string"
					},
					"total_monthly": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"line_items": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"description": {
									"type": "string"
								},
								"provider": {
									"type": "string"
								},
								"service": {
									"type": "string"
								},
								"region": {
									"type": "string"
								},
								"quantity": {
									"type": "number"
								},
								"price_per_unit": {
									"type": "object",
									"properties": {
										"amount": {
											"type": "number"
										},
										"unit": {
											"type": "string"
										},
										"currency": {
											"type": "string"
										},
										"display": {
											"type": "string"
										}
									}
								},
								"monthly_cost": {
									"type": "object",
									"properties": {
										"amount": {
											"type": "number"
										},
										"currency": {
											"type": "string"
										},
										"display": {
											"type": "string"
										}
									}
								},
								"fallback": {
									"type": "string"
								},
								"fallback_note": {
									"type": "string"
								}
							}
						}
					},
					"errors": {
						"type": "array",
						"items": {
							"type": "string"
						}
					},
					"status": {
						"type": "string"
					},
					"delta_monthly": {
						"type": [
							"string",
							"null"
						]
					},
					"delta_pct": {
						"type": [
							"string",
							"null"
						]
					}
				}
			}
		},
		"baseline_region": {
			"type": "string"
		},
		"baseline_region_error": {
			"type": "string"
		},
		"not_supported": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"item": {
						"type": "string"
					},
					"provider": {
						"type": "string"
					},
					"source": {
						"type": "string"
					},
					"reason": {
						"type": "string"
					}
				}
			}
		},
		"error": {
			"type": "string"
		},
		"message": {
			"type": "string"
		}
	}
}`
	schemaRefreshCacheOutput = `{
	"type": "object",
	"properties": {
		"message": {
			"type": "string"
		},
		"prices_deleted": {
			"type": "number"
		},
		"metadata_deleted": {
			"type": "number"
		},
		"cache_stats": {
			"type": "object",
			"properties": {
				"price_entries": {
					"type": "number"
				},
				"metadata_entries": {
					"type": "number"
				},
				"db_size_mb": {
					"type": "number"
				},
				"db_path": {
					"type": "string"
				}
			}
		},
		"error": {
			"type": "string"
		}
	}
}`
	schemaCacheStatsOutput = `{
	"type": "object",
	"properties": {
		"price_entries": {
			"type": "number"
		},
		"metadata_entries": {
			"type": "number"
		},
		"db_size_mb": {
			"type": "number"
		},
		"db_path": {
			"type": "string"
		},
		"by_provider": {
			"type": "object"
		},
		"as_of": {
			"type": "string"
		},
		"as_of_age_seconds": {
			"type": "number"
		},
		"error": {
			"type": "string"
		}
	}
}`
	schemaWarmCacheOutput = `{
	"type": "object",
	"properties": {
		"provider": {
			"type": "string"
		},
		"regions": {
			"type": "array",
			"items": {
				"type": "string"
			}
		},
		"targets_warmed": {
			"type": "array",
			"items": {
				"type": "string"
			}
		},
		"combinations_attempted": {
			"type": "number"
		},
		"warmed": {
			"type": "number"
		},
		"cache_entries_after": {
			"type": "number"
		},
		"skipped_services": {
			"type": "array",
			"items": {
				"type": "string"
			}
		},
		"errors": {
			"type": "object"
		},
		"error": {
			"type": "string"
		},
		"message": {
			"type": "string"
		}
	}
}`
	schemaListRegionsOutput = `{
	"type": "object",
	"properties": {
		"provider": {
			"type": "string"
		},
		"domain": {
			"type": "string"
		},
		"regions": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"code": {
						"type": "string"
					},
					"name": {
						"type": "string"
					}
				}
			}
		},
		"count": {
			"type": "number"
		},
		"error": {
			"type": "string"
		}
	}
}`
	schemaListInstanceTypesOutput = `{
	"type": "object",
	"properties": {
		"provider": {
			"type": "string"
		},
		"region": {
			"type": "string"
		},
		"count": {
			"type": "number"
		},
		"instance_types": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"instance_type": {
						"type": "string"
					},
					"vcpu": {
						"type": "number"
					},
					"memory_gb": {
						"type": "number"
					},
					"gpu_count": {
						"type": "number"
					},
					"gpu_type": {
						"type": "string"
					}
				}
			}
		},
		"note": {
			"type": "string"
		},
		"error": {
			"type": "string"
		},
		"message": {
			"type": "string"
		}
	}
}`
	schemaFindCheapestRegionOutput = `{
	"type": "object",
	"properties": {
		"provider": {
			"type": "string"
		},
		"domain": {
			"type": "string"
		},
		"service": {
			"type": "string"
		},
		"cheapest_region": {
			"type": "string"
		},
		"cheapest_region_name": {
			"type": "string"
		},
		"cheapest_price": {
			"type": "object",
			"properties": {
				"amount": {
					"type": "number"
				},
				"unit": {
					"type": "string"
				},
				"currency": {
					"type": "string"
				},
				"display": {
					"type": "string"
				}
			}
		},
		"most_expensive_region": {
			"type": "string"
		},
		"most_expensive_price": {
			"type": "object",
			"properties": {
				"amount": {
					"type": "number"
				},
				"unit": {
					"type": "string"
				},
				"currency": {
					"type": "string"
				},
				"display": {
					"type": "string"
				}
			}
		},
		"price_delta_pct": {
			"type": [
				"number",
				"null"
			]
		},
		"all_regions_sorted": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"region": {
						"type": "string"
					},
					"region_name": {
						"type": "string"
					},
					"price_per_unit": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"unit": {
								"type": "string"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"monthly_estimate": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"delta_per_hour": {
						"type": "string"
					},
					"delta_monthly": {
						"type": "string"
					},
					"delta_pct": {
						"type": "string"
					}
				}
			}
		},
		"not_available_in": {
			"type": [
				"array",
				"null"
			],
			"items": {
				"type": "string"
			}
		},
		"baseline_region": {
			"type": "string"
		},
		"baseline_region_name": {
			"type": "string"
		},
		"note": {
			"type": "string"
		},
		"error": {
			"type": "string"
		},
		"reason": {
			"type": "string"
		},
		"hint": {
			"type": "string"
		},
		"fix": {
			"type": "string"
		},
		"regions": {
			"type": [
				"array",
				"null"
			],
			"items": {
				"type": "string"
			}
		},
		"message": {
			"type": "string"
		},
		"retryable": {
			"type": "boolean"
		},
		"result": {
			"type": "string"
		}
	}
}`
	schemaFindAvailableRegionsOutput = `{
	"type": "object",
	"properties": {
		"provider": {
			"type": "string"
		},
		"domain": {
			"type": "string"
		},
		"available_in": {
			"type": "number"
		},
		"not_available_in": {
			"type": [
				"array",
				"null"
			],
			"items": {
				"type": "string"
			}
		},
		"regions_sorted_cheapest_first": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"region": {
						"type": "string"
					},
					"region_name": {
						"type": "string"
					},
					"price_per_unit": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"unit": {
								"type": "string"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"monthly_estimate": {
						"type": "object",
						"properties": {
							"amount": {
								"type": "number"
							},
							"currency": {
								"type": "string"
							},
							"display": {
								"type": "string"
							}
						}
					},
					"delta_per_hour": {
						"type": "string"
					},
					"delta_monthly": {
						"type": "string"
					},
					"delta_pct": {
						"type": "string"
					}
				}
			}
		},
		"baseline_region": {
			"type": "string"
		},
		"note": {
			"type": "string"
		},
		"error": {
			"type": "string"
		},
		"reason": {
			"type": "string"
		},
		"hint": {
			"type": "string"
		},
		"fix": {
			"type": "string"
		},
		"regions": {
			"type": [
				"array",
				"null"
			],
			"items": {
				"type": "string"
			}
		},
		"message": {
			"type": "string"
		},
		"retryable": {
			"type": "boolean"
		},
		"result": {
			"type": "string"
		}
	}
}`
)

// BuildMCPServerForTest exposes the internal MCP server construction for
// package-external tests. It is intended for use only in test files.
func (s *AppServer) BuildMCPServerForTest() *mcp.Server {
	return s.buildMCPServer()
}

// BuildHTTPHandlerForTest exposes the HTTP handler (mux with /healthz, /readyz,
// and MCP endpoint) for package-external tests without starting a listener.
func (s *AppServer) BuildHTTPHandlerForTest() http.Handler {
	mcpSrv := s.buildMCPServer()
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcpSrv
	}, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.Handle("/", mcpHandler)
	return mux
}

// ---- Tool descriptions (verbatim from tools-snapshot.json) ----

const (
	descGetPrice = "\n        Unified pricing tool — returns public catalog rates plus contracted/effective prices\n        where credentials are available.\n\n        Pass a spec dict with at minimum: provider, domain, region.\n        Domain-specific required fields (call describe_catalog for the complete list):\n\n          COMPUTE  : resource_type (\"m5.xlarge\" / \"n1-standard-4\" / \"Standard_D4s_v3\")\n                     os (\"Linux\" or \"Windows\"), term (\"on_demand\"/\"spot\"/\"cud_1yr\")\n                     Fargate: vcpu (e.g. 2.0), memory_gb (e.g. 4.0), service=\"fargate\"\n          STORAGE  : storage_type (\"gp3\"/\"io2\"/\"sc1\"/\"standard\"/\"nearline\"/\"pd-extreme\"/\"hyperdisk-extreme\"/\"premium-ssd\")\n                     size_gb — disk size for monthly estimate\n                     iops — provisioned IOPS for io1/io2 (AWS) or pd-extreme/hyperdisk-extreme (GCP)\n                     throughput_mbps — provisioned throughput MB/s for gp3 (AWS); charge above 125 MB/s baseline\n          DATABASE : resource_type (\"db.r5.large\"/\"db-n1-standard-4\"), engine (\"MySQL\"),\n                     deployment (\"single-az\"/\"ha\"/\"multi-az\"), service (\"rds\"/\"cloud_sql\"/\"memorystore\")\n          AI       : model (\"claude-3-5-sonnet\"/\"gemini-1.5-flash\"), service (\"bedrock\"/\"gemini\"/\"vertex\"),\n                     input_tokens, output_tokens  |  machine_type + task for Vertex\n          CONTAINER: service (\"gke\"/\"eks\"), mode (\"standard\"/\"autopilot\"), node_count, vcpu, memory_gb\n          ANALYTICS: service (\"bigquery\"), query_tb, active_storage_gb, longterm_storage_gb, streaming_gb\n          NETWORK  : service (\"cloud_lb\"/\"cloud_cdn\"/\"cloud_nat\"/\"cloud_armor\"),\n                     lb_type, rule_count, data_gb, gateway_count, egress_gb, policy_count\n          OBSERVABILITY: service (\"cloudwatch\"/\"cloud_monitoring\"), ingestion_mib, log_gb\n          INTER_REGION_EGRESS: source_region, dest_region (empty = internet), data_gb\n                     Example: {\"provider\": \"aws\", \"domain\": \"inter_region_egress\",\n                               \"source_region\": \"us-east-1\", \"dest_region\": \"eu-west-1\"}\n\n        Returns public_prices[] always. When auth exists: contracted_prices[], effective_price,\n        auth_available=true.\n\n        Call describe_catalog(provider, domain, service) for an example_invocation you can\n        copy directly into this tool.\n\n        Args:\n            spec: PricingSpec dict — see field descriptions above.\n\n        Examples:\n            {\"provider\": \"aws\",   \"domain\": \"compute\",     \"resource_type\": \"m5.xlarge\",       \"region\": \"us-east-1\"}\n            {\"provider\": \"aws\",   \"domain\": \"ai\",          \"service\": \"bedrock\", \"model\": \"claude-3-5-sonnet\", \"region\": \"us-east-1\", \"input_tokens\": 1000000, \"output_tokens\": 1000000}\n            {\"provider\": \"gcp\",   \"domain\": \"compute\",     \"resource_type\": \"n1-standard-4\",   \"region\": \"us-central1\", \"term\": \"cud_1yr\"}\n            {\"provider\": \"gcp\",   \"domain\": \"analytics\",   \"service\": \"bigquery\", \"query_tb\": 10.0, \"active_storage_gb\": 500.0, \"region\": \"us\"}\n            {\"provider\": \"azure\", \"domain\": \"compute\",     \"resource_type\": \"Standard_D4s_v3\", \"region\": \"eastus\"}\n            {\"provider\": \"aws\",   \"domain\": \"database\",    \"service\": \"rds\", \"resource_type\": \"db.r5.large\", \"engine\": \"MySQL\", \"deployment\": \"single-az\", \"region\": \"us-east-1\"}\n        "

	descGetPricesBatch = "\n        Get prices for multiple compute instance types in a single region in one call.\n\n        Fetches all prices concurrently. Useful for comparing a shortlist of candidate\n        instance types (e.g. m5.xlarge vs c5.xlarge vs r5.xlarge) without separate calls.\n\n        Args:\n            provider: Cloud provider — \"aws\", \"gcp\", or \"azure\"\n            instance_types: List of instance types, e.g. [\"m5.xlarge\", \"c5.xlarge\", \"r5.large\"]\n            region: Region code, e.g. \"us-east-1\" or \"us-central1\"\n            os: Operating system — \"Linux\" (default) or \"Windows\"\n            term: Pricing term — \"on_demand\" (default), \"spot\", \"reserved_1yr\", \"cud_1yr\"\n        "

	descComparePrices = "\n        Compare pricing for any service across multiple regions.\n\n        Fetches concurrently. Returns results sorted cheapest first, with % delta between\n        cheapest and most expensive. Optionally shows delta vs a baseline region.\n\n        Args:\n            spec: PricingSpec dict (same as get_price). The region field is overridden\n                  per comparison — you can pass any region in the spec.\n            regions: List of region codes to compare, e.g. [\"us-east-1\", \"eu-west-1\", \"ap-northeast-1\"]\n            baseline_region: Optional region for delta comparison, e.g. \"us-east-1\".\n        "

	descGetPriceBySKU = "\n        Resolve a raw AWS usage-type/SKU string — exactly as it appears in a Cost & Usage Report\n        (CUR) export, e.g. \"CAN1-BoxUsage:r5a.8xlarge\" — to a price, across one or more regions.\n\n        Use this instead of get_price/compare_prices when you have a raw billing export line item\n        (a \"UsageType\" or \"SKU\" column value) and need to reconcile it against current public\n        pricing, rather than starting from a known resource_type/domain spec. This tool strips the\n        region-prefix token from the usage-type string (e.g. \"CAN1-\", \"EU-\", or no prefix at all\n        for us-east-1) to get a region-independent suffix, then matches that suffix against each\n        target region's pricing catalog.\n\n        If service is omitted, the AWS servicecode is inferred from the usage-type pattern (e.g.\n        \"BoxUsage:\" implies AmazonEC2, \"LCUUsage\" implies AWSELB) — service_source in the response\n        indicates \"explicit\" or \"inferred\". If a supplied service hint finds no match but the\n        inferred servicecode does (real CUR data isn't always internally consistent — e.g. data-\n        transfer usage types sometimes appear against an \"AmazonEC2\" AWS Product column but are\n        actually billed under AWSDataTransfer), the tool falls back to the inferred match and flags\n        service_mismatch on that region's result rather than reporting no match.\n\n        Some usage-type suffixes are shared by multiple distinct billable products (e.g. ELB's\n        \"LCUUsage\" suffix matches Application/Network/Gateway load balancer pricing alike; RDS's\n        \"InstanceUsage:<type>\" suffix matches every database engine on that instance type). When\n        that happens the affected region is reported under ambiguous_in (NOT in\n        all_regions_sorted/cheapest_price/most_expensive_price — an ambiguous multi-product match\n        is never silently resolved to \"cheapest\"), with every candidate row listed under\n        alternate_matches. Pass operation and/or product_family — the same columns a CUR export\n        carries alongside the usage-type/SKU column — to resolve it: e.g. for an Application Load\n        Balancer LCU usage-type, product_family=\"Load Balancer-Application\" picks the correct row\n        out of the Application/Network/Gateway alternatives.\n\n        Regions with no catalog entry for the resolved suffix are reported in no_mapping_in\n        (checked, not found) — this is distinct from errors_in (the catalog fetch itself failed)\n        and from ambiguous_in (matched, but more than one product row and not yet resolved).\n        Known limitation: compound inter-region/wavelength data-transfer SKUs with two region-\n        shaped tokens (e.g. \"USE1WL1ATL1-CAN1-AWS-Out-Bytes\") are not fully resolved by the\n        single-prefix-strip model; these produce a warning rather than a silently wrong match.\n\n        Args:\n            provider: Cloud provider — only \"aws\" is supported (raw usage-type SKUs are an AWS\n                      CUR concept with no GCP/Azure equivalent).\n            sku: The raw usage-type/SKU string exactly as it appears in the CUR export.\n            service: Optional AWS servicecode hint (e.g. \"AmazonEC2\", \"AWSELB\", \"AmazonRDS\",\n                     \"AmazonDynamoDB\", \"AmazonElastiCache\", \"AWSDataTransfer\"). If omitted, it is\n                     inferred from the usage-type pattern.\n            regions: List of AWS region codes to check, e.g. [\"us-east-1\", \"eu-west-1\"]. Required,\n                     max 30.\n            baseline_region: Optional region for delta comparison, e.g. \"us-east-1\".\n            operation: Optional disambiguating hint — the AWS product \"operation\" attribute (e.g.\n                       \"CreateDBInstance:0021\" identifies Aurora PostgreSQL among RDS engines on\n                       the same instance type), matched case-insensitively. Use this when a region\n                       comes back in ambiguous_in.\n            product_family: Optional disambiguating hint — the AWS top-level \"productFamily\" (e.g.\n                            \"Load Balancer-Application\" for an ALB vs NLB/GLB), matched\n                            case-insensitively. Use this when a region comes back in ambiguous_in.\n\n        Examples:\n            {\"sku\": \"CAN1-BoxUsage:r5a.8xlarge\", \"regions\": [\"ca-central-1\", \"us-east-1\"]}\n            {\"sku\": \"CAN1-AWS-Out-Bytes\", \"service\": \"AmazonEC2\", \"regions\": [\"ca-central-1\"]}\n            {\"sku\": \"CAN1-LCUUsage\", \"service\": \"AWSELB\", \"product_family\": \"Load Balancer-Application\",\n             \"regions\": [\"ca-central-1\", \"us-east-1\"]}\n        "

	descGetPricesBySKU = "\n        Batch form of get_price_by_sku: resolve many raw AWS usage-type/SKU strings — each exactly\n        as it appears in a Cost & Usage Report (CUR) export — against the same set of target\n        regions in one call.\n\n        Use this to reconcile many CUR line items at once (e.g. every distinct usage-type/SKU in a\n        monthly export) instead of issuing one get_price_by_sku call per SKU. Each sku is resolved\n        independently via the same logic get_price_by_sku uses, so per-region ambiguous_in/\n        no_mapping_in/errors_in bucketing and baseline_region deltas all apply per sku exactly as\n        they would in a standalone get_price_by_sku call — this tool only adds the batching and\n        aggregation layer on top.\n\n        service/operation/product_family hints are not supported here (they are inherently\n        per-sku, and different SKUs in a batch usually resolve to different services) — the AWS\n        servicecode is inferred per sku from its usage-type pattern. If a particular sku needs a\n        hint to resolve an ambiguous_in entry, follow up with a single get_price_by_sku call for\n        that sku, passing operation/product_family.\n\n        Each successfully-processed sku appears in \"results\", in the same order as the input skus\n        list (NOT re-sorted by price — distinct SKUs commonly price in different units, e.g.\n        per-hour vs per-GB vs per-request, that are not meaningfully comparable). A sku that fails\n        outright (e.g. an empty string, or a usage-type pattern no service could be inferred for)\n        is instead reported in the top-level \"errors\" map, keyed by that sku string, with\n        message/status/retryable fields mirroring get_prices_batch's per-item error shape.\n\n        Args:\n            provider: Cloud provider — only \"aws\" is supported (raw usage-type SKUs are an AWS\n                      CUR concept with no GCP/Azure equivalent).\n            skus: List of raw usage-type/SKU strings, each exactly as it appears in the CUR\n                  export. Required, max 25.\n            regions: List of AWS region codes to check, e.g. [\"us-east-1\", \"eu-west-1\"]. Required,\n                     max 30 (applies to every sku).\n            baseline_region: Optional region for delta comparison, applied to every sku,\n                             e.g. \"us-east-1\".\n\n        Examples:\n            {\"skus\": [\"CAN1-BoxUsage:r5a.8xlarge\", \"USW2-BoxUsage:m5.large\"], \"regions\": [\"us-east-1\", \"ca-central-1\"]}\n        "

	descSearchPricing = "Deprecated helper that redirects to the correct tools. Use describe_catalog to browse available services by domain/provider, or get_price with a known spec."

	descGetDiscountSummary = "\n        Return a summary of all active cloud discounts for the authenticated account.\n\n        For AWS: active Savings Plans (type, commitment $/hr, utilization %) and\n        active Reserved Instances (instance type, count, payment type, days remaining),\n        plus Cost Explorer utilization for the previous month.\n\n        Requires credentials and OCC_AWS_ENABLE_COST_EXPLORER=true for AWS.\n\n        Args:\n            provider: Cloud provider — \"aws\" (GCP CUD support coming later)\n        "

	descGetSpotHistory = "get_spot_history is not available. For spot pricing, use get_price with term=\"spot\" to look up current spot prices, or use list_instance_types to browse instance families and their spot price ranges."

	descRefreshCache = "\n        Invalidate the pricing cache to force fresh data on next request.\n\n        Args:\n            provider: Provider to clear (\"aws\", \"gcp\", \"azure\"), or empty string to purge expired entries.\n        "

	descListRegions = "\n        List all regions where a cloud service is available for the given provider.\n\n        Args:\n            provider: Cloud provider — \"aws\", \"gcp\", or \"azure\"\n            domain: Domain filter — \"compute\" (default), \"storage\", \"database\"\n        "

	descListInstanceTypes = "\n        List available compute instance types matching the given filters.\n\n        Args:\n            provider: Cloud provider — \"aws\", \"gcp\", or \"azure\"\n            region: Region code, e.g. \"us-east-1\" (AWS), \"us-central1\" (GCP), \"eastus\" (Azure)\n            family: Instance family prefix filter, e.g. \"m5\" (AWS), \"n2\" (GCP)\n            min_vcpu: Minimum vCPU count filter\n            min_memory_gb: Minimum memory in GB filter\n            gpu: If True, only return GPU-enabled instance types\n        "

	descDescribeCatalog = "\n        Discover what each provider supports and how to call get_price.\n\n        - No args → full support matrix across all configured providers.\n        - provider only → all domains/services for that provider.\n        - provider + domain [+ service] → targeted guidance with required_fields,\n          supported_terms, filter_hints, and a ready-to-use example_invocation\n          you can pass directly to get_price.\n\n        Use this before get_price when unsure of exact field names or values.\n\n        Args:\n            provider: Cloud provider — \"aws\", \"gcp\", or \"azure\". Empty = all providers.\n            domain: Domain — \"compute\", \"storage\", \"database\", \"ai\", \"container\",\n                    \"serverless\", \"analytics\", \"network\", \"observability\". Empty = all.\n            service: Service — e.g. \"bedrock\", \"rds\", \"gke\", \"bigquery\". Empty = all.\n        "

	descCompareBOMRegions = "\n        Compare a Bill of Materials' total monthly cost across multiple AWS regions.\n\n        v1 scope: AWS-only. Each item is an open PricingSpec dict, same shape as\n        estimate_bom's items (provider, domain, resource_type/region/etc, plus\n        quantity/hours_per_month/size_gb/description). The region field on each\n        item is overridden per comparison — pass any region in the item dicts.\n        Non-AWS items are reported once under \"not_supported\" rather than\n        guessed or dropped silently; GCP/Azure support is tracked separately.\n\n        Returns regions[] sorted cheapest-first, each with total_monthly, the\n        resolved line_items, and any per-item errors. Optionally shows delta vs\n        a baseline region.\n\n        Args:\n            items: List of PricingSpec dicts (same shape as estimate_bom).\n            regions: List of AWS region codes to compare, e.g. [\"us-east-1\", \"eu-west-1\"].\n            baseline_region: Optional region for delta comparison, e.g. \"us-east-1\".\n        "

	descGetCoverage = "\n        Report which domains/services this server actually covers, per provider.\n\n        v1 scope: structural coverage from the catalog only — each domain is\n        reported as \"catalog\" (with its known services) unless the provider\n        has no entry for it at all. This does NOT fan out a live get_price call\n        per region — whether a specific region's live price is a real catalog\n        rate or a degraded fallback constant is only observable by calling\n        get_price for that spec and checking its \"fallback\" field, since that\n        is a live fetch outcome rather than a fixed property of the catalog.\n\n        Use this to answer \"what does this server know about\" before trial-\n        and-error against describe_catalog and individual get_price calls.\n\n        Args:\n            provider: Cloud provider — \"aws\", \"gcp\", or \"azure\". Empty = all\n                      configured providers.\n        "

	descFindCheapestRegion = "\n        Find the cheapest region for any cloud service.\n\n        Queries pricing concurrently across regions and returns results sorted cheapest\n        first, with the price delta between cheapest and most expensive regions.\n\n        Args:\n            spec: PricingSpec dict (same as get_price). The region field is overridden\n                  for each comparison — pass any region in the spec.\n            regions: List of region codes to compare. Omit for major regions (faster).\n                     Pass [\"all\"] to search every available region (slow on first run without cache).\n            baseline_region: Optional region for delta comparison, e.g. \"us-east-1\".\n        "

	descFindAvailableRegions = "\n        Find all regions where a specific service/instance type is available, cheapest first.\n\n        All fields must be nested under \"spec\" — do not pass provider/domain/resource_type\n        etc. as top-level arguments. Example call:\n          {\"spec\": {\"provider\": \"aws\", \"domain\": \"compute\", \"resource_type\": \"m5.xlarge\", \"region\": \"us-east-1\"}}\n\n        Args:\n            spec: PricingSpec dict (same as get_price). The region field is overridden\n                  per comparison — pass any region in the spec.\n            regions: Region codes to check. Omit for major regions.\n                     Pass [\"all\"] to search every available region.\n            baseline_region: Optional region for delta comparison.\n        "

	descCacheStats = "Return statistics about the local pricing cache (entry counts, DB size), including a per-provider/service breakdown and as_of age of the most recent write."

	descWarmCache = "\n        Pre-populate the pricing cache for a provider before a large sweep (e.g. a\n        multi-region compare_prices or get_prices_batch call), so that sweep hits a warm\n        cache instead of paying fetch latency on every combination.\n\n        Resolves each requested service to its catalog example_invocation (the same data\n        describe_catalog returns) and fans the resulting spec x region combinations out\n        concurrently, mirroring the compare_prices/get_prices_batch fan-out pattern.\n\n        Args:\n            provider: Cloud provider — \"aws\", \"gcp\", or \"azure\"\n            regions: List of region codes to warm, e.g. [\"us-east-1\", \"eu-west-1\"]\n            services: Optional list of service names or describe_catalog keys, e.g.\n                      [\"ec2\", \"rds\", \"compute/fargate\"]. Omit to warm every service the\n                      provider's catalog has an example invocation for.\n        "

	descEstimateBOM = "\n        Use this tool for total infrastructure cost, TCO, monthly spend for a multi-resource\n        stack, or cost comparison between architectures.\n\n        Handles compute + storage + database + AI together in a single call — do NOT call\n        get_price individually for multi-resource questions; use this tool instead.\n\n        Returns per-item and total monthly/annual costs with real public pricing data,\n        plus a not_included list of supplementary costs (egress, load balancers, monitoring).\n        These are SUPPLEMENTARY — only price them if the user asked for TCO; for most\n        questions just note 'additional costs may apply'.\n\n        Each item should be a PricingSpec dict PLUS a quantity field:\n          - provider: \"aws\" | \"gcp\" | \"azure\"\n          - domain: \"compute\" | \"storage\" | \"database\" | \"ai\" | ...\n          - region: region code\n          - quantity: number of units (default 1)\n          - hours_per_month: hours/month for compute (default 730 = always-on)\n          - description: optional label for this line item\n          Plus domain-specific fields (see get_price or describe_catalog for details).\n\n        Examples:\n          Compute + database + storage on AWS:\n          [\n            {\"provider\": \"aws\", \"domain\": \"compute\", \"resource_type\": \"m5.xlarge\",   \"region\": \"us-east-1\", \"quantity\": 3},\n            {\"provider\": \"aws\", \"domain\": \"database\", \"service\": \"rds\", \"resource_type\": \"db.r6g.large\", \"engine\": \"MySQL\", \"deployment\": \"single-az\", \"region\": \"us-east-1\"},\n            {\"provider\": \"aws\", \"domain\": \"storage\",  \"storage_type\": \"gp3\", \"size_gb\": 500, \"region\": \"us-east-1\"}\n          ]\n\n          Mixed cloud:\n          [\n            {\"provider\": \"gcp\",   \"domain\": \"compute\", \"resource_type\": \"n1-standard-4\", \"region\": \"us-central1\", \"quantity\": 2},\n            {\"provider\": \"azure\", \"domain\": \"compute\", \"resource_type\": \"Standard_D4s_v3\", \"region\": \"eastus\", \"quantity\": 1}\n          ]\n        "

	descEstimateUnitEconomics = "\n        Estimate per-unit economics (cost per user, per request, per transaction) given\n        a Bill of Materials and expected monthly usage volume.\n\n        Args:\n            items: Same format as estimate_bom — list of cloud resource PricingSpec dicts\n                   plus quantity field. See estimate_bom for full item format.\n            units_per_month: Monthly volume being measured (e.g. 10000 users)\n            unit_label: What the unit represents — \"user\", \"request\", \"transaction\", etc.\n        "

	descCompareBOM = `Price a multi-service workload across multiple cloud providers simultaneously and return a side-by-side cost comparison. Use this when the user wants to compare total costs across AWS, GCP, and/or Azure for the same infrastructure.

OUTPUT FORMAT — aggregate totals only: for each workload key, storage capacity, provisioned IOPS, and provisioned throughput costs are summed into ONE number in the breakdown map. This tool does NOT return separate line items for storage $, IOPS $, and throughput $. If the user asks for a cost breakdown with storage capacity, provisioned IOPS, and provisioned throughput as separate line items per disk, use estimate_bom instead — it returns one row per price component.

Storage: accepts abstract tiers ("ssd" → gp3/pd-ssd/premium-ssd, "hdd" → sc1/pd-standard/standard-hdd) or provider-specific types (gp3, io2, sc1, pd-ssd, pd-extreme, hyperdisk-extreme, etc.) with iops and throughput_mbps for IOPS pricing. Use compare_bom when a provider-vs-provider total-cost summary is sufficient.

Returns per-provider totals keyed by pricing term, a breakdown map (workload_key → aggregate monthly $), committed vs on-demand savings, and any supplementary costs not included in the estimate.

The workload is described in cloud-agnostic terms (vcpus, memory_gb, storage_gb) — the tool selects the closest equivalent instance type per provider automatically.

Args:
    providers: Which providers to compare — ["aws", "gcp", "azure"] (default: all three).
    region_preference: Region tier — "us" (default), "eu", "apac".
    workload: Map of logical name → resource spec. Each spec needs 'type' (compute/storage/database/cache) plus vcpus, memory_gb, quantity, etc.
    terms: Pricing terms — default ["on_demand", "reserved_1yr"]. Term translation is automatic: reserved_1yr maps to cud_1yr for GCP.

Example:
    workload: {
      "web_servers": {"type": "compute", "vcpus": 4, "memory_gb": 16, "quantity": 3},
      "database":    {"type": "database", "vcpus": 8, "memory_gb": 32},
      "storage":     {"type": "storage", "storage_gb": 500, "storage_type": "ssd"}
    }

  Multi-disk storage (gp3/io2 vs pd-ssd/pd-extreme):
    providers:["aws","gcp"], workload:{"p_a":{"type":"storage","storage_gb":10000,"storage_type":"gp3","iops":3000},"p_c":{"type":"storage","storage_gb":500,"storage_type":"io2","iops":64000}}`
)

// ---- registerTools registers all 17 MCP tools on the server ----
//
// Each handler is a typed ToolHandlerFor[In, any] closure that:
//   1. Calls s.callTool() to apply timeout, logging, and panic recovery.
//   2. Delegates to the corresponding tools.Handler method.

func (s *AppServer) registerTools(srv *mcp.Server) {
	h := s.handler

	// ---- Lookup tools ----

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "get_price",
		Description:  descGetPrice,
		InputSchema:  rawSchema(schemaGetPrice),
		OutputSchema: rawSchema(schemaGetPriceOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.GetPriceInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "get_price", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleGetPrice(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "get_prices_batch",
		Description:  descGetPricesBatch,
		InputSchema:  rawSchema(schemaGetPricesBatch),
		OutputSchema: rawSchema(schemaGetPricesBatchOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.GetPricesBatchInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "get_prices_batch", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleGetPricesBatch(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "compare_prices",
		Description:  descComparePrices,
		InputSchema:  rawSchema(schemaComparePrices),
		OutputSchema: rawSchema(schemaComparePricesOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.ComparePricesInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "compare_prices", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleComparePrices(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "get_price_by_sku",
		Description:  descGetPriceBySKU,
		InputSchema:  rawSchema(schemaGetPriceBySKU),
		OutputSchema: rawSchema(schemaGetPriceBySKUOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.GetPriceBySKUInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "get_price_by_sku", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleGetPriceBySKU(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "get_prices_by_sku",
		Description:  descGetPricesBySKU,
		InputSchema:  rawSchema(schemaGetPricesBySKU),
		OutputSchema: rawSchema(schemaGetPricesBySKUOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.GetPricesBySKUInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "get_prices_by_sku", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleGetPricesBySKU(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "search_pricing",
		Description:  descSearchPricing,
		InputSchema:  rawSchema(schemaSearchPricing),
		OutputSchema: rawSchema(schemaSearchPricingOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.SearchPricingInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "search_pricing", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleSearchPricing(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "describe_catalog",
		Description:  descDescribeCatalog,
		InputSchema:  rawSchema(schemaDescribeCatalog),
		OutputSchema: rawSchema(schemaDescribeCatalogOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.DescribeCatalogInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "describe_catalog", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleDescribeCatalog(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "get_coverage",
		Description:  descGetCoverage,
		InputSchema:  rawSchema(schemaGetCoverage),
		OutputSchema: rawSchema(schemaGetCoverageOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.GetCoverageInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "get_coverage", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleGetCoverage(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "get_spot_history",
		Description:  descGetSpotHistory,
		InputSchema:  rawSchema(schemaGetSpotHistory),
		OutputSchema: rawSchema(schemaGetSpotHistoryOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.SpotHistoryStubInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "get_spot_history", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleSpotHistoryStub(ctx, req, in)
		})
	})

	// ---- FinOps tools ----

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "get_discount_summary",
		Description:  descGetDiscountSummary,
		InputSchema:  rawSchema(schemaGetDiscountSummary),
		OutputSchema: rawSchema(schemaGetDiscountSummaryOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.GetDiscountSummaryInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "get_discount_summary", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleGetDiscountSummary(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "estimate_bom",
		Description:  descEstimateBOM,
		InputSchema:  rawSchema(schemaEstimateBOM),
		OutputSchema: rawSchema(schemaEstimateBOMOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.EstimateBOMInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "estimate_bom", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleEstimateBOM(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "estimate_unit_economics",
		Description:  descEstimateUnitEconomics,
		InputSchema:  rawSchema(schemaEstimateUnitEconomics),
		OutputSchema: rawSchema(schemaEstimateUnitEconomicsOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.EstimateUnitEconomicsInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "estimate_unit_economics", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleEstimateUnitEconomics(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "compare_bom",
		Description:  descCompareBOM,
		InputSchema:  rawSchema(schemaCompareBOM),
		OutputSchema: rawSchema(schemaCompareBOMOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.CompareBOMInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "compare_bom", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleCompareBOM(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "compare_bom_regions",
		Description:  descCompareBOMRegions,
		InputSchema:  rawSchema(schemaCompareBOMRegions),
		OutputSchema: rawSchema(schemaCompareBOMRegionsOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.CompareBOMRegionsInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "compare_bom_regions", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleCompareBOMRegions(ctx, req, in)
		})
	})

	// ---- Cache tools ----

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "refresh_cache",
		Description:  descRefreshCache,
		InputSchema:  rawSchema(schemaRefreshCache),
		OutputSchema: rawSchema(schemaRefreshCacheOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.RefreshCacheInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "refresh_cache", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleRefreshCache(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "cache_stats",
		Description:  descCacheStats,
		InputSchema:  rawSchema(schemaCacheStats),
		OutputSchema: rawSchema(schemaCacheStatsOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.CacheStatsInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "cache_stats", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleCacheStats(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "warm_cache",
		Description:  descWarmCache,
		InputSchema:  rawSchema(schemaWarmCache),
		OutputSchema: rawSchema(schemaWarmCacheOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.WarmCacheInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "warm_cache", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleWarmCache(ctx, req, in)
		})
	})

	// ---- Availability / discovery tools ----

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "list_regions",
		Description:  descListRegions,
		InputSchema:  rawSchema(schemaListRegions),
		OutputSchema: rawSchema(schemaListRegionsOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.ListRegionsInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "list_regions", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleListRegions(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "list_instance_types",
		Description:  descListInstanceTypes,
		InputSchema:  rawSchema(schemaListInstanceTypes),
		OutputSchema: rawSchema(schemaListInstanceTypesOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.ListInstanceTypesInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "list_instance_types", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleListInstanceTypes(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "find_cheapest_region",
		Description:  descFindCheapestRegion,
		InputSchema:  rawSchema(schemaFindCheapestRegion),
		OutputSchema: rawSchema(schemaFindCheapestRegionOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.FindCheapestRegionInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "find_cheapest_region", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleFindCheapestRegion(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:         "find_available_regions",
		Description:  descFindAvailableRegions,
		InputSchema:  rawSchema(schemaFindAvailableRegions),
		OutputSchema: rawSchema(schemaFindAvailableRegionsOutput),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.FindAvailableRegionsInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "find_available_regions", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleFindAvailableRegions(ctx, req, in)
		})
	})
}
