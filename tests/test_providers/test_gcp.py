"""Tests for the GCP provider — mocks httpx to avoid live API calls."""
from __future__ import annotations

from decimal import Decimal
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from opencloudcosts.cache import CacheManager
from opencloudcosts.config import Settings
from opencloudcosts.models import CloudProvider, PriceUnit, PricingTerm
from opencloudcosts.providers.base import NotConfiguredError
from opencloudcosts.providers.gcp import GCPProvider
from opencloudcosts.utils.gcp_specs import parse_instance_type, get_machine_family


# ---------------------------------------------------------------------------
# Minimal SKU data matching what the Catalog API returns
# ---------------------------------------------------------------------------

def _make_sku(description: str, usage_type: str, regions: list[str], price_nanos: int) -> dict:
    return {
        "name": f"services/6F81-5844-456A/skus/FAKE-{description[:4]}",
        "skuId": f"FAKE-{description[:4]}",
        "description": description,
        "category": {
            "serviceDisplayName": "Compute Engine",
            "resourceFamily": "Compute",
            "resourceGroup": "CPU",
            "usageType": usage_type,
        },
        "serviceRegions": regions,
        "pricingInfo": [{
            "pricingExpression": {
                "usageUnit": "h",
                "tieredRates": [{
                    "startUsageAmount": 0,
                    "unitPrice": {
                        "currencyCode": "USD",
                        "units": "0",
                        "nanos": price_nanos,
                    },
                }],
            }
        }],
    }


FAKE_SKUS = [
    # A2 on-demand (GPU, A100)
    _make_sku("A2 Instance Core running in Americas", "OnDemand", ["us-central1", "us-east1"], 1116757000),  # $1.116757/core-hr
    _make_sku("A2 Instance Ram running in Americas",  "OnDemand", ["us-central1", "us-east1"], 149688000),   # $0.149688/GB-hr
    # A2 preemptible
    _make_sku("Preemptible A2 Instance Core running in Americas", "Preemptible", ["us-central1"], 334958000),  # $0.334958/core-hr
    _make_sku("Preemptible A2 Instance Ram running in Americas",  "Preemptible", ["us-central1"], 44906000),   # $0.044906/GB-hr
    # N2 on-demand
    _make_sku("N2 Instance Core running in Americas", "OnDemand", ["us-central1", "us-east1", "us-east4"], 31611000),    # $0.031611/core-hr
    _make_sku("N2 Instance Ram running in Americas",  "OnDemand", ["us-central1", "us-east1", "us-east4"], 4237000),     # $0.004237/GB-hr
    # N2 preemptible
    _make_sku("Preemptible N2 Instance Core running in Americas", "Preemptible", ["us-central1", "us-east1"], 7610000),  # $0.007610/core-hr
    _make_sku("Preemptible N2 Instance Ram running in Americas",  "Preemptible", ["us-central1", "us-east1"], 1020000),  # $0.001020/GB-hr
    # N2 CUD 1yr
    _make_sku("Committed Use Discount for N2 VCPU in Americas", "Commit1Yr", ["us-central1", "us-east1"], 19560000),     # $0.019560/core-hr
    _make_sku("Committed Use Discount for N2 Memory in Americas", "Commit1Yr", ["us-central1", "us-east1"], 2626000),    # $0.002626/GB-hr
    # N2 Windows license SKUs (T31: per-vCPU and per-GB-RAM on top of base Linux)
    _make_sku("N2 Instance Core running Windows", "OnDemand", ["us-central1", "us-east1"], 45000000),                    # $0.045/core-hr
    _make_sku("N2 Instance Ram running Windows",  "OnDemand", ["us-central1", "us-east1"], 6000000),                     # $0.006/GB-hr
    # E2 on-demand
    _make_sku("E2 Instance Core running in Americas", "OnDemand", ["us-central1", "us-east1"], 21840000),                # $0.021840/core-hr
    _make_sku("E2 Instance Ram running in Americas",  "OnDemand", ["us-central1", "us-east1"], 2923000),                 # $0.002923/GB-hr
    # PD storage
    _make_sku("Storage PD Capacity", "OnDemand", ["us-central1", "us-east1"], 40000000),                                 # $0.04/GB-mo
    _make_sku("SSD backed PD Capacity", "OnDemand", ["us-central1", "us-east1"], 170000000),                             # $0.17/GB-mo
]


