// warmcache_test.go tests the warm_cache tool handler.
package tools_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/tools"
)

// callWarmCache invokes HandleWarmCache and decodes the response.
func callWarmCache(t *testing.T, h *tools.Handler, in tools.WarmCacheInput) map[string]any {
	t.Helper()
	result, _, err := h.HandleWarmCache(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleWarmCache returned err: %v", err)
	}
	return decodeResult(t, result)
}

// warmCacheCatalog is a representative catalog exercising every resolution
// path warm_cache needs to handle:
//   - "compute" is domain-level only; both "ec2" (bare service) and "fargate"
//     (service-level example) live under it.
//   - "storage" has no per-service examples in Services (empty list) but does
//     have a domain-level example.
//   - "database/rds" has its own service-level example.
//   - "inter_region_egress" has no services at all (empty slice) but a
//     domain-level example — the empty-Services fallback path.
func warmCacheCatalog() *models.ProviderCatalog {
	return &models.ProviderCatalog{
		Provider: models.CloudProviderAWS,
		Domains: []models.PricingDomain{
			models.PricingDomainCompute, models.PricingDomainStorage,
			models.PricingDomainDatabase, models.PricingDomainInterRegionEgress,
		},
		Services: map[string][]string{
			"compute":             {"ec2", "fargate"},
			"storage":             {},
			"database":            {"rds"},
			"inter_region_egress": {},
		},
		ExampleInvocations: map[string]map[string]any{
			"compute": {
				"provider":      "aws",
				"domain":        "compute",
				"resource_type": "m5.xlarge",
				"region":        "us-east-1",
			},
			"compute/fargate": {
				"provider":  "aws",
				"domain":    "compute",
				"service":   "fargate",
				"vcpu":      2.0,
				"memory_gb": 4.0,
				"region":    "us-east-1",
			},
			"storage": {
				"provider":     "aws",
				"domain":       "storage",
				"storage_type": "gp3",
				"region":       "us-east-1",
			},
			"database/rds": {
				"provider":      "aws",
				"domain":        "database",
				"service":       "rds",
				"resource_type": "db.r5.large",
				"engine":        "MySQL",
				"deployment":    "single-az",
				"region":        "us-east-1",
			},
			"inter_region_egress": {
				"provider":      "aws",
				"domain":        "inter_region_egress",
				"source_region": "us-east-1",
				"dest_region":   "eu-west-1",
			},
		},
	}
}

// TestWarmCache_NoCacheConfigured verifies the error when no cache is wired.
func TestWarmCache_NoCacheConfigured(t *testing.T) {
	pvdr := &mockProvider{name: "aws"}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callWarmCache(t, h, tools.WarmCacheInput{Provider: "aws", Regions: []string{"us-east-1"}})
	if _, ok := resp["error"]; !ok {
		t.Error("expected error key when cache is not configured")
	}
}

// TestWarmCache_MissingProvider verifies the error when provider is empty.
func TestWarmCache_MissingProvider(t *testing.T) {
	cm, cleanup := newTestCache(t)
	defer cleanup()
	h := newHandlerWithCache(cm)
	resp := callWarmCache(t, h, tools.WarmCacheInput{Regions: []string{"us-east-1"}})
	if _, ok := resp["error"]; !ok {
		t.Error("expected error key when provider is empty")
	}
}

// TestWarmCache_UnconfiguredProvider verifies the error when the requested
// provider has no registered implementation.
func TestWarmCache_UnconfiguredProvider(t *testing.T) {
	cm, cleanup := newTestCache(t)
	defer cleanup()
	h := newHandlerWithCache(cm)
	resp := callWarmCache(t, h, tools.WarmCacheInput{Provider: "aws", Regions: []string{"us-east-1"}})
	if _, ok := resp["error"]; !ok {
		t.Error("expected error key for unconfigured provider")
	}
}

// TestWarmCache_MissingRegions verifies the error when regions is empty.
func TestWarmCache_MissingRegions(t *testing.T) {
	cm, cleanup := newTestCache(t)
	defer cleanup()
	pvdr := &mockProvider{name: "aws"}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	h.SetCache(cm)
	resp := callWarmCache(t, h, tools.WarmCacheInput{Provider: "aws"})
	if _, ok := resp["error"]; !ok {
		t.Error("expected error key when regions is empty")
	}
}

