// Package utils provides shared utilities for cloud pricing providers.
// This file ports money.py: price and money formatting helpers.
package utils

import "fmt"

// PriceDict is a structured price value with embedded display string.
// It mirrors the dict returned by Python's _price() helper.
type PriceDict struct {
	Amount   float64 `json:"amount"`
	Unit     string  `json:"unit"`
	Currency string  `json:"currency"`
	Display  string  `json:"display"`
}

// MoneyDict is a structured aggregate money value (monthly costs, totals).
// It mirrors the dict returned by Python's _money() helper.
type MoneyDict struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
	Display  string  `json:"display"`
}

// Price returns a structured price dict with an embedded display string.
// For amounts between 0 and 0.0000005 (exclusive), scientific notation is used.
// All other values (including zero) use fixed 6-decimal-place notation.
func Price(amount float64, unit string) PriceDict {
	var display string
	if amount > 0 && amount < 0.0000005 {
		display = fmt.Sprintf("$%.2e/%s", amount, unit)
	} else {
		display = fmt.Sprintf("$%.6f/%s", amount, unit)
	}
	return PriceDict{
		Amount:   amount,
		Unit:     unit,
		Currency: "USD",
		Display:  display,
	}
}

// Money returns a structured aggregate money value with display string.
// label is appended to the display string (e.g. "/mo", "/yr", or "").
func Money(amount float64, label string) MoneyDict {
	return MoneyDict{
		Amount:   amount,
		Currency: "USD",
		Display:  fmt.Sprintf("$%.2f%s", amount, label),
	}
}

// MonthlyEstimate computes the monthly cost from a per-hour rate.
// Uses 730 hours/month to match Python's Decimal("730").
func MonthlyEstimate(hourlyRate float64) float64 {
	return hourlyRate * 730.0
}

// HourlyFromMonthly converts a per-month price to per-hour (÷ 730).
func HourlyFromMonthly(monthlyRate float64) float64 {
	return monthlyRate / 730.0
}
