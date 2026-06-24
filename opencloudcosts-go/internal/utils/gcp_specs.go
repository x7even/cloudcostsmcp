// Package utils provides shared utilities for cloud pricing providers.
// This file ports gcp_specs.py: GCP instance family → SKU mapping data.
package utils

import (
	"strconv"
	"strings"
)

// InstanceSpec holds vCPU and memory for a GCP instance type.
type InstanceSpec struct {
	VCPU     int
	MemoryGB float64
}

// GCPInstanceSpecs maps instance type names to their (vcpu, memory_gb) specs.
var GCPInstanceSpecs = map[string]InstanceSpec{
	// ---- E2 standard (4 GB/vCPU) ----
	"e2-standard-2":  {2, 8.0},
	"e2-standard-4":  {4, 16.0},
	"e2-standard-8":  {8, 32.0},
	"e2-standard-16": {16, 64.0},
	"e2-standard-32": {32, 128.0},
	// ---- E2 highmem (8 GB/vCPU) ----
	"e2-highmem-2":  {2, 16.0},
	"e2-highmem-4":  {4, 32.0},
	"e2-highmem-8":  {8, 64.0},
	"e2-highmem-16": {16, 128.0},
	// ---- E2 highcpu (2 GB/vCPU) ----
	"e2-highcpu-2":  {2, 4.0},
	"e2-highcpu-4":  {4, 8.0},
	"e2-highcpu-8":  {8, 16.0},
	"e2-highcpu-16": {16, 32.0},
	"e2-highcpu-32": {32, 64.0},
	// ---- E2 micro/small/medium ----
	"e2-micro":  {2, 1.0},
	"e2-small":  {2, 2.0},
	"e2-medium": {2, 4.0},
	// ---- N1 standard (3.75 GB/vCPU) ----
	"n1-standard-1":  {1, 3.75},
	"n1-standard-2":  {2, 7.5},
	"n1-standard-4":  {4, 15.0},
	"n1-standard-8":  {8, 30.0},
	"n1-standard-16": {16, 60.0},
	"n1-standard-32": {32, 120.0},
	"n1-standard-64": {64, 240.0},
	"n1-standard-96": {96, 360.0},
	// ---- N1 highmem (6.5 GB/vCPU) ----
	"n1-highmem-2":  {2, 13.0},
	"n1-highmem-4":  {4, 26.0},
	"n1-highmem-8":  {8, 52.0},
	"n1-highmem-16": {16, 104.0},
	"n1-highmem-32": {32, 208.0},
	"n1-highmem-64": {64, 416.0},
	"n1-highmem-96": {96, 624.0},
	// ---- N1 highcpu (0.9 GB/vCPU) ----
	"n1-highcpu-2":  {2, 1.8},
	"n1-highcpu-4":  {4, 3.6},
	"n1-highcpu-8":  {8, 7.2},
	"n1-highcpu-16": {16, 14.4},
	"n1-highcpu-32": {32, 28.8},
	"n1-highcpu-64": {64, 57.6},
	"n1-highcpu-96": {96, 86.4},
	// ---- N2 standard (4 GB/vCPU) ----
	"n2-standard-2":   {2, 8.0},
	"n2-standard-4":   {4, 16.0},
	"n2-standard-8":   {8, 32.0},
	"n2-standard-16":  {16, 64.0},
	"n2-standard-32":  {32, 128.0},
	"n2-standard-48":  {48, 192.0},
	"n2-standard-64":  {64, 256.0},
	"n2-standard-80":  {80, 320.0},
	"n2-standard-96":  {96, 384.0},
	"n2-standard-128": {128, 512.0},
	// ---- N2 highmem (8 GB/vCPU) ----
	"n2-highmem-2":   {2, 16.0},
	"n2-highmem-4":   {4, 32.0},
	"n2-highmem-8":   {8, 64.0},
	"n2-highmem-16":  {16, 128.0},
	"n2-highmem-32":  {32, 256.0},
	"n2-highmem-48":  {48, 384.0},
	"n2-highmem-64":  {64, 512.0},
	"n2-highmem-80":  {80, 640.0},
	"n2-highmem-96":  {96, 768.0},
	"n2-highmem-128": {128, 864.0},
	// ---- N2 highcpu (2 GB/vCPU) ----
	"n2-highcpu-2":  {2, 4.0},
	"n2-highcpu-4":  {4, 8.0},
	"n2-highcpu-8":  {8, 16.0},
	"n2-highcpu-16": {16, 32.0},
	"n2-highcpu-32": {32, 64.0},
	"n2-highcpu-48": {48, 96.0},
	"n2-highcpu-64": {64, 128.0},
	"n2-highcpu-80": {80, 160.0},
	"n2-highcpu-96": {96, 192.0},
	// ---- N2D (AMD EPYC, same memory ratios as N2) ----
	"n2d-standard-2":   {2, 8.0},
	"n2d-standard-4":   {4, 16.0},
	"n2d-standard-8":   {8, 32.0},
	"n2d-standard-16":  {16, 64.0},
	"n2d-standard-32":  {32, 128.0},
	"n2d-standard-48":  {48, 192.0},
	"n2d-standard-64":  {64, 256.0},
	"n2d-standard-96":  {96, 384.0},
	"n2d-standard-128": {128, 512.0},
	"n2d-standard-224": {224, 896.0},
	"n2d-highmem-2":    {2, 16.0},
	"n2d-highmem-4":    {4, 32.0},
	"n2d-highmem-8":    {8, 64.0},
	"n2d-highmem-16":   {16, 128.0},
	"n2d-highmem-32":   {32, 256.0},
	"n2d-highmem-48":   {48, 384.0},
	"n2d-highmem-64":   {64, 512.0},
	"n2d-highmem-96":   {96, 768.0},
	"n2d-highcpu-2":    {2, 4.0},
	"n2d-highcpu-4":    {4, 8.0},
	"n2d-highcpu-8":    {8, 16.0},
	"n2d-highcpu-16":   {16, 32.0},
	"n2d-highcpu-32":   {32, 64.0},
	"n2d-highcpu-48":   {48, 96.0},
	"n2d-highcpu-64":   {64, 128.0},
	"n2d-highcpu-96":   {96, 192.0},
	"n2d-highcpu-128":  {128, 256.0},
	"n2d-highcpu-224":  {224, 448.0},
	// ---- C2 (compute-optimised, 4 GB/vCPU) ----
	"c2-standard-4":  {4, 16.0},
	"c2-standard-8":  {8, 32.0},
	"c2-standard-16": {16, 64.0},
	"c2-standard-30": {30, 120.0},
	"c2-standard-60": {60, 240.0},
	// ---- C3 standard (4 GB/vCPU) ----
	"c3-standard-4":   {4, 16.0},
	"c3-standard-8":   {8, 32.0},
	"c3-standard-22":  {22, 88.0},
	"c3-standard-44":  {44, 176.0},
	"c3-standard-88":  {88, 352.0},
	"c3-standard-176": {176, 704.0},
	// ---- C3 highcpu (2 GB/vCPU) ----
	"c3-highcpu-4":   {4, 8.0},
	"c3-highcpu-8":   {8, 16.0},
	"c3-highcpu-22":  {22, 44.0},
	"c3-highcpu-44":  {44, 88.0},
	"c3-highcpu-88":  {88, 176.0},
	"c3-highcpu-176": {176, 352.0},
	// ---- C3 highmem (8 GB/vCPU) ----
	"c3-highmem-4":  {4, 32.0},
	"c3-highmem-8":  {8, 64.0},
	"c3-highmem-22": {22, 176.0},
	"c3-highmem-44": {44, 352.0},
	"c3-highmem-88": {88, 704.0},
	// ---- C2D (AMD EPYC, compute-optimised) ----
	"c2d-standard-2":   {2, 8.0},
	"c2d-standard-4":   {4, 16.0},
	"c2d-standard-8":   {8, 32.0},
	"c2d-standard-16":  {16, 64.0},
	"c2d-standard-32":  {32, 128.0},
	"c2d-standard-56":  {56, 224.0},
	"c2d-standard-112": {112, 448.0},
	"c2d-highcpu-2":    {2, 4.0},
	"c2d-highcpu-4":    {4, 8.0},
	"c2d-highcpu-8":    {8, 16.0},
	"c2d-highcpu-16":   {16, 32.0},
	"c2d-highcpu-32":   {32, 64.0},
	"c2d-highcpu-56":   {56, 112.0},
	"c2d-highcpu-112":  {112, 224.0},
	"c2d-highmem-2":    {2, 16.0},
	"c2d-highmem-4":    {4, 32.0},
	"c2d-highmem-8":    {8, 64.0},
	"c2d-highmem-16":   {16, 128.0},
	"c2d-highmem-32":   {32, 256.0},
	"c2d-highmem-56":   {56, 448.0},
	"c2d-highmem-112":  {112, 896.0},
	// ---- T2D (scale-out AMD, 4 GB/vCPU) ----
	"t2d-standard-1":  {1, 4.0},
	"t2d-standard-2":  {2, 8.0},
	"t2d-standard-4":  {4, 16.0},
	"t2d-standard-8":  {8, 32.0},
	"t2d-standard-16": {16, 64.0},
	"t2d-standard-32": {32, 128.0},
	"t2d-standard-48": {48, 192.0},
	"t2d-standard-60": {60, 240.0},
	// ---- T2A (Arm, 4 GB/vCPU) ----
	"t2a-standard-1":  {1, 4.0},
	"t2a-standard-2":  {2, 8.0},
	"t2a-standard-4":  {4, 16.0},
	"t2a-standard-8":  {8, 32.0},
	"t2a-standard-16": {16, 64.0},
	"t2a-standard-32": {32, 128.0},
	"t2a-standard-48": {48, 192.0},
	// ---- M1 memory-optimised ----
	"m1-megamem-96":   {96, 1433.6},
	"m1-ultramem-40":  {40, 961.0},
	"m1-ultramem-80":  {80, 1922.0},
	"m1-ultramem-160": {160, 3844.0},
	// ---- M2 memory-optimised ----
	"m2-ultramem-208": {208, 5888.0},
	"m2-ultramem-416": {416, 11776.0},
	"m2-megamem-416":  {416, 5888.0},
	"m2-hypermem-416": {416, 8832.0},
	// ---- A2 (GPU, A100) ----
	"a2-highgpu-1g":  {12, 85.0},
	"a2-highgpu-2g":  {24, 170.0},
	"a2-highgpu-4g":  {48, 340.0},
	"a2-highgpu-8g":  {96, 680.0},
	"a2-megagpu-16g": {96, 1360.0},
	"a2-ultragpu-1g": {12, 170.0},
	"a2-ultragpu-2g": {24, 340.0},
	"a2-ultragpu-4g": {48, 680.0},
	"a2-ultragpu-8g": {96, 1360.0},
}