// TestWarmCache_AllServices verifies that omitting services warms every
// resolvable (domain, service) pair in the catalog across every region,
// deduplicating targets that share the same domain-level example, and that
// the fan-out actually invokes GetPrice once per (target, region) pair.
func TestWarmCache_AllServices(t *testing.T) {
	cm, cleanup := newTestCache(t)
	defer cleanup()

	var calls int32
	var mu sync.Mutex
	seenRegions := make(map[string]bool)

	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		describeCatFunc: func(_ context.Context) (*models.ProviderCatalog, error) {
			return warmCacheCatalog(), nil
		},
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			atomic.AddInt32(&calls, 1)
			mu.Lock()
			seenRegions[spec.GetRegion()] = true
			mu.Unlock()
			return &models.PricingResult{
				PublicPrices: []models.NormalizedPrice{{PricePerUnit: 0.1}},
				Source:       "test",
			}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	h.SetCache(cm)

	resp := callWarmCache(t, h, tools.WarmCacheInput{
		Provider: "aws",
		Regions:  []string{"us-east-1", "eu-west-1"},
	})

	if errVal, ok := resp["error"]; ok {
		t.Fatalf("unexpected error: %v", errVal)
	}

	// 5 distinct targets: compute/ec2 (falls back to the domain-level
	// example since "compute/ec2" has no dedicated one), compute/fargate
	// (its own service-level example), database/rds, and the two
	// domain-only services (storage, inter_region_egress) whose Services
	// list is empty — each x 2 regions.
	targets, ok := resp["targets_warmed"].([]any)
	if !ok {
		t.Fatalf("expected targets_warmed list, got %v", resp["targets_warmed"])
	}
	wantTargets := []string{"compute/ec2", "compute/fargate", "database/rds", "inter_region_egress", "storage"}
	if len(targets) != len(wantTargets) {
		t.Fatalf("targets_warmed = %v, want %d entries matching %v", targets, len(wantTargets), wantTargets)
	}

	wantCombinations := float64(len(wantTargets) * 2)
	if combos, _ := resp["combinations_attempted"].(float64); combos != wantCombinations {
		t.Errorf("combinations_attempted = %v, want %v", resp["combinations_attempted"], wantCombinations)
	}
	if warmed, _ := resp["warmed"].(float64); warmed != wantCombinations {
		t.Errorf("warmed = %v, want %v", resp["warmed"], wantCombinations)
	}
	if got := atomic.LoadInt32(&calls); got != int32(wantCombinations) {
		t.Errorf("GetPrice was called %d times, want %v", got, wantCombinations)
	}
	if len(seenRegions) != 2 || !seenRegions["us-east-1"] || !seenRegions["eu-west-1"] {
		t.Errorf("expected both regions to be queried, got %v", seenRegions)
	}
	if _, ok := resp["cache_entries_after"]; !ok {
		t.Error("expected cache_entries_after field")
	}
}

// TestWarmCache_SpecificServices verifies that a bare service name ("ec2")
// resolves via its domain's service list to the domain-level example
// invocation (since "compute/ec2" has no dedicated example), and that an
// exact describe_catalog key ("database/rds") resolves directly.
func TestWarmCache_SpecificServices(t *testing.T) {
	cm, cleanup := newTestCache(t)
	defer cleanup()

	var gotDomains []string
	var mu sync.Mutex

	pvdr := &mockProvider{
		name: "aws",
		describeCatFunc: func(_ context.Context) (*models.ProviderCatalog, error) {
			return warmCacheCatalog(), nil
		},
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			mu.Lock()
			gotDomains = append(gotDomains, string(spec.GetDomain()))
			mu.Unlock()
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{{PricePerUnit: 0.1}}}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	h.SetCache(cm)

	resp := callWarmCache(t, h, tools.WarmCacheInput{
		Provider: "aws",
		Regions:  []string{"us-east-1"},
		Services: []string{"ec2", "rds"},
	})

	if errVal, ok := resp["error"]; ok {
		t.Fatalf("unexpected error: %v", errVal)
	}
	if warmed, _ := resp["warmed"].(float64); warmed != 2 {
		t.Errorf("warmed = %v, want 2", resp["warmed"])
	}
	if len(gotDomains) != 2 {
		t.Fatalf("GetPrice called %d times, want 2 (got domains %v)", len(gotDomains), gotDomains)
	}
	foundCompute, foundDatabase := false, false
	for _, d := range gotDomains {
		if d == "compute" {
			foundCompute = true
		}
		if d == "database" {
			foundDatabase = true
		}
	}
	if !foundCompute || !foundDatabase {
		t.Errorf("expected one compute and one database call, got domains %v", gotDomains)
	}
}

