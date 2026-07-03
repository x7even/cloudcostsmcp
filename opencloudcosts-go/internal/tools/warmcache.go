// warmcache.go implements the warm_cache MCP tool.
//
// warm_cache pre-populates the pricing cache for a provider across a set of
// regions (and, optionally, a set of services) before a large sweep (e.g. a
// multi-region compare_prices or get_prices_batch call), so that sweep hits
// a warm cache instead of paying the fetch latency on every combination.
//
// It resolves each requested service to a catalog example_invocation (the
// same data describe_catalog uses) and fans the resulting spec x region
// combinations out to the provider's GetPrice, mirroring the concurrency
// pattern used by compare_prices/get_prices_batch (internal/tools/lookup.go).
// The prices returned are discarded — the only externally visible effect is
// that the provider's own cache.Set calls, made internally while resolving
// each price, populate the shared CacheManager.
package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/utils"
)

// warmCacheConcurrency bounds the number of in-flight GetPrice calls, matching
// the semaphore size used by compare_prices (internal/tools/lookup.go).
const warmCacheConcurrency = 10

// WarmCacheInput is the typed input for the warm_cache tool.
type WarmCacheInput struct {
	Provider string   `json:"provider"`
	Regions  []string `json:"regions"`
	Services []string `json:"services"`
}

// warmCacheTarget is one resolved (domain, service) example spec to warm
// across every requested region.
type warmCacheTarget struct {
	domain  string
	service string
	spec    map[string]any
}

// resolveServiceExample resolves a caller-supplied service token to a catalog
// example_invocation. requested may be:
//   - an exact describe_catalog key ("compute", "compute/fargate", "database/rds", ...)
//   - a bare service name that appears somewhere in catalog.Services (e.g. "ec2",
//     "rds"), which is then resolved via its domain's service-level example,
//     falling back to the domain-level example (mirrors HandleDescribeCatalog's
//     own service->domain fallback behaviour).
//
// Returns ok=false if the token cannot be resolved to any example_invocation.
func resolveServiceExample(catalog *models.ProviderCatalog, requested string) (domain, service string, spec map[string]any, ok bool) {
	req := strings.TrimSpace(requested)
	if req == "" {
		return "", "", nil, false
	}

	// 1. Exact key match against example_invocations (covers both bare domain
	// keys like "compute" and compound keys like "compute/csp").
	if ex, found := catalog.ExampleInvocations[req]; found {
		d, s := req, ""
		if before, after, found := strings.Cut(req, "/"); found {
			d, s = before, after
		}
		return d, s, ex, true
	}

	// 2. Bare service name: search every domain's service list.
	for d, svcs := range catalog.Services {
		for _, s := range svcs {
			if !strings.EqualFold(s, req) {
				continue
			}
			if ex, found := catalog.ExampleInvocations[d+"/"+s]; found {
				return d, s, ex, true
			}
			if ex, found := catalog.ExampleInvocations[d]; found {
				return d, s, ex, true
			}
			return d, s, nil, false
		}
	}

	return "", "", nil, false
}