@pytest.fixture
async def gcp_provider(tmp_path: Path):
    settings = Settings(
        cache_dir=tmp_path / "cache",
        cache_ttl_hours=1,
        gcp_api_key="fake-api-key",
    )
    cache = CacheManager(settings.cache_dir)
    await cache.initialize()
    provider = GCPProvider(settings, cache)
    yield provider
    await cache.close()
    await provider.close()


# ---------------------------------------------------------------------------
# gcp_specs unit tests
# ---------------------------------------------------------------------------

def test_parse_instance_type_exact():
    assert parse_instance_type("n2-standard-4") == (4, 16.0)
    assert parse_instance_type("e2-standard-8") == (8, 32.0)
    assert parse_instance_type("n1-highmem-16") == (16, 104.0)


def test_parse_instance_type_pattern_fallback():
    # Not in table but follows naming convention
    result = parse_instance_type("n2-standard-200")
    assert result is not None
    vcpus, mem = result
    assert vcpus == 200
    assert mem == 800.0  # 200 * 4.0


def test_parse_instance_type_unknown():
    assert parse_instance_type("custom-weird") is None


def test_get_machine_family():
    assert get_machine_family("n2-standard-4") == "n2"
    assert get_machine_family("n2d-standard-8") == "n2d"
    assert get_machine_family("e2-highmem-4") == "e2"
    assert get_machine_family("c2-standard-16") == "c2"


# ---------------------------------------------------------------------------
# GCPProvider tests (httpx mocked)
# ---------------------------------------------------------------------------

async def test_get_compute_price_n2_standard_4(gcp_provider: GCPProvider):
    """n2-standard-4: 4 vCPU * $0.031611 + 16 GB * $0.004237 = $0.194236/hr"""
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=FAKE_SKUS)):
        prices = await gcp_provider.get_compute_price("n2-standard-4", "us-central1")

    assert len(prices) == 1
    p = prices[0]
    assert p.provider == CloudProvider.GCP
    assert p.pricing_term == PricingTerm.ON_DEMAND
    assert p.unit == PriceUnit.PER_HOUR
    assert p.region == "us-central1"
    assert p.attributes["vcpu"] == "4"
    assert p.attributes["memory"] == "16.0 GB"
    # 4 * 0.031611 + 16 * 0.004237 = 0.126444 + 0.067792 = 0.194236
    expected = Decimal("4") * Decimal("0.031611") + Decimal("16") * Decimal("0.004237")
    assert abs(p.price_per_unit - expected) < Decimal("0.000001")


async def test_get_compute_price_e2_standard_2(gcp_provider: GCPProvider):
    """e2-standard-2: 2 vCPU * $0.021840 + 8 GB * $0.002923"""
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=FAKE_SKUS)):
        prices = await gcp_provider.get_compute_price("e2-standard-2", "us-central1")

    assert len(prices) == 1
    expected = Decimal("2") * Decimal("0.021840") + Decimal("8") * Decimal("0.002923")
    assert abs(prices[0].price_per_unit - expected) < Decimal("0.000001")


async def test_get_compute_price_spot(gcp_provider: GCPProvider):
    """Spot (preemptible) pricing uses different SKU descriptions."""
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=FAKE_SKUS)):
        prices = await gcp_provider.get_compute_price(
            "n2-standard-4", "us-central1", term=PricingTerm.SPOT
        )

    assert len(prices) == 1
    assert prices[0].pricing_term == PricingTerm.SPOT
    # Preemptible: 4 * 0.007610 + 16 * 0.001020
    expected = Decimal("4") * Decimal("0.007610") + Decimal("16") * Decimal("0.001020")
    assert abs(prices[0].price_per_unit - expected) < Decimal("0.000001")
    # Spot should be cheaper than on-demand
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=FAKE_SKUS)):
        od = await gcp_provider.get_compute_price("n2-standard-4", "us-central1")
    assert prices[0].price_per_unit < od[0].price_per_unit


