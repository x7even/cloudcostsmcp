// Package gcp — unit tests for Cloud KMS pricing (issue #77).
//
// All SKU descriptions used in these fixtures are the exact, live-verified
// strings recovered from the GCP Cloud Billing Catalog API for service
// EE2F-D110-890C ("Cloud Key Management Service (KMS)") — see the PR
// description for the full verified-data audit trail.
package gcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// --------------------------------------------------------------------------
// KMS SKU helpers
// --------------------------------------------------------------------------

// kmsSKUSingleTier builds a single-tier global SKU: one tier at
// startUsageAmount=0 carrying the rate directly. Models the flat-rate shape
// used by software/external key versions, HSM symmetric/RSA2048/MAC key
// versions, and all non-tiered crypto-operation SKUs.
func kmsSKUSingleTier(desc, units string, nanos int) map[string]any {
	return map[string]any{
		"description":    desc,
		"serviceRegions": []any{"asia", "asia-east1"},
		"category": map[string]any{
			"resourceGroup": "KMS",
			"usageType":     "OnDemand",
		},
		"pricingInfo": []any{
			map[string]any{
				"pricingExpression": map[string]any{
					"tieredRates": []any{
						map[string]any{
							"startUsageAmount": float64(0),
							"unitPrice": map[string]any{
								"units": units,
								"nanos": float64(nanos),
							},
						},
					},
				},
			},
		},
	}
}

// kmsSKUTwoTier builds a two-tier global SKU with BOTH tiers priced (a
// volume discount, not a free tier): tier1 at startUsageAmount=0, tier2 at
// startUsageAmount=threshold. Models the 6 HSM asymmetric key-version SKUs
// (ECDSA P-256/P-384/SECP256K1, RSA 3072/4096, PKCS1 v1.5): $2.50/mo for the
// first 2,000 key versions, $1.00/mo thereafter.
func kmsSKUTwoTier(desc string, threshold float64, tier1Units string, tier1Nanos int, tier2Units string, tier2Nanos int) map[string]any {
	return map[string]any{
		"description":    desc,
		"serviceRegions": []any{"asia", "asia-east1"},
		"category": map[string]any{
			"resourceGroup": "KMS",
			"usageType":     "OnDemand",
		},
		"pricingInfo": []any{
			map[string]any{
				"pricingExpression": map[string]any{
					"tieredRates": []any{
						map[string]any{
							"startUsageAmount": float64(0),
							"unitPrice": map[string]any{
								"units": tier1Units,
								"nanos": float64(tier1Nanos),
							},
						},
						map[string]any{
							"startUsageAmount": threshold,
							"unitPrice": map[string]any{
								"units": tier2Units,
								"nanos": float64(tier2Nanos),
							},
						},
					},
				},
			},
		},
	}
}

// kmsSKUFreeThenPaid builds a two-tier global SKU with a genuine $0.00 free
// tier at startUsageAmount=0, followed by the paid rate at
// startUsageAmount=threshold. Models the Autokey SKUs: 100 free key
// versions/month (77F8-D8AF-3CCE) or 10,000 free operations/month
// (88D6-F2EE-C781), then the standard HSM symmetric paid rate.
func kmsSKUFreeThenPaid(desc string, threshold float64, paidUnits string, paidNanos int) map[string]any {
	return map[string]any{
		"description":    desc,
		"serviceRegions": []any{"asia", "asia-east1"},
		"category": map[string]any{
			"resourceGroup": "KMS",
			"usageType":     "OnDemand",
		},
		"pricingInfo": []any{
			map[string]any{
				"pricingExpression": map[string]any{
					"tieredRates": []any{
						map[string]any{
							"startUsageAmount": float64(0),
							"unitPrice": map[string]any{
								"units": "0",
								"nanos": float64(0),
							},
						},
						map[string]any{
							"startUsageAmount": threshold,
							"unitPrice": map[string]any{
								"units": paidUnits,
								"nanos": float64(paidNanos),
							},
						},
					},
				},
			},
		},
	}
}

// kmsSKUResponse wraps KMS SKUs for the httptest server.
func kmsSKUResponse(skus []map[string]any) []byte {
	resp := map[string]any{
		"skus":          skus,
		"nextPageToken": "",
	}
	b, _ := json.Marshal(resp)
	return b
}