// allWarmCacheTargets builds one target per (domain, service) pair known to
// the catalog, deduplicated by the example_invocation key actually used (so a
// domain whose services all fall back to the same domain-level example is
// only warmed once). Domains with no services of their own (e.g.
// "inter_region_egress") are included as a single domain-only target when a
// domain-level example exists.
func allWarmCacheTargets(catalog *models.ProviderCatalog) []warmCacheTarget {
	domains := make([]string, 0, len(catalog.Services))
	for d := range catalog.Services {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	var targets []warmCacheTarget
	seenKeys := make(map[string]bool)

	for _, d := range domains {
		svcs := append([]string(nil), catalog.Services[d]...)
		sort.Strings(svcs)

		if len(svcs) == 0 {
			if ex, found := catalog.ExampleInvocations[d]; found && !seenKeys[d] {
				seenKeys[d] = true
				targets = append(targets, warmCacheTarget{domain: d, spec: ex})
			}
			continue
		}

		for _, s := range svcs {
			key := d + "/" + s
			ex, found := catalog.ExampleInvocations[key]
			resolvedKey := key
			if !found {
				ex, found = catalog.ExampleInvocations[d]
				resolvedKey = d
			}
			if !found || seenKeys[resolvedKey] {
				continue
			}
			seenKeys[resolvedKey] = true
			targets = append(targets, warmCacheTarget{domain: d, service: s, spec: ex})
		}
	}
	return targets
}

// HandleWarmCache implements the warm_cache MCP tool.
func (h *Handler) HandleWarmCache(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in WarmCacheInput,
) (*mcp.CallToolResult, any, error) {
	defer recoverToResult(ctx, "warm_cache")

	if h.cm == nil {
		return errResult(map[string]any{
			"error": "cache not configured",
		}), nil, nil
	}

	if in.Provider == "" {
		return errResult(map[string]any{
			"error": "provider is required.",
		}), nil, nil
	}

	pvdr := h.provider(in.Provider)
	if pvdr == nil {
		available := make([]string, 0, len(h.providers))
		for k := range h.providers {
			available = append(available, k)
		}
		sort.Strings(available)
		return errResult(map[string]any{
			"error": fmt.Sprintf("Provider '%s' not configured. Available: %v", in.Provider, available),
		}), nil, nil
	}

	if len(in.Regions) == 0 {
		return errResult(map[string]any{
			"error": "regions is required and must contain at least one region.",
		}), nil, nil
	}

	catalog, err := pvdr.DescribeCatalog(ctx)
	if err != nil {
		return errResult(map[string]any{
			"error":   "upstream_failure",
			"message": fmt.Sprintf("Failed to describe catalog for %s: %v", in.Provider, err),
		}), nil, nil
	}

	var targets []warmCacheTarget
	var skipped []string

	if len(in.Services) == 0 {
		targets = allWarmCacheTargets(catalog)
	} else {
		seen := make(map[string]bool)
		for _, requested := range in.Services {
			d, s, spec, ok := resolveServiceExample(catalog, requested)
			if !ok {
				skipped = append(skipped, requested)
				continue
			}
			key := d + "/" + s
			if seen[key] {
				continue
			}
			seen[key] = true
			targets = append(targets, warmCacheTarget{domain: d, service: s, spec: spec})
		}
	}

	type warmResult struct {
		region string
		target warmCacheTarget
		err    string
		status string // transient_error | no_data; only meaningful when err != ""
	}

	total := len(targets) * len(in.Regions)
	results := make([]warmResult, 0, total)
	var mu sync.Mutex
	sem := make(chan struct{}, warmCacheConcurrency)
	var wg sync.WaitGroup

	for _, tgt := range targets {
		for _, region := range in.Regions {
			wg.Add(1)
			go func(tgt warmCacheTarget, region string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				specMap := fillDomain(tgt.spec)
				specMap["provider"] = in.Provider
				specMap["region"] = region

				spec, err := unmarshalSpec(specMap)
				if err != nil {
					mu.Lock()
					results = append(results, warmResult{region: region, target: tgt, err: err.Error(), status: regionStatusNoData})
					mu.Unlock()
					return
				}

				_, err = pvdr.GetPrice(ctx, spec)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					// Classify like compare_prices/get_prices_batch (RC3-001):
					// ErrNotSupported and non-transient errors are treated as
					// no-data for this combination rather than retryable.
					status := regionStatusNoData
					if !errors.Is(err, providers.ErrNotSupported) && utils.IsTransient(err) {
						status = regionStatusTransient
					}
					results = append(results, warmResult{region: region, target: tgt, err: err.Error(), status: status})
					return
				}
				results = append(results, warmResult{region: region, target: tgt})
			}(tgt, region)
		}
	}
	wg.Wait()

	warmed := 0
	errMap := make(map[string]any)
	for _, r := range results {
		if r.err == "" {
			warmed++
			continue
		}
		key := fmt.Sprintf("%s:%s", r.region, r.target.domain)
		if r.target.service != "" {
			key = fmt.Sprintf("%s/%s", key, r.target.service)
		}
		errMap[key] = map[string]any{
			"message":   r.err,
			"status":    r.status,
			"retryable": r.status == regionStatusTransient,
		}
	}

	targetKeys := make([]string, 0, len(targets))
	for _, t := range targets {
		if t.service != "" {
			targetKeys = append(targetKeys, t.domain+"/"+t.service)
		} else {
			targetKeys = append(targetKeys, t.domain)
		}
	}
	sort.Strings(targetKeys)

	out := map[string]any{
		"provider":               in.Provider,
		"regions":                in.Regions,
		"targets_warmed":         targetKeys,
		"combinations_attempted": total,
		"warmed":                 warmed,
		"cache_entries_after":    h.cm.Stats().EntryCount,
	}
	if len(skipped) > 0 {
		out["skipped_services"] = skipped
	}
	if len(errMap) > 0 {
		out["errors"] = errMap
	}
	return jsonText(out), nil, nil
}