async def test_get_compute_price_cud_1yr(gcp_provider: GCPProvider):
    """CUD 1yr uses Commit1Yr SKUs."""
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=FAKE_SKUS)):
        prices = await gcp_provider.get_compute_price(
            "n2-standard-4", "us-central1", term=PricingTerm.CUD_1YR
        )

    assert len(prices) == 1
    assert prices[0].pricing_term == PricingTerm.CUD_1YR
    # CUD should be cheaper than on-demand
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=FAKE_SKUS)):
        od = await gcp_provider.get_compute_price("n2-standard-4", "us-central1")
    assert prices[0].price_per_unit < od[0].price_per_unit


async def test_get_compute_price_region_not_available(gcp_provider: GCPProvider):
    """Region not in serviceRegions -> empty result."""
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=FAKE_SKUS)):
        prices = await gcp_provider.get_compute_price("n2-standard-4", "europe-west1")
    assert prices == []


async def test_get_compute_price_cached(gcp_provider: GCPProvider):
    """Second call should use cache — _fetch_skus called only once."""
    mock = AsyncMock(return_value=FAKE_SKUS)
    with patch.object(gcp_provider, "_fetch_skus", mock):
        await gcp_provider.get_compute_price("n2-standard-4", "us-central1")
        # Warm the price index cache too
        await gcp_provider.get_compute_price("n2-standard-4", "us-central1")

    # _fetch_skus is called once to build the SKU list (cached in metadata)
    assert mock.call_count == 1


async def test_get_compute_price_unknown_type(gcp_provider: GCPProvider):
    # Completely unparseable type raises "Unknown GCP instance type"
    with pytest.raises(ValueError):
        await gcp_provider.get_compute_price("notaninstance", "us-central1")
    # Parseable but unsupported family raises "not yet supported"
    with pytest.raises(ValueError, match="not yet supported"):
        await gcp_provider.get_compute_price("zz-standard-4", "us-central1")


async def test_get_storage_price_pd_standard(gcp_provider: GCPProvider):
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=FAKE_SKUS)):
        prices = await gcp_provider.get_storage_price("pd-standard", "us-central1")

    assert len(prices) == 1
    assert prices[0].unit == PriceUnit.PER_GB_MONTH
    assert prices[0].price_per_unit == Decimal("0.04")


async def test_get_storage_price_ssd(gcp_provider: GCPProvider):
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=FAKE_SKUS)):
        prices = await gcp_provider.get_storage_price("pd-ssd", "us-central1")

    assert len(prices) == 1
    assert prices[0].price_per_unit == Decimal("0.17")


async def test_get_storage_price_unknown_type(gcp_provider: GCPProvider):
    with pytest.raises(ValueError, match="Unknown GCP storage type"):
        await gcp_provider.get_storage_price("nvme-ultra", "us-central1")


async def test_list_regions(gcp_provider: GCPProvider):
    regions = await gcp_provider.list_regions()
    assert "us-central1" in regions
    assert "europe-west1" in regions
    assert len(regions) > 20


async def test_list_instance_types_family_filter(gcp_provider: GCPProvider):
    instances = await gcp_provider.list_instance_types("us-central1", family="n2-standard")
    assert all(i.instance_type.startswith("n2-standard") for i in instances)
    assert len(instances) > 0


async def test_list_instance_types_min_vcpus(gcp_provider: GCPProvider):
    instances = await gcp_provider.list_instance_types("us-central1", min_vcpus=32)
    assert all(i.vcpu >= 32 for i in instances)


async def test_check_availability(gcp_provider: GCPProvider):
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=FAKE_SKUS)):
        assert await gcp_provider.check_availability("compute", "n2-standard-4", "us-central1")
        assert not await gcp_provider.check_availability("compute", "n2-standard-4", "mars-west1")


async def test_effective_price_not_implemented(gcp_provider: GCPProvider):
    with pytest.raises(NotConfiguredError, match="BigQuery"):
        await gcp_provider.get_effective_price("compute", "n2-standard-4", "us-central1")


# ---------------------------------------------------------------------------
# Cross-provider comparison smoke test
# ---------------------------------------------------------------------------

