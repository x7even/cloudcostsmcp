"""
GCP compute instance type specifications and machine family metadata.

GCP pricing works differently from AWS: there is no single SKU per instance type.
Instead, the Catalog API has separate per-vCPU and per-GB-RAM SKUs, identified by
machine family. To price an instance we look up:
    total_price = vcpus * cpu_price_per_hour + memory_gb * ram_price_per_hour

This module provides:
- GCP_INSTANCE_SPECS: exact (vcpu, memory_gb) for all common instance types
- parse_instance_type(): derive specs from naming convention for unlisted types
- GCP_FAMILY_SKU: machine family -> Catalog API SKU description substrings
"""
from __future__ import annotations

# ---------------------------------------------------------------------------
# Exact instance type specs: instance_type -> (vcpus, memory_gb)
# ---------------------------------------------------------------------------
GCP_INSTANCE_SPECS: dict[str, tuple[int, float]] = {
    # ---- E2 standard (4 GB/vCPU) ----
    "e2-standard-2": (2, 8.0),
    "e2-standard-4": (4, 16.0),
    "e2-standard-8": (8, 32.0),
    "e2-standard-16": (16, 64.0),
    "e2-standard-32": (32, 128.0),
    # ---- E2 highmem (8 GB/vCPU) ----
    "e2-highmem-2": (2, 16.0),
    "e2-highmem-4": (4, 32.0),
    "e2-highmem-8": (8, 64.0),
    "e2-highmem-16": (16, 128.0),
    # ---- E2 highcpu (2 GB/vCPU) ----
    "e2-highcpu-2": (2, 4.0),
    "e2-highcpu-4": (4, 8.0),
    "e2-highcpu-8": (8, 16.0),
    "e2-highcpu-16": (16, 32.0),
    "e2-highcpu-32": (32, 64.0),
    # ---- E2 micro/small/medium ----
    "e2-micro": (2, 1.0),
    "e2-small": (2, 2.0),
    "e2-medium": (2, 4.0),
    # ---- N1 standard (3.75 GB/vCPU) ----
    "n1-standard-1": (1, 3.75),
    "n1-standard-2": (2, 7.5),
    "n1-standard-4": (4, 15.0),
    "n1-standard-8": (8, 30.0),
    "n1-standard-16": (16, 60.0),
    "n1-standard-32": (32, 120.0),
    "n1-standard-64": (64, 240.0),
    "n1-standard-96": (96, 360.0),
    # ---- N1 highmem (6.5 GB/vCPU) ----
    "n1-highmem-2": (2, 13.0),
    "n1-highmem-4": (4, 26.0),
    "n1-highmem-8": (8, 52.0),
    "n1-highmem-16": (16, 104.0),
    "n1-highmem-32": (32, 208.0),
    "n1-highmem-64": (64, 416.0),
    "n1-highmem-96": (96, 624.0),
    # ---- N1 highcpu (0.9 GB/vCPU) ----
    "n1-highcpu-2": (2, 1.8),
    "n1-highcpu-4": (4, 3.6),
    "n1-highcpu-8": (8, 7.2),
    "n1-highcpu-16": (16, 14.4),
    "n1-highcpu-32": (32, 28.8),
    "n1-highcpu-64": (64, 57.6),
    "n1-highcpu-96": (96, 86.4),
    # ---- N2 standard (4 GB/vCPU) ----
    "n2-standard-2": (2, 8.0),
    "n2-standard-4": (4, 16.0),
    "n2-standard-8": (8, 32.0),
    "n2-standard-16": (16, 64.0),
    "n2-standard-32": (32, 128.0),
    "n2-standard-48": (48, 192.0),
    "n2-standard-64": (64, 256.0),
    "n2-standard-80": (80, 320.0),
    "n2-standard-96": (96, 384.0),
    "n2-standard-128": (128, 512.0),
    # ---- N2 highmem (8 GB/vCPU) ----
    "n2-highmem-2": (2, 16.0),
    "n2-highmem-4": (4, 32.0),
    "n2-highmem-8": (8, 64.0),
    "n2-highmem-16": (16, 128.0),
    "n2-highmem-32": (32, 256.0),
    "n2-highmem-48": (48, 384.0),
    "n2-highmem-64": (64, 512.0),
    "n2-highmem-80": (80, 640.0),
    "n2-highmem-96": (96, 768.0),
    "n2-highmem-128": (128, 864.0),
    # ---- N2 highcpu (2 GB/vCPU) ----
    "n2-highcpu-2": (2, 4.0),
    "n2-highcpu-4": (4, 8.0),
    "n2-highcpu-8": (8, 16.0),
    "n2-highcpu-16": (16, 32.0),
    "n2-highcpu-32": (32, 64.0),
    "n2-highcpu-48": (48, 96.0),
    "n2-highcpu-64": (64, 128.0),
    "n2-highcpu-80": (80, 160.0),
    "n2-highcpu-96": (96, 192.0),
    # ---- N2D (AMD EPYC, same memory ratios as N2) ----
    "n2d-standard-2": (2, 8.0),
    "n2d-standard-4": (4, 16.0),
    "n2d-standard-8": (8, 32.0),
    "n2d-standard-16": (16, 64.0),
    "n2d-standard-32": (32, 128.0),
    "n2d-standard-48": (48, 192.0),
    "n2d-standard-64": (64, 256.0),
    "n2d-standard-96": (96, 384.0),
    "n2d-standard-128": (128, 512.0),
    "n2d-standard-224": (224, 896.0),
    "n2d-highmem-2": (2, 16.0),
    "n2d-highmem-4": (4, 32.0),
    "n2d-highmem-8": (8, 64.0),
    "n2d-highmem-16": (16, 128.0),
    "n2d-highmem-32": (32, 256.0),
    "n2d-highmem-48": (48, 384.0),
    "n2d-highmem-64": (64, 512.0),
    "n2d-highmem-96": (96, 768.0),
    "n2d-highcpu-2": (2, 4.0),
    "n2d-highcpu-4": (4, 8.0),
    "n2d-highcpu-8": (8, 16.0),
    "n2d-highcpu-16": (16, 32.0),
    "n2d-highcpu-32": (32, 64.0),
    "n2d-highcpu-48": (48, 96.0),
    "n2d-highcpu-64": (64, 128.0),
    "n2d-highcpu-96": (96, 192.0),
    "n2d-highcpu-128": (128, 256.0),
    "n2d-highcpu-224": (224, 448.0),
    # ---- C2 (compute-optimised, 4 GB/vCPU) ----
    "c2-standard-4": (4, 16.0),
    "c2-standard-8": (8, 32.0),
    "c2-standard-16": (16, 64.0),
    "c2-standard-30": (30, 120.0),
    "c2-standard-60": (60, 240.0),
    # ---- C3 standard (4 GB/vCPU) ----
    "c3-standard-4": (4, 16.0),
    "c3-standard-8": (8, 32.0),
    "c3-standard-22": (22, 88.0),
    "c3-standard-44": (44, 176.0),
    "c3-standard-88": (88, 352.0),
    "c3-standard-176": (176, 704.0),
    # ---- C3 highcpu (2 GB/vCPU) ----
    "c3-highcpu-4": (4, 8.0),
    "c3-highcpu-8": (8, 16.0),
    "c3-highcpu-22": (22, 44.0),
    "c3-highcpu-44": (44, 88.0),
    "c3-highcpu-88": (88, 176.0),
    "c3-highcpu-176": (176, 352.0),
    # ---- C3 highmem (8 GB/vCPU) ----
    "c3-highmem-4": (4, 32.0),
    "c3-highmem-8": (8, 64.0),
    "c3-highmem-22": (22, 176.0),
    "c3-highmem-44": (44, 352.0),
    "c3-highmem-88": (88, 704.0),
    # ---- C2D (AMD EPYC, compute-optimised) ----
    "c2d-standard-2": (2, 8.0),
    "c2d-standard-4": (4, 16.0),
    "c2d-standard-8": (8, 32.0),
    "c2d-standard-16": (16, 64.0),
    "c2d-standard-32": (32, 128.0),
    "c2d-standard-56": (56, 224.0),
    "c2d-standard-112": (112, 448.0),
    "c2d-highcpu-2": (2, 4.0),
    "c2d-highcpu-4": (4, 8.0),
    "c2d-highcpu-8": (8, 16.0),
    "c2d-highcpu-16": (16, 32.0),
    "c2d-highcpu-32": (32, 64.0),
    "c2d-highcpu-56": (56, 112.0),
    "c2d-highcpu-112": (112, 224.0),
    "c2d-highmem-2": (2, 16.0),
    "c2d-highmem-4": (4, 32.0),
    "c2d-highmem-8": (8, 64.0),
    "c2d-highmem-16": (16, 128.0),
    "c2d-highmem-32": (32, 256.0),
    "c2d-highmem-56": (56, 448.0),
    "c2d-highmem-112": (112, 896.0),
    # ---- T2D (scale-out AMD, 4 GB/vCPU) ----
    "t2d-standard-1": (1, 4.0),
    "t2d-standard-2": (2, 8.0),
    "t2d-standard-4": (4, 16.0),
    "t2d-standard-8": (8, 32.0),
    "t2d-standard-16": (16, 64.0),
    "t2d-standard-32": (32, 128.0),
    "t2d-standard-48": (48, 192.0),
    "t2d-standard-60": (60, 240.0),
    # ---- T2A (Arm, 4 GB/vCPU) ----
    "t2a-standard-1": (1, 4.0),
    "t2a-standard-2": (2, 8.0),
    "t2a-standard-4": (4, 16.0),
    "t2a-standard-8": (8, 32.0),
    "t2a-standard-16": (16, 64.0),
    "t2a-standard-32": (32, 128.0),
    "t2a-standard-48": (48, 192.0),
    # ---- M1 memory-optimised ----
    "m1-megamem-96": (96, 1433.6),
    "m1-ultramem-40": (40, 961.0),
    "m1-ultramem-80": (80, 1922.0),
    "m1-ultramem-160": (160, 3844.0),
    # ---- M2 memory-optimised ----
    "m2-ultramem-208": (208, 5888.0),
    "m2-ultramem-416": (416, 11776.0),
    "m2-megamem-416": (416, 5888.0),
    "m2-hypermem-416": (416, 8832.0),
    # ---- A2 (GPU, A100) ----
    "a2-highgpu-1g": (12, 85.0),
    "a2-highgpu-2g": (24, 170.0),
    "a2-highgpu-4g": (48, 340.0),
    "a2-highgpu-8g": (96, 680.0),
    "a2-megagpu-16g": (96, 1360.0),
    "a2-ultragpu-1g": (12, 170.0),
    "a2-ultragpu-2g": (24, 340.0),
    "a2-ultragpu-4g": (48, 680.0),
    "a2-ultragpu-8g": (96, 1360.0),
}