// fakeKMSSKUs returns a representative subset of the 33 live-verified Cloud
// KMS SKUs (service EE2F-D110-890C), using their exact description strings,
// covering all three tier shapes plus the Single-Tenant-HSM exclusion.
func fakeKMSSKUs() []map[string]any {
	return []map[string]any{
		// Software key versions — flat $0.06/mo (single-tier).
		kmsSKUSingleTier("Active software symmetric key versions", "0", 60_000_000),  // E09C-32B3-9AC7
		kmsSKUSingleTier("Active software asymmetric key versions", "0", 60_000_000), // 7907-37BC-DD97
		kmsSKUSingleTier("Active software MAC key versions", "0", 60_000_000),        // 1913-F7CE-A439

		// HSM key versions — flat $1.00/mo (single-tier, NOT tiered).
		kmsSKUSingleTier("Active HSM symmetric key versions", "1", 0),    // 46B1-C76A-0B7D
		kmsSKUSingleTier("Active HSM RSA 2048 bit key versions", "1", 0), // 1686-718D-035F
		kmsSKUSingleTier("Active HSM MAC key versions", "1", 0),          // BE8A-8B15-11B1

		// HSM key versions — tiered $2.50 (0-2000) then $1.00 (2000+).
		kmsSKUTwoTier("Active HSM ECDSA P-256 key versions", 2000, "2", 500_000_000, "1", 0),  // 1017-1BAF-7159
		kmsSKUTwoTier("Active HSM ECDSA P-384 key versions", 2000, "2", 500_000_000, "1", 0),  // 83C4-80AE-F5DC
		kmsSKUTwoTier("Active HSM RSA 3072 bit key versions", 2000, "2", 500_000_000, "1", 0), // 93F6-CB12-A862

		// External key versions — flat $3.00/mo.
		kmsSKUSingleTier("Active external asymmetric key versions", "3", 0), // D57A-D245-5FDB
		kmsSKUSingleTier("Active external symmetric key versions", "3", 0),  // E0AA-8721-5338

		// Autokey key versions — free tier (100/mo) then $1.00/mo paid.
		kmsSKUFreeThenPaid("Active HSM symmetric key versions for Autokey", 100, "1", 0), // 77F8-D8AF-3CCE

		// Out of scope: Single-Tenant HSM (different flat-instance-fee model).
		kmsSKUSingleTier("Active Single Tenant HSM key versions (above 15000)", "1", 0), // 4A51-C764-8B93
		kmsSKUSingleTier("Active single tenant HSM instances", "3500", 0),               // EE6E-FF5D-19F0

		// Crypto operations — $0.000003/op (single-tier).
		kmsSKUSingleTier("Cryptographic operations with a software symmetric key", "0", 3_000), // 3BDA-77FB-678B
		kmsSKUSingleTier("HSM symmetric cryptographic operations", "0", 3_000),                 // A301-A092-05E7
		kmsSKUSingleTier("HSM cryptographic operations with a RSA 2048 bit key", "0", 3_000),   // 44FA-E035-E3C7
		kmsSKUSingleTier("External symmetric cryptographic operations", "0", 3_000),            // 1BB4-32BC-93CE

		// Crypto operations — $0.000015/op (HSM asymmetric EC/RSA3072/4096, higher rate).
		kmsSKUSingleTier("HSM cryptographic operations with an ECDSA P-256 key", "0", 15_000), // 3F11-7A35-EBAE

		// Autokey crypto operations — free tier (10,000/mo) then $0.000003/op paid.
		kmsSKUFreeThenPaid("HSM symmetric cryptographic operations for Autokey", 10000, "0", 3_000), // 88D6-F2EE-C781

		// Generate Random Bytes — $0.000015/call.
		kmsSKUSingleTier("Generate Random Bytes Call", "0", 15_000), // B9A7-CAEB-23CD
	}
}

// --------------------------------------------------------------------------
// Key-version-month tests
// --------------------------------------------------------------------------

