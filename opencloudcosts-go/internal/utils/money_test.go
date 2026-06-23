package utils

import (
	"math"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Price
// ---------------------------------------------------------------------------

func TestPriceReturnsRequiredFields(t *testing.T) {
	p := Price(0.2, "hr")
	if p.Unit != "hr" {
		t.Errorf("unit: got %q want %q", p.Unit, "hr")
	}
	if p.Currency != "USD" {
		t.Errorf("currency: got %q want %q", p.Currency, "USD")
	}
	if p.Display == "" {
		t.Error("display must not be empty")
	}
}

func TestPriceCurrencyIsUSD(t *testing.T) {
	p := Price(0.1, "hr")
	if p.Currency != "USD" {
		t.Errorf("got %q want USD", p.Currency)
	}
}

func TestPriceAmountRoundtrip(t *testing.T) {
	p := Price(0.123456, "hr")
	if math.Abs(p.Amount-0.123456) > 1e-9 {
		t.Errorf("amount: got %v want 0.123456", p.Amount)
	}
}

func TestPriceUnitPreserved(t *testing.T) {
	p := Price(1.5, "GB")
	if p.Unit != "GB" {
		t.Errorf("unit: got %q want GB", p.Unit)
	}
}

func TestPriceNormalDisplayFormat(t *testing.T) {
	p := Price(0.2, "hr")
	if p.Display != "$0.200000/hr" {
		t.Errorf("display: got %q want $0.200000/hr", p.Display)
	}
}

func TestPriceNormalDisplaySixDecimalPlaces(t *testing.T) {
	p := Price(0.001234, "hr")
	if p.Display != "$0.001234/hr" {
		t.Errorf("display: got %q want $0.001234/hr", p.Display)
	}
}

func TestPriceTinyValueUsesScientificNotation(t *testing.T) {
	// 0.0000001 < 0.0000005 → scientific notation
	p := Price(0.0000001, "GB")
	if !strings.ContainsAny(p.Display, "eE") {
		t.Errorf("expected scientific notation in display: %q", p.Display)
	}
}

func TestPriceZeroUsesFixedNotation(t *testing.T) {
	// zero is NOT above 0, so fixed notation
	p := Price(0, "hr")
	lower := strings.ToLower(p.Display)
	if strings.Contains(lower, "e") {
		t.Errorf("expected fixed notation for zero: %q", p.Display)
	}
	if p.Display != "$0.000000/hr" {
		t.Errorf("display: got %q want $0.000000/hr", p.Display)
	}
}

func TestPriceBoundaryJustAboveTiny(t *testing.T) {
	// 0.0000005 is NOT below threshold (condition is strictly less than)
	p := Price(0.0000005, "hr")
	lower := strings.ToLower(p.Display)
	if strings.Contains(lower, "e") {
		t.Errorf("0.0000005 must use fixed notation: %q", p.Display)
	}
}

func TestPriceBoundaryJustBelowTiny(t *testing.T) {
	// 0.0000004 IS below threshold
	p := Price(0.0000004, "hr")
	lower := strings.ToLower(p.Display)
	if !strings.Contains(lower, "e") {
		t.Errorf("0.0000004 must use scientific notation: %q", p.Display)
	}
}

func TestPriceLargeValue(t *testing.T) {
	p := Price(1234.56789, "mo")
	if p.Display != "$1234.567890/mo" {
		t.Errorf("display: got %q want $1234.567890/mo", p.Display)
	}
	if math.Abs(p.Amount-1234.56789) > 1e-9 {
		t.Errorf("amount: got %v want 1234.56789", p.Amount)
	}
}

// ---------------------------------------------------------------------------
// Money
// ---------------------------------------------------------------------------

func TestMoneyReturnsRequiredFields(t *testing.T) {
	m := Money(146.0, "")
	if m.Currency != "USD" {
		t.Errorf("currency: got %q want USD", m.Currency)
	}
	if m.Display == "" {
		t.Error("display must not be empty")
	}
}

func TestMoneyCurrencyIsUSD(t *testing.T) {
	m := Money(50.0, "")
	if m.Currency != "USD" {
		t.Errorf("got %q want USD", m.Currency)
	}
}

func TestMoneyAmountRoundtrip(t *testing.T) {
	m := Money(99.99, "")
	if math.Abs(m.Amount-99.99) > 1e-9 {
		t.Errorf("amount: got %v want 99.99", m.Amount)
	}
}

func TestMoneyDisplayTwoDecimalPlaces(t *testing.T) {
	m := Money(146.0, "")
	if m.Display != "$146.00" {
		t.Errorf("display: got %q want $146.00", m.Display)
	}
}

func TestMoneyDisplayWithLabel(t *testing.T) {
	m := Money(146.0, "/mo")
	if m.Display != "$146.00/mo" {
		t.Errorf("display: got %q want $146.00/mo", m.Display)
	}
}

func TestMoneyZeroAmount(t *testing.T) {
	m := Money(0, "")
	if m.Display != "$0.00" {
		t.Errorf("display: got %q want $0.00", m.Display)
	}
	if m.Amount != 0.0 {
		t.Errorf("amount: got %v want 0.0", m.Amount)
	}
}

func TestMoneyLabelEmptyString(t *testing.T) {
	m := Money(10.5, "")
	if m.Display != "$10.50" {
		t.Errorf("display: got %q want $10.50", m.Display)
	}
}

func TestMoneyRoundsDisplayToTwoDecimals(t *testing.T) {
	m := Money(9.9, "")
	if m.Display != "$9.90" {
		t.Errorf("display: got %q want $9.90", m.Display)
	}
}

func TestMoneyLargeValue(t *testing.T) {
	m := Money(10000.0, "/yr")
	if m.Display != "$10000.00/yr" {
		t.Errorf("display: got %q want $10000.00/yr", m.Display)
	}
	if math.Abs(m.Amount-10000.0) > 1e-9 {
		t.Errorf("amount: got %v want 10000.0", m.Amount)
	}
}

// ---------------------------------------------------------------------------
// MonthlyEstimate
// ---------------------------------------------------------------------------

func TestMonthlyEstimate730Hours(t *testing.T) {
	// 0.192 * 730 = 140.16 exactly in float64/shortest-repr
	got := MonthlyEstimate(0.192)
	want := 0.192 * 730.0
	if got != want {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestMonthlyEstimateZero(t *testing.T) {
	if MonthlyEstimate(0) != 0 {
		t.Error("expected 0")
	}
}

// ---------------------------------------------------------------------------
// HourlyFromMonthly
// ---------------------------------------------------------------------------

func TestHourlyFromMonthlyRoundtrip(t *testing.T) {
	hourly := 0.5
	monthly := MonthlyEstimate(hourly)
	back := HourlyFromMonthly(monthly)
	if math.Abs(back-hourly) > 1e-12 {
		t.Errorf("roundtrip: got %v want %v", back, hourly)
	}
}
