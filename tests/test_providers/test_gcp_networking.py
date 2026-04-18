"""Tests for GCP Cloud Load Balancing, Cloud CDN, and Cloud NAT pricing provider methods."""
from __future__ import annotations

from decimal import Decimal
from pathlib import Path
from unittest.mock import AsyncMock, patch

import pytest

from opencloudcosts.cache import CacheManager
from opencloudcosts.config import Settings
from opencloudcosts.providers.gcp import GCPProvider


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _make_sku(
    description: str,
    regions: list[str],
    price_units: str = "0",
    price_nanos: int = 0,
) -> dict:
    """Create a minimal Compute Engine SKU dict with a single-tier price."""
    return {
        "description": description,
        "serviceRegions": regions,
        "category": {"usageType": "OnDemand"},
        "pricingInfo": [
            {
                "pricingExpression": {
                    "tieredRates": [
                        {
                            "startUsageAmount": 0,
                            "unitPrice": {
                                "units": price_units,
                                "nanos": price_nanos,
                            },
                        }
                    ]
                }
            }
        ],
    }


# $0.008/hr – External HTTP(S) forwarding rule
_LB_HTTPS_RULE_SKU = _make_sku(
    "External HTTP(S) Load Balancing Rule",
    ["us-central1", "global"],
    price_units="0",
    price_nanos=8_000_000,  # $0.008
)

# $0.006/hr – TCP Proxy rule
_LB_TCP_RULE_SKU = _make_sku(
    "TCP Proxy Load Balancing Rule",
    ["us-central1", "global"],
    price_units="0",
    price_nanos=6_000_000,  # $0.006
)

# $0.008/GB – data processed
_LB_DATA_SKU = _make_sku(
    "Data processed by External HTTP(S) Load Balancing",
    ["global"],
    price_units="0",
    price_nanos=8_000_000,  # $0.008
)

# $0.02/GB – Cloud CDN cache egress (North America)
_CDN_EGRESS_SKU = _make_sku(
    "Cloud CDN Cache Egress North America",
    ["us-central1", "global"],
    price_units="0",
    price_nanos=20_000_000,  # $0.02
)

# $0.01/GB – Cloud CDN cache fill
_CDN_FILL_SKU = _make_sku(
    "Cloud CDN Cache Fill",
    ["us-central1", "global"],
    price_units="0",
    price_nanos=10_000_000,  # $0.01
)

# $0.044/hr – Cloud NAT gateway
_NAT_GATEWAY_SKU = _make_sku(
    "Cloud NAT Gateway",
    ["us-central1"],
    price_units="0",
    price_nanos=44_000_000,  # $0.044
)

# $0.045/GB – Cloud NAT data processed
_NAT_DATA_SKU = _make_sku(
    "Cloud NAT Data Processed",
    ["us-central1"],
    price_units="0",
    price_nanos=45_000_000,  # $0.045
)

FAKE_LB_SKUS = [_LB_HTTPS_RULE_SKU, _LB_TCP_RULE_SKU, _LB_DATA_SKU]
FAKE_CDN_SKUS = [_CDN_EGRESS_SKU, _CDN_FILL_SKU]
FAKE_NAT_SKUS = [_NAT_GATEWAY_SKU, _NAT_DATA_SKU]
FAKE_ALL_NETWORKING_SKUS = FAKE_LB_SKUS + FAKE_CDN_SKUS + FAKE_NAT_SKUS


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
# Load Balancing tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_lb_rate_from_sku(gcp_provider):
    """SKU with 'External HTTP(S) Load Balancing Rule' → rule rate extracted as $0.008/hr."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=FAKE_LB_SKUS)):
        result = await gcp_provider.get_cloud_lb_price(region="us-central1", lb_type="https")

    assert "rule_rate_per_hr" in result
    assert result["rule_rate_per_hr"] == "$0.008000/hr"
    assert result.get("fallback") is None or result.get("fallback") is False


@pytest.mark.asyncio
async def test_lb_cost_math(gcp_provider):
    """rule_count=3, hours=730, data_gb=100 → verify arithmetic."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=FAKE_LB_SKUS)):
        result = await gcp_provider.get_cloud_lb_price(
            region="us-central1",
            lb_type="https",
            rule_count=3,
            data_gb=100.0,
            hours_per_month=730.0,
        )

    # Rule cost: 3 * $0.008 * 730 = $17.52
    expected_rule = Decimal("3") * Decimal("0.008") * Decimal("730")
    assert result["monthly_rule_cost"] == f"${expected_rule:.2f}"

    # Data cost: 100 * $0.008 = $0.80
    expected_data = Decimal("100") * Decimal("0.008")
    assert result["monthly_data_cost"] == f"${expected_data:.2f}"

    # Total: $17.52 + $0.80 = $18.32
    expected_total = expected_rule + expected_data
    assert result["monthly_total"] == f"${expected_total:.2f}"


