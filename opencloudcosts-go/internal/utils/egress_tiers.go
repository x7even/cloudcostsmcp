// Package utils provides shared utilities for cloud pricing providers.
// This file ports egress_tiers.py: tiered egress pricing calculator.
//
// The output format matches the Python implementation exactly: all cost and
// rate values are 4-decimal-place strings (e.g. "0.0000", "0.0900"), and tier
// rates are stored as strings to preserve their exact decimal representation
// (matching Python's Decimal str() output, e.g. "0.09", "0.000", "0.090").
package utils

import (
	"fmt"
	"math"
	"strconv"
)

// EgressPriceTier describes a single price tier for egress data transfer.
// RateStr is the USD-per-GB rate expressed as a decimal string (e.g. "0.09",
// "0.000") — this matches the string a Python Decimal would produce when
// passed through str(). ThresholdGB is the inclusive lower bound of this tier.
// The last tier in a list absorbs all remaining volume.
type EgressPriceTier struct {
	ThresholdGB float64
	RateStr     string // e.g. "0.09", "0.000", "0.090"
	Label       string
}

// Rate parses RateStr and returns the numeric rate. Returns 0 on parse failure.
func (e EgressPriceTier) Rate() float64 {
	f, _ := strconv.ParseFloat(e.RateStr, 64)
	return f
}

// TieredCostResult holds the output of ComputeEgressTieredCost.
// All cost and blended-rate fields are 4-decimal-place strings matching
// Python's Decimal.quantize(Decimal("0.0001")) output.
type TieredCostResult struct {
	TotalCost        string                   `json:"total_cost"`
	BlendedRatePerGB string                   `json:"blended_rate_per_gb"`
	DataGB           float64                  `json:"data_gb"`
	Tiers            []map[string]interface{} `json:"tiers"`
}

// ComputeEgressTieredCost computes the blended egress cost across ordered tiers.
// tiers must be sorted ascending by ThresholdGB. dataGB is the total monthly volume.
// Returns string-formatted costs matching the Python Decimal output exactly.
func ComputeEgressTieredCost(tiers []EgressPriceTier, dataGB float64) TieredCostResult {
	// Zero or negative data → zero result with empty tiers slice (not nil).
	// nil would marshal to JSON null; [] is the correct empty array.
	if dataGB <= 0 || len(tiers) == 0 {
		return TieredCostResult{
			TotalCost:        "0.0000",
			BlendedRatePerGB: "0.0000",
			DataGB:           dataGB,
			Tiers:            []map[string]interface{}{},
		}
	}

	remaining := dataGB
	total := 0.0
	tierSplits := make([]map[string]interface{}, 0, len(tiers))

	for i, tier := range tiers {
		if remaining <= 0 {
			break
		}

		rate := tier.Rate()

		var tierCapacity float64
		if i+1 < len(tiers) {
			tierCapacity = tiers[i+1].ThresholdGB - tier.ThresholdGB
		} else {
			// Last tier: absorbs all remaining volume
			tierCapacity = remaining
		}

		vol := math.Min(remaining, tierCapacity)
		cost := vol * rate
		total += cost

		tierSplits = append(tierSplits, map[string]interface{}{
			"label": tier.Label,
			"gb":    vol,
			"rate":  tier.RateStr, // preserve original string (e.g. "0.09", "0.000")
			"cost":  fmt4(cost),
		})
		remaining -= vol
	}

	var blended float64
	if dataGB > 0 {
		blended = total / dataGB
	}

	return TieredCostResult{
		TotalCost:        fmt4(total),
		BlendedRatePerGB: fmt4(blended),
		DataGB:           dataGB,
		Tiers:            tierSplits,
	}
}

// fmt4 formats a float64 to a string with exactly 4 decimal places,
// matching Python's Decimal.quantize(Decimal("0.0001")) output.
func fmt4(f float64) string {
	return fmt.Sprintf("%.4f", f)
}