# ---------------------------------------------------------------------------
# Memory ratio per series (GB/vCPU) for pattern-based fallback
# ---------------------------------------------------------------------------
_SERIES_RAM_RATIO: dict[str, float] = {
    "standard": 4.0,   # N2, E2, C2, T2D standard
    "highmem": 8.0,    # N2, E2 highmem
    "highcpu": 2.0,    # N2, E2 highcpu
    "ultramem": 24.0,  # M1
    "megamem": 14.9,   # M1 megamem (96 vCPUs, 1433.6 GB ≈ 14.93 GB/vCPU)
}

# Override memory ratios that differ from 4.0 for 'standard' by family
_FAMILY_STANDARD_RAM: dict[str, float] = {
    "n1": 3.75,
    "n2": 4.0,
    "n2d": 4.0,
    "e2": 4.0,
    "c2": 4.0,
    "c2d": 4.0,
    "c3": 4.0,
    "t2d": 4.0,
    "t2a": 4.0,
}


def parse_instance_type(instance_type: str) -> tuple[int, float] | None:
    """
    Return (vcpus, memory_gb) for a GCP instance type.

    Tries exact table lookup first, then falls back to naming-convention parsing.
    Returns None if the type is unknown or can't be parsed.
    """
    if instance_type in GCP_INSTANCE_SPECS:
        return GCP_INSTANCE_SPECS[instance_type]

    parts = instance_type.split("-")
    # Minimum: family-series-vcpus  e.g. n2-standard-4
    if len(parts) < 3:
        return None

    # Handle compound families like n2d, c2d, t2a
    if parts[0] in ("n2d", "c2d", "t2a"):
        family = parts[0]
        series = parts[1]
        vcpu_str = parts[2]
    else:
        family = parts[0]
        series = parts[1]
        vcpu_str = parts[2]

    try:
        vcpus = int(vcpu_str)
    except ValueError:
        return None

    # Determine GB/vCPU
    if series == "standard":
        ram_ratio = _FAMILY_STANDARD_RAM.get(family, 4.0)
    else:
        ram_ratio = _SERIES_RAM_RATIO.get(series, 4.0)

    return vcpus, round(vcpus * ram_ratio, 2)


