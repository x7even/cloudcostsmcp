package utils

import (
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// ParseAWSUnit
// ---------------------------------------------------------------------------

var awsUnitCases = []struct {
	input string
	want  PriceUnit
}{
	{"Hrs", PriceUnitPerHour},
	{"Hours", PriceUnitPerHour},
	{"hr", PriceUnitPerHour},
	{"vCPU-Hours", PriceUnitPerHour},
	{"ACU-Hr", PriceUnitPerHour},
	{"GB-Mo", PriceUnitPerGBMonth},
	{"GB-month", PriceUnitPerGBMonth},
	{"GB", PriceUnitPerGB},
	{"IOPS-Mo", PriceUnitPerIOPSMonth},
	{"Requests", PriceUnitPerRequest},
	{"Request", PriceUnitPerRequest},
	{"queries", PriceUnitPerRequest},
	{"IOs", PriceUnitPerRequest},
	{"Events", PriceUnitPerRequest},
	{"Messages", PriceUnitPerRequest},
	{"Queries", PriceUnitPerQuery},
	{"Lambda-GB-Second", PriceUnitPerGBSecond},
	{"seconds", PriceUnitPerUnit},
	{"Units", PriceUnitPerUnit},
	{"unit", PriceUnitPerUnit},
	{"RCU", PriceUnitPerUnit},
	{"WCU", PriceUnitPerUnit},
	{"Rule", PriceUnitPerUnit},
	{"Alarm", PriceUnitPerUnit},
	{"Metrics", PriceUnitPerUnit},
}

func TestParseAWSUnit(t *testing.T) {
	for _, tc := range awsUnitCases {
		got := ParseAWSUnit(tc.input)
		if got != tc.want {
			t.Errorf("ParseAWSUnit(%q): got %q want %q", tc.input, got, tc.want)
		}
	}
}

func TestParseAWSUnitUnknownFallsBackToPerUnit(t *testing.T) {
	got := ParseAWSUnit("SomeWeirdUnit")
	if got != PriceUnitPerUnit {
		t.Errorf("got %q want per_unit", got)
	}
}

// ---------------------------------------------------------------------------
// ParseGCPUnit
// ---------------------------------------------------------------------------

var gcpUnitCases = []struct {
	input string
	want  PriceUnit
}{
	{"h", PriceUnitPerHour},
	{"hour", PriceUnitPerHour},
	{"GiBy.mo", PriceUnitPerGBMonth},
	{"GBy.mo", PriceUnitPerGBMonth},
	{"GBy", PriceUnitPerGB},
	{"GiBy", PriceUnitPerGB},
	{"count", PriceUnitPerRequest},
	{"mo", PriceUnitPerMonth},
}

func TestParseGCPUnit(t *testing.T) {
	for _, tc := range gcpUnitCases {
		got := ParseGCPUnit(tc.input)
		if got != tc.want {
			t.Errorf("ParseGCPUnit(%q): got %q want %q", tc.input, got, tc.want)
		}
	}
}

func TestParseGCPUnitUnknownFallsBackToPerUnit(t *testing.T) {
	got := ParseGCPUnit("unknownUnit")
	if got != PriceUnitPerUnit {
		t.Errorf("got %q want per_unit", got)
	}
}

// ---------------------------------------------------------------------------
// HoursToMonthly / MonthlyToHourly
// ---------------------------------------------------------------------------

func TestHoursToMonthly730(t *testing.T) {
	// 730 hours/month definition
	got := HoursToMonthly(1.0)
	if math.Abs(got-730.0) > 1e-9 {
		t.Errorf("got %v want 730.0", got)
	}
}

func TestHoursToMonthlyZero(t *testing.T) {
	if HoursToMonthly(0) != 0 {
		t.Error("expected 0")
	}
}

func TestMonthlyToHourlyRoundtrip(t *testing.T) {
	hourly := 0.192
	monthly := HoursToMonthly(hourly)
	back := MonthlyToHourly(monthly)
	if math.Abs(back-hourly) > 1e-12 {
		t.Errorf("roundtrip: got %v want %v", back, hourly)
	}
}

func TestMonthlyToHourlyKnownValue(t *testing.T) {
	// $146.00/mo ÷ 730 ≈ $0.2/hr
	got := MonthlyToHourly(146.0)
	want := 146.0 / 730.0
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("got %v want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// GBToTB / TBToGB
// ---------------------------------------------------------------------------

func TestGBToTB(t *testing.T) {
	if math.Abs(GBToTB(1024.0)-1.0) > 1e-12 {
		t.Errorf("1024 GB → got %v want 1.0 TB", GBToTB(1024.0))
	}
}

func TestTBToGB(t *testing.T) {
	if math.Abs(TBToGB(1.0)-1024.0) > 1e-12 {
		t.Errorf("1 TB → got %v want 1024.0 GB", TBToGB(1.0))
	}
}

func TestGBTBRoundtrip(t *testing.T) {
	gb := 5120.0
	if math.Abs(TBToGB(GBToTB(gb))-gb) > 1e-9 {
		t.Error("GB→TB→GB roundtrip failed")
	}
}

// ---------------------------------------------------------------------------
// GCPMoneyToFloat
// ---------------------------------------------------------------------------

func TestGCPMoneyToFloatZero(t *testing.T) {
	v := GCPMoneyToFloat("0", 0)
	if v != 0.0 {
		t.Errorf("got %v want 0.0", v)
	}
}

func TestGCPMoneyToFloatIntegerUnits(t *testing.T) {
	v := GCPMoneyToFloat("3", 0)
	if math.Abs(v-3.0) > 1e-12 {
		t.Errorf("got %v want 3.0", v)
	}
}

func TestGCPMoneyToFloatNanos(t *testing.T) {
	// 0 units + 500_000_000 nanos = 0.5
	v := GCPMoneyToFloat("0", 500000000)
	if math.Abs(v-0.5) > 1e-12 {
		t.Errorf("got %v want 0.5", v)
	}
}