// TestPriceKMS_SoftwareKeyVersion verifies the flat software key-version rate
// (E09C-32B3-9AC7 et al.): $0.06/mo.
func TestPriceKMS_SoftwareKeyVersion(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.KMSPricingSpec{KeyType: "software"}
	prices, breakdown, err := p.priceKMS(ctx, spec)
	if err != nil {
		t.Fatalf("priceKMS: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected exactly one price, got %d", len(prices))
	}
	if abs(prices[0].PricePerUnit-0.06) > 1e-9 {
		t.Errorf("software key version rate = %.6f, want 0.060000", prices[0].PricePerUnit)
	}
	if prices[0].Region != "global" || prices[0].Attributes["scope"] != "global" {
		t.Errorf("region/scope not tagged global: region=%q attrs=%v", prices[0].Region, prices[0].Attributes)
	}
	if fb, ok := breakdown["fallback"]; ok && fb == true {
		t.Error("expected no fallback when the software key-version SKU is present")
	}
}

// TestPriceKMS_HSMRSA2048FlatNotTiered verifies that HSM RSA-2048 key
// versions are flat $1.00/mo — NOT tiered like EC/RSA3072/4096 — guarding
// against a regression that lumps RSA2048 into the high-algo tiered bucket.
func TestPriceKMS_HSMRSA2048FlatNotTiered(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.KMSPricingSpec{KeyType: "hsm", Algorithm: "asymmetric-rsa2048"}
	prices, breakdown, err := p.priceKMS(ctx, spec)
	if err != nil {
		t.Fatalf("priceKMS: %v", err)
	}
	if abs(prices[0].PricePerUnit-1.00) > 1e-9 {
		t.Errorf("HSM RSA2048 key version rate = %.6f, want 1.000000 (flat)", prices[0].PricePerUnit)
	}
	if _, tiered := breakdown["tier1_rate"]; tiered {
		t.Error("HSM RSA2048 key versions must not be reported as tiered")
	}
}

// TestPriceKMS_HSMECTiered verifies the HSM EC key-version volume discount:
// $2.50/mo for the first 2,000 versions, $1.00/mo thereafter, with BOTH
// tiers exposed in breakdown (1017-1BAF-7159).
func TestPriceKMS_HSMECTiered(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.KMSPricingSpec{KeyType: "hsm", Algorithm: "asymmetric-ec"}
	prices, breakdown, err := p.priceKMS(ctx, spec)
	if err != nil {
		t.Fatalf("priceKMS: %v", err)
	}
	if abs(prices[0].PricePerUnit-2.50) > 1e-9 {
		t.Errorf("HSM EC key version headline rate = %.6f, want 2.500000 (tier1)", prices[0].PricePerUnit)
	}
	tier1 := mustFloat64(t, breakdown["tier1_rate"], "tier1_rate")
	tier2 := mustFloat64(t, breakdown["tier2_rate"], "tier2_rate")
	if abs(tier1-2.50) > 1e-9 {
		t.Errorf("tier1_rate = %.6f, want 2.500000", tier1)
	}
	if abs(tier2-1.00) > 1e-9 {
		t.Errorf("tier2_rate = %.6f, want 1.000000", tier2)
	}
}

// TestPriceKMS_ExternalKeyVersion verifies the flat external key-version rate
// (D57A-D245-5FDB / E0AA-8721-5338): $3.00/mo.
func TestPriceKMS_ExternalKeyVersion(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.KMSPricingSpec{KeyType: "external"}
	prices, _, err := p.priceKMS(ctx, spec)
	if err != nil {
		t.Fatalf("priceKMS: %v", err)
	}
	if abs(prices[0].PricePerUnit-3.00) > 1e-9 {
		t.Errorf("external key version rate = %.6f, want 3.000000", prices[0].PricePerUnit)
	}
}

// TestPriceKMS_AutokeyKeyVersionUsesPaidRate verifies that Autokey key
// versions report the PAID rate ($1.00/mo), not the $0.00 free-tier rate,
// while surfacing the free allowance (100 versions/mo) in breakdown
// (77F8-D8AF-3CCE).
func TestPriceKMS_AutokeyKeyVersionUsesPaidRate(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.KMSPricingSpec{KeyType: "hsm", Autokey: true}
	prices, breakdown, err := p.priceKMS(ctx, spec)
	if err != nil {
		t.Fatalf("priceKMS: %v", err)
	}
	if abs(prices[0].PricePerUnit-1.00) > 1e-9 {
		t.Errorf("Autokey key version rate = %.6f, want 1.000000 (paid rate, not $0.00 free tier)", prices[0].PricePerUnit)
	}
	freeTier, ok := breakdown["free_tier_key_versions_per_month"]
	if !ok || toFloat64(freeTier) != 100 {
		t.Errorf("free_tier_key_versions_per_month = %v, want 100", freeTier)
	}
}