// seriesRamRatio is GB/vCPU by series name.
var seriesRamRatio = map[string]float64{
	"standard": 4.0,
	"highmem":  8.0,
	"highcpu":  2.0,
	"ultramem": 24.0,
	"megamem":  14.9,
}

// familyStandardRam is the GB/vCPU override for "standard" series by family.
var familyStandardRam = map[string]float64{
	"n1":  3.75,
	"n2":  4.0,
	"n2d": 4.0,
	"e2":  4.0,
	"c2":  4.0,
	"c2d": 4.0,
	"c3":  4.0,
	"t2d": 4.0,
	"t2a": 4.0,
}

// ParseInstanceType returns (vcpu, memoryGB, ok) for a GCP instance type.
// It tries an exact table lookup first, then falls back to naming-convention parsing.
func ParseInstanceType(instanceType string) (int, float64, bool) {
	if spec, ok := GCPInstanceSpecs[instanceType]; ok {
		return spec.VCPU, spec.MemoryGB, true
	}

	// Parse by naming convention: family-series-vcpus
	parts := splitN(instanceType, "-", 3)
	if len(parts) < 3 {
		return 0, 0, false
	}

	family := parts[0]
	series := parts[1]
	vcpuStr := parts[2]

	vcpus := parseInt(vcpuStr)
	if vcpus <= 0 {
		return 0, 0, false
	}

	var ramRatio float64
	if series == "standard" {
		if r, ok := familyStandardRam[family]; ok {
			ramRatio = r
		} else {
			ramRatio = 4.0
		}
	} else {
		if r, ok := seriesRamRatio[series]; ok {
			ramRatio = r
		} else {
			ramRatio = 4.0
		}
	}

	memGB := roundTo2(float64(vcpus) * ramRatio)
	return vcpus, memGB, true
}

