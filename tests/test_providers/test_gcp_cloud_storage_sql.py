"""Tests for GCP Cloud Storage (GCS) and Cloud SQL pricing."""
from __future__ import annotations

from decimal import Decimal
from pathlib import Path
from unittest.mock import AsyncMock, patch

import pytest

from opencloudcosts.cache import CacheManager
from opencloudcosts.config import Settings
from opencloudcosts.models import PriceUnit, PricingTerm
from opencloudcosts.providers.gcp import GCPProvider


# ---------------------------------------------------------------------------
# Helpers: minimal but realistic GCP SKU structures
# ---------------------------------------------------------------------------

def _make_gcs_sku(description: str, regions: list[str], price_units: str = "0", price_nanos: int = 0) -> dict:
    return {
        "description": description,
        "serviceRegions": regions,
        "category": {"resourceFamily": "Storage", "usageType": "OnDemand"},
        "pricingInfo": [{"pricingExpression": {"tieredRates": [
            {"startUsageAmount": 0, "unitPrice": {"units": price_units, "nanos": price_nanos}}
        ]}}],
    }


def _make_cloud_sql_sku(description: str, regions: list[str], price_units: str = "0", price_nanos: int = 0) -> dict:
    return {
        "description": description,
        "serviceRegions": regions,
        "category": {"usageType": "OnDemand"},
        "pricingInfo": [{"pricingExpression": {"tieredRates": [
            {"startUsageAmount": 0, "unitPrice": {"units": price_units, "nanos": price_nanos}}
        ]}}],
    }


# Realistic GCS SKUs for us-central1
GCS_FAKE_SKUS = [
    # Standard Storage — $0.02/GiB-month
    _make_gcs_sku("Standard Storage US Regional", ["us-central1"], "0", 20_000_000),
    # Nearline Storage — $0.01/GiB-month
    _make_gcs_sku("Nearline Storage US Regional", ["us-central1"], "0", 10_000_000),
    # Coldline Storage — $0.004/GiB-month
    _make_gcs_sku("Coldline Storage US Regional", ["us-central1"], "0", 4_000_000),
    # Archive Storage — $0.0012/GiB-month
    _make_gcs_sku("Archive Storage US Regional", ["us-central1"], "0", 1_200_000),
    # Operations SKU — should be excluded
    _make_gcs_sku("Standard Storage US Operations", ["us-central1"], "0", 5_000_000),
    # Retrieval SKU — should be excluded
    _make_gcs_sku("Coldline Storage Retrieval", ["us-central1"], "0", 1_000_000),
    # Early Delete SKU — should be excluded
    _make_gcs_sku("Nearline Storage Early Delete", ["us-central1"], "0", 500_000),
]

# Realistic Cloud SQL SKUs for us-central1
# GCP encodes instance size directly in the description:
# "Cloud SQL for MySQL: Zonal - 4 vCPU + 15GB RAM in Americas"
# db-n1-standard-4 = 4 vCPU, 15 GB RAM
# db-n1-standard-8 = 8 vCPU, 30 GB RAM
CLOUD_SQL_FAKE_SKUS = [
    # MySQL Zonal db-n1-standard-4: 4*$0.0413 + 15*$0.007 = $0.2702/hr
    _make_cloud_sql_sku(
        "Cloud SQL for MySQL: Zonal - 4 vCPU + 15GB RAM in Americas",
        ["us-central1"], "0", 270_200_000,
    ),
    # MySQL Zonal db-n1-standard-8: 8*$0.0413 + 30*$0.007 = $0.5404/hr
    _make_cloud_sql_sku(
        "Cloud SQL for MySQL: Zonal - 8 vCPU + 30GB RAM in Americas",
        ["us-central1"], "0", 540_400_000,
    ),
    # MySQL Regional (HA) db-n1-standard-4: 4*$0.0826 + 15*$0.014 = $0.5404/hr
    _make_cloud_sql_sku(
        "Cloud SQL for MySQL: Regional - 4 vCPU + 15GB RAM in Americas",
        ["us-central1"], "0", 540_400_000,
    ),
    # PostgreSQL Zonal db-n1-standard-4: $0.2702/hr
    _make_cloud_sql_sku(
        "Cloud SQL for PostgreSQL: Zonal - 4 vCPU + 15GB RAM in Americas",
        ["us-central1"], "0", 270_200_000,
    ),
    # PostgreSQL Regional (HA) db-n1-standard-4: $0.5404/hr
    _make_cloud_sql_sku(
        "Cloud SQL for PostgreSQL: Regional - 4 vCPU + 15GB RAM in Americas",
        ["us-central1"], "0", 540_400_000,
    ),
]


# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

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
# GCS tests
# ---------------------------------------------------------------------------

async def test_gcs_standard_price(gcp_provider):
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=GCS_FAKE_SKUS)):
        prices = await gcp_provider.get_storage_price("standard", "us-central1")

    assert len(prices) == 1
    p = prices[0]
    assert p.service == "storage"
    assert p.product_family == "Cloud Storage"
    assert p.unit == PriceUnit.PER_GB_MONTH
    assert p.pricing_term == PricingTerm.ON_DEMAND
    assert p.price_per_unit == Decimal("0.02")
    assert p.attributes.get("storage_type") == "standard"
    assert p.attributes.get("class") == "standard"
    assert p.region == "us-central1"


async def test_gcs_nearline_price(gcp_provider):
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=GCS_FAKE_SKUS)):
        prices = await gcp_provider.get_storage_price("nearline", "us-central1")

    assert len(prices) == 1
    assert prices[0].price_per_unit == Decimal("0.01")


