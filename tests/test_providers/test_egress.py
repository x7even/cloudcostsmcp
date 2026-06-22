"""Comprehensive egress pricing tests for AWS, GCP, and Azure providers.

Tests cover:
  a) Internet egress — tier boundary tests (50 GB, 500 GB, 5000 GB)
  b) Cross-region egress
  c) Intra-region / same-zone (GCP: free)
  d) Unknown destination_type → NotSupportedError (structured error)
  e) Trust metadata (as_of, source_url, cache_age_seconds)
  f) Spec validation (cross_region without destination_region, data_gb=0)

Minimum 18 test functions.
"""

from __future__ import annotations

from decimal import Decimal
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from opencloudcosts.cache import CacheManager
from opencloudcosts.config import Settings
from opencloudcosts.models import (
    NetworkPricingSpec,
    PricingDomain,
)
from opencloudcosts.providers.aws import AWSProvider
from opencloudcosts.providers.azure import AzureProvider
from opencloudcosts.providers.base import NotSupportedError
from opencloudcosts.providers.gcp import GCPProvider

# ---------------------------------------------------------------------------
# Shared helpers
# ---------------------------------------------------------------------------


def _make_httpx_mock(data: dict) -> MagicMock:
    """Mock for httpx.get that returns a JSON response."""
    m = MagicMock()
    m.raise_for_status = MagicMock()
    m.json.return_value = data
    return m


_EMPTY_AZURE_API = {"Items": [], "NextPageLink": None}


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------


@pytest.fixture
async def aws_provider(tmp_path: Path):
    settings = Settings(
        cache_dir=tmp_path / "cache",
        cache_ttl_hours=1,
        aws_enable_cost_explorer=False,
    )
    cache = CacheManager(settings.cache_dir)
    await cache.initialize()
    provider = AWSProvider(settings, cache)
    yield provider
    await cache.close()


@pytest.fixture
async def gcp_provider(tmp_path: Path):
    settings = Settings(
        cache_dir=tmp_path / "cache",
        cache_ttl_hours=1,
        gcp_api_key="fake-key",
    )
    cache = CacheManager(settings.cache_dir)
    await cache.initialize()
    provider = GCPProvider(settings, cache)
    yield provider
    await cache.close()
    await provider.close()


@pytest.fixture
async def azure_provider(tmp_path: Path):
    settings = Settings(
        cache_dir=tmp_path / "cache",
        cache_ttl_hours=1,
        aws_enable_cost_explorer=False,
    )
    cache = CacheManager(settings.cache_dir)
    await cache.initialize()
    provider = AzureProvider(settings, cache)
    yield provider
    await cache.close()


# ===========================================================================
# AWS Tests
# ===========================================================================


# --- (a) AWS Internet egress tier boundary tests ---


async def test_aws_internet_egress_50gb_free(aws_provider: AWSProvider):
    """50 GB falls entirely within the 100 GB free tier: $0 total."""
    spec = NetworkPricingSpec(
        provider="aws",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="us-east-1",
        destination_type="internet",
        data_gb_per_month=50.0,
    )
    result = await aws_provider.get_price(spec)

    assert len(result.public_prices) == 1
    breakdown = result.breakdown
    assert breakdown["total_cost"] == "0.0000"
    assert float(breakdown["blended_rate_per_gb"]) == pytest.approx(0.0)


async def test_aws_internet_egress_500gb_first_tier(aws_provider: AWSProvider):
    """500 GB: 100 GB free + 400 GB at $0.090/GB = $36.00."""
    spec = NetworkPricingSpec(
        provider="aws",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="us-east-1",
        destination_type="internet",
        data_gb_per_month=500.0,
    )
    result = await aws_provider.get_price(spec)

    breakdown = result.breakdown
    # 100 free + 400 at $0.090 = $36.00
    assert float(breakdown["total_cost"]) == pytest.approx(36.0, rel=1e-4)
    tiers = breakdown["tiers"]
    free_tier = tiers[0]
    assert free_tier["gb"] == pytest.approx(100.0)
    assert free_tier["rate"] == "0.000"


async def test_aws_internet_egress_5000gb_crosses_tiers(aws_provider: AWSProvider):
    """5000 GB crosses into tier 2 (>10 TB boundary at 10,240 GB is not crossed here).

    Tiers:
      - 0–100 GB: free
      - 100–10,340 GB: $0.090/GB
    5000 GB: 100 GB free + 4,900 GB at $0.090 = $441.00
    """
    spec = NetworkPricingSpec(
        provider="aws",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="us-east-1",
        destination_type="internet",
        data_gb_per_month=5000.0,
    )
    result = await aws_provider.get_price(spec)

    breakdown = result.breakdown
    # 100 free + 4900 at $0.090 = $441.00
    expected = 4900 * 0.090
    assert float(breakdown["total_cost"]) == pytest.approx(expected, rel=1e-4)


