package utils

import (
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// ParseInstanceType — exact table lookups
// ---------------------------------------------------------------------------

func TestParseInstanceTypeExactLookupE2Standard4(t *testing.T) {
	vcpu, mem, ok := ParseInstanceType("e2-standard-4")
	if !ok {
		t.Fatal("expected ok")
	}
	if vcpu != 4 {
		t.Errorf("vcpu: got %d want 4", vcpu)
	}
	if math.Abs(mem-16.0) > 1e-9 {
		t.Errorf("mem: got %v want 16.0", mem)
	}
}

func TestParseInstanceTypeExactLookupN1Standard8(t *testing.T) {
	vcpu, mem, ok := ParseInstanceType("n1-standard-8")
	if !ok {
		t.Fatal("expected ok")
	}
	if vcpu != 8 {
		t.Errorf("vcpu: got %d want 8", vcpu)
	}
	if math.Abs(mem-30.0) > 1e-9 {
		t.Errorf("mem: got %v want 30.0", mem)
	}
}

func TestParseInstanceTypeExactLookupN2Highmem128(t *testing.T) {
	vcpu, mem, ok := ParseInstanceType("n2-highmem-128")
	if !ok {
		t.Fatal("expected ok")
	}
	if vcpu != 128 {
		t.Errorf("vcpu: got %d want 128", vcpu)
	}
	if math.Abs(mem-864.0) > 1e-9 {
		t.Errorf("mem: got %v want 864.0", mem)
	}
}

func TestParseInstanceTypeExactLookupA2Highgpu1g(t *testing.T) {
	vcpu, mem, ok := ParseInstanceType("a2-highgpu-1g")
	if !ok {
		t.Fatal("expected ok")
	}
	if vcpu != 12 {
		t.Errorf("vcpu: got %d want 12", vcpu)
	}
	if math.Abs(mem-85.0) > 1e-9 {
		t.Errorf("mem: got %v want 85.0", mem)
	}
}

func TestParseInstanceTypeExactLookupM1Megamem96(t *testing.T) {
	vcpu, mem, ok := ParseInstanceType("m1-megamem-96")
	if !ok {
		t.Fatal("expected ok")
	}
	if vcpu != 96 {
		t.Errorf("vcpu: got %d want 96", vcpu)
	}
	if math.Abs(mem-1433.6) > 0.01 {
		t.Errorf("mem: got %v want 1433.6", mem)
	}
}

// ---------------------------------------------------------------------------
// ParseInstanceType — naming convention fallback
// ---------------------------------------------------------------------------

func TestParseInstanceTypeFallbackN2Standard16(t *testing.T) {
	// n2-standard-16 IS in the table; ensure consistent result
	vcpu, mem, ok := ParseInstanceType("n2-standard-16")
	if !ok {
		t.Fatal("expected ok")
	}
	if vcpu != 16 {
		t.Errorf("vcpu: got %d want 16", vcpu)
	}
	if math.Abs(mem-64.0) > 1e-9 {
		t.Errorf("mem: got %v want 64.0", mem)
	}
}

func TestParseInstanceTypeFallbackN1HighcpuUnlisted(t *testing.T) {
	// n1-highcpu-48 is NOT in the table; falls back to naming convention.
	// The naming convention uses seriesRamRatio["highcpu"] = 2.0 (generic default).
	// N1 highcpu's actual 0.9 GB/vCPU ratio is only encoded in the exact-spec table.
	vcpu, mem, ok := ParseInstanceType("n1-highcpu-48")
	if !ok {
		t.Fatal("expected ok for unlisted type (fallback applies)")
	}
	if vcpu != 48 {
		t.Errorf("vcpu: got %d want 48", vcpu)
	}
	// Fallback: highcpu series ratio = 2.0 → 48 * 2.0 = 96.0
	if math.Abs(mem-96.0) > 1e-9 {
		t.Errorf("mem (fallback): got %v want 96.0", mem)
	}
}

func TestParseInstanceTypeUnknownFamilyFallsBackToDefaultRatio(t *testing.T) {
	// "xx-unknown-99": family="xx", series="unknown", vcpu="99"
	// "99" parses as integer; unknown series defaults to 4.0 GB/vCPU.
	// The code does NOT reject unknown families — it falls through to the default.
	vcpu, _, ok := ParseInstanceType("xx-unknown-99")
	if !ok {
		t.Fatal("naming-convention fallback should succeed for parseable vcpu string")
	}
	if vcpu != 99 {
		t.Errorf("vcpu: got %d want 99", vcpu)
	}
}

func TestParseInstanceTypeNonNumericVCPUReturnsNotOK(t *testing.T) {
	// "a2-highgpu-abc": vcpu="abc" → parseInt returns -1 → not ok
	_, _, ok := ParseInstanceType("a2-highgpu-abc")
	if ok {
		t.Error("non-numeric vcpu must return not ok")
	}
}

func TestParseInstanceTypeTooFewParts(t *testing.T) {
	_, _, ok := ParseInstanceType("n2standard")
	if ok {
		t.Error("expected not ok for badly formatted type")
	}
}

// ---------------------------------------------------------------------------
// GetMachineFamily
// ---------------------------------------------------------------------------

func TestGetMachineFamilyN2(t *testing.T) {
	if f := GetMachineFamily("n2-standard-4"); f != "n2" {
		t.Errorf("got %q want n2", f)
	}
}

func TestGetMachineFamilyN2D(t *testing.T) {
	if f := GetMachineFamily("n2d-standard-8"); f != "n2d" {
		t.Errorf("got %q want n2d", f)
	}
}

func TestGetMachineFamilyE2(t *testing.T) {
	if f := GetMachineFamily("e2-standard-16"); f != "e2" {
		t.Errorf("got %q want e2", f)
	}
}

func TestGetMachineFamilyA2(t *testing.T) {
	if f := GetMachineFamily("a2-highgpu-1g"); f != "a2" {
		t.Errorf("got %q want a2", f)
	}
}

// ---------------------------------------------------------------------------
// GCPFamilySKU
// ---------------------------------------------------------------------------

func TestGCPFamilySKUE2CPUDesc(t *testing.T) {
	sku, ok := GCPFamilySKU["e2"]
	if !ok {
		t.Fatal("e2 family not in GCPFamilySKU")
	}
	if sku.CPUDesc != "E2 Instance Core" {
		t.Errorf("got %q want E2 Instance Core", sku.CPUDesc)
	}
}

func TestGCPFamilySKUN1NoCUDs(t *testing.T) {
	sku, ok := GCPFamilySKU["n1"]
	if !ok {
		t.Fatal("n1 family not in GCPFamilySKU")
	}
	if sku.CUDCPUDesc != "" {
		t.Errorf("n1 must have empty CUDCPUDesc (uses SUD, not CUD), got %q", sku.CUDCPUDesc)
	}
}

func TestGCPFamilySKUC3SpotLabel(t *testing.T) {
	sku, ok := GCPFamilySKU["c3"]
	if !ok {
		t.Fatal("c3 family not in GCPFamilySKU")
	}
	if sku.PreemptCPUDesc != "Spot Preemptible C3 Instance Core" {
		t.Errorf("c3 preempt desc: got %q", sku.PreemptCPUDesc)
	}
}

func TestGCPFamilySKUT2ANoCUDs(t *testing.T) {
	sku, ok := GCPFamilySKU["t2a"]
	if !ok {
		t.Fatal("t2a family not in GCPFamilySKU")
	}
	if sku.CUDCPUDesc != "" || sku.CUDRAMDesc != "" {
		t.Errorf("t2a must have empty CUD descs, got cpu=%q ram=%q", sku.CUDCPUDesc, sku.CUDRAMDesc)
	}
}

// ---------------------------------------------------------------------------
// C4 / Z3 catalog coverage (RC3-003)
// ---------------------------------------------------------------------------

func TestParseInstanceTypeExactLookupC4Standard4(t *testing.T) {
	// Source: https://docs.cloud.google.com/compute/docs/general-purpose-machines
	vcpu, mem, ok := ParseInstanceType("c4-standard-4")
	if !ok {
		t.Fatal("expected ok")
	}
	if vcpu != 4 {
		t.Errorf("vcpu: got %d want 4", vcpu)
	}
	if math.Abs(mem-15.0) > 1e-9 {
		t.Errorf("mem: got %v want 15.0", mem)
	}
}

func TestParseInstanceTypeExactLookupC4Highmem192(t *testing.T) {
	// 192 vCPU * 7.75 GB/vCPU = 1488 GB.
	vcpu, mem, ok := ParseInstanceType("c4-highmem-192")
	if !ok {
		t.Fatal("expected ok")
	}
	if vcpu != 192 {
		t.Errorf("vcpu: got %d want 192", vcpu)
	}
	if math.Abs(mem-1488.0) > 1e-9 {
		t.Errorf("mem: got %v want 1488.0", mem)
	}
}

func TestParseInstanceTypeExactLookupC4Highcpu16(t *testing.T) {
	// 16 vCPU * 2 GB/vCPU = 32 GB.
	vcpu, mem, ok := ParseInstanceType("c4-highcpu-16")
	if !ok {
		t.Fatal("expected ok")
	}
	if vcpu != 16 {
		t.Errorf("vcpu: got %d want 16", vcpu)
	}
	if math.Abs(mem-32.0) > 1e-9 {
		t.Errorf("mem: got %v want 32.0", mem)
	}
}

func TestParseInstanceTypeExactLookupZ3Highlssd(t *testing.T) {
	// Z3 requires the local-SSD suffix in the full instance type name.
	// Source: https://docs.cloud.google.com/compute/docs/storage-optimized-machines
	vcpu, mem, ok := ParseInstanceType("z3-highmem-88-highlssd")
	if !ok {
		t.Fatal("expected ok")
	}
	if vcpu != 88 {
		t.Errorf("vcpu: got %d want 88", vcpu)
	}
	if math.Abs(mem-704.0) > 1e-9 {
		t.Errorf("mem: got %v want 704.0", mem)
	}
}

func TestParseInstanceTypeExactLookupZ3Standardlssd(t *testing.T) {
	vcpu, mem, ok := ParseInstanceType("z3-highmem-176-standardlssd")
	if !ok {
		t.Fatal("expected ok")
	}
	if vcpu != 176 {
		t.Errorf("vcpu: got %d want 176", vcpu)
	}
	if math.Abs(mem-1406.0) > 1e-9 {
		t.Errorf("mem: got %v want 1406.0", mem)
	}
}

func TestGetMachineFamilyC4(t *testing.T) {
	if f := GetMachineFamily("c4-standard-4"); f != "c4" {
		t.Errorf("got %q want c4", f)
	}
}

func TestGetMachineFamilyZ3(t *testing.T) {
	// Family extraction must work even with the mandatory lssd suffix.
	if f := GetMachineFamily("z3-highmem-88-highlssd"); f != "z3" {
		t.Errorf("got %q want z3", f)
	}
}

func TestGCPFamilySKUC4OnDemandAndSpot(t *testing.T) {
	// Source: https://cloud.google.com/skus/sku-groups/c4-on-demand-vms and
	// https://cloud.google.com/skus/sku-groups/c4-spot-preemptible-vms
	sku, ok := GCPFamilySKU["c4"]
	if !ok {
		t.Fatal("c4 family not in GCPFamilySKU")
	}
	if sku.CPUDesc != "C4 Instance Core" {
		t.Errorf("CPUDesc: got %q want %q", sku.CPUDesc, "C4 Instance Core")
	}
	if sku.RAMDesc != "C4 Instance Ram" {
		t.Errorf("RAMDesc: got %q want %q", sku.RAMDesc, "C4 Instance Ram")
	}
	if sku.PreemptCPUDesc != "Spot Preemptible C4 Instance Core" {
		t.Errorf("PreemptCPUDesc: got %q", sku.PreemptCPUDesc)
	}
	if sku.PreemptRAMDesc != "Spot Preemptible C4 Instance Ram" {
		t.Errorf("PreemptRAMDesc: got %q", sku.PreemptRAMDesc)
	}
}

func TestGCPFamilySKUZ3OnDemandAndSpot(t *testing.T) {
	// Source: https://cloud.google.com/skus/sku-groups/z3-on-demand-vms and
	// https://cloud.google.com/skus/sku-groups/z3-spot-preemptible-vms
	sku, ok := GCPFamilySKU["z3"]
	if !ok {
		t.Fatal("z3 family not in GCPFamilySKU")
	}
	if sku.CPUDesc != "Z3 Instance Core" {
		t.Errorf("CPUDesc: got %q want %q", sku.CPUDesc, "Z3 Instance Core")
	}
	if sku.RAMDesc != "Z3 Instance Ram" {
		t.Errorf("RAMDesc: got %q want %q", sku.RAMDesc, "Z3 Instance Ram")
	}
	if sku.PreemptCPUDesc != "Spot Preemptible Z3 Instance Core" {
		t.Errorf("PreemptCPUDesc: got %q", sku.PreemptCPUDesc)
	}
	if sku.PreemptRAMDesc != "Spot Preemptible Z3 Instance Ram" {
		t.Errorf("PreemptRAMDesc: got %q", sku.PreemptRAMDesc)
	}
}

// ---------------------------------------------------------------------------
// GCPMajorRegions
// ---------------------------------------------------------------------------

func TestGCPMajorRegionsLength(t *testing.T) {
	if len(GCPMajorRegions) != 12 {
		t.Errorf("expected 12 major regions, got %d", len(GCPMajorRegions))
	}
}

func TestGCPMajorRegionsContainsUSCentral1(t *testing.T) {
	found := false
	for _, r := range GCPMajorRegions {
		if r == "us-central1" {
			found = true
			break
		}
	}
	if !found {
		t.Error("us-central1 not in GCPMajorRegions")
	}
}

// ---------------------------------------------------------------------------
// GCPMoney
// ---------------------------------------------------------------------------

func TestGCPMoneyZero(t *testing.T) {
	v := GCPMoney("0", 0)
	if v != 0.0 {
		t.Errorf("got %v want 0.0", v)
	}
}

func TestGCPMoneyIntegerUnits(t *testing.T) {
	// 1 unit, 0 nanos = 1.0
	v := GCPMoney("1", 0)
	if math.Abs(v-1.0) > 1e-12 {
		t.Errorf("got %v want 1.0", v)
	}
}

func TestGCPMoneyNanosOnly(t *testing.T) {
	// 0 units, 500000000 nanos = 0.5
	v := GCPMoney("0", 500000000)
	if math.Abs(v-0.5) > 1e-12 {
		t.Errorf("got %v want 0.5", v)
	}
}

func TestGCPMoneyCombined(t *testing.T) {
	// 2 units, 340000000 nanos = 2.34
	v := GCPMoney("2", 340000000)
	if math.Abs(v-2.34) > 1e-9 {
		t.Errorf("got %v want 2.34", v)
	}
}

// ---------------------------------------------------------------------------
// CloudSQLInstanceSpecs
// ---------------------------------------------------------------------------

func TestCloudSQLInstanceSpecsDbF1Micro(t *testing.T) {
	spec, ok := CloudSQLInstanceSpecs["db-f1-micro"]
	if !ok {
		t.Fatal("db-f1-micro not found")
	}
	if math.Abs(spec[0]-0.2) > 1e-9 {
		t.Errorf("vcpu: got %v want 0.2", spec[0])
	}
	if math.Abs(spec[1]-0.614) > 1e-9 {
		t.Errorf("mem: got %v want 0.614", spec[1])
	}
}

// ---------------------------------------------------------------------------
// GCPStorageSKU
// ---------------------------------------------------------------------------

func TestGCPStorageSKUPDStandard(t *testing.T) {
	sku, ok := GCPStorageSKU["pd-standard"]
	if !ok {
		t.Fatal("pd-standard not in GCPStorageSKU")
	}
	if sku.Desc != "Storage PD Capacity" {
		t.Errorf("desc: got %q want Storage PD Capacity", sku.Desc)
	}
}
