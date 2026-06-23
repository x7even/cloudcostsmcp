package utils

import (
	"math"
	"strconv"
	"testing"
)

// awsInternetEgressTiers mirrors _AWS_INTERNET_EGRESS_TIERS from providers/aws.py.
// Used here to replicate the Python test_compute_tiered_cost_large_volume test.
var awsInternetEgressTiers = []EgressPriceTier{
	{ThresholdGB: 0, RateStr: "0.000", Label: "0-100 GB (free)"},
	{ThresholdGB: 100, RateStr: "0.090", Label: "100 GB-10 TB"},
	{ThresholdGB: 10_340, RateStr: "0.085", Label: "10-50 TB"},
	{ThresholdGB: 51_300, RateStr: "0.070", Label: "50-150 TB"},
	{ThresholdGB: 153_700, RateStr: "0.050", Label: "150-500 TB"},
	{ThresholdGB: 512_100, RateStr: "0.050", Label: ">500 TB"},
}

// ---------------------------------------------------------------------------
// EgressPriceTier
// ---------------------------------------------------------------------------

func TestEgressPriceTierRateParsesCorrectly(t *testing.T) {
	tier := EgressPriceTier{ThresholdGB: 0, RateStr: "0.09", Label: "all"}
	if math.Abs(tier.Rate()-0.09) > 1e-12 {
		t.Errorf("Rate(): got %v want 0.09", tier.Rate())
	}
}

func TestEgressPriceTierZeroRate(t *testing.T) {
	tier := EgressPriceTier{ThresholdGB: 0, RateStr: "0.000", Label: "free"}
	if tier.Rate() != 0 {
		t.Errorf("Rate(): got %v want 0", tier.Rate())
	}
}

// ---------------------------------------------------------------------------
// ComputeEgressTieredCost — zero data_gb
// ---------------------------------------------------------------------------

func TestComputeEgressTieredCostZeroDataGB(t *testing.T) {
	tiers := []EgressPriceTier{
		{ThresholdGB: 0, RateStr: "0.09", Label: "all"},
	}
	result := ComputeEgressTieredCost(tiers, 0.0)
	if result.TotalCost != "0.0000" {
		t.Errorf("total_cost: got %q want 0.0000", result.TotalCost)
	}
	if result.BlendedRatePerGB != "0.0000" {
		t.Errorf("blended_rate_per_gb: got %q want 0.0000", result.BlendedRatePerGB)
	}
	if result.DataGB != 0.0 {
		t.Errorf("data_gb: got %v want 0.0", result.DataGB)
	}
	if result.Tiers == nil || len(result.Tiers) != 0 {
		t.Errorf("tiers must be empty slice (not nil): got %v", result.Tiers)
	}
}

// ---------------------------------------------------------------------------
// ComputeEgressTieredCost — single tier
// ---------------------------------------------------------------------------

func TestComputeEgressTieredCostSingleTier(t *testing.T) {
	tiers := []EgressPriceTier{
		{ThresholdGB: 0, RateStr: "0.09", Label: "all"},
	}
	result := ComputeEgressTieredCost(tiers, 100.0)
	if result.TotalCost != "9.0000" {
		t.Errorf("total_cost: got %q want 9.0000", result.TotalCost)
	}
	if result.BlendedRatePerGB != "0.0900" {
		t.Errorf("blended_rate_per_gb: got %q want 0.0900", result.BlendedRatePerGB)
	}
	if len(result.Tiers) != 1 {
		t.Fatalf("tiers: got %d want 1", len(result.Tiers))
	}
	if gb := result.Tiers[0]["gb"].(float64); math.Abs(gb-100.0) > 1e-9 {
		t.Errorf("tier gb: got %v want 100.0", gb)
	}
	if rate := result.Tiers[0]["rate"]; rate != "0.09" {
		t.Errorf("tier rate: got %q want 0.09", rate)
	}
}