@pytest.mark.asyncio
async def test_lb_fallback_rate(gcp_provider):
    """No matching SKUs → fallback $0.008/hr used and result has fallback=True."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=[])):
        result = await gcp_provider.get_cloud_lb_price(region="us-central1", lb_type="https")

    assert result.get("fallback") is True
    assert result["rule_rate_per_hr"] == "$0.008000/hr"


# ---------------------------------------------------------------------------
# Cloud CDN tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_cdn_egress_rate_from_sku(gcp_provider):
    """SKU with 'Cloud CDN Cache Egress' → egress rate extracted as $0.02/GB."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=FAKE_CDN_SKUS)):
        result = await gcp_provider.get_cloud_cdn_price(region="us-central1")

    assert "egress_rate_per_gb" in result
    assert result["egress_rate_per_gb"] == "$0.020000/GB"
    assert result.get("fallback") is None or result.get("fallback") is False


@pytest.mark.asyncio
async def test_cdn_cost_math(gcp_provider):
    """egress_gb=1000, cache_fill_gb=200 → verify costs."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=FAKE_CDN_SKUS)):
        result = await gcp_provider.get_cloud_cdn_price(
            region="us-central1",
            egress_gb=1000.0,
            cache_fill_gb=200.0,
        )

    # Egress: 1000 * $0.02 = $20.00
    expected_egress = Decimal("1000") * Decimal("0.02")
    assert result["monthly_egress_cost"] == f"${expected_egress:.2f}"

    # Fill: 200 * $0.01 = $2.00
    expected_fill = Decimal("200") * Decimal("0.01")
    assert result["monthly_cache_fill_cost"] == f"${expected_fill:.2f}"

    # Total: $20.00 + $2.00 = $22.00
    expected_total = expected_egress + expected_fill
    assert result["monthly_total"] == f"${expected_total:.2f}"


@pytest.mark.asyncio
async def test_cdn_fallback_rate(gcp_provider):
    """No SKUs → fallback rates used and result has fallback=True."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=[])):
        result = await gcp_provider.get_cloud_cdn_price(region="us-central1")

    assert result.get("fallback") is True
    assert result["egress_rate_per_gb"] == "$0.020000/GB"


# ---------------------------------------------------------------------------
# Cloud NAT tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_nat_gateway_rate_from_sku(gcp_provider):
    """SKU with 'Cloud NAT Gateway' → gateway rate extracted as $0.044/hr."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=FAKE_NAT_SKUS)):
        result = await gcp_provider.get_cloud_nat_price(region="us-central1")

    assert "gateway_rate_per_hr" in result
    assert result["gateway_rate_per_hr"] == "$0.044000/hr"
    assert result.get("fallback") is None or result.get("fallback") is False


@pytest.mark.asyncio
async def test_nat_data_rate_from_sku(gcp_provider):
    """SKU with 'Cloud NAT Data Processed' → data rate extracted as $0.045/GB."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=FAKE_NAT_SKUS)):
        result = await gcp_provider.get_cloud_nat_price(region="us-central1")

    assert "data_processed_rate_per_gb" in result
    assert result["data_processed_rate_per_gb"] == "$0.045000/GB"


@pytest.mark.asyncio
async def test_nat_cost_math(gcp_provider):
    """gateway_count=2, data_gb=500, hours=730 → verify arithmetic."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=FAKE_NAT_SKUS)):
        result = await gcp_provider.get_cloud_nat_price(
            region="us-central1",
            gateway_count=2,
            data_gb=500.0,
            hours_per_month=730.0,
        )

    # Gateway cost: 2 * $0.044 * 730 = $64.24
    expected_gateway = Decimal("2") * Decimal("0.044") * Decimal("730")
    assert result["monthly_gateway_cost"] == f"${expected_gateway:.2f}"

    # Data cost: 500 * $0.045 = $22.50
    expected_data = Decimal("500") * Decimal("0.045")
    assert result["monthly_data_cost"] == f"${expected_data:.2f}"

    # Total
    expected_total = expected_gateway + expected_data
    assert result["monthly_total"] == f"${expected_total:.2f}"


@pytest.mark.asyncio
async def test_nat_fallback_rate(gcp_provider):
    """No SKUs → fallback rates used and result has fallback=True."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=[])):
        result = await gcp_provider.get_cloud_nat_price(region="us-central1")

    assert result.get("fallback") is True
    assert result["gateway_rate_per_hr"] == "$0.044000/hr"
    assert result["data_processed_rate_per_gb"] == "$0.045000/GB"
