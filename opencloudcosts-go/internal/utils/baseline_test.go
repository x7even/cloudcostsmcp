package utils

import (
	"testing"
)

func makeResults(entries []struct{ region, priceHour, priceMo string }) []map[string]interface{} {
	out := make([]map[string]interface{}, len(entries))
	for i, e := range entries {
		out[i] = map[string]interface{}{
			"region":           e.region,
			"price_per_hour":   e.priceHour,
			"monthly_estimate": e.priceMo,
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Basic delta computation
// ---------------------------------------------------------------------------

func TestBaselineRegionShowsZeroDelta(t *testing.T) {
	results := makeResults([]struct{ region, priceHour, priceMo string }{
		{"us-east-1", "$0.200000/hr", "$146.00/mo"},
		{"eu-west-1", "$0.230000/hr", "$167.90/mo"},
	})
	if err := ApplyBaselineDeltas(results, "us-east-1", "price_per_hour", "monthly_estimate"); err != nil {
		t.Fatal(err)
	}
	var base map[string]interface{}
	for _, r := range results {
		if r["region"] == "us-east-1" {
			base = r
		}
	}
	assertStr(t, base, "delta_per_hour", "$+0.000000")
	assertStr(t, base, "delta_monthly", "$+0.00/mo")
	assertStr(t, base, "delta_pct", "+0.0%")
}

func TestDeltaIncrease(t *testing.T) {
	results := makeResults([]struct{ region, priceHour, priceMo string }{
		{"us-east-1", "$0.200000/hr", "$146.00/mo"},
		{"eu-west-1", "$0.260000/hr", "$189.80/mo"},
	})
	if err := ApplyBaselineDeltas(results, "us-east-1", "price_per_hour", "monthly_estimate"); err != nil {
		t.Fatal(err)
	}
	eu := findRegion(results, "eu-west-1")
	assertStr(t, eu, "delta_per_hour", "$+0.060000")
	assertStr(t, eu, "delta_monthly", "$+43.80/mo")
	assertStr(t, eu, "delta_pct", "+30.0%")
}

func TestDeltaDecrease(t *testing.T) {
	results := makeResults([]struct{ region, priceHour, priceMo string }{
		{"us-east-1", "$0.200000/hr", "$146.00/mo"},
		{"ap-southeast-1", "$0.160000/hr", "$116.80/mo"},
	})
	if err := ApplyBaselineDeltas(results, "us-east-1", "price_per_hour", "monthly_estimate"); err != nil {
		t.Fatal(err)
	}
	ap := findRegion(results, "ap-southeast-1")
	assertStr(t, ap, "delta_per_hour", "$-0.040000")
	assertStr(t, ap, "delta_monthly", "$-29.20/mo")
	assertStr(t, ap, "delta_pct", "-20.0%")
}

func TestDeltaUnchangedNonBaselineSamePrice(t *testing.T) {
	results := makeResults([]struct{ region, priceHour, priceMo string }{
		{"us-east-1", "$0.200000/hr", "$146.00/mo"},
		{"us-west-2", "$0.200000/hr", "$146.00/mo"},
	})
	if err := ApplyBaselineDeltas(results, "us-east-1", "price_per_hour", "monthly_estimate"); err != nil {
		t.Fatal(err)
	}
	west := findRegion(results, "us-west-2")
	assertStr(t, west, "delta_per_hour", "$+0.000000")
	assertStr(t, west, "delta_pct", "+0.0%")
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

func TestZeroBaselinePricePctIsZero(t *testing.T) {
	results := makeResults([]struct{ region, priceHour, priceMo string }{
		{"us-east-1", "$0.000000/hr", "$0.00/mo"},
		{"eu-west-1", "$0.100000/hr", "$73.00/mo"},
	})
	if err := ApplyBaselineDeltas(results, "us-east-1", "price_per_hour", "monthly_estimate"); err != nil {
		t.Fatal(err)
	}
	eu := findRegion(results, "eu-west-1")
	assertStr(t, eu, "delta_pct", "+0.0%")
}

func TestBaselineRegionNotFoundReturnsError(t *testing.T) {
	results := makeResults([]struct{ region, priceHour, priceMo string }{
		{"us-east-1", "$0.200000/hr", "$146.00/mo"},
	})
	err := ApplyBaselineDeltas(results, "ap-northeast-1", "price_per_hour", "monthly_estimate")
	if err == nil {
		t.Error("expected error")
	}
	if !containsSubstring(err.Error(), "not found") {
		t.Errorf("error must say 'not found': %v", err)
	}
}

func TestErrorMessageIncludesAvailableRegions(t *testing.T) {
	results := makeResults([]struct{ region, priceHour, priceMo string }{
		{"us-east-1", "$0.200000/hr", "$146.00/mo"},
		{"eu-west-1", "$0.230000/hr", "$167.90/mo"},
	})
	err := ApplyBaselineDeltas(results, "missing-region", "price_per_hour", "monthly_estimate")
	if err == nil {
		t.Fatal("expected error")
	}
	if !containsSubstring(err.Error(), "us-east-1") {
		t.Errorf("error must mention available regions: %v", err)
	}
}

func TestCustomPriceKeys(t *testing.T) {
	results := []map[string]interface{}{
		{"region": "us-east-1", "cost_hr": "$0.100000/hr", "cost_mo": "$73.00/mo"},
		{"region": "eu-west-1", "cost_hr": "$0.150000/hr", "cost_mo": "$109.50/mo"},
	}
	if err := ApplyBaselineDeltas(results, "us-east-1", "cost_hr", "cost_mo"); err != nil {
		t.Fatal(err)
	}
	eu := findRegion(results, "eu-west-1")
	assertStr(t, eu, "delta_per_hour", "$+0.050000")
	assertStr(t, eu, "delta_pct", "+50.0%")
}

func TestDictPriceFormat(t *testing.T) {
	results := []map[string]interface{}{
		{
			"region": "us-east-1",
			"price_per_hour": map[string]interface{}{
				"amount": 0.2, "unit": "hr", "currency": "USD", "display": "$0.200000/hr",
			},
			"monthly_estimate": map[string]interface{}{
				"amount": 146.0, "currency": "USD", "display": "$146.00/mo",
			},
		},
		{
			"region": "eu-west-1",
			"price_per_hour": map[string]interface{}{
				"amount": 0.25, "unit": "hr", "currency": "USD", "display": "$0.250000/hr",
			},
			"monthly_estimate": map[string]interface{}{
				"amount": 182.5, "currency": "USD", "display": "$182.50/mo",
			},
		},
	}
	if err := ApplyBaselineDeltas(results, "us-east-1", "price_per_hour", "monthly_estimate"); err != nil {
		t.Fatal(err)
	}
	eu := findRegion(results, "eu-west-1")
	assertStr(t, eu, "delta_per_hour", "$+0.050000")
	assertStr(t, eu, "delta_monthly", "$+36.50/mo")
	assertStr(t, eu, "delta_pct", "+25.0%")
}

func TestMultipleRegionsAllGetDeltaFields(t *testing.T) {
	results := makeResults([]struct{ region, priceHour, priceMo string }{
		{"us-east-1", "$0.200000/hr", "$146.00/mo"},
		{"us-west-2", "$0.210000/hr", "$153.30/mo"},
		{"eu-west-1", "$0.230000/hr", "$167.90/mo"},
		{"ap-southeast-1", "$0.180000/hr", "$131.40/mo"},
	})
	if err := ApplyBaselineDeltas(results, "us-east-1", "price_per_hour", "monthly_estimate"); err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if _, ok := r["delta_per_hour"]; !ok {
			t.Errorf("region %q missing delta_per_hour", r["region"])
		}
		if _, ok := r["delta_monthly"]; !ok {
			t.Errorf("region %q missing delta_monthly", r["region"])
		}
		if _, ok := r["delta_pct"]; !ok {
			t.Errorf("region %q missing delta_pct", r["region"])
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func assertStr(t *testing.T, m map[string]interface{}, key, want string) {
	t.Helper()
	got, ok := m[key]
	if !ok {
		t.Errorf("key %q missing", key)
		return
	}
	if got != want {
		t.Errorf("%q: got %q want %q", key, got, want)
	}
}

func findRegion(results []map[string]interface{}, region string) map[string]interface{} {
	for _, r := range results {
		if r["region"] == region {
			return r
		}
	}
	return nil
}

func containsSubstring(s, sub string) bool {
	return len(s) > 0 && len(sub) > 0 && len(s) >= len(sub) && func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}()
}
