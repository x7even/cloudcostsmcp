// Package server wires the modelcontextprotocol/go-sdk MCP server, registers
// all 16 tools, and exposes RunStdio / RunHTTP transports.
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

// buildMCPServer constructs the mcp.Server with all 15 tools registered.
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

	descSearchPricing = "Deprecated helper that redirects to the correct tools. Use describe_catalog to browse available services by domain/provider, or get_price with a known spec."

	descGetDiscountSummary = "\n        Return a summary of all active cloud discounts for the authenticated account.\n\n        For AWS: active Savings Plans (type, commitment $/hr, utilization %) and\n        active Reserved Instances (instance type, count, payment type, days remaining),\n        plus Cost Explorer utilization for the previous month.\n\n        Requires credentials and OCC_AWS_ENABLE_COST_EXPLORER=true for AWS.\n\n        Args:\n            provider: Cloud provider — \"aws\" (GCP CUD support coming later)\n        "

	descGetSpotHistory = "get_spot_history is not available. For spot pricing, use get_price with term=\"spot\" to look up current spot prices, or use list_instance_types to browse instance families and their spot price ranges."

	descRefreshCache = "\n        Invalidate the pricing cache to force fresh data on next request.\n\n        Args:\n            provider: Provider to clear (\"aws\", \"gcp\", \"azure\"), or empty string to purge expired entries.\n        "

	descListRegions = "\n        List all regions where a cloud service is available for the given provider.\n\n        Args:\n            provider: Cloud provider — \"aws\", \"gcp\", or \"azure\"\n            domain: Domain filter — \"compute\" (default), \"storage\", \"database\"\n        "

	descListInstanceTypes = "\n        List available compute instance types matching the given filters.\n\n        Args:\n            provider: Cloud provider — \"aws\", \"gcp\", or \"azure\"\n            region: Region code, e.g. \"us-east-1\" (AWS), \"us-central1\" (GCP), \"eastus\" (Azure)\n            family: Instance family prefix filter, e.g. \"m5\" (AWS), \"n2\" (GCP)\n            min_vcpu: Minimum vCPU count filter\n            min_memory_gb: Minimum memory in GB filter\n            gpu: If True, only return GPU-enabled instance types\n        "

	descDescribeCatalog = "\n        Discover what each provider supports and how to call get_price.\n\n        - No args → full support matrix across all configured providers.\n        - provider only → all domains/services for that provider.\n        - provider + domain [+ service] → targeted guidance with required_fields,\n          supported_terms, filter_hints, and a ready-to-use example_invocation\n          you can pass directly to get_price.\n\n        Use this before get_price when unsure of exact field names or values.\n\n        Args:\n            provider: Cloud provider — \"aws\", \"gcp\", or \"azure\". Empty = all providers.\n            domain: Domain — \"compute\", \"storage\", \"database\", \"ai\", \"container\",\n                    \"serverless\", \"analytics\", \"network\", \"observability\". Empty = all.\n            service: Service — e.g. \"bedrock\", \"rds\", \"gke\", \"bigquery\". Empty = all.\n        "

	descFindCheapestRegion = "\n        Find the cheapest region for any cloud service.\n\n        Queries pricing concurrently across regions and returns results sorted cheapest\n        first, with the price delta between cheapest and most expensive regions.\n\n        Args:\n            spec: PricingSpec dict (same as get_price). The region field is overridden\n                  for each comparison — pass any region in the spec.\n            regions: List of region codes to compare. Omit for major regions (faster).\n                     Pass [\"all\"] to search every available region (slow on first run without cache).\n            baseline_region: Optional region for delta comparison, e.g. \"us-east-1\".\n        "

	descFindAvailableRegions = "\n        Find all regions where a specific service/instance type is available, cheapest first.\n\n        Args:\n            spec: PricingSpec dict (same as get_price). The region field is overridden\n                  per comparison — pass any region in the spec.\n            regions: Region codes to check. Omit for major regions.\n                     Pass [\"all\"] to search every available region.\n            baseline_region: Optional region for delta comparison.\n        "

	descCacheStats = "Return statistics about the local pricing cache (entry counts, DB size)."

	descEstimateBOM = "\n        Use this tool for total infrastructure cost, TCO, monthly spend for a multi-resource\n        stack, or cost comparison between architectures.\n\n        Handles compute + storage + database + AI together in a single call — do NOT call\n        get_price individually for multi-resource questions; use this tool instead.\n\n        Returns per-item and total monthly/annual costs with real public pricing data,\n        plus a not_included list of supplementary costs (egress, load balancers, monitoring).\n        These are SUPPLEMENTARY — only price them if the user asked for TCO; for most\n        questions just note 'additional costs may apply'.\n\n        Each item should be a PricingSpec dict PLUS a quantity field:\n          - provider: \"aws\" | \"gcp\" | \"azure\"\n          - domain: \"compute\" | \"storage\" | \"database\" | \"ai\" | ...\n          - region: region code\n          - quantity: number of units (default 1)\n          - hours_per_month: hours/month for compute (default 730 = always-on)\n          - description: optional label for this line item\n          Plus domain-specific fields (see get_price or describe_catalog for details).\n\n        Examples:\n          Compute + database + storage on AWS:\n          [\n            {\"provider\": \"aws\", \"domain\": \"compute\", \"resource_type\": \"m5.xlarge\",   \"region\": \"us-east-1\", \"quantity\": 3},\n            {\"provider\": \"aws\", \"domain\": \"database\", \"service\": \"rds\", \"resource_type\": \"db.r6g.large\", \"engine\": \"MySQL\", \"deployment\": \"single-az\", \"region\": \"us-east-1\"},\n            {\"provider\": \"aws\", \"domain\": \"storage\",  \"storage_type\": \"gp3\", \"size_gb\": 500, \"region\": \"us-east-1\"}\n          ]\n\n          Mixed cloud:\n          [\n            {\"provider\": \"gcp\",   \"domain\": \"compute\", \"resource_type\": \"n1-standard-4\", \"region\": \"us-central1\", \"quantity\": 2},\n            {\"provider\": \"azure\", \"domain\": \"compute\", \"resource_type\": \"Standard_D4s_v3\", \"region\": \"eastus\", \"quantity\": 1}\n          ]\n        "

	descEstimateUnitEconomics = "\n        Estimate per-unit economics (cost per user, per request, per transaction) given\n        a Bill of Materials and expected monthly usage volume.\n\n        Args:\n            items: Same format as estimate_bom — list of cloud resource PricingSpec dicts\n                   plus quantity field. See estimate_bom for full item format.\n            units_per_month: Monthly volume being measured (e.g. 10000 users)\n            unit_label: What the unit represents — \"user\", \"request\", \"transaction\", etc.\n        "

	descCompareBOM = "Price a multi-service workload across multiple cloud providers simultaneously " +
		"and return a side-by-side cost comparison. Use this when the user wants to compare " +
		"costs across AWS, GCP, and/or Azure for the same infrastructure.\n\n" +
		"Storage: accepts abstract tiers (\"ssd\" → gp3/pd-ssd/premium-ssd, " +
		"\"hdd\" → sc1/pd-standard/standard-hdd) or provider-specific types " +
		"(gp3, io2, pd-ssd, pd-extreme, etc.) with iops and throughput_mbps for IOPS pricing. " +
		"Preferred tool for cross-cloud block storage comparisons.\n\n" +
		"Returns per-provider totals keyed by pricing term, committed vs on-demand savings, " +
		"and any supplementary costs not included in the estimate.\n\n" +
		"The workload is described in cloud-agnostic terms (vcpus, memory_gb, storage_gb) — " +
		"the tool selects the closest equivalent instance type per provider automatically.\n\n" +
		"Args:\n" +
		"    providers: Which providers to compare — [\"aws\", \"gcp\", \"azure\"] (default: all three).\n" +
		"    region_preference: Region tier — \"us\" (default), \"eu\", \"apac\".\n" +
		"    workload: Map of logical name → resource spec. Each spec needs 'type' " +
		"(compute/storage/database/cache) plus vcpus, memory_gb, quantity, etc.\n" +
		"    terms: Pricing terms — default [\"on_demand\", \"reserved_1yr\"]. " +
		"Term translation is automatic: reserved_1yr maps to cud_1yr for GCP.\n\n" +
		"Example:\n" +
		"    workload: {\n" +
		"      \"web_servers\": {\"type\": \"compute\", \"vcpus\": 4, \"memory_gb\": 16, \"quantity\": 3},\n" +
		"      \"database\":    {\"type\": \"database\", \"vcpus\": 8, \"memory_gb\": 32},\n" +
		"      \"storage\":     {\"type\": \"storage\", \"storage_gb\": 500, \"storage_type\": \"ssd\"}\n" +
		"    }\n\n" +
		"  Multi-disk storage (gp3/io2 vs pd-ssd/pd-extreme):\n" +
		"    providers:[\"aws\",\"gcp\"], workload:{" +
		"\"p_a\":{\"type\":\"storage\",\"storage_gb\":10000,\"storage_type\":\"gp3\",\"iops\":3000}," +
		"\"p_c\":{\"type\":\"storage\",\"storage_gb\":500,\"storage_type\":\"io2\",\"iops\":64000}}"
)