def get_machine_family(instance_type: str) -> str:
    """
    Extract the machine family from an instance type.
    e.g. "n2-standard-4" -> "n2", "n2d-standard-8" -> "n2d"
    """
    parts = instance_type.split("-")
    # Compound families (n2d, c2d, t2a, etc.) — first segment
    return parts[0]


# ---------------------------------------------------------------------------
# SKU description patterns used to identify CPU and RAM SKUs in the Catalog API
# These substrings appear in the `description` field of GCP Billing SKUs.
# ---------------------------------------------------------------------------
GCP_FAMILY_SKU: dict[str, dict[str, str]] = {
    "e2": {
        "cpu_desc": "E2 Instance Core",
        "ram_desc": "E2 Instance Ram",
        "preempt_cpu_desc": "Preemptible E2 Instance Core",
        "preempt_ram_desc": "Preemptible E2 Instance Ram",
        "cud_cpu_desc": "Committed Use Discount for E2 VCPU",
        "cud_ram_desc": "Committed Use Discount for E2 Memory",
    },
    "n1": {
        "cpu_desc": "N1 Predefined Instance Core",
        "ram_desc": "N1 Predefined Instance Ram",
        "preempt_cpu_desc": "Preemptible N1 Predefined Instance Core",
        "preempt_ram_desc": "Preemptible N1 Predefined Instance Ram",
        "cud_cpu_desc": "Committed Use Discount for N1 VCPU",
        "cud_ram_desc": "Committed Use Discount for N1 Memory",
    },
    "n2": {
        "cpu_desc": "N2 Instance Core",
        "ram_desc": "N2 Instance Ram",
        "preempt_cpu_desc": "Preemptible N2 Instance Core",
        "preempt_ram_desc": "Preemptible N2 Instance Ram",
        "cud_cpu_desc": "Committed Use Discount for N2 VCPU",
        "cud_ram_desc": "Committed Use Discount for N2 Memory",
    },
    "n2d": {
        "cpu_desc": "N2D AMD Instance Core",
        "ram_desc": "N2D AMD Instance Ram",
        "preempt_cpu_desc": "Preemptible N2D AMD Instance Core",
        "preempt_ram_desc": "Preemptible N2D AMD Instance Ram",
        "cud_cpu_desc": "Committed Use Discount for N2D VCPU",
        "cud_ram_desc": "Committed Use Discount for N2D Memory",
    },
    "c2": {
        "cpu_desc": "Compute optimized Core",
        "ram_desc": "Compute optimized Ram",
        "preempt_cpu_desc": "Preemptible Compute optimized Core",
        "preempt_ram_desc": "Preemptible Compute optimized Ram",
        "cud_cpu_desc": "Committed Use Discount for C2 VCPU",
        "cud_ram_desc": "Committed Use Discount for C2 Memory",
    },
    "c2d": {
        "cpu_desc": "C2D AMD Instance Core",
        "ram_desc": "C2D AMD Instance Ram",
        "preempt_cpu_desc": "Preemptible C2D AMD Instance Core",
        "preempt_ram_desc": "Preemptible C2D AMD Instance Ram",
        "cud_cpu_desc": "Committed Use Discount for C2D VCPU",
        "cud_ram_desc": "Committed Use Discount for C2D Memory",
    },
    "c3": {
        "cpu_desc": "C3 Instance Core",
        "ram_desc": "C3 Instance Ram",
        "preempt_cpu_desc": "Spot Preemptible C3 Instance Core",
        "preempt_ram_desc": "Spot Preemptible C3 Instance Ram",
        "cud_cpu_desc": "Committed Use Discount for C3 VCPU",
        "cud_ram_desc": "Committed Use Discount for C3 Memory",
    },
    "t2d": {
        "cpu_desc": "T2D AMD Instance Core",
        "ram_desc": "T2D AMD Instance Ram",
        "preempt_cpu_desc": "Preemptible T2D AMD Instance Core",
        "preempt_ram_desc": "Preemptible T2D AMD Instance Ram",
        "cud_cpu_desc": "Committed Use Discount for T2D VCPU",
        "cud_ram_desc": "Committed Use Discount for T2D Memory",
    },
    "t2a": {
        "cpu_desc": "T2A Arm Instance Core",
        "ram_desc": "T2A Arm Instance Ram",
        "preempt_cpu_desc": "Preemptible T2A Arm Instance Core",
        "preempt_ram_desc": "Preemptible T2A Arm Instance Ram",
        "cud_cpu_desc": "Committed Use Discount for T2A VCPU",
        "cud_ram_desc": "Committed Use Discount for T2A Memory",
    },
    "m1": {
        "cpu_desc": "Memory-optimized Instance Core",
        "ram_desc": "Memory-optimized Instance Ram",
        "preempt_cpu_desc": "Preemptible Memory-optimized Instance Core",
        "preempt_ram_desc": "Preemptible Memory-optimized Instance Ram",
        "cud_cpu_desc": "Committed Use Discount for Memory Optimized VCPU",
        "cud_ram_desc": "Committed Use Discount for Memory Optimized Memory",
    },
    "a2": {
        "cpu_desc": "A2 Instance Core",
        "ram_desc": "A2 Instance Ram",
        "preempt_cpu_desc": "Preemptible A2 Instance Core",
        "preempt_ram_desc": "Preemptible A2 Instance Ram",
        "cud_cpu_desc": "Committed Use Discount for A2 VCPU",
        "cud_ram_desc": "Committed Use Discount for A2 Memory",
    },
}