async def test_gcs_coldline_price(gcp_provider):
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=GCS_FAKE_SKUS)):
        prices = await gcp_provider.get_storage_price("coldline", "us-central1")

    assert len(prices) == 1
    assert prices[0].price_per_unit == Decimal("0.004")


async def test_gcs_archive_price(gcp_provider):
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=GCS_FAKE_SKUS)):
        prices = await gcp_provider.get_storage_price("archive", "us-central1")

    assert len(prices) == 1
    assert prices[0].price_per_unit == Decimal("0.0012")


async def test_gcs_nearline_cheaper_than_standard(gcp_provider):
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=GCS_FAKE_SKUS)):
        std = await gcp_provider.get_storage_price("standard", "us-central1")
        nrl = await gcp_provider.get_storage_price("nearline", "us-central1")

    assert nrl[0].price_per_unit < std[0].price_per_unit


async def test_gcs_archive_cheapest(gcp_provider):
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=GCS_FAKE_SKUS)):
        std = await gcp_provider.get_storage_price("standard", "us-central1")
        nrl = await gcp_provider.get_storage_price("nearline", "us-central1")
        cld = await gcp_provider.get_storage_price("coldline", "us-central1")
        arc = await gcp_provider.get_storage_price("archive", "us-central1")

    prices = [std[0].price_per_unit, nrl[0].price_per_unit, cld[0].price_per_unit, arc[0].price_per_unit]
    # Archive should be the cheapest
    assert arc[0].price_per_unit == min(prices)


async def test_gcs_unknown_class_raises(gcp_provider):
    with pytest.raises(ValueError, match="Unknown GCP storage type"):
        await gcp_provider.get_storage_price("glacier", "us-central1")


async def test_gcs_excludes_operations(gcp_provider):
    """Operations SKU should not be returned as a capacity price."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=GCS_FAKE_SKUS)):
        index = await gcp_provider._build_gcs_price_index("us-central1")

    # The operations/retrieval/early-delete SKUs must not appear in the index
    for (desc, _) in index:
        desc_lower = desc.lower()
        assert "operations" not in desc_lower
        assert "retrieval" not in desc_lower
        assert "early delete" not in desc_lower


# ---------------------------------------------------------------------------
# Cloud SQL tests
# ---------------------------------------------------------------------------

async def test_cloud_sql_mysql_zonal(gcp_provider):
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=CLOUD_SQL_FAKE_SKUS)):
        prices = await gcp_provider.get_cloud_sql_price("db-n1-standard-4", "us-central1", "MySQL", ha=False)

    assert len(prices) == 1
    p = prices[0]
    assert p.service == "database"
    assert p.product_family == "Cloud SQL"
    assert p.unit == PriceUnit.PER_HOUR
    assert p.pricing_term == PricingTerm.ON_DEMAND
    assert p.attributes["engine"] == "MySQL"
    assert p.attributes["ha"] == "False"
    assert p.attributes["instanceType"] == "db-n1-standard-4"
    # 4 vCPU * $0.0413 + 15 GB * $0.007 = $0.1652 + $0.105 = $0.2702
    assert p.price_per_unit == Decimal("0.0413") * 4 + Decimal("0.007") * 15


async def test_cloud_sql_postgresql_ha(gcp_provider):
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=CLOUD_SQL_FAKE_SKUS)):
        prices = await gcp_provider.get_cloud_sql_price("db-n1-standard-4", "us-central1", "PostgreSQL", ha=True)

    assert len(prices) == 1
    p = prices[0]
    assert p.attributes["engine"] == "PostgreSQL"
    assert p.attributes["ha"] == "True"
    assert "HA/Regional" in p.description


async def test_cloud_sql_pricing_math(gcp_provider):
    """Verify: price = vcpu_count * cpu_rate + mem_gb * ram_rate."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=CLOUD_SQL_FAKE_SKUS)):
        prices = await gcp_provider.get_cloud_sql_price("db-n1-standard-8", "us-central1", "MySQL", ha=False)

    assert len(prices) == 1
    p = prices[0]
    # db-n1-standard-8: 8 vCPU, 30 GB
    cpu_rate = Decimal("0.0413")
    ram_rate = Decimal("0.007")
    expected = cpu_rate * 8 + ram_rate * 30
    assert p.price_per_unit == expected


async def test_cloud_sql_ha_costs_more(gcp_provider):
    """HA (Regional) pricing should be greater than Zonal for the same instance."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=CLOUD_SQL_FAKE_SKUS)):
        zonal = await gcp_provider.get_cloud_sql_price("db-n1-standard-4", "us-central1", "MySQL", ha=False)
        regional = await gcp_provider.get_cloud_sql_price("db-n1-standard-4", "us-central1", "MySQL", ha=True)

    assert zonal[0].price_per_unit < regional[0].price_per_unit


async def test_cloud_sql_unknown_instance_raises(gcp_provider):
    with pytest.raises(ValueError, match="Unknown Cloud SQL instance type"):
        await gcp_provider.get_cloud_sql_price("db-r5.large", "us-central1", "MySQL")


async def test_cloud_sql_engine_normalization(gcp_provider):
    """postgres input should be normalized to PostgreSQL for SKU lookup."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=CLOUD_SQL_FAKE_SKUS)):
        prices = await gcp_provider.get_cloud_sql_price("db-n1-standard-4", "us-central1", "postgres", ha=False)

    assert len(prices) == 1
    assert prices[0].attributes["engine"] == "PostgreSQL"
