"""Tests for the GCP provider — mocks httpx to avoid live API calls."""
from __future__ import annotations

from decimal import Decimal
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from cloudcostmcp.cache import CacheManager
from cloudcostmcp.config import Settings
from cloudcostmcp.models import CloudProvider, PriceUnit, PricingTerm
from cloudcostmcp.providers.base import NotConfiguredError
from cloudcostmcp.providers.gcp import GCPProvider
from cloudcostmcp.utils.gcp_specs import parse_instance_type, get_machine_family


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
    # N2 on-demand
    _make_sku("N2 Instance Core running in Americas", "OnDemand", ["us-central1", "us-east1", "us-east4"], 31611000),    # $0.031611/core-hr
    _make_sku("N2 Instance Ram running in Americas",  "OnDemand", ["us-central1", "us-east1", "us-east4"], 4237000),     # $0.004237/GB-hr
    # N2 preemptible
    _make_sku("Preemptible N2 Instance Core running in Americas", "Preemptible", ["us-central1", "us-east1"], 7610000),  # $0.007610/core-hr
    _make_sku("Preemptible N2 Instance Ram running in Americas",  "Preemptible", ["us-central1", "us-east1"], 1020000),  # $0.001020/GB-hr
    # N2 CUD 1yr
    _make_sku("Committed Use Discount for N2 VCPU in Americas", "Commit1Yr", ["us-central1", "us-east1"], 19560000),     # $0.019560/core-hr
    _make_sku("Committed Use Discount for N2 Memory in Americas", "Commit1Yr", ["us-central1", "us-east1"], 2626000),    # $0.002626/GB-hr
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
