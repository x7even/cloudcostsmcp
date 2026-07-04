// coverage.go implements the get_coverage MCP tool.
//
// v1 scope (see docs/plans/RC3-038-get-coverage-design.md): structural
// coverage only, derived from DescribeCatalog(). It answers "does this
// server know about this domain/service at all," not "is today's live
// price for a given region a real catalog rate or a fallback constant" —
// that per-request signal is already surfaced on individual get_price
// responses via the "fallback" field and is not fanned out across every
// region here, since fallback is a live fetch outcome, not a static
// property of (domain, service, region).
package tools

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// GetCoverageInput is the typed input for the get_coverage tool.
type GetCoverageInput struct {
	Provider string `json:"provider"`
}

// coverageDomain describes one domain's coverage for a single provider.
type coverageDomain struct {
	Status   string   `json:"status"`
	Services []string `json:"services"`
	Note     string   `json:"note,omitempty"`
}

// buildCoverage composes a per-domain coverage map from a provider's catalog.
func buildCoverage(cat *models.ProviderCatalog) map[string]coverageDomain {
	out := make(map[string]coverageDomain, len(cat.Domains))
	for _, d := range cat.Domains {
		key := string(d)
		services := cat.Services[key]
		if len(services) == 0 {
			// Some providers (GCP, Azure inter_region_egress) hardcode an
			// empty services array for domains that are parameterized
			// rather than sub-serviced. The domain is still fully
			// functional (see AWS's fixed equivalent, RC3-020) — report it
			// as covered rather than absent.
			out[key] = coverageDomain{
				Status: "catalog",
				Note:   "parameterized domain — no discrete sub-services; see describe_catalog filter_hints/example_invocations",
			}
			continue
		}
		out[key] = coverageDomain{
			Status:   "catalog",
			Services: services,
		}
	}
	return out
}

// HandleGetCoverage implements the get_coverage tool.
func (h *Handler) HandleGetCoverage(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in GetCoverageInput,
) (*mcp.CallToolResult, any, error) {
	defer recoverToResult(ctx, "get_coverage")

	asOf := time.Now().UTC().Format(time.RFC3339)
	note := "v1: structural coverage from the catalog only (does not fan out live per-region fallback status — see describe_catalog + get_price's per-response \"fallback\" field for that)."

	if in.Provider != "" {
		pvdr := h.provider(in.Provider)
		if pvdr == nil {
			return errResult(map[string]any{
				"error": fmt.Sprintf("Provider '%s' not configured.", in.Provider),
			}), nil, nil
		}
		cat, err := pvdr.DescribeCatalog(ctx)
		if err != nil {
			return errResult(map[string]any{
				"error":   "upstream_failure",
				"message": fmt.Sprintf("Failed to describe catalog for %s: %v", in.Provider, err),
			}), nil, nil
		}
		return jsonText(map[string]any{
			"provider": in.Provider,
			"as_of":    asOf,
			"domains":  buildCoverage(cat),
			"note":     note,
		}), nil, nil
	}

	var pvdrNames []string
	for name := range h.providers {
		pvdrNames = append(pvdrNames, name)
	}
	sort.Strings(pvdrNames)

	matrix := make(map[string]any, len(pvdrNames))
	for _, pname := range pvdrNames {
		cat, err := h.providers[pname].DescribeCatalog(ctx)
		if err != nil {
			matrix[pname] = map[string]any{"error": err.Error()}
			continue
		}
		matrix[pname] = map[string]any{"domains": buildCoverage(cat)}
	}
	return jsonText(map[string]any{
		"as_of":    asOf,
		"coverage": matrix,
		"note":     note,
	}), nil, nil
}