// ---------------------------------------------------------------------------
// ComputeEgressTieredCost — free first tier then paid
// ---------------------------------------------------------------------------

func TestComputeEgressTieredCostFreeFirstTier(t *testing.T) {
	tiers := []EgressPriceTier{
		{ThresholdGB: 0, RateStr: "0.000", Label: "0-100 GB (free)"},
		{ThresholdGB: 100, RateStr: "0.090", Label: "100 GB+"},
	}
	// 150 GB: 100 free + 50 paid at $0.09
	result := ComputeEgressTieredCost(tiers, 150.0)
	if result.TotalCost != "4.5000" {
		t.Errorf("total_cost: got %q want 4.5000", result.TotalCost)
	}
	if len(result.Tiers) != 2 {
		t.Fatalf("tiers: got %d want 2", len(result.Tiers))
	}
	if gb0 := result.Tiers[0]["gb"].(float64); math.Abs(gb0-100.0) > 1e-9 {
		t.Errorf("tier[0] gb: got %v want 100.0", gb0)
	}
	if cost0 := result.Tiers[0]["cost"]; cost0 != "0.0000" {
		t.Errorf("tier[0] cost: got %q want 0.0000", cost0)
	}
	if gb1 := result.Tiers[1]["gb"].(float64); math.Abs(gb1-50.0) > 1e-9 {
		t.Errorf("tier[1] gb: got %v want 50.0", gb1)
	}
	if cost1 := result.Tiers[1]["cost"]; cost1 != "4.5000" {
		t.Errorf("tier[1] cost: got %q want 4.5000", cost1)
	}
}

// ---------------------------------------------------------------------------
// ComputeEgressTieredCost — all in free tier
// ---------------------------------------------------------------------------

func TestComputeEgressTieredCostAllInFreeTier(t *testing.T) {
	tiers := []EgressPriceTier{
		{ThresholdGB: 0, RateStr: "0.000", Label: "0-100 GB (free)"},
		{ThresholdGB: 100, RateStr: "0.090", Label: "100 GB+"},
	}
	result := ComputeEgressTieredCost(tiers, 50.0)
	if result.TotalCost != "0.0000" {
		t.Errorf("total_cost: got %q want 0.0000", result.TotalCost)
	}
	if result.BlendedRatePerGB != "0.0000" {
		t.Errorf("blended_rate_per_gb: got %q want 0.0000", result.BlendedRatePerGB)
	}
	if len(result.Tiers) != 1 {
		t.Fatalf("tiers: got %d want 1", len(result.Tiers))
	}
	if gb := result.Tiers[0]["gb"].(float64); math.Abs(gb-50.0) > 1e-9 {
		t.Errorf("tier[0] gb: got %v want 50.0", gb)
	}
}

// ---------------------------------------------------------------------------
// ComputeEgressTieredCost — three tiers
// ---------------------------------------------------------------------------

func TestComputeEgressTieredCostThreeTiers(t *testing.T) {
	tiers := []EgressPriceTier{
		{ThresholdGB: 0, RateStr: "0.000", Label: "0-100 GB (free)"},
		{ThresholdGB: 100, RateStr: "0.090", Label: "100 GB-10 TB"},
		{ThresholdGB: 10_335, RateStr: "0.085", Label: "10-50 TB"},
	}
	// Price exactly 10335 GB: 100 free + 10235 at $0.09
	data := 10_335.0
	result := ComputeEgressTieredCost(tiers, data)
	expectedCost := 10235.0 * 0.090
	got, _ := strconv.ParseFloat(result.TotalCost, 64)
	if math.Abs(got-expectedCost)/expectedCost > 1e-6 {
		t.Errorf("total_cost: got %q (%.6f) want %.6f", result.TotalCost, got, expectedCost)
	}
}

// ---------------------------------------------------------------------------
// ComputeEgressTieredCost — crosses tier boundary
// ---------------------------------------------------------------------------

