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
