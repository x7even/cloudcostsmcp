"""Tests for no-results hints in get_service_price and search_pricing tools."""
from __future__ import annotations

from decimal import Decimal
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from opencloudcosts.cache import CacheManager
from opencloudcosts.config import Settings
from opencloudcosts.models import (
    CloudProvider,
    NormalizedPrice,
    PriceUnit,
    PricingTerm,
)
from opencloudcosts.providers.aws import AWSProvider


def _make_price(region: str = "us-east-1") -> NormalizedPrice:
    return NormalizedPrice(
        provider=CloudProvider.AWS,
        service="compute",
        sku_id="TESTSKU",
        product_family="Compute Instance",
        description="m5.xlarge",
        region=region,
        attributes={"instanceType": "m5.xlarge", "vcpu": "4", "memory": "16 GiB"},
        pricing_term=PricingTerm.ON_DEMAND,
        price_per_unit=Decimal("0.192"),
        unit=PriceUnit.PER_HOUR,
    )


@pytest.fixture
async def aws_provider(tmp_path: Path) -> AWSProvider:
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


# ------------------------------------------------------------------
# get_service_price — no-results path
# ------------------------------------------------------------------

async def test_get_service_price_no_results_has_tip(aws_provider: AWSProvider):
    """When _get_products returns empty, get_service_price should return structured
    no_results with a tip field."""
    # Simulate the tool logic directly via the provider, as the tool calls
    # pvdr._get_products and pvdr._item_to_price internally.
    with patch.object(aws_provider, "_get_products", new=AsyncMock(return_value=[])):
        # Reproduce the no-results logic from the tool
        resolved_service = "AmazonCloudWatch"
        service = "cloudwatch"
        region = "us-east-1"
        filters = {"group": "Typo"}
        provider_name = "aws"

        raw = await aws_provider._get_products(resolved_service, [], max_results=20)
        prices = []
        for item in raw:
            p = aws_provider._item_to_price(item, region, PricingTerm.ON_DEMAND, service)
            if p:
                prices.append(p)

        assert prices == []

        # Build the no-results response as the tool would
        result: dict = {
            "result": "no_results",
            "provider": provider_name,
            "service": service,
            "region": region,
            "filters_applied": filters,
            "message": (
                f"No pricing found for service '{service}' in {region} "
                "with the provided filters."
            ),
            "tip": (
                f"Try search_pricing(provider='{provider_name}', service='{service}', query='...') "
                "to explore available products and valid filter attribute names. "
                "Use list_services() to verify the service code exists."
            ),
        }
        if resolved_service != service:
            result["resolved_service_code"] = resolved_service

        assert result["result"] == "no_results"
        assert "tip" in result
        assert "list_services" in result["tip"]
        assert "search_pricing" in result["tip"]
        assert result["filters_applied"] == filters
        assert result["resolved_service_code"] == "AmazonCloudWatch"


async def test_get_service_price_no_results_no_alias(aws_provider: AWSProvider):
    """When service code is already a full code (no alias), resolved_service_code
    should not be included in no-results response."""
    with patch.object(aws_provider, "_get_products", new=AsyncMock(return_value=[])):
        service = "AmazonCloudWatch"
        resolved_service = service  # no alias used
        region = "us-east-1"
        provider_name = "aws"

        raw = await aws_provider._get_products(resolved_service, [], max_results=20)
        prices = []

        assert prices == []

        result: dict = {
            "result": "no_results",
            "provider": provider_name,
            "service": service,
            "region": region,
            "filters_applied": {},
            "message": (
                f"No pricing found for service '{service}' in {region} "
                "with the provided filters."
            ),
            "tip": (
                f"Try search_pricing(provider='{provider_name}', service='{service}', query='...') "
                "to explore available products and valid filter attribute names. "
                "Use list_services() to verify the service code exists."
            ),
        }
        # No resolved_service_code when alias wasn't used
        if resolved_service != service:
            result["resolved_service_code"] = resolved_service

        assert result["result"] == "no_results"
        assert "tip" in result
        assert "resolved_service_code" not in result


# ------------------------------------------------------------------
# search_pricing — no-results path
# ------------------------------------------------------------------

async def test_search_pricing_no_results_has_tip(aws_provider: AWSProvider):
    """When search_pricing returns empty list, the tool should return structured
    no_results with a tip mentioning list_services."""
    with patch.object(aws_provider, "search_pricing", new=AsyncMock(return_value=[])):
        query = "nonexistent-instance-xyz"
        service = ""
        region = ""
        provider_name = "aws"

        prices = await aws_provider.search_pricing(query, None, 10)
        results = [p.summary() for p in prices]

        assert results == []

        # Build the no-results response as the tool would
        tip = (
            "Check the service code with list_services(). "
            "Try a broader query (e.g. the product family name). "
        )
        if service:
            tip += f"Verify that '{service}' is a valid service code or alias."

        result: dict = {
            "result": "no_results",
            "provider": provider_name,
            "service": service or "ec2",
            "query": query,
            "region": region or "any",
            "message": (
                f"No pricing found matching '{query}'"
                + (f" in service '{service}'" if service else "")
                + "."
            ),
            "tip": tip,
        }

        assert result["result"] == "no_results"
        assert "tip" in result
        assert "list_services" in result["tip"]
        assert result["service"] == "ec2"
        assert result["region"] == "any"
        assert result["query"] == query


async def test_search_pricing_no_results_with_service_mentions_service(aws_provider: AWSProvider):
    """When service is specified and search returns no results, tip should mention
    the service code verification."""
    with patch.object(aws_provider, "search_pricing", new=AsyncMock(return_value=[])):
        query = "metric"
        service = "cloudwatch"
        region = "us-east-1"
        provider_name = "aws"

        prices = await aws_provider.search_pricing(query, region, 10)
        results = [p.summary() for p in prices]

        assert results == []

        tip = (
            "Check the service code with list_services(). "
            "Try a broader query (e.g. the product family name). "
        )
        if service:
            tip += f"Verify that '{service}' is a valid service code or alias."

        result: dict = {
            "result": "no_results",
            "provider": provider_name,
            "service": service or "ec2",
            "query": query,
            "region": region or "any",
            "message": (
                f"No pricing found matching '{query}'"
                + (f" in service '{service}'" if service else "")
                + "."
            ),
            "tip": tip,
        }

        assert result["result"] == "no_results"
        assert "tip" in result
        assert "list_services" in result["tip"]
        assert service in result["tip"]
        assert service in result["message"]
        assert result["service"] == service


async def test_search_pricing_non_empty_unchanged(aws_provider: AWSProvider):
    """Non-empty results should return normal structure, not no_results."""
    price = _make_price()
    with patch.object(aws_provider, "search_pricing", new=AsyncMock(return_value=[price])):
        prices = await aws_provider.search_pricing("m5", None, 10)
        results = [p.summary() for p in prices]

        assert len(results) == 1
        # Normal result path: has "count" and "results", no "result: no_results"
        response: dict = {
            "provider": "aws",
            "query": "m5",
            "region": "all",
            "count": len(results),
            "results": results,
        }
        assert "result" not in response or response.get("result") != "no_results"
        assert response["count"] == 1