func TestComputeEgressTieredCostCrossesTierBoundary(t *testing.T) {
	tiers := []EgressPriceTier{
		{ThresholdGB: 0, RateStr: "0.000", Label: "0-100 GB (free)"},
		{ThresholdGB: 100, RateStr: "0.090", Label: "100 GB-1 TB"},
		{ThresholdGB: 1_024, RateStr: "0.065", Label: "1-10 TB"},
	}
	// 2048 GB: 100 free + 924 at $0.09 + 1024 at $0.065
	result := ComputeEgressTieredCost(tiers, 2048.0)
	expected := 924.0*0.090 + 1024.0*0.065
	got, _ := strconv.ParseFloat(result.TotalCost, 64)
	if math.Abs(got-expected)/expected > 1e-6 {
		t.Errorf("total_cost: got %q (%.6f) want %.6f", result.TotalCost, got, expected)
	}
	if len(result.Tiers) != 3 {
		t.Errorf("tiers: got %d want 3", len(result.Tiers))
	}
}

// ---------------------------------------------------------------------------
// ComputeEgressTieredCost — blended rate
// ---------------------------------------------------------------------------

func TestComputeEgressTieredCostBlendedRate(t *testing.T) {
	tiers := []EgressPriceTier{
		{ThresholdGB: 0, RateStr: "0.000", Label: "0-100 GB (free)"},
		{ThresholdGB: 100, RateStr: "0.090", Label: "100 GB+"},
	}
	// 200 GB: 100 free + 100 at $0.09 = $9.00; blended = 9.00/200 = 0.045
	result := ComputeEgressTieredCost(tiers, 200.0)
	if result.TotalCost != "9.0000" {
		t.Errorf("total_cost: got %q want 9.0000", result.TotalCost)
	}
	if result.BlendedRatePerGB != "0.0450" {
		t.Errorf("blended_rate_per_gb: got %q want 0.0450", result.BlendedRatePerGB)
	}
}

// ---------------------------------------------------------------------------
// ComputeEgressTieredCost — large volume (AWS-style)
// ---------------------------------------------------------------------------

func TestComputeEgressTieredCostLargeVolume(t *testing.T) {
	// AWS 5 TB = 5120 GB: 100 free + 5020 at $0.09
	result := ComputeEgressTieredCost(awsInternetEgressTiers, 5120.0)
	expectedCost := 5020.0 * 0.090
	got, _ := strconv.ParseFloat(result.TotalCost, 64)
	if math.Abs(got-expectedCost)/expectedCost > 1e-4 {
		t.Errorf("total_cost: got %q (%.4f) want %.4f", result.TotalCost, got, expectedCost)
	}
	// blended = total / 5120
	blended := expectedCost / 5120.0
	gotBlended, _ := strconv.ParseFloat(result.BlendedRatePerGB, 64)
	if math.Abs(gotBlended-blended)/blended > 1e-3 {
		t.Errorf("blended_rate_per_gb: got %q (%.6f) want %.6f", result.BlendedRatePerGB, gotBlended, blended)
	}
}

// ---------------------------------------------------------------------------
// TestEgressTiers_FirstTierBreakpoint: first 10TB at one rate, beyond at lower
// ---------------------------------------------------------------------------

