// search_pricing.go implements a graceful stub for the search_pricing MCP tool.
//
// The model (qwen3.6-35b-128k) occasionally hallucinates a tool called
// "search_pricing" and emits it as XML rather than a proper function call.
// Registering a real stub converts that XML emission into a proper tool
// response the model can recover from, eliminating a class of stochastic
// XML hallucination failures.
//
// The stub ignores all input and returns structured alternatives pointing
// the model to describe_catalog, get_price, and estimate_bom.
package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SearchPricingInput is the (empty) typed input for the search_pricing stub.
// All fields in the caller's request are silently ignored; the stub always
// returns the same redirect response regardless of input.
type SearchPricingInput struct{}

// HandleSearchPricing implements the search_pricing stub tool.
//
// Returns a structured error with alternatives so the model can recover
// immediately without emitting invalid XML or retrying with the same call.
func (h *Handler) HandleSearchPricing(
	_ context.Context,
	_ *mcp.CallToolRequest,
	_ SearchPricingInput,
) (*mcp.CallToolResult, any, error) {
	return jsonText(map[string]any{
		"error":   "search_pricing_unavailable",
		"message": "search_pricing is deprecated and does not perform a search; use one of the alternatives below.",
		"alternatives": map[string]any{
			"browse_catalog":    "Use describe_catalog with domain and provider to list available services and their specs",
			"price_known_service": "Use get_price with a complete spec including domain, provider, and resource_type",
			"estimate_workload": "Use estimate_bom to price a multi-service workload",
		},
	}), nil, nil
}