// --------------------------------------------------------------------------
// Crypto-operation tests
// --------------------------------------------------------------------------

// TestPriceKMS_LowOperationRate verifies the standard $0.000003/op rate
// shared by software/external/HSM-symmetric-RSA2048-MAC operations
// (A301-A092-05E7 et al.).
func TestPriceKMS_LowOperationRate(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.KMSPricingSpec{KeyType: "hsm", Algorithm: "symmetric", Unit: "crypto_operations"}
	prices, _, err := p.priceKMS(ctx, spec)
	if err != nil {
		t.Fatalf("priceKMS: %v", err)
	}
	if abs(prices[0].PricePerUnit-0.000003) > 1e-12 {
		t.Errorf("HSM symmetric operation rate = %.9f, want 0.000003000", prices[0].PricePerUnit)
	}
}

// TestPriceKMS_HSMHighOperationRate verifies the higher $0.000015/op rate for
// HSM asymmetric EC/RSA3072/RSA4096 operations (3F11-7A35-EBAE et al.).
func TestPriceKMS_HSMHighOperationRate(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.KMSPricingSpec{KeyType: "hsm", Algorithm: "asymmetric-ec", Unit: "crypto_operations"}
	prices, _, err := p.priceKMS(ctx, spec)
	if err != nil {
		t.Fatalf("priceKMS: %v", err)
	}
	if abs(prices[0].PricePerUnit-0.000015) > 1e-12 {
		t.Errorf("HSM EC operation rate = %.9f, want 0.000015000", prices[0].PricePerUnit)
	}
}

// TestPriceKMS_AutokeyOperationUsesPaidRate verifies Autokey crypto
// operations report the paid rate ($0.000003/op), not $0.00, with the free
// allowance (10,000 ops/mo) surfaced in breakdown (88D6-F2EE-C781).
func TestPriceKMS_AutokeyOperationUsesPaidRate(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.KMSPricingSpec{KeyType: "hsm", Autokey: true, Unit: "crypto_operations"}
	prices, breakdown, err := p.priceKMS(ctx, spec)
	if err != nil {
		t.Fatalf("priceKMS: %v", err)
	}
	if abs(prices[0].PricePerUnit-0.000003) > 1e-12 {
		t.Errorf("Autokey operation rate = %.9f, want 0.000003000 (paid rate)", prices[0].PricePerUnit)
	}
	freeTier, ok := breakdown["free_tier_operations_per_month"]
	if !ok || toFloat64(freeTier) != 10000 {
		t.Errorf("free_tier_operations_per_month = %v, want 10000", freeTier)
	}
}

// TestPriceKMS_RandomBytes verifies the Generate Random Bytes Call rate:
// $0.000015/call (B9A7-CAEB-23CD).
func TestPriceKMS_RandomBytes(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.KMSPricingSpec{Unit: "random_bytes"}
	prices, _, err := p.priceKMS(ctx, spec)
	if err != nil {
		t.Fatalf("priceKMS: %v", err)
	}
	if abs(prices[0].PricePerUnit-0.000015) > 1e-12 {
		t.Errorf("Generate Random Bytes rate = %.9f, want 0.000015000", prices[0].PricePerUnit)
	}
}

// --------------------------------------------------------------------------
// Cost-math, fallback, and dispatch tests
// --------------------------------------------------------------------------

// TestPriceKMS_CostMath verifies key_versions * rate = monthly_cost:
// 500 versions * $0.06/mo = $30.00.
func TestPriceKMS_CostMath(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	keyVersions := 500.0
	spec := &models.KMSPricingSpec{KeyType: "software", KeyVersions: &keyVersions}
	_, breakdown, err := p.priceKMS(ctx, spec)
	if err != nil {
		t.Fatalf("priceKMS: %v", err)
	}
	got := toFloat64(breakdown["monthly_cost"])
	if abs(got-30.00) > 1e-6 {
		t.Errorf("monthly_cost = %.4f, want 30.0000", got)
	}
}