// GetMachineFamily extracts the machine family from an instance type.
// e.g. "n2-standard-4" -> "n2", "n2d-standard-8" -> "n2d"
func GetMachineFamily(instanceType string) string {
	parts := splitN(instanceType, "-", 2)
	if len(parts) == 0 {
		return instanceType
	}
	return parts[0]
}

// FamilySKU holds the SKU description substrings for a machine family.
type FamilySKU struct {
	CPUDesc        string
	RAMDesc        string
	PreemptCPUDesc string
	PreemptRAMDesc string
	CUDCPUDesc     string
	CUDRAMDesc     string
	FlexCUDCPUDesc string // CmtCudPremium usageType description (empty = not eligible for Flex CUD)
	FlexCUDRAMDesc string
}

// GCPFamilySKU maps machine family to SKU description patterns.
var GCPFamilySKU = map[string]FamilySKU{
	"e2": {
		CPUDesc:        "E2 Instance Core",
		RAMDesc:        "E2 Instance Ram",
		PreemptCPUDesc: "Preemptible E2 Instance Core",
		PreemptRAMDesc: "Preemptible E2 Instance Ram",
		CUDCPUDesc:     "Commitment v1: E2 Cpu",
		CUDRAMDesc:     "Commitment v1: E2 Ram",
		FlexCUDCPUDesc: "Commitment v1: E2 Cpu",
		FlexCUDRAMDesc: "Commitment v1: E2 Ram",
	},
	"n1": {
		CPUDesc:        "N1 Predefined Instance Core",
		RAMDesc:        "N1 Predefined Instance Ram",
		PreemptCPUDesc: "Preemptible N1 Predefined Instance Core",
		PreemptRAMDesc: "Preemptible N1 Predefined Instance Ram",
		// N1 uses Sustained Use Discounts, not CUDs. No Flex CUD either.
		CUDCPUDesc:     "",
		CUDRAMDesc:     "",
		FlexCUDCPUDesc: "",
		FlexCUDRAMDesc: "",
	},
	"n2": {
		CPUDesc:        "N2 Instance Core",
		RAMDesc:        "N2 Instance Ram",
		PreemptCPUDesc: "Preemptible N2 Instance Core",
		PreemptRAMDesc: "Preemptible N2 Instance Ram",
		CUDCPUDesc:     "Commitment v1: N2 Cpu",
		CUDRAMDesc:     "Commitment v1: N2 Ram",
		FlexCUDCPUDesc: "Commitment v1: N2 Cpu",
		FlexCUDRAMDesc: "Commitment v1: N2 Ram",
	},
	"n2d": {
		CPUDesc:        "N2D AMD Instance Core",
		RAMDesc:        "N2D AMD Instance Ram",
		PreemptCPUDesc: "Preemptible N2D AMD Instance Core",
		PreemptRAMDesc: "Preemptible N2D AMD Instance Ram",
		CUDCPUDesc:     "Commitment v1: N2D AMD Cpu",
		CUDRAMDesc:     "Commitment v1: N2D AMD Ram",
		FlexCUDCPUDesc: "Commitment v1: N2D AMD Cpu",
		FlexCUDRAMDesc: "Commitment v1: N2D AMD Ram",
	},
	"c2": {
		CPUDesc:        "Compute optimized Core",
		RAMDesc:        "Compute optimized Ram",
		PreemptCPUDesc: "Preemptible Compute optimized Core",
		PreemptRAMDesc: "Preemptible Compute optimized Ram",
		CUDCPUDesc:     "Commitment: Compute optimized Core",
		CUDRAMDesc:     "Commitment: Compute optimized Ram",
		FlexCUDCPUDesc: "Commitment: Compute optimized Core",
		FlexCUDRAMDesc: "Commitment: Compute optimized Ram",
	},
	"c2d": {
		CPUDesc:        "C2D AMD Instance Core",
		RAMDesc:        "C2D AMD Instance Ram",
		PreemptCPUDesc: "Preemptible C2D AMD Instance Core",
		PreemptRAMDesc: "Preemptible C2D AMD Instance Ram",
		CUDCPUDesc:     "Commitment v1: C2D AMD Cpu",
		CUDRAMDesc:     "Commitment v1: C2D AMD Ram",
		FlexCUDCPUDesc: "Commitment v1: C2D AMD Cpu",
		FlexCUDRAMDesc: "Commitment v1: C2D AMD Ram",
	},
	"c3": {
		CPUDesc:        "C3 Instance Core",
		RAMDesc:        "C3 Instance Ram",
		PreemptCPUDesc: "Spot Preemptible C3 Instance Core",
		PreemptRAMDesc: "Spot Preemptible C3 Instance Ram",
		CUDCPUDesc:     "Commitment v1: C3 Cpu",
		CUDRAMDesc:     "Commitment v1: C3 Ram",
		// C3 does not offer Flex CUD.
		FlexCUDCPUDesc: "",
		FlexCUDRAMDesc: "",
	},
	"t2d": {
		CPUDesc:        "T2D AMD Instance Core",
		RAMDesc:        "T2D AMD Instance Ram",
		PreemptCPUDesc: "Preemptible T2D AMD Instance Core",
		PreemptRAMDesc: "Preemptible T2D AMD Instance Ram",
		CUDCPUDesc:     "Commitment v1: T2D AMD Cpu",
		CUDRAMDesc:     "Commitment v1: T2D AMD Ram",
		// T2D does not offer Flex CUD.
		FlexCUDCPUDesc: "",
		FlexCUDRAMDesc: "",
	},
	"t2a": {
		CPUDesc:        "T2A Arm Instance Core",
		RAMDesc:        "T2A Arm Instance Ram",
		PreemptCPUDesc: "Preemptible T2A Arm Instance Core",
		PreemptRAMDesc: "Preemptible T2A Arm Instance Ram",
		// T2A does not offer CUDs or Flex CUD.
		CUDCPUDesc:     "",
		CUDRAMDesc:     "",
		FlexCUDCPUDesc: "",
		FlexCUDRAMDesc: "",
	},
	"m1": {
		CPUDesc:        "Memory-optimized Instance Core",
		RAMDesc:        "Memory-optimized Instance Ram",
		PreemptCPUDesc: "Preemptible Memory-optimized Instance Core",
		PreemptRAMDesc: "Preemptible Memory-optimized Instance Ram",
		CUDCPUDesc:     "Commitment v1: Memory-optimized Cpu",
		CUDRAMDesc:     "Commitment v1: Memory-optimized Ram",
		// M1 does not offer Flex CUD.
		FlexCUDCPUDesc: "",
		FlexCUDRAMDesc: "",
	},
	"a2": {
		CPUDesc:        "A2 Instance Core",
		RAMDesc:        "A2 Instance Ram",
		PreemptCPUDesc: "Preemptible A2 Instance Core",
		PreemptRAMDesc: "Preemptible A2 Instance Ram",
		CUDCPUDesc:     "Commitment v1: A2 Cpu",
		CUDRAMDesc:     "Commitment v1: A2 Ram",
		// A2 (GPU family) does not offer Flex CUD.
		FlexCUDCPUDesc: "",
		FlexCUDRAMDesc: "",
	},
}