# --- (b) AWS Cross-region egress ---


async def test_aws_cross_region_us_east_to_us_west(aws_provider: AWSProvider):
    """us-east-1 → us-west-2: both 'us' continent → rate $0.02/GB."""
    spec = NetworkPricingSpec(
        provider="aws",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="us-east-1",
        destination_type="cross_region",
        destination_region="us-west-2",
        data_gb_per_month=1000.0,
    )
    result = await aws_provider.get_price(spec)

    assert len(result.public_prices) == 1
    p = result.public_prices[0]
    assert p.service == "egress"
    assert float(p.price_per_unit) == pytest.approx(0.02, rel=1e-4)


# --- (d) AWS Unknown destination_type → NotSupportedError ---


async def test_aws_unknown_destination_type_raises(aws_provider: AWSProvider):
    """Unknown destination_type raises NotSupportedError (structured error)."""
    spec = NetworkPricingSpec(
        provider="aws",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="us-east-1",
        destination_type="vpn",
        data_gb_per_month=100.0,
    )
    with pytest.raises(NotSupportedError) as exc_info:
        await aws_provider._price_network_egress(spec)

    err = exc_info.value
    resp = err.to_response()
    assert resp["error"] == "not_supported"
    assert "vpn" in resp["reason"]


# --- (e) AWS Trust metadata ---


async def test_aws_egress_trust_metadata(aws_provider: AWSProvider):
    """Internet egress result has cache_age_seconds=0 and source_url set."""
    spec = NetworkPricingSpec(
        provider="aws",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="us-east-1",
        destination_type="internet",
        data_gb_per_month=100.0,
    )
    result = await aws_provider.get_price(spec)

    p = result.public_prices[0]
    assert p.cache_age_seconds == 0
    assert p.source_url is not None and "aws.amazon.com" in p.source_url
    assert p.fetched_at is not None


# --- (f) AWS Spec validation ---


async def test_aws_cross_region_without_destination_uses_continent(aws_provider: AWSProvider):
    """cross_region without destination_region still returns a result using source continent."""
    spec = NetworkPricingSpec(
        provider="aws",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="us-east-1",
        destination_type="cross_region",
        destination_region="",
        data_gb_per_month=100.0,
    )
    result = await aws_provider.get_price(spec)

    # Should not raise — defaults to same-continent rate
    assert len(result.public_prices) == 1
    p = result.public_prices[0]
    assert float(p.price_per_unit) >= 0.0


async def test_aws_egress_zero_gb_returns_rate_no_error(aws_provider: AWSProvider):
    """data_gb_per_month=0 returns a price with $0.00 total — no exception."""
    spec = NetworkPricingSpec(
        provider="aws",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="us-east-1",
        destination_type="internet",
        data_gb_per_month=0.0,
    )
    result = await aws_provider.get_price(spec)

    assert len(result.public_prices) == 1
    # blended rate is 0 for 0 GB (no chargeable data)
    assert float(result.breakdown.get("total_cost", 0)) == pytest.approx(0.0)


# ===========================================================================
# GCP Tests
# ===========================================================================


# --- (a) GCP Internet egress tier boundary tests ---


async def test_gcp_internet_egress_50gb_first_tier(gcp_provider: GCPProvider):
    """50 GB in 'americas' at $0.080/GB = $4.00 (no free tier for GCP internet egress)."""
    with patch.object(
        gcp_provider, "_fetch_internet_egress_rate", AsyncMock(return_value=Decimal("0.080"))
    ):
        spec = NetworkPricingSpec(
            provider="gcp",
            domain=PricingDomain.NETWORK,
            service="egress",
            source_region="us-central1",
            destination_type="internet",
            data_gb_per_month=50.0,
        )
        result = await gcp_provider.get_price(spec)

    breakdown = result.breakdown
    # 50 GB at $0.080 (no free tier)
    assert float(breakdown["total_cost"]) == pytest.approx(4.0, rel=1e-4)


async def test_gcp_internet_egress_500gb_first_tier(gcp_provider: GCPProvider):
    """500 GB at $0.080/GB = $40.00 (all within first tier, threshold at 1 TB)."""
    with patch.object(
        gcp_provider, "_fetch_internet_egress_rate", AsyncMock(return_value=Decimal("0.080"))
    ):
        spec = NetworkPricingSpec(
            provider="gcp",
            domain=PricingDomain.NETWORK,
            service="egress",
            source_region="us-central1",
            destination_type="internet",
            data_gb_per_month=500.0,
        )
        result = await gcp_provider.get_price(spec)

    breakdown = result.breakdown
    assert float(breakdown["total_cost"]) == pytest.approx(40.0, rel=1e-4)