// TestPriceKMS_CostMath_Operations verifies the operations_per_month
// breakdown branch (the key_versions branch is covered by
// TestPriceKMS_CostMath above; this covers the sibling crypto_operations /
// random_bytes branch that was previously untested).
func TestPriceKMS_CostMath_Operations(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	ops := 100000.0
	spec := &models.KMSPricingSpec{KeyType: "software", Unit: "crypto_operations", OperationsPerMonth: &ops}
	_, breakdown, err := p.priceKMS(ctx, spec)
	if err != nil {
		t.Fatalf("priceKMS: %v", err)
	}
	got := toFloat64(breakdown["monthly_cost"])
	if abs(got-0.30) > 1e-6 {
		t.Errorf("monthly_cost = %.6f, want 0.300000 (100000 * 0.000003)", got)
	}
}

// TestPriceKMS_CostMath_TieredCrossesThreshold verifies monthly_cost for a
// tiered HSM asymmetric key version applies the volume-discount split
// (0-2,000 @ tier1, 2,000+ @ tier2) instead of naively multiplying the
// headline (tier1) rate across the full quantity, which would overcharge.
func TestPriceKMS_CostMath_TieredCrossesThreshold(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	keyVersions := 5000.0
	spec := &models.KMSPricingSpec{KeyType: "hsm", Algorithm: "asymmetric-ec", KeyVersions: &keyVersions}
	_, breakdown, err := p.priceKMS(ctx, spec)
	if err != nil {
		t.Fatalf("priceKMS: %v", err)
	}
	// 2,000 @ $2.50 + 3,000 @ $1.00 = $8,000.00 (not 5,000 * $2.50 = $12,500).
	want := 2000.0*2.50 + 3000.0*1.00
	got := toFloat64(breakdown["monthly_cost"])
	if abs(got-want) > 1e-6 {
		t.Errorf("monthly_cost = %.4f, want %.4f (tiered split, not flat tier1*qty)", got, want)
	}
}

// TestPriceKMS_CostMath_AutokeyFreeAllowance verifies monthly_cost for
// Autokey deducts the free allowance before applying the paid rate, on both
// billing dimensions.
func TestPriceKMS_CostMath_AutokeyFreeAllowance(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	t.Run("key_version_month", func(t *testing.T) {
		keyVersions := 150.0
		spec := &models.KMSPricingSpec{KeyType: "hsm", Autokey: true, KeyVersions: &keyVersions}
		_, breakdown, err := p.priceKMS(ctx, spec)
		if err != nil {
			t.Fatalf("priceKMS: %v", err)
		}
		// (150 - 100 free) * $1.00 = $50.00 (not 150 * $1.00 = $150.00).
		want := 50.0
		got := toFloat64(breakdown["monthly_cost"])
		if abs(got-want) > 1e-6 {
			t.Errorf("monthly_cost = %.4f, want %.4f (free allowance deducted)", got, want)
		}
	})

	t.Run("crypto_operations", func(t *testing.T) {
		ops := 15000.0
		spec := &models.KMSPricingSpec{KeyType: "hsm", Autokey: true, Unit: "crypto_operations", OperationsPerMonth: &ops}
		_, breakdown, err := p.priceKMS(ctx, spec)
		if err != nil {
			t.Fatalf("priceKMS: %v", err)
		}
		// (15000 - 10000 free) * $0.000003 = $0.015 (not 15000 * $0.000003 = $0.045).
		want := 5000.0 * 0.000003
		got := toFloat64(breakdown["monthly_cost"])
		if abs(got-want) > 1e-9 {
			t.Errorf("monthly_cost = %.9f, want %.9f (free allowance deducted)", got, want)
		}
	})
}

// TestPriceKMS_AutokeyHSMAsymmetricInvalidCombo verifies that hsm +
// asymmetric + autokey=true (a combination that does not exist in the
// verified SKU catalog — Autokey is HSM-symmetric only) falls through to
// the ordinary tiered asymmetric rate rather than misapplying the
// Autokey-symmetric paid rate.
func TestPriceKMS_AutokeyHSMAsymmetricInvalidCombo(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.KMSPricingSpec{KeyType: "hsm", Algorithm: "asymmetric-ec", Autokey: true}
	prices, breakdown, err := p.priceKMS(ctx, spec)
	if err != nil {
		t.Fatalf("priceKMS: %v", err)
	}
	if abs(prices[0].PricePerUnit-2.50) > 1e-9 {
		t.Errorf("rate = %.6f, want 2.500000 (tiered asymmetric tier1, not autokey paid rate)", prices[0].PricePerUnit)
	}
	if v, ok := prices[0].Attributes["autokey"]; ok {
		t.Errorf(`Attributes["autokey"] = %q, want absent for invalid hsm+asymmetric+autokey combo`, v)
	}
	if _, ok := breakdown["tier2_rate"]; !ok {
		t.Errorf("breakdown missing tier2_rate; invalid autokey combo should still price as tiered asymmetric")
	}
}