// ---- registerTools registers all 16 MCP tools on the server ----
//
// Each handler is a typed ToolHandlerFor[In, any] closure that:
//   1. Calls s.callTool() to apply timeout, logging, and panic recovery.
//   2. Delegates to the corresponding tools.Handler method.

func (s *AppServer) registerTools(srv *mcp.Server) {
	h := s.handler

	// ---- Lookup tools ----

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_price",
		Description: descGetPrice,
		InputSchema: rawSchema(schemaGetPrice),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.GetPriceInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "get_price", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleGetPrice(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_prices_batch",
		Description: descGetPricesBatch,
		InputSchema: rawSchema(schemaGetPricesBatch),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.GetPricesBatchInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "get_prices_batch", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleGetPricesBatch(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "compare_prices",
		Description: descComparePrices,
		InputSchema: rawSchema(schemaComparePrices),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.ComparePricesInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "compare_prices", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleComparePrices(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "search_pricing",
		Description: descSearchPricing,
		InputSchema: rawSchema(schemaSearchPricing),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.SearchPricingInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "search_pricing", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleSearchPricing(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "describe_catalog",
		Description: descDescribeCatalog,
		InputSchema: rawSchema(schemaDescribeCatalog),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.DescribeCatalogInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "describe_catalog", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleDescribeCatalog(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_spot_history",
		Description: descGetSpotHistory,
		InputSchema: rawSchema(schemaGetSpotHistory),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.SpotHistoryStubInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "get_spot_history", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleSpotHistoryStub(ctx, req, in)
		})
	})

	// ---- FinOps tools ----

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_discount_summary",
		Description: descGetDiscountSummary,
		InputSchema: rawSchema(schemaGetDiscountSummary),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.GetDiscountSummaryInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "get_discount_summary", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleGetDiscountSummary(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "estimate_bom",
		Description: descEstimateBOM,
		InputSchema: rawSchema(schemaEstimateBOM),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.EstimateBOMInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "estimate_bom", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleEstimateBOM(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "estimate_unit_economics",
		Description: descEstimateUnitEconomics,
		InputSchema: rawSchema(schemaEstimateUnitEconomics),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.EstimateUnitEconomicsInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "estimate_unit_economics", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleEstimateUnitEconomics(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "compare_bom",
		Description: descCompareBOM,
		InputSchema: rawSchema(schemaCompareBOM),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.CompareBOMInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "compare_bom", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleCompareBOM(ctx, req, in)
		})
	})

	// ---- Cache tools ----

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "refresh_cache",
		Description: descRefreshCache,
		InputSchema: rawSchema(schemaRefreshCache),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.RefreshCacheInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "refresh_cache", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleRefreshCache(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "cache_stats",
		Description: descCacheStats,
		InputSchema: rawSchema(schemaCacheStats),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.CacheStatsInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "cache_stats", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleCacheStats(ctx, req, in)
		})
	})

	// ---- Availability / discovery tools ----

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_regions",
		Description: descListRegions,
		InputSchema: rawSchema(schemaListRegions),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.ListRegionsInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "list_regions", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleListRegions(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_instance_types",
		Description: descListInstanceTypes,
		InputSchema: rawSchema(schemaListInstanceTypes),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.ListInstanceTypesInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "list_instance_types", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleListInstanceTypes(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "find_cheapest_region",
		Description: descFindCheapestRegion,
		InputSchema: rawSchema(schemaFindCheapestRegion),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.FindCheapestRegionInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "find_cheapest_region", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleFindCheapestRegion(ctx, req, in)
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "find_available_regions",
		Description: descFindAvailableRegions,
		InputSchema: rawSchema(schemaFindAvailableRegions),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tools.FindAvailableRegionsInput) (*mcp.CallToolResult, any, error) {
		return s.callTool(ctx, "find_available_regions", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
			return h.HandleFindAvailableRegions(ctx, req, in)
		})
	})
}