async def test_gcp_internet_egress_5000gb_crosses_tiers(gcp_provider: GCPProvider):
    """5000 GB crosses the 1 TB tier boundary (1024 GB).

    Tiers (americas):
      - 0–1024 GB: $0.080/GB
      - 1024–10240 GB: $0.065/GB
    5000 GB: 1024 × $0.080 + 3976 × $0.065 = $81.92 + $258.44 = $340.36
    """
    with patch.object(
        gcp_provider, "_fetch_internet_egress_rate", AsyncMock(return_value=Decimal("0.080"))
    ):
        spec = NetworkPricingSpec(
            provider="gcp",
            domain=PricingDomain.NETWORK,
            service="egress",
            source_region="us-central1",
            destination_type="internet",
            data_gb_per_month=5000.0,
        )
        result = await gcp_provider.get_price(spec)

    breakdown = result.breakdown
    expected = 1024 * 0.080 + 3976 * 0.065
    assert float(breakdown["total_cost"]) == pytest.approx(expected, rel=1e-4)
    tiers = breakdown["tiers"]
    assert len(tiers) == 2  # both tiers consumed


# --- (b) GCP Cross-region egress ---


async def test_gcp_cross_region_us_to_europe(gcp_provider: GCPProvider):
    """us-central1 → europe-west1: different continents → cross-continent rate $0.08/GB."""
    spec = NetworkPricingSpec(
        provider="gcp",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="us-central1",
        destination_type="cross_region",
        destination_region="europe-west1",
        data_gb_per_month=100.0,
    )
    result = await gcp_provider.get_price(spec)

    assert len(result.public_prices) == 1
    p = result.public_prices[0]
    assert p.service == "egress"
    # cross-continent rate = $0.08/GB
    assert float(p.price_per_unit) == pytest.approx(0.08, rel=1e-4)


# --- (c) GCP Intra-region (same zone = free) ---


async def test_gcp_same_zone_is_free(gcp_provider: GCPProvider):
    """Traffic within the same GCP zone is free ($0.00/GB)."""
    spec = NetworkPricingSpec(
        provider="gcp",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="us-central1",
        destination_type="same_zone",
        data_gb_per_month=500.0,
    )
    result = await gcp_provider.get_price(spec)

    assert len(result.public_prices) == 1
    p = result.public_prices[0]
    assert float(p.price_per_unit) == pytest.approx(0.0)
    breakdown = result.breakdown
    assert breakdown["total_cost"] == "0.0000"


# --- (d) GCP Unknown destination_type → NotSupportedError ---


async def test_gcp_unknown_destination_type_raises(gcp_provider: GCPProvider):
    """Unknown destination_type raises NotSupportedError (structured error)."""
    spec = NetworkPricingSpec(
        provider="gcp",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="us-central1",
        destination_type="cross_contient",  # intentional typo
        data_gb_per_month=100.0,
    )
    with pytest.raises(NotSupportedError) as exc_info:
        await gcp_provider._price_network_egress(spec)

    err = exc_info.value
    resp = err.to_response()
    assert resp["error"] == "not_supported"
    assert "cross_contient" in resp["reason"]


# --- (e) GCP Trust metadata ---


async def test_gcp_egress_trust_metadata(gcp_provider: GCPProvider):
    """Internet egress result has cache_age_seconds=0 and source_url set."""
    with patch.object(
        gcp_provider, "_fetch_internet_egress_rate", AsyncMock(return_value=Decimal("0.080"))
    ):
        spec = NetworkPricingSpec(
            provider="gcp",
            domain=PricingDomain.NETWORK,
            service="egress",
            source_region="us-central1",
            destination_type="internet",
            data_gb_per_month=100.0,
        )
        result = await gcp_provider.get_price(spec)

    p = result.public_prices[0]
    assert p.cache_age_seconds == 0
    assert p.source_url is not None and "cloud.google.com" in p.source_url
    assert p.fetched_at is not None


# --- (f) GCP Spec validation ---


async def test_gcp_cross_region_without_dest_uses_same_continent(gcp_provider: GCPProvider):
    """cross_region without destination_region uses same-continent rate ($0.01/GB)."""
    spec = NetworkPricingSpec(
        provider="gcp",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="us-central1",
        destination_type="cross_region",
        destination_region="",
        data_gb_per_month=100.0,
    )
    result = await gcp_provider.get_price(spec)

    assert len(result.public_prices) == 1
    p = result.public_prices[0]
    # Same continent (both americas) = $0.01/GB
    assert float(p.price_per_unit) == pytest.approx(0.01, rel=1e-4)