// TestPriceKMS_RandomBytesAutokeyNotApplied verifies that unit="random_bytes"
// (which has no Autokey SKU or free tier at all — Generate Random Bytes Call,
// B9A7-CAEB-23CD, is a flat-rate call with no Autokey variant) is never
// mistakenly treated as an Autokey billing dimension just because
// key_type=hsm, algorithm=symmetric, autokey=true happen to be set on the
// spec. Regression test for autokeyApplied being derived independently of
// which unit/rate branch actually fired.
func TestPriceKMS_RandomBytesAutokeyNotApplied(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	ops := 15000.0
	spec := &models.KMSPricingSpec{
		KeyType:            "hsm",
		Autokey:            true,
		Unit:               "random_bytes",
		OperationsPerMonth: &ops,
	}
	prices, breakdown, err := p.priceKMS(ctx, spec)
	if err != nil {
		t.Fatalf("priceKMS: %v", err)
	}
	if abs(prices[0].PricePerUnit-0.000015) > 1e-12 {
		t.Errorf("rate = %.9f, want 0.000015000 (ordinary random-bytes rate)", prices[0].PricePerUnit)
	}
	if v, ok := prices[0].Attributes["autokey"]; ok {
		t.Errorf(`Attributes["autokey"] = %q, want absent for unit=random_bytes`, v)
	}
	if _, ok := breakdown["free_tier_operations_per_month"]; ok {
		t.Errorf("breakdown unexpectedly contains free_tier_operations_per_month for unit=random_bytes")
	}
	// No free allowance for random_bytes: full 15,000 * $0.000015 = $0.225,
	// not (15000-10000) * $0.000015 = $0.075.
	want := 15000.0 * 0.000015
	got := toFloat64(breakdown["monthly_cost"])
	if abs(got-want) > 1e-9 {
		t.Errorf("monthly_cost = %.9f, want %.9f (no autokey free tier for random_bytes)", got, want)
	}
}

// TestPriceKMS_AutokeySoftwareKeyTypeNotAnnotated verifies that Autokey is
// only ever treated as an Autokey billing dimension for key_type="hsm"
// (verified live: Autokey SKUs 77F8-D8AF-3CCE / 88D6-F2EE-C781 are HSM
// symmetric only). A software key_type with autokey=true must still price
// as an ordinary software key/operation and must NOT carry the
// autokey/free-tier annotations, which would misleadingly imply a free
// allowance that doesn't exist for software keys.
func TestPriceKMS_AutokeySoftwareKeyTypeNotAnnotated(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	t.Run("key_version_month", func(t *testing.T) {
		spec := &models.KMSPricingSpec{KeyType: "software", Autokey: true}
		prices, breakdown, err := p.priceKMS(ctx, spec)
		if err != nil {
			t.Fatalf("priceKMS: %v", err)
		}
		if abs(prices[0].PricePerUnit-0.06) > 1e-9 {
			t.Errorf("rate = %.6f, want 0.060000 (ordinary software rate)", prices[0].PricePerUnit)
		}
		if v, ok := prices[0].Attributes["autokey"]; ok {
			t.Errorf(`Attributes["autokey"] = %q, want absent for key_type=software`, v)
		}
		if _, ok := breakdown["free_tier_key_versions_per_month"]; ok {
			t.Errorf("breakdown unexpectedly contains free_tier_key_versions_per_month for key_type=software")
		}
		if _, ok := breakdown["autokey_note"]; ok {
			t.Errorf("breakdown unexpectedly contains autokey_note for key_type=software")
		}
	})

	t.Run("crypto_operations", func(t *testing.T) {
		spec := &models.KMSPricingSpec{KeyType: "software", Autokey: true, Unit: "crypto_operations"}
		prices, breakdown, err := p.priceKMS(ctx, spec)
		if err != nil {
			t.Fatalf("priceKMS: %v", err)
		}
		if abs(prices[0].PricePerUnit-0.000003) > 1e-12 {
			t.Errorf("rate = %.9f, want 0.000003000 (ordinary low-operation rate)", prices[0].PricePerUnit)
		}
		if _, ok := breakdown["free_tier_operations_per_month"]; ok {
			t.Errorf("breakdown unexpectedly contains free_tier_operations_per_month for key_type=software")
		}
	})
}

