// spot_history.go implements a graceful stub for the get_spot_history MCP tool.
//
// When get_spot_history returns upstream_failure with retryable:true (no live
// AWS credentials available), the model enters a retry loop that terminates in
// raw XML hallucination after the harness loop-break threshold fires.
//
// This stub replaces that behavior with a structured redirect that sets
// retryable:false — stopping the retry loop immediately — and points the model
// to the correct alternative for spot pricing without live AWS auth.
//
// The stub ignores all input and returns the same redirect response regardless
// of instance type, region, or hours window.
package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SpotHistoryStubInput is the typed input for the get_spot_history stub.
// All fields in the caller's request are silently ignored; the stub always
// returns the same redirect response regardless of input.
type SpotHistoryStubInput struct{}

// HandleSpotHistoryStub implements the get_spot_history stub tool.
//
// Returns a structured error with retryable:false and actionable alternatives
// so the model can recover immediately without entering a retry loop or
// emitting invalid XML tool calls.
func (h *Handler) HandleSpotHistoryStub(
	_ context.Context,
	_ *mcp.CallToolRequest,
	_ SpotHistoryStubInput,
) (*mcp.CallToolResult, any, error) {
	return jsonText(map[string]any{
		"error":     "get_spot_history_unavailable",
		"retryable": false,
		"message":   "get_spot_history does not exist. Use get_price with term=spot for spot pricing.",
		"alternatives": map[string]any{
			"spot_price":      "Call get_price with your compute spec and term=\"spot\" to get current spot rates",
			"browse_instances": "Call list_instance_types to browse instance families including spot price ranges",
			"compare_spot":    "Call compare_bom with workload items to compare spot pricing across clouds",
		},
	}), nil, nil
}
