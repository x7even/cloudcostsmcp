// spot_history_test.go tests the get_spot_history stub tool handler.
package tools_test

import (
	"context"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/tools"
)

// callSpotHistoryStub invokes HandleSpotHistoryStub and decodes the response.
func callSpotHistoryStub(t *testing.T, h *tools.Handler) map[string]any {
	t.Helper()
	result, _, err := h.HandleSpotHistoryStub(context.Background(), nil, tools.SpotHistoryStubInput{})
	if err != nil {
		t.Fatalf("HandleSpotHistoryStub returned err: %v", err)
	}
	return decodeResult(t, result)
}

// TestSpotHistoryStub_ReturnsUnavailableError verifies the stub always
// returns the "get_spot_history_unavailable" error regardless of input.
func TestSpotHistoryStub_ReturnsUnavailableError(t *testing.T) {
	h := tools.New(nil)
	resp := callSpotHistoryStub(t, h)

	if resp["error"] != "get_spot_history_unavailable" {
		t.Errorf("error: got %v, want get_spot_history_unavailable", resp["error"])
	}
}

// TestSpotHistoryStub_RetryableIsFalse verifies retryable is explicitly false
// to prevent the model from entering a retry loop.
func TestSpotHistoryStub_RetryableIsFalse(t *testing.T) {
	h := tools.New(nil)
	resp := callSpotHistoryStub(t, h)

	retryable, ok := resp["retryable"].(bool)
	if !ok {
		t.Fatalf("expected retryable bool, got: %T %v", resp["retryable"], resp["retryable"])
	}
	if retryable {
		t.Error("retryable must be false to prevent retry loop and XML hallucination")
	}
}

// TestSpotHistoryStub_MessagePresent verifies the response includes a message.
func TestSpotHistoryStub_MessagePresent(t *testing.T) {
	h := tools.New(nil)
	resp := callSpotHistoryStub(t, h)

	msg, ok := resp["message"].(string)
	if !ok || msg == "" {
		t.Errorf("expected non-empty message string, got: %v", resp["message"])
	}
}

// TestSpotHistoryStub_AlternativesPresent verifies the response includes the
// alternatives map pointing the model to the correct tools.
func TestSpotHistoryStub_AlternativesPresent(t *testing.T) {
	h := tools.New(nil)
	resp := callSpotHistoryStub(t, h)

	alts, ok := resp["alternatives"].(map[string]any)
	if !ok {
		t.Fatalf("expected alternatives map, got: %T %v", resp["alternatives"], resp["alternatives"])
	}
	if _, ok := alts["spot_price"]; !ok {
		t.Error("alternatives missing spot_price key")
	}
	if _, ok := alts["browse_instances"]; !ok {
		t.Error("alternatives missing browse_instances key")
	}
	if _, ok := alts["compare_spot"]; !ok {
		t.Error("alternatives missing compare_spot key")
	}
}

// TestSpotHistoryStub_DoesNotPanic verifies that the stub does not panic
// regardless of whether providers are configured.
func TestSpotHistoryStub_DoesNotPanic(t *testing.T) {
	// Without any providers.
	h := tools.New(nil)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("HandleSpotHistoryStub panicked: %v", r)
			}
		}()
		callSpotHistoryStub(t, h)
	}()

	// With a provider configured.
	pvdr := &mockProvider{name: "aws", defaultRegion: "us-east-1"}
	h2 := tools.New(map[string]tools.Provider{"aws": pvdr})
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("HandleSpotHistoryStub panicked with provider: %v", r)
			}
		}()
		callSpotHistoryStub(t, h2)
	}()
}