// TestPriceKMS_Fallback verifies that when no matching SKU is found,
// priceKMS falls back to the hardcoded published rates and sets
// breakdown["fallback"]=true.
func TestPriceKMS_Fallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(nil))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.KMSPricingSpec{}
	prices, breakdown, err := p.priceKMS(ctx, spec)
	if err != nil {
		t.Fatalf("priceKMS (fallback): %v", err)
	}
	fb, ok := breakdown["fallback"]
	if !ok || fb != true {
		t.Errorf("expected fallback=true, got %v (ok=%v)", fb, ok)
	}
	if abs(prices[0].PricePerUnit-0.06) > 1e-9 {
		t.Errorf("fallback software key version rate = %.6f, want 0.060000", prices[0].PricePerUnit)
	}
}

// TestPriceKMS_SingleTenantHSMExcluded verifies that the two Single-Tenant
// HSM SKUs never leak into any of the core rate buckets — a description
// match to any core bucket here would silently corrupt one of the flat/
// tiered rates with the $3500/mo or $1.00-above-15000 dedicated-instance
// figures.
func TestPriceKMS_SingleTenantHSMExcluded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	rates := p.fetchKMSRates(ctx)
	if rates.HSMKeyVersionFlat != 1.00 {
		t.Errorf("HSMKeyVersionFlat = %.6f, want 1.000000 (must not be corrupted by single-tenant SKUs)", rates.HSMKeyVersionFlat)
	}
}

// TestPriceKMS_DispatchViaGetPrice verifies that a domain="security",
// service="kms" spec routes through GetPrice → Supports → getPart3Price →
// priceKMS end-to-end.
func TestPriceKMS_DispatchViaGetPrice(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	if !p.Supports(models.PricingDomainSecurity, "kms") {
		t.Fatal("Supports(security, kms) = false, want true")
	}

	spec := &models.KMSPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Domain:  models.PricingDomainSecurity,
			Service: "kms",
		},
		KeyType: "software",
	}
	result, err := p.GetPrice(ctx, spec)
	if err != nil {
		t.Fatalf("GetPrice: %v", err)
	}
	if len(result.PublicPrices) != 1 || result.PublicPrices[0].Service != "kms" {
		t.Fatalf("GetPrice did not dispatch to priceKMS: %+v", result.PublicPrices)
	}
}

// TestPriceKMS_RatesCached verifies that fetchKMSRates reads its derived rate
// map from cache instead of calling fetchSKUs/HTTP at all, mirroring
// TestPriceNetworkExternalIP_RatesCached.
func TestPriceKMS_RatesCached(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected HTTP call: fetchKMSRates should have used the cached rate map")
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	seeded := kmsRates{SoftwareKeyVersion: 0.099}
	raw, err := json.Marshal(seeded)
	if err != nil {
		t.Fatalf("marshal seeded rates: %v", err)
	}
	p.cache.SetMetadata(kmsRatesCacheKey, raw, p.cfg.MetadataTTL())

	spec := &models.KMSPricingSpec{KeyType: "software"}
	prices, breakdown, err := p.priceKMS(ctx, spec)
	if err != nil {
		t.Fatalf("priceKMS: %v", err)
	}
	if abs(prices[0].PricePerUnit-0.099) > 1e-9 {
		t.Errorf("software key version rate = %.6f, want 0.099000 (from cache)", prices[0].PricePerUnit)
	}
	if fb, ok := breakdown["fallback"]; ok && fb == true {
		t.Error("expected no fallback when the derived-rate cache is populated")
	}
}