// TestWarmCache_UnresolvedServiceIsSkipped verifies that a service token that
// cannot be resolved to any catalog example_invocation is reported under
// skipped_services rather than causing an error or a GetPrice call.
func TestWarmCache_UnresolvedServiceIsSkipped(t *testing.T) {
	cm, cleanup := newTestCache(t)
	defer cleanup()

	called := false
	pvdr := &mockProvider{
		name: "aws",
		describeCatFunc: func(_ context.Context) (*models.ProviderCatalog, error) {
			return warmCacheCatalog(), nil
		},
		getPriceFunc: func(_ context.Context, _ models.PricingSpec) (*models.PricingResult, error) {
			called = true
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{{PricePerUnit: 0.1}}}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	h.SetCache(cm)

	resp := callWarmCache(t, h, tools.WarmCacheInput{
		Provider: "aws",
		Regions:  []string{"us-east-1"},
		Services: []string{"not-a-real-service"},
	})

	skipped, ok := resp["skipped_services"].([]any)
	if !ok || len(skipped) != 1 || skipped[0] != "not-a-real-service" {
		t.Errorf("expected skipped_services = [not-a-real-service], got %v", resp["skipped_services"])
	}
	if combos, _ := resp["combinations_attempted"].(float64); combos != 0 {
		t.Errorf("combinations_attempted = %v, want 0", resp["combinations_attempted"])
	}
	if called {
		t.Error("GetPrice should not have been called for an unresolved service")
	}
}

// TestWarmCache_ErrorClassification verifies that a transient upstream error
// is reported as retryable and a permanent/not-supported error is not.
func TestWarmCache_ErrorClassification(t *testing.T) {
	cm, cleanup := newTestCache(t)
	defer cleanup()

	pvdr := &mockProvider{
		name: "aws",
		describeCatFunc: func(_ context.Context) (*models.ProviderCatalog, error) {
			return &models.ProviderCatalog{
				Services: map[string][]string{"compute": {"ec2"}},
				ExampleInvocations: map[string]map[string]any{
					"compute": {"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", "region": "us-east-1"},
				},
			}, nil
		},
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			if spec.GetRegion() == "us-east-1" {
				return nil, errors.New("connection reset by peer")
			}
			return nil, providers.ErrNotSupported
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	h.SetCache(cm)

	resp := callWarmCache(t, h, tools.WarmCacheInput{
		Provider: "aws",
		Regions:  []string{"us-east-1", "eu-west-1"},
	})

	if warmed, _ := resp["warmed"].(float64); warmed != 0 {
		t.Errorf("warmed = %v, want 0", resp["warmed"])
	}
	errs, ok := resp["errors"].(map[string]any)
	if !ok || len(errs) != 2 {
		t.Fatalf("expected 2 entries in errors, got %v", resp["errors"])
	}
	usEast, ok := errs["us-east-1:compute/ec2"].(map[string]any)
	if !ok {
		t.Fatalf("expected errors[us-east-1:compute/ec2], got %v", errs)
	}
	if retryable, _ := usEast["retryable"].(bool); !retryable {
		t.Errorf("expected us-east-1:compute/ec2 to be retryable (transient), got %v", usEast)
	}
	euWest, ok := errs["eu-west-1:compute/ec2"].(map[string]any)
	if !ok {
		t.Fatalf("expected errors[eu-west-1:compute/ec2], got %v", errs)
	}
	if retryable, _ := euWest["retryable"].(bool); retryable {
		t.Errorf("expected eu-west-1:compute/ec2 to be non-retryable (not_supported), got %v", euWest)
	}
}
