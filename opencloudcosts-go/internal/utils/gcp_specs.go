// Package utils provides shared utilities for cloud pricing providers.
// This file ports gcp_specs.py: GCP instance family → SKU mapping data.
package utils

// Catalog freshness: instance family coverage last reviewed 2026-07 against
// https://docs.cloud.google.com/compute/docs/general-purpose-machines (C4) and
// https://docs.cloud.google.com/compute/docs/storage-optimized-machines (Z3).
// When GCP GAs a new machine family, add it here (GCPInstanceSpecs +
// GCPFamilySKU) -- GetComputePrice fetches live prices from
// cloudbilling.googleapis.com, but only for families this catalog recognizes.

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
	// ---- A2 (GPU, A100 40GB for highgpu/megagpu; A100 80GB for ultragpu) ----
	"a2-highgpu-1g":  {12, 85.0},
	"a2-highgpu-2g":  {24, 170.0},
	"a2-highgpu-4g":  {48, 340.0},
	"a2-highgpu-8g":  {96, 680.0},
	"a2-megagpu-16g": {96, 1360.0},
	"a2-ultragpu-1g": {12, 170.0},
	"a2-ultragpu-2g": {24, 340.0},
	"a2-ultragpu-4g": {48, 680.0},
	"a2-ultragpu-8g": {96, 1360.0},
	// ---- G2 (GPU, L4) — non-linear GPU counts ----
	"g2-standard-4":  {4, 16.0},
	"g2-standard-8":  {8, 32.0},
	"g2-standard-12": {12, 48.0},
	"g2-standard-16": {16, 64.0},
	"g2-standard-24": {24, 96.0},
	"g2-standard-32": {32, 128.0},
	"g2-standard-48": {48, 192.0},
	"g2-standard-96": {96, 384.0},
	// ---- A3 (GPU, H100 80GB) ----
	"a3-highgpu-8g": {208, 1872.0},
	// ---- A3 Ultra (GPU, H200 141GB) ----
	// Source: https://cloud.google.com/compute/docs/gpus (accelerator-optimized
	// machine family table), verified 2026-07-04.
	"a3-ultragpu-8g": {224, 2952.0},
	// ---- C4 standard (3.75 GB/vCPU) ----
	// Source: https://docs.cloud.google.com/compute/docs/general-purpose-machines (C4 section)
	// cross-checked against https://instances.vantage.sh/gcp/c4-standard-8 (2026-07).
	"c4-standard-2":   {2, 7.0},
	"c4-standard-4":   {4, 15.0},
	"c4-standard-8":   {8, 30.0},
	"c4-standard-16":  {16, 60.0},
	"c4-standard-24":  {24, 90.0},
	"c4-standard-32":  {32, 120.0},
	"c4-standard-48":  {48, 180.0},
	"c4-standard-96":  {96, 360.0},
	"c4-standard-144": {144, 540.0},
	"c4-standard-192": {192, 720.0},
	"c4-standard-288": {288, 1080.0},
	// ---- C4 highcpu (2 GB/vCPU) ----
	"c4-highcpu-2":   {2, 4.0},
	"c4-highcpu-4":   {4, 8.0},
	"c4-highcpu-8":   {8, 16.0},
	"c4-highcpu-16":  {16, 32.0},
	"c4-highcpu-32":  {32, 64.0},
	"c4-highcpu-48":  {48, 96.0},
	"c4-highcpu-96":  {96, 192.0},
	"c4-highcpu-192": {192, 384.0},
	// ---- C4 highmem (7.75 GB/vCPU) ----
	"c4-highmem-2":   {2, 15.0},
	"c4-highmem-4":   {4, 31.0},
	"c4-highmem-8":   {8, 62.0},
	"c4-highmem-16":  {16, 124.0},
	"c4-highmem-32":  {32, 248.0},
	"c4-highmem-48":  {48, 372.0},
	"c4-highmem-96":  {96, 744.0},
	"c4-highmem-192": {192, 1488.0},
	// ---- Z3 storage-optimized (8 GB/vCPU, mandatory local-SSD suffix) ----
	// Z3 has no bare "z3-highmem-N" name -- every shape requires a "-standardlssd"
	// or "-highlssd"[-metal] suffix that selects the attached local-SSD size.
	// Source: https://docs.cloud.google.com/compute/docs/storage-optimized-machines
	// (Z3 standardlssd / highlssd tables), verified 2026-07.
	"z3-highmem-14-standardlssd":    {14, 112.0},
	"z3-highmem-22-standardlssd":    {22, 176.0},
	"z3-highmem-44-standardlssd":    {44, 352.0},
	"z3-highmem-88-standardlssd":    {88, 704.0},
	"z3-highmem-176-standardlssd":   {176, 1406.0},
	"z3-highmem-8-highlssd":         {8, 64.0},
	"z3-highmem-16-highlssd":        {16, 128.0},
	"z3-highmem-22-highlssd":        {22, 176.0},
	"z3-highmem-32-highlssd":        {32, 256.0},
	"z3-highmem-44-highlssd":        {44, 352.0},
	"z3-highmem-88-highlssd":        {88, 704.0},
	"z3-highmem-192-highlssd-metal": {192, 1536.0},
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
	// FlexCUDCPUDesc/FlexCUDRAMDesc are reserved but unused: Flex CUD is spend-based
	// (computed from on-demand × 0.72) and does not use catalog SKU lookups.
	FlexCUDCPUDesc string
	FlexCUDRAMDesc string
	// GPUDesc is the on-demand accelerator SKU description substring for the entire
	// family when every instance in the family has the same GPU model (a2 is an
	// exception — see GCPInstanceGPU for per-instance overrides). Empty = no bundled GPU
	// at the family level; use GCPInstanceGPU for per-type lookup instead.
	GPUDesc string
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
		FlexCUDCPUDesc: "", // unused; Flex CUD is computed from on-demand rates
		FlexCUDRAMDesc: "",
	},
	"n1": {
		CPUDesc:        "N1 Predefined Instance Core",
		RAMDesc:        "N1 Predefined Instance Ram",
		PreemptCPUDesc: "Preemptible N1 Predefined Instance Core",
		PreemptRAMDesc: "Preemptible N1 Predefined Instance Ram",
		// N1 uses Sustained Use Discounts, not resource-based CUDs.
		CUDCPUDesc:     "",
		CUDRAMDesc:     "",
		FlexCUDCPUDesc: "", // unused; Flex CUD is computed from on-demand rates
		FlexCUDRAMDesc: "",
	},
	"n2": {
		CPUDesc:        "N2 Instance Core",
		RAMDesc:        "N2 Instance Ram",
		PreemptCPUDesc: "Preemptible N2 Instance Core",
		PreemptRAMDesc: "Preemptible N2 Instance Ram",
		CUDCPUDesc:     "Commitment v1: N2 Cpu",
		CUDRAMDesc:     "Commitment v1: N2 Ram",
		FlexCUDCPUDesc: "", // unused; Flex CUD is computed from on-demand rates
		FlexCUDRAMDesc: "",
	},
	"n2d": {
		CPUDesc:        "N2D AMD Instance Core",
		RAMDesc:        "N2D AMD Instance Ram",
		PreemptCPUDesc: "Preemptible N2D AMD Instance Core",
		PreemptRAMDesc: "Preemptible N2D AMD Instance Ram",
		CUDCPUDesc:     "Commitment v1: N2D AMD Cpu",
		CUDRAMDesc:     "Commitment v1: N2D AMD Ram",
		FlexCUDCPUDesc: "", // unused; Flex CUD is computed from on-demand rates
		FlexCUDRAMDesc: "",
	},
	"c2": {
		CPUDesc:        "Compute optimized Core",
		RAMDesc:        "Compute optimized Ram",
		PreemptCPUDesc: "Preemptible Compute optimized Core",
		PreemptRAMDesc: "Preemptible Compute optimized Ram",
		CUDCPUDesc:     "Commitment: Compute optimized Core",
		CUDRAMDesc:     "Commitment: Compute optimized Ram",
		FlexCUDCPUDesc: "", // unused; Flex CUD is computed from on-demand rates
		FlexCUDRAMDesc: "",
	},
	"c2d": {
		CPUDesc:        "C2D AMD Instance Core",
		RAMDesc:        "C2D AMD Instance Ram",
		PreemptCPUDesc: "Preemptible C2D AMD Instance Core",
		PreemptRAMDesc: "Preemptible C2D AMD Instance Ram",
		CUDCPUDesc:     "Commitment v1: C2D AMD Cpu",
		CUDRAMDesc:     "Commitment v1: C2D AMD Ram",
		FlexCUDCPUDesc: "", // unused; Flex CUD is computed from on-demand rates
		FlexCUDRAMDesc: "",
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
		// A2 has two different GPU models (A100 40GB vs A100 80GB) depending on instance type.
		// Per-instance GPU info is in GCPInstanceGPU; GPUDesc is intentionally empty here.
		GPUDesc: "",
	},
	"g2": {
		CPUDesc:        "G2 Instance Core",
		RAMDesc:        "G2 Instance Ram",
		PreemptCPUDesc: "Spot Preemptible G2 Instance Core",
		PreemptRAMDesc: "Spot Preemptible G2 Instance Ram",
		CUDCPUDesc:     "Commitment v1: G2 Cpu",
		CUDRAMDesc:     "Commitment v1: G2 Ram",
		// G2 is a GPU family; it does not qualify for Flex CUD (CmtCudPremium).
		FlexCUDCPUDesc: "",
		FlexCUDRAMDesc: "",
		// G2 GPU info (L4) is in GCPInstanceGPU; GPUDesc is intentionally empty here.
		GPUDesc: "",
	},
	"a3": {
		CPUDesc:        "A3 Instance Core",
		RAMDesc:        "A3 Instance Ram",
		PreemptCPUDesc: "Spot Preemptible A3 Instance Core",
		PreemptRAMDesc: "Spot Preemptible A3 Instance Ram",
		CUDCPUDesc:     "Commitment v1: A3 Cpu",
		CUDRAMDesc:     "Commitment v1: A3 Ram",
		// A3 does not offer Flex CUD.
		FlexCUDCPUDesc: "",
		FlexCUDRAMDesc: "",
		// A3 GPU info (H100 80GB) is in GCPInstanceGPU; GPUDesc is intentionally empty here.
		GPUDesc: "",
	},
	// C4: general-purpose, Intel Granite Rapids/Emerald Rapids. On-demand and
	// Spot SKU description prefixes confirmed against
	// https://cloud.google.com/skus/sku-groups/c4-on-demand-vms and
	// https://cloud.google.com/skus/sku-groups/c4-spot-preemptible-vms (2026-07).
	// No public SKU-groups page confirming resource-based (Commitment v1) CUD
	// description text for C4 was found, so CUDCPUDesc/CUDRAMDesc are left
	// empty (cud_1yr/cud_3yr terms return an empty price list for C4, same as
	// the existing pattern for T2A) rather than guessing the SKU text. Flex CUD
	// and SUD eligibility (sudEligibleFamilies / flexCUDEligibleFamilies) are
	// separate, pre-existing gates this change intentionally does not touch --
	// out of scope for RC3-003, which only fixes on-demand/spot family
	// recognition.
	"c4": {
		CPUDesc:        "C4 Instance Core",
		RAMDesc:        "C4 Instance Ram",
		PreemptCPUDesc: "Spot Preemptible C4 Instance Core",
		PreemptRAMDesc: "Spot Preemptible C4 Instance Ram",
		CUDCPUDesc:     "",
		CUDRAMDesc:     "",
		FlexCUDCPUDesc: "", // unused; Flex CUD is computed from on-demand rates
		FlexCUDRAMDesc: "",
	},
	// Z3: storage-optimized, local-SSD attached (every instance type name carries
	// a mandatory "-standardlssd"/"-highlssd" suffix; see GCPInstanceSpecs). SKU
	// description prefixes confirmed against
	// https://cloud.google.com/skus/sku-groups/z3-on-demand-vms and
	// https://cloud.google.com/skus/sku-groups/z3-spot-preemptible-vms (2026-07).
	// No public resource-based (Commitment v1) CUD SKU-groups page was found for
	// Z3, so CUDCPUDesc/CUDRAMDesc are left empty rather than guessing.
	"z3": {
		CPUDesc:        "Z3 Instance Core",
		RAMDesc:        "Z3 Instance Ram",
		PreemptCPUDesc: "Spot Preemptible Z3 Instance Core",
		PreemptRAMDesc: "Spot Preemptible Z3 Instance Ram",
		CUDCPUDesc:     "",
		CUDRAMDesc:     "",
		FlexCUDCPUDesc: "", // unused; Flex CUD is computed from on-demand rates
		FlexCUDRAMDesc: "",
	},
}