// TestPriceKMS_TieredFallbackWhenTier2Zero verifies that priceKMS engages the
// fallback path when the live rate map has a nonzero HSMKeyVersionTier1 but a
// zero HSMKeyVersionTier2 (e.g. a live catalog response that is missing the
// second tier for one SKU). Regression test: the tiered branch previously
// only checked `if rate == 0` (tier1) to decide whether to fall back, so a
// zero tier2Rate alone silently priced all usage above the threshold at
// $0.00/mo instead of the fallback rate.
func TestPriceKMS_TieredFallbackWhenTier2Zero(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected HTTP call: fetchKMSRates should have used the cached rate map")
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	// Seed a live rates map with a nonzero tier1 but a zero tier2 — the
	// scenario that must trigger fallback for BOTH tiers, not just tier2.
	seeded := kmsRates{HSMKeyVersionTier1: 9.99, HSMKeyVersionTier2: 0}
	raw, err := json.Marshal(seeded)
	if err != nil {
		t.Fatalf("marshal seeded rates: %v", err)
	}
	p.cache.SetMetadata(kmsRatesCacheKey, raw, p.cfg.MetadataTTL())

	spec := &models.KMSPricingSpec{KeyType: "hsm", Algorithm: "asymmetric-ec"}
	prices, breakdown, err := p.priceKMS(ctx, spec)
	if err != nil {
		t.Fatalf("priceKMS: %v", err)
	}
	fb, ok := breakdown["fallback"]
	if !ok || fb != true {
		t.Errorf("expected fallback=true when tier2 resolves to 0, got %v (ok=%v)", fb, ok)
	}
	if abs(prices[0].PricePerUnit-kmsFallbackRates.HSMKeyVersionTier1) > 1e-9 {
		t.Errorf("headline rate = %.6f, want %.6f (fallback tier1)", prices[0].PricePerUnit, kmsFallbackRates.HSMKeyVersionTier1)
	}
	tier2 := mustFloat64(t, breakdown["tier2_rate"], "tier2_rate")
	if abs(tier2-kmsFallbackRates.HSMKeyVersionTier2) > 1e-9 {
		t.Errorf("tier2_rate = %.6f, want %.6f (fallback tier2, not $0.00)", tier2, kmsFallbackRates.HSMKeyVersionTier2)
	}

	// Verify tier2 usage is actually priced at the fallback rate, not $0.00:
	// 3,000 versions crosses the 2,000 threshold, so 1,000 must be billed at
	// the fallback tier2 rate.
	keyVersions := 3000.0
	spec.KeyVersions = &keyVersions
	_, breakdown, err = p.priceKMS(ctx, spec)
	if err != nil {
		t.Fatalf("priceKMS: %v", err)
	}
	want := kmsHSMTierThreshold*kmsFallbackRates.HSMKeyVersionTier1 + (3000.0-kmsHSMTierThreshold)*kmsFallbackRates.HSMKeyVersionTier2
	got := toFloat64(breakdown["monthly_cost"])
	if abs(got-want) > 1e-6 {
		t.Errorf("monthly_cost = %.4f, want %.4f (tier2 usage priced at fallback rate, not $0.00)", got, want)
	}
}

// TestPriceKMS_InvalidAlgorithm verifies that an unrecognized Algorithm
// value returns an explicit error instead of silently falling through to a
// wrong-but-plausible rate bucket.
func TestPriceKMS_InvalidAlgorithm(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.KMSPricingSpec{KeyType: "hsm", Algorithm: "asymmetric-rsa9999"}
	prices, _, err := p.priceKMS(ctx, spec)
	if err == nil {
		t.Fatalf("priceKMS: expected error for invalid algorithm, got prices %+v", prices)
	}
}

// TestPriceKMS_InvalidUnit verifies that an unrecognized Unit value returns
// an explicit error instead of silently defaulting to key_version_month.
func TestPriceKMS_InvalidUnit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.KMSPricingSpec{KeyType: "software", Unit: "per_gigabyte"}
	prices, _, err := p.priceKMS(ctx, spec)
	if err == nil {
		t.Fatalf("priceKMS: expected error for invalid unit, got prices %+v", prices)
	}
}

// TestPriceKMS_InvalidKeyType verifies that an unrecognized KeyType value
// returns an explicit error instead of silently defaulting to software.
func TestPriceKMS_InvalidKeyType(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(kmsSKUResponse(fakeKMSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.KMSPricingSpec{KeyType: "quantum"}
	prices, _, err := p.priceKMS(ctx, spec)
	if err == nil {
		t.Fatalf("priceKMS: expected error for invalid key_type, got prices %+v", prices)
	}
}