async def test_gcp_egress_zero_gb_no_error(gcp_provider: GCPProvider):
    """data_gb_per_month=0 returns a price with $0.00 total — no exception."""
    with patch.object(
        gcp_provider, "_fetch_internet_egress_rate", AsyncMock(return_value=Decimal("0.080"))
    ):
        spec = NetworkPricingSpec(
            provider="gcp",
            domain=PricingDomain.NETWORK,
            service="egress",
            source_region="us-central1",
            destination_type="internet",
            data_gb_per_month=0.0,
        )
        result = await gcp_provider.get_price(spec)

    assert len(result.public_prices) == 1
    assert float(result.breakdown.get("total_cost", 0)) == pytest.approx(0.0)


# ===========================================================================
# Azure Tests
# ===========================================================================


# --- (a) Azure Internet egress tier boundary tests ---


async def test_azure_internet_egress_50gb_crosses_free_tier(azure_provider: AzureProvider):
    """50 GB: first 5 GB free, then 45 GB at $0.087 = $3.915."""
    spec = NetworkPricingSpec(
        provider="azure",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="eastus",
        destination_type="internet",
        data_gb_per_month=50.0,
    )
    with patch("httpx.get", return_value=_make_httpx_mock(_EMPTY_AZURE_API)):
        result = await azure_provider.get_price(spec)

    breakdown = result.breakdown
    # 5 GB free, 45 GB at $0.087 = $3.915
    assert float(breakdown["total_cost"]) == pytest.approx(45 * 0.087, rel=1e-4)


async def test_azure_internet_egress_500gb_first_tier(azure_provider: AzureProvider):
    """500 GB: 5 GB free, 495 GB at $0.087/GB = $43.065."""
    spec = NetworkPricingSpec(
        provider="azure",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="eastus",
        destination_type="internet",
        data_gb_per_month=500.0,
    )
    with patch("httpx.get", return_value=_make_httpx_mock(_EMPTY_AZURE_API)):
        result = await azure_provider.get_price(spec)

    breakdown = result.breakdown
    # 5 free + 495 at $0.087
    expected = 495 * 0.087
    assert float(breakdown["total_cost"]) == pytest.approx(expected, rel=1e-4)


async def test_azure_internet_egress_5gb_all_free(azure_provider: AzureProvider):
    """Exactly 5 GB is entirely in the free tier: $0 total."""
    spec = NetworkPricingSpec(
        provider="azure",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="eastus",
        destination_type="internet",
        data_gb_per_month=5.0,
    )
    with patch("httpx.get", return_value=_make_httpx_mock(_EMPTY_AZURE_API)):
        result = await azure_provider.get_price(spec)

    assert result.breakdown["total_cost"] == "0.0000"


async def test_azure_internet_egress_5000gb_crosses_tiers(azure_provider: AzureProvider):
    """5000 GB: 5 GB free, 4995 GB at $0.087/GB = $434.565 (all in first paid tier)."""
    spec = NetworkPricingSpec(
        provider="azure",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="eastus",
        destination_type="internet",
        data_gb_per_month=5000.0,
    )
    with patch("httpx.get", return_value=_make_httpx_mock(_EMPTY_AZURE_API)):
        result = await azure_provider.get_price(spec)

    breakdown = result.breakdown
    # 5 GB free, 4995 GB × $0.087 = $434.565
    expected = 4995 * 0.087
    assert float(breakdown["total_cost"]) == pytest.approx(expected, rel=1e-4)
    tiers = breakdown["tiers"]
    assert tiers[0]["rate"] == "0.000"  # free tier first


# --- (b) Azure Cross-region egress ---


async def test_azure_cross_region_eastus_to_westeurope(azure_provider: AzureProvider):
    """eastus → westeurope: both zone1 → flat cross-region rate $0.02/GB."""
    spec = NetworkPricingSpec(
        provider="azure",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="eastus",
        destination_type="cross_region",
        destination_region="westeurope",
        data_gb_per_month=500.0,
    )
    with patch("httpx.get", return_value=_make_httpx_mock(_EMPTY_AZURE_API)):
        result = await azure_provider.get_price(spec)

    assert len(result.public_prices) == 1
    p = result.public_prices[0]
    assert p.service == "egress"
    assert float(p.price_per_unit) == pytest.approx(0.02, rel=1e-4)


# --- (d) Azure Unknown destination_type → NotSupportedError ---