# ---------------------------------------------------------------------------
# T31: Windows pricing tests
# ---------------------------------------------------------------------------

async def test_get_compute_price_gcp_windows_higher_than_linux(gcp_provider: GCPProvider):
    """Windows price should be higher than Linux for a supported family (N2)."""
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=FAKE_SKUS)):
        windows_prices = await gcp_provider.get_compute_price(
            "n2-standard-4", "us-central1", os="Windows"
        )
        linux_prices = await gcp_provider.get_compute_price(
            "n2-standard-4", "us-central1", os="Linux"
        )

    assert len(windows_prices) == 1
    assert len(linux_prices) == 1

    win_price = windows_prices[0].price_per_unit
    lin_price = linux_prices[0].price_per_unit
    assert win_price > lin_price, (
        f"Windows price ({win_price}) should be higher than Linux price ({lin_price})"
    )

    # Verify attributes reflect Windows OS
    assert windows_prices[0].attributes["operatingSystem"] == "Windows"

    # Sanity check: Windows license adds 4 * $0.045 + 16 * $0.006 = $0.180 + $0.096 = $0.276 extra
    expected_linux = Decimal("4") * Decimal("0.031611") + Decimal("16") * Decimal("0.004237")
    expected_windows_license = Decimal("4") * Decimal("0.045") + Decimal("16") * Decimal("0.006")
    expected_windows = expected_linux + expected_windows_license
    assert abs(win_price - expected_windows) < Decimal("0.000001")


async def test_get_compute_price_gcp_windows_e2_not_supported(gcp_provider: GCPProvider):
    """E2 instances don't support Windows — should return [] not Linux price."""
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=FAKE_SKUS)):
        prices = await gcp_provider.get_compute_price(
            "e2-standard-4", "us-central1", os="Windows"
        )
    assert prices == [], (
        "E2 + Windows should return empty list, not silently return Linux price"
    )


async def test_get_compute_price_gcp_linux_unchanged(gcp_provider: GCPProvider):
    """os='Linux' default behaviour must be unchanged after T31 changes."""
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=FAKE_SKUS)):
        prices_default = await gcp_provider.get_compute_price("n2-standard-4", "us-central1")
        prices_explicit = await gcp_provider.get_compute_price(
            "n2-standard-4", "us-central1", os="Linux"
        )

    assert len(prices_default) == 1
    assert len(prices_explicit) == 1
    # Both should return identical Linux-only price
    assert prices_default[0].price_per_unit == prices_explicit[0].price_per_unit
    # Price should match the Linux-only calculation (no Windows uplift)
    expected = Decimal("4") * Decimal("0.031611") + Decimal("16") * Decimal("0.004237")
    assert abs(prices_default[0].price_per_unit - expected) < Decimal("0.000001")


async def test_get_compute_price_gcp_windows_sku_not_found_returns_empty(gcp_provider: GCPProvider):
    """If Windows SKUs are not in the catalog for the region, return [] gracefully."""
    # Use a SKU set that has no Windows SKUs (just the base Linux ones)
    skus_without_windows = [s for s in FAKE_SKUS if "running Windows" not in s["description"]]
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=skus_without_windows)):
        prices = await gcp_provider.get_compute_price(
            "n2-standard-4", "us-central1", os="Windows"
        )
    assert prices == [], "Missing Windows SKU should return [] not raise an exception"


async def test_gcp_cheaper_than_aws_for_equivalent(gcp_provider: GCPProvider):
    """
    GCP n2-standard-4 (4 vCPU, 16GB) vs AWS m5.xlarge (4 vCPU, 16GB).
    GCP is typically cheaper on-demand. This is a sanity check on price magnitudes.
    """
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=FAKE_SKUS)):
        gcp_prices = await gcp_provider.get_compute_price("n2-standard-4", "us-central1")

    assert len(gcp_prices) == 1
    gcp_price = gcp_prices[0].price_per_unit
    # GCP n2-standard-4 on-demand ~$0.19/hr, AWS m5.xlarge ~$0.192/hr
    # Both should be in the $0.10-$0.50/hr range
    assert Decimal("0.05") < gcp_price < Decimal("0.50"), f"Unexpected GCP price: {gcp_price}"