// sudEligibleFamilies lists GCP machine families that qualify for Sustained Use
// Discounts. GPU/accelerator families (a2, a3, g2) are not eligible.
var sudEligibleFamilies = map[string]bool{
	"n1": true, "n2": true, "n2d": true, "e2": true,
	"c2": true, "c2d": true, "c3": true, "t2d": true, "t2a": true,
	"m1": true, "m2": true, "m3": true,
}

// SUDEligible reports whether the given machine family qualifies for GCP
// Sustained Use Discounts. GPU families (a2, a3, g2) and preemptible VMs
// are not eligible.
func SUDEligible(family string) bool {
	return sudEligibleFamilies[strings.ToLower(family)]
}

// CloudSQLInstanceSpecs maps Cloud SQL instance type names to (vcpu, memoryGB).
var CloudSQLInstanceSpecs = map[string][2]float64{
	// Shared core (special pricing)
	"db-f1-micro": {0.2, 0.614},
	"db-g1-small": {0.5, 1.700},
	// Standard (n1-style)
	"db-n1-standard-1":  {1, 3.75},
	"db-n1-standard-2":  {2, 7.5},
	"db-n1-standard-4":  {4, 15.0},
	"db-n1-standard-8":  {8, 30.0},
	"db-n1-standard-16": {16, 60.0},
	"db-n1-standard-32": {32, 120.0},
	"db-n1-standard-64": {64, 240.0},
	// High memory (n1-style)
	"db-n1-highmem-2":  {2, 13.0},
	"db-n1-highmem-4":  {4, 26.0},
	"db-n1-highmem-8":  {8, 52.0},
	"db-n1-highmem-16": {16, 104.0},
	"db-n1-highmem-32": {32, 208.0},
	"db-n1-highmem-64": {64, 416.0},
	// Custom / newer standard tiers
	"db-standard-1":  {1, 3.75},
	"db-standard-2":  {2, 7.5},
	"db-standard-4":  {4, 15.0},
	"db-standard-8":  {8, 30.0},
	"db-standard-16": {16, 60.0},
	"db-standard-32": {32, 120.0},
	// High memory custom tiers
	"db-highmem-2":  {2, 13.0},
	"db-highmem-4":  {4, 26.0},
	"db-highmem-8":  {8, 52.0},
	"db-highmem-16": {16, 104.0},
}