// InstanceGPU holds the GPU accelerator specification bundled with an instance type.
type InstanceGPU struct {
	Count    int    // number of GPU accelerators in this instance
	Model    string // human-readable GPU model name (e.g. "A100 40GB", "L4", "H100 80GB")
	OnDemand string // on-demand SKU description substring (partial, case-insensitive match)
}

// GCPInstanceGPU maps instance type → GPU spec for GCP instances with bundled accelerators.
// Only covers instances whose GPU pricing can be looked up from the Compute Engine catalog.
// SKU descriptions have been verified against the GCP Cloud Billing Catalog API (June 2026).
var GCPInstanceGPU = map[string]InstanceGPU{
	// ---- A2 highgpu / megagpu — 1× to 16× NVIDIA A100 40GB ----
	"a2-highgpu-1g":  {1, "A100 40GB", "Nvidia Tesla A100 GPU running in"},
	"a2-highgpu-2g":  {2, "A100 40GB", "Nvidia Tesla A100 GPU running in"},
	"a2-highgpu-4g":  {4, "A100 40GB", "Nvidia Tesla A100 GPU running in"},
	"a2-highgpu-8g":  {8, "A100 40GB", "Nvidia Tesla A100 GPU running in"},
	"a2-megagpu-16g": {16, "A100 40GB", "Nvidia Tesla A100 GPU running in"},
	// ---- A2 ultragpu — 1× to 8× NVIDIA A100 80GB ----
	"a2-ultragpu-1g": {1, "A100 80GB", "Nvidia Tesla A100 80GB GPU running in"},
	"a2-ultragpu-2g": {2, "A100 80GB", "Nvidia Tesla A100 80GB GPU running in"},
	"a2-ultragpu-4g": {4, "A100 80GB", "Nvidia Tesla A100 80GB GPU running in"},
	"a2-ultragpu-8g": {8, "A100 80GB", "Nvidia Tesla A100 80GB GPU running in"},
	// ---- G2 — non-linear L4 GPU counts per GCP docs ----
	"g2-standard-4":  {1, "L4", "Nvidia L4 GPU running in"},
	"g2-standard-8":  {1, "L4", "Nvidia L4 GPU running in"},
	"g2-standard-12": {1, "L4", "Nvidia L4 GPU running in"},
	"g2-standard-16": {1, "L4", "Nvidia L4 GPU running in"},
	"g2-standard-24": {2, "L4", "Nvidia L4 GPU running in"},
	"g2-standard-32": {1, "L4", "Nvidia L4 GPU running in"},
	"g2-standard-48": {4, "L4", "Nvidia L4 GPU running in"},
	"g2-standard-96": {8, "L4", "Nvidia L4 GPU running in"},
	// ---- A3 highgpu — 8× NVIDIA H100 80GB ----
	"a3-highgpu-8g": {8, "H100 80GB", "Nvidia H100 80GB GPU running in"},
	// ---- A3 Ultra — 8× NVIDIA H200 141GB ----
	// NOTE: unlike the other entries in this map, this OnDemand substring is a
	// best-effort guess following the established naming pattern — it has NOT
	// been verified against a live GCP Cloud Billing Catalog query (no GCP
	// credentials were available when this was added). If the GPU surcharge
	// is silently missing from a3-ultragpu-8g on-demand pricing (falls back to
	// CPU+RAM only, logged as "gcp: GPU SKU not found"), correct this string
	// against the real catalog.
	"a3-ultragpu-8g": {8, "H200 141GB", "Nvidia H200 141GB GPU running in"},
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

// flexCUDEligibleFamilies lists GCP machine families that qualify for Flexible
// Committed Use Discounts (spend-based: 28% off for 1yr, 46% off for 3yr).
// Source: https://cloud.google.com/blog/products/compute/save-money-with-the-new-compute-engine-flexible-cuds
var flexCUDEligibleFamilies = map[string]bool{
	"n1": true, "n2": true, "n2d": true, "e2": true,
	"c2": true, "c2d": true,
}

// FlexCUDEligible reports whether the given machine family qualifies for GCP
// Flexible Committed Use Discounts. GPU/accelerator families and Arm (t2a) are
// not eligible.
func FlexCUDEligible(family string) bool {
	return flexCUDEligibleFamilies[strings.ToLower(family)]
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
	"hyperdisk-extreme": {
		Desc:    "Hyperdisk Extreme Capacity",
		AltDesc: "Hyperdisk Extreme",
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