# ---------------------------------------------------------------------------
# Cloud SQL instance type specs: instance_type -> (vcpus, memory_gb)
# ---------------------------------------------------------------------------
CLOUD_SQL_INSTANCE_SPECS: dict[str, tuple[float, float]] = {
    # Shared core (special pricing - different SKU)
    "db-f1-micro":   (0.2, 0.614),
    "db-g1-small":   (0.5, 1.700),
    # Standard (n1-style)
    "db-n1-standard-1":  (1,  3.75),
    "db-n1-standard-2":  (2,  7.5),
    "db-n1-standard-4":  (4,  15.0),
    "db-n1-standard-8":  (8,  30.0),
    "db-n1-standard-16": (16, 60.0),
    "db-n1-standard-32": (32, 120.0),
    "db-n1-standard-64": (64, 240.0),
    # High memory (n1-style)
    "db-n1-highmem-2":  (2,  13.0),
    "db-n1-highmem-4":  (4,  26.0),
    "db-n1-highmem-8":  (8,  52.0),
    "db-n1-highmem-16": (16, 104.0),
    "db-n1-highmem-32": (32, 208.0),
    "db-n1-highmem-64": (64, 416.0),
    # Custom / newer standard tiers
    "db-standard-1":   (1,  3.75),
    "db-standard-2":   (2,  7.5),
    "db-standard-4":   (4,  15.0),
    "db-standard-8":   (8,  30.0),
    "db-standard-16":  (16, 60.0),
    "db-standard-32":  (32, 120.0),
    # High memory custom tiers
    "db-highmem-2":  (2,  13.0),
    "db-highmem-4":  (4,  26.0),
    "db-highmem-8":  (8,  52.0),
    "db-highmem-16": (16, 104.0),
}

# Persistent storage SKU description patterns
GCP_STORAGE_SKU: dict[str, dict[str, str]] = {
    "pd-standard": {
        "desc": "Storage PD Capacity",
        "alt_desc": "Regional Storage PD Capacity",
    },
    "pd-ssd": {
        "desc": "SSD backed PD Capacity",
        "alt_desc": "Regional SSD backed PD Capacity",
    },
    "pd-balanced": {
        "desc": "Balanced PD Capacity",
        "alt_desc": "Regional Balanced PD Capacity",
    },
    "pd-extreme": {
        "desc": "Extreme PD Capacity",
        "alt_desc": "Extreme PD Capacity",
    },
}