async def test_azure_unknown_destination_type_raises(azure_provider: AzureProvider):
    """Unknown destination_type raises NotSupportedError (structured error)."""
    spec = NetworkPricingSpec(
        provider="azure",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="eastus",
        destination_type="peering",
        data_gb_per_month=100.0,
    )
    with pytest.raises(NotSupportedError) as exc_info:
        await azure_provider._price_network_egress(spec)

    err = exc_info.value
    resp = err.to_response()
    assert resp["error"] == "not_supported"
    assert "peering" in resp["reason"]


# --- (e) Azure Trust metadata ---


async def test_azure_egress_trust_metadata(azure_provider: AzureProvider):
    """Internet egress result has cache_age_seconds=0 and source_url set."""
    spec = NetworkPricingSpec(
        provider="azure",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="eastus",
        destination_type="internet",
        data_gb_per_month=100.0,
    )
    with patch("httpx.get", return_value=_make_httpx_mock(_EMPTY_AZURE_API)):
        result = await azure_provider.get_price(spec)

    p = result.public_prices[0]
    assert p.cache_age_seconds == 0
    assert p.source_url is not None and "azure.microsoft.com" in p.source_url
    assert p.fetched_at is not None


# --- (e) Azure swedencentral zone fix ---


async def test_azure_swedencentral_is_zone1(azure_provider: AzureProvider):
    """swedencentral is European and must use zone1 ($0.087/GB), not zone2 ($0.120/GB)."""
    from opencloudcosts.providers.azure import _AZURE_EGRESS_ZONE

    zone = _AZURE_EGRESS_ZONE.get("swedencentral", "zone1")
    assert zone == "zone1", f"swedencentral should be zone1, got {zone}"

    spec = NetworkPricingSpec(
        provider="azure",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="swedencentral",
        destination_type="internet",
        data_gb_per_month=500.0,
    )
    with patch("httpx.get", return_value=_make_httpx_mock(_EMPTY_AZURE_API)):
        result = await azure_provider.get_price(spec)

    # zone1: 5 GB free, 495 GB at $0.087 (not $0.120)
    assert float(result.breakdown["total_cost"]) == pytest.approx(495 * 0.087, rel=1e-4)


# --- (f) Azure Spec validation ---


async def test_azure_cross_region_without_destination_uses_zone_rate(azure_provider: AzureProvider):
    """cross_region without destination_region uses zone1 cross-region rate ($0.02/GB)."""
    spec = NetworkPricingSpec(
        provider="azure",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="eastus",
        destination_type="cross_region",
        destination_region="",
        data_gb_per_month=100.0,
    )
    with patch("httpx.get", return_value=_make_httpx_mock(_EMPTY_AZURE_API)):
        result = await azure_provider.get_price(spec)

    assert len(result.public_prices) == 1
    p = result.public_prices[0]
    assert float(p.price_per_unit) == pytest.approx(0.02, rel=1e-4)


async def test_azure_egress_zero_gb_no_error(azure_provider: AzureProvider):
    """data_gb_per_month=0 returns a price with $0.00 total — no exception."""
    spec = NetworkPricingSpec(
        provider="azure",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="eastus",
        destination_type="internet",
        data_gb_per_month=0.0,
    )
    with patch("httpx.get", return_value=_make_httpx_mock(_EMPTY_AZURE_API)):
        result = await azure_provider.get_price(spec)

    assert len(result.public_prices) == 1
    assert float(result.breakdown.get("total_cost", 0)) == pytest.approx(0.0)


# --- (f) Azure service field consistency ---


async def test_azure_network_egress_service_field_is_egress(azure_provider: AzureProvider):
    """Both domain=network/egress and domain=inter_region_egress must return service='egress'."""
    from opencloudcosts.models import EgressPricingSpec

    # Path 1: network/egress domain
    net_spec = NetworkPricingSpec(
        provider="azure",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="eastus",
        destination_type="internet",
        data_gb_per_month=100.0,
    )
    with patch("httpx.get", return_value=_make_httpx_mock(_EMPTY_AZURE_API)):
        net_result = await azure_provider.get_price(net_spec)
    assert net_result.public_prices[0].service == "egress"

    # Path 2: inter_region_egress domain
    egress_spec = EgressPricingSpec(
        provider="azure",
        domain=PricingDomain.INTER_REGION_EGRESS,
        source_region="eastus",
        dest_region="westeurope",
        data_gb=100.0,
    )
    with patch("httpx.get", return_value=_make_httpx_mock(_EMPTY_AZURE_API)):
        egress_result = await azure_provider.get_price(egress_spec)
    assert egress_result.public_prices[0].service == "egress"
