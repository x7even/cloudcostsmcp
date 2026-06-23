// Package utils provides shared utilities for cloud pricing providers.
// This file ports units.py: unit conversion helpers and provider unit maps.
package utils

// PriceUnit represents the billing granularity of a price.
// These values mirror the PriceUnit enum in models.py / models.go.
type PriceUnit string

const (
	PriceUnitPerHour      PriceUnit = "per_hour"
	PriceUnitPerMonth     PriceUnit = "per_month"
	PriceUnitPerGBMonth   PriceUnit = "per_gb_month"
	PriceUnitPerGB        PriceUnit = "per_gb"
	PriceUnitPerIOPSMonth PriceUnit = "per_iops_month"
	PriceUnitPerMBPSMonth PriceUnit = "per_mbps_month"
	PriceUnitPerRequest   PriceUnit = "per_request"
	PriceUnitPerGBSecond  PriceUnit = "per_gb_second"
	PriceUnitPerQuery     PriceUnit = "per_query"
	PriceUnitPerUnit      PriceUnit = "per_unit"
)

// AWSUnitMap maps AWS Pricing API unit strings to our PriceUnit enum.
// Mirrors AWS_UNIT_MAP in units.py.
var AWSUnitMap = map[string]PriceUnit{
	"Hrs":              PriceUnitPerHour,
	"Hours":            PriceUnitPerHour,
	"hr":               PriceUnitPerHour,
	"GB-Mo":            PriceUnitPerGBMonth,
	"GB-month":         PriceUnitPerGBMonth,
	"GB":               PriceUnitPerGB,
	"IOPS-Mo":          PriceUnitPerIOPSMonth,
	"Requests":         PriceUnitPerRequest,
	"Request":          PriceUnitPerRequest,
	"queries":          PriceUnitPerRequest,
	"Queries":          PriceUnitPerQuery,
	"Lambda-GB-Second": PriceUnitPerGBSecond,
	"seconds":          PriceUnitPerUnit,
	"Units":            PriceUnitPerUnit,
	"unit":             PriceUnitPerUnit,
	"vCPU-Hours":       PriceUnitPerHour,
	"ACU-Hr":           PriceUnitPerHour,
	"RCU":              PriceUnitPerUnit,
	"WCU":              PriceUnitPerUnit,
	"IOs":              PriceUnitPerRequest,
	"Rule":             PriceUnitPerUnit,
	"Alarm":            PriceUnitPerUnit,
	"Metrics":          PriceUnitPerUnit,
	"Events":           PriceUnitPerRequest,
	"Messages":         PriceUnitPerRequest,
}

// GCPUnitMap maps GCP Billing Catalog API usageUnit strings to our PriceUnit enum.
// Mirrors GCP_UNIT_MAP in units.py.
var GCPUnitMap = map[string]PriceUnit{
	"h":       PriceUnitPerHour,
	"hour":    PriceUnitPerHour,
	"GiBy.mo": PriceUnitPerGBMonth,
	"GBy.mo":  PriceUnitPerGBMonth,
	"GBy":     PriceUnitPerGB,
	"GiBy":    PriceUnitPerGB,
	"count":   PriceUnitPerRequest,
	"mo":      PriceUnitPerMonth,
}

// ParseAWSUnit converts an AWS Pricing API unit string to a PriceUnit.
// Returns PriceUnitPerUnit for unknown strings.
func ParseAWSUnit(unitStr string) PriceUnit {
	if u, ok := AWSUnitMap[unitStr]; ok {
		return u
	}
	return PriceUnitPerUnit
}

// ParseGCPUnit converts a GCP Billing Catalog usageUnit string to a PriceUnit.
// Returns PriceUnitPerUnit for unknown strings.
func ParseGCPUnit(unitStr string) PriceUnit {
	if u, ok := GCPUnitMap[unitStr]; ok {
		return u
	}
	return PriceUnitPerUnit
}

// GCPMoneyToFloat converts a GCP Money proto (units string + nanos int) to float64.
// Mirrors gcp_money_to_decimal() in units.py.
// Note: GCPMoney in gcp_specs.go provides the same conversion — this variant
// accepts the units field as a numeric-only string (no sign or separator).
func GCPMoneyToFloat(units string, nanos int) float64 {
	return GCPMoney(units, nanos)
}

// HoursToMonthly converts a per-hour price to an estimated monthly price.
// Uses 730 hours/month (matching Python's default).
func HoursToMonthly(pricePerHour float64) float64 {
	return pricePerHour * 730.0
}

// MonthlyToHourly converts a per-month price to a per-hour price.
// Uses 730 hours/month (matching Python's default).
func MonthlyToHourly(pricePerMonth float64) float64 {
	return pricePerMonth / 730.0
}

// GBToTB converts gigabytes to terabytes.
func GBToTB(gb float64) float64 {
	return gb / 1024.0
}

// TBToGB converts terabytes to gigabytes.
func TBToGB(tb float64) float64 {
	return tb * 1024.0
}