// StorageSKU holds description patterns for persistent disk storage types.
type StorageSKU struct {
	Desc    string
	AltDesc string
}

// GCPStorageSKU maps persistent disk type to SKU description patterns.
var GCPStorageSKU = map[string]StorageSKU{
	"pd-standard": {
		Desc:    "Storage PD Capacity",
		AltDesc: "Regional Storage PD Capacity",
	},
	"pd-ssd": {
		Desc:    "SSD backed PD Capacity",
		AltDesc: "Regional SSD backed PD Capacity",
	},
	"pd-balanced": {
		Desc:    "Balanced PD Capacity",
		AltDesc: "Regional Balanced PD Capacity",
	},
	"pd-extreme": {
		Desc:    "Extreme PD Capacity",
		AltDesc: "Extreme PD Capacity",
	},
}

// WindowsSKU returns the (cpuDescFragment, ramDescFragment) for Windows SKU
// lookup for the given machine family, or ("", "") if unsupported.
func WindowsSKU(family string) (string, string) {
	switch family {
	case "n1":
		return "N1 Predefined Instance Core running Windows",
			"N1 Predefined Instance Ram running Windows"
	case "n2":
		return "N2 Instance Core running Windows",
			"N2 Instance Ram running Windows"
	case "n2d":
		return "N2D AMD Instance Core running Windows",
			"N2D AMD Instance Ram running Windows"
	case "c2":
		return "Compute optimized Core running Windows",
			"Compute optimized Ram running Windows"
	}
	// E2, C2D, T2D, T2A, M1, A2: no Windows support
	return "", ""
}