# ---------------------------------------------------------------------------
# A2 GPU instance family tests
# ---------------------------------------------------------------------------

async def test_get_compute_price_a2_highgpu_1g(gcp_provider: GCPProvider):
    """
    a2-highgpu-1g: 12 vCPU, 85 GB RAM.
    OnDemand: 12 * $1.116757 + 85 * $0.149688
    """
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=FAKE_SKUS)):
        prices = await gcp_provider.get_compute_price("a2-highgpu-1g", "us-central1")

    assert len(prices) == 1
    p = prices[0]
    assert p.provider == CloudProvider.GCP
    assert p.pricing_term == PricingTerm.ON_DEMAND
    assert p.unit == PriceUnit.PER_HOUR
    assert p.region == "us-central1"
    assert p.attributes["vcpu"] == "12"
    assert p.attributes["memory"] == "85.0 GB"
    assert p.attributes["machineFamily"] == "a2"
    # 12 * 1.116757 + 85 * 0.149688 = 13.401084 + 12.72348 = 26.124564
    expected = Decimal("12") * Decimal("1.116757") + Decimal("85") * Decimal("0.149688")
    assert abs(p.price_per_unit - expected) < Decimal("0.000001")
    # A2 GPU instances are significantly more expensive than CPU-only
    assert p.price_per_unit > Decimal("20"), "A2 GPU instances should be well over $20/hr"


async def test_get_compute_price_a2_spot(gcp_provider: GCPProvider):
    """A2 preemptible pricing uses Preemptible A2 SKUs."""
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=FAKE_SKUS)):
        spot_prices = await gcp_provider.get_compute_price(
            "a2-highgpu-1g", "us-central1", term=PricingTerm.SPOT
        )
        od_prices = await gcp_provider.get_compute_price(
            "a2-highgpu-1g", "us-central1"
        )

    assert len(spot_prices) == 1
    assert spot_prices[0].pricing_term == PricingTerm.SPOT
    # Spot should be cheaper than on-demand
    assert spot_prices[0].price_per_unit < od_prices[0].price_per_unit


async def test_get_compute_price_a2_no_longer_unsupported(gcp_provider: GCPProvider):
    """a2 family must no longer raise 'not yet supported'."""
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=FAKE_SKUS)):
        # Should not raise ValueError about unsupported family
        prices = await gcp_provider.get_compute_price("a2-highgpu-1g", "us-central1")
    assert prices != [], "a2-highgpu-1g should return pricing, not an empty list"


async def test_get_compute_price_a2_windows_not_supported(gcp_provider: GCPProvider):
    """A2 does not support Windows — should return []."""
    with patch.object(gcp_provider, "_fetch_skus", AsyncMock(return_value=FAKE_SKUS)):
        prices = await gcp_provider.get_compute_price(
            "a2-highgpu-1g", "us-central1", os="Windows"
        )
    assert prices == [], "A2 + Windows should return empty list (no Windows support)"


def test_a2_in_supported_families():
    """a2 must appear in the GCP_FAMILY_SKU dict so supported family errors name it."""
    from opencloudcosts.utils.gcp_specs import GCP_FAMILY_SKU
    assert "a2" in GCP_FAMILY_SKU, "a2 must be a supported GCP machine family"


def test_a2_instance_specs_present():
    """All A2 instance types must be in GCP_INSTANCE_SPECS with correct vCPU/RAM."""
    from opencloudcosts.utils.gcp_specs import GCP_INSTANCE_SPECS
    expected = {
        "a2-highgpu-1g": (12, 85.0),
        "a2-highgpu-2g": (24, 170.0),
        "a2-highgpu-4g": (48, 340.0),
        "a2-highgpu-8g": (96, 680.0),
        "a2-megagpu-16g": (96, 1360.0),
    }
    for itype, (vcpus, mem) in expected.items():
        assert itype in GCP_INSTANCE_SPECS, f"{itype} missing from GCP_INSTANCE_SPECS"
        assert GCP_INSTANCE_SPECS[itype] == (vcpus, mem), (
            f"{itype}: expected ({vcpus}, {mem}), got {GCP_INSTANCE_SPECS[itype]}"
        )
