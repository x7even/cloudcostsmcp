// Package utils provides shared utilities for cloud pricing providers.
// This file ports baseline.py: regional price delta computation.
package utils

import (
	"fmt"
	"strconv"
	"strings"
)

// ApplyBaselineDeltas mutates result maps in-place to add delta fields vs a
// baseline region. Each map gets three new keys:
//
//   - delta_per_hour:  e.g. "$+0.046800"
//   - delta_monthly:   e.g. "$+34.16/mo"
//   - delta_pct:       e.g. "+30.6%" or "-38.9%"
//
// The baseline region entry shows $0.000000 / $0.00/mo / +0.0%.
// hourlyKey and monthlyKey name the price fields in each result map.
// Returns an error if baselineRegion is not present in results.
func ApplyBaselineDeltas(
	results []map[string]interface{},
	baselineRegion string,
	hourlyKey string,
	monthlyKey string,
) error {
	// Find baseline entry
	var baseline map[string]interface{}
	for _, r := range results {
		if r["region"] == baselineRegion {
			baseline = r
			break
		}
	}
	if baseline == nil {
		available := make([]string, 0, len(results))
		for _, r := range results {
			if reg, ok := r["region"].(string); ok {
				available = append(available, reg)
			}
		}
		return fmt.Errorf("baseline region %q not found in results. Available: %v", baselineRegion, available)
	}

	baseH, err := parseHourly(baseline[hourlyKey])
	if err != nil {
		return fmt.Errorf("baseline hourly price: %w", err)
	}
	baseM, err := parseMonthly(baseline[monthlyKey])
	if err != nil {
		return fmt.Errorf("baseline monthly price: %w", err)
	}

	for _, r := range results {
		h, err := parseHourly(r[hourlyKey])
		if err != nil {
			return fmt.Errorf("hourly price in result: %w", err)
		}
		m, err := parseMonthly(r[monthlyKey])
		if err != nil {
			return fmt.Errorf("monthly price in result: %w", err)
		}

		dh := h - baseH
		dm := m - baseM
		var pct float64
		if baseH > 0 {
			pct = (dh / baseH) * 100
		}

		r["delta_per_hour"] = fmt.Sprintf("$%+.6f", dh)
		r["delta_monthly"] = fmt.Sprintf("$%+.2f/mo", dm)
		r["delta_pct"] = fmt.Sprintf("%+.1f%%", pct)
	}

	return nil
}

// parseHourly extracts a float64 hourly price from either a map{"amount": ...}
// or a string like "$0.200000/hr".
func parseHourly(val interface{}) (float64, error) {
	if val == nil {
		return 0, nil
	}
	if m, ok := val.(map[string]interface{}); ok {
		return toFloat64(m["amount"])
	}
	s := strings.TrimPrefix(fmt.Sprintf("%v", val), "$")
	// strip everything from "/" onwards
	if idx := strings.Index(s, "/"); idx >= 0 {
		s = s[:idx]
	}
	return strconv.ParseFloat(strings.TrimSpace(s), 64)
}

// parseMonthly extracts a float64 monthly cost from either a map{"amount": ...}
// or a string like "$146.00/mo" or "$0.00/mo".
func parseMonthly(val interface{}) (float64, error) {
	if val == nil {
		return 0, nil
	}
	if m, ok := val.(map[string]interface{}); ok {
		return toFloat64(m["amount"])
	}
	s := fmt.Sprintf("%v", val)
	s = strings.TrimPrefix(s, "$")
	s = strings.ReplaceAll(s, "/mo", "")
	return strconv.ParseFloat(strings.TrimSpace(s), 64)
}

// toFloat64 converts an interface{} to float64, supporting float32/float64/int/string.
func toFloat64(v interface{}) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case string:
		return strconv.ParseFloat(x, 64)
	default:
		return strconv.ParseFloat(fmt.Sprintf("%v", v), 64)
	}
}