// GCPMajorRegions is the curated list of 12 major GCP regions.
var GCPMajorRegions = []string{
	"us-central1",
	"us-east1",
	"us-west1",
	"us-west2",
	"europe-west1",
	"europe-west2",
	"europe-west3",
	"europe-west4",
	"asia-east1",
	"asia-northeast1",
	"asia-southeast1",
	"australia-southeast1",
}

// GCPRegions is the full list of GCP regions.
var GCPRegions = []string{
	// Americas
	"us-east1", "us-east4", "us-east5", "us-central1",
	"us-west1", "us-west2", "us-west3", "us-west4", "us-south1",
	"northamerica-northeast1", "northamerica-northeast2", "northamerica-south1",
	"southamerica-east1", "southamerica-west1",
	// Europe
	"europe-west1", "europe-west2", "europe-west3", "europe-west4",
	"europe-west6", "europe-west8", "europe-west9", "europe-west10", "europe-west12",
	"europe-north1", "europe-central2", "europe-southwest1",
	// Asia Pacific
	"asia-east1", "asia-east2",
	"asia-northeast1", "asia-northeast2", "asia-northeast3",
	"asia-south1", "asia-south2",
	"asia-southeast1", "asia-southeast2",
	"australia-southeast1", "australia-southeast2",
	// Middle East & Africa
	"me-west1", "me-central1", "me-central2",
	"africa-south1",
}

// GCPMoney converts a GCP Money proto (units string, nanos int) to float64.
// units is a decimal integer string (int64), nanos is the sub-unit fractional part.
func GCPMoney(units string, nanos int) float64 {
	u, err := strconv.ParseInt(units, 10, 64)
	if err != nil {
		u = 0
	}
	return float64(u) + float64(nanos)/1e9
}

// --------------------------------------------------------------------------
// Private helpers
// --------------------------------------------------------------------------

// splitN splits s by sep up to n parts (returns all parts).
func splitN(s, sep string, n int) []string {
	result := make([]string, 0, n)
	for i := 0; i < n-1; i++ {
		idx := indexOf(s, sep)
		if idx < 0 {
			break
		}
		result = append(result, s[:idx])
		s = s[idx+len(sep):]
	}
	result = append(result, s)
	return result
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func parseInt(s string) int {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return -1
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

func roundTo2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
