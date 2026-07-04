// search_pricing_test.go tests the search_pricing stub tool handler.
package tools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/tools"
)

// callSearchPricing invokes HandleSearchPricing and decodes the response.
func callSearchPricing(t *testing.T, h *tools.Handler) map[string]any {
	t.Helper()
	result, _, err := h.HandleSearchPricing(context.Background(), nil, tools.SearchPricingInput{})
	if err != nil {
		t.Fatalf("HandleSearchPricing returned err: %v", err)
	}
	return decodeResult(t, result)
}

// TestSearchPricingStub_ReturnsUnavailableError verifies the stub always
// returns the "search_pricing_unavailable" error regardless of input.
func TestSearchPricingStub_ReturnsUnavailableError(t *testing.T) {
	h := tools.New(nil)
	resp := callSearchPricing(t, h)

	if resp["error"] != "search_pricing_unavailable" {
		t.Errorf("error: got %v, want search_pricing_unavailable", resp["error"])
	}
}

// TestSearchPricingStub_MessagePresent verifies the response includes a message.
func TestSearchPricingStub_MessagePresent(t *testing.T) {
	h := tools.New(nil)
	resp := callSearchPricing(t, h)

	msg, ok := resp["message"].(string)
	if !ok || msg == "" {
		t.Errorf("expected non-empty message string, got: %v", resp["message"])
	}
}

// TestSearchPricingStub_MessageIsDeprecationFramed verifies the message text
// frames the tool as deprecated rather than nonexistent/broken, matching the
// honest "Deprecated helper" tool-list description instead of contradicting it.
func TestSearchPricingStub_MessageIsDeprecationFramed(t *testing.T) {
	h := tools.New(nil)
	resp := callSearchPricing(t, h)

	const want = "search_pricing is deprecated and does not perform a search; use one of the alternatives below."
	msg, _ := resp["message"].(string)
	if msg != want {
		t.Errorf("message: got %q, want %q", msg, want)
	}
	if strings.Contains(msg, "does not exist") {
		t.Errorf("message still uses existential/broken-tool wording: %q", msg)
	}
}

// TestSearchPricingStub_AlternativesPresent verifies the response includes the
// alternatives map pointing the model to the correct tools.
func TestSearchPricingStub_AlternativesPresent(t *testing.T) {
	h := tools.New(nil)
	resp := callSearchPricing(t, h)

	alts, ok := resp["alternatives"].(map[string]any)
	if !ok {
		t.Fatalf("expected alternatives map, got: %T %v", resp["alternatives"], resp["alternatives"])
	}
	if _, ok := alts["browse_catalog"]; !ok {
		t.Error("alternatives missing browse_catalog key")
	}
	if _, ok := alts["price_known_service"]; !ok {
		t.Error("alternatives missing price_known_service key")
	}
	if _, ok := alts["estimate_workload"]; !ok {
		t.Error("alternatives missing estimate_workload key")
	}
}

// TestSearchPricingStub_DoesNotPanic verifies that the stub does not panic
// regardless of whether providers are configured.
func TestSearchPricingStub_DoesNotPanic(t *testing.T) {
	// Without any providers.
	h := tools.New(nil)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("HandleSearchPricing panicked: %v", r)
			}
		}()
		callSearchPricing(t, h)
	}()

	// With a provider configured.
	pvdr := &mockProvider{name: "aws", defaultRegion: "us-east-1"}
	h2 := tools.New(map[string]tools.Provider{"aws": pvdr})
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("HandleSearchPricing panicked with provider: %v", r)
			}
		}()
		callSearchPricing(t, h2)
	}()
}