// TestEgressTiers_FirstTierBreakpoint verifies that volumes crossing the 10 TB
// boundary are billed correctly: the first 10 TB (10240 GB) of paid egress uses
// the higher $0.090/GB rate and volume above 10 TB uses the reduced $0.085/GB
// rate. This mirrors the Python test_aws_internet_egress_5000gb_crosses_tiers
// intent, extended to actually cross the 10 TB boundary.
//
// Using awsInternetEgressTiers (100 GB free, then $0.090 up to 10340 GB
// cumulative = 10240 GB paid, then $0.085):
//   - 15000 GB: 100 free + 10240 at $0.090 + 4660 at $0.085
func TestEgressTiers_FirstTierBreakpoint(t *testing.T) {
	const dataGB = 15_000.0
	result := ComputeEgressTieredCost(awsInternetEgressTiers, dataGB)

	// Tier 0: 0-100 GB free → 100 GB at $0.000
	// Tier 1: 100-10340 GB → 10240 GB at $0.090
	// Tier 2: 10340+ GB → 4660 GB at $0.085
	expectedTier1Cost := 10_240.0 * 0.090
	expectedTier2Cost := 4_660.0 * 0.085
	expectedTotal := expectedTier1Cost + expectedTier2Cost

	got, _ := strconv.ParseFloat(result.TotalCost, 64)
	if math.Abs(got-expectedTotal)/expectedTotal > 1e-6 {
		t.Errorf("total_cost: got %q (%.4f) want %.4f", result.TotalCost, got, expectedTotal)
	}

	// Verify that at least 3 tier entries are present (free + first paid + second paid)
	if len(result.Tiers) < 3 {
		t.Fatalf("expected >= 3 tiers, got %d: %v", len(result.Tiers), result.Tiers)
	}

	// Tier 0 must be free ($0.000)
	if rate := result.Tiers[0]["rate"]; rate != "0.000" {
		t.Errorf("tier[0] rate: got %q want 0.000", rate)
	}
	// Tier 1 must be at $0.090
	if rate := result.Tiers[1]["rate"]; rate != "0.090" {
		t.Errorf("tier[1] rate: got %q want 0.090", rate)
	}
	// Tier 2 must be at the reduced $0.085 rate
	if rate := result.Tiers[2]["rate"]; rate != "0.085" {
		t.Errorf("tier[2] rate: got %q want 0.085", rate)
	}
	// Tier 1 volume must be exactly 10240 GB (the full first paid tier)
	if gb := result.Tiers[1]["gb"].(float64); math.Abs(gb-10_240.0) > 1e-9 {
		t.Errorf("tier[1] gb: got %v want 10240.0", gb)
	}
	// Tier 2 volume must be 4660 GB (remaining after free + first paid tier)
	if gb := result.Tiers[2]["gb"].(float64); math.Abs(gb-4_660.0) > 1e-9 {
		t.Errorf("tier[2] gb: got %v want 4660.0", gb)
	}
}

// ---------------------------------------------------------------------------
// TestEgressTiers_ZeroBytes: zero egress returns zero cost (no error)
// ---------------------------------------------------------------------------

// TestEgressTiers_ZeroBytes verifies that zero data_gb returns $0.00 with no
// error and an empty (non-nil) tiers slice. This mirrors the Python
// test_aws_egress_zero_gb_returns_rate_no_error and
// test_gcp_egress_zero_gb_no_error tests.
// Note: this behaviour is also covered by TestComputeEgressTieredCostZeroDataGB;
// this test uses the full AWS tier table to confirm zero-input is safe regardless
// of tier complexity.
func TestEgressTiers_ZeroBytes(t *testing.T) {
	result := ComputeEgressTieredCost(awsInternetEgressTiers, 0.0)

	if result.TotalCost != "0.0000" {
		t.Errorf("total_cost: got %q want 0.0000", result.TotalCost)
	}
	if result.BlendedRatePerGB != "0.0000" {
		t.Errorf("blended_rate_per_gb: got %q want 0.0000", result.BlendedRatePerGB)
	}
	if result.DataGB != 0.0 {
		t.Errorf("data_gb: got %v want 0.0", result.DataGB)
	}
	// Must be an empty slice, not nil (marshals to [] not null).
	if result.Tiers == nil {
		t.Error("tiers must be a non-nil empty slice, got nil")
	}
	if len(result.Tiers) != 0 {
		t.Errorf("tiers: got %d entries want 0", len(result.Tiers))
	}
}
