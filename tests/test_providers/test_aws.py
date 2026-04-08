"""Tests for the AWS provider, mocking _get_products to avoid boto3 paginator complexity."""
from __future__ import annotations

import json
from decimal import Decimal
from pathlib import Path
from unittest.mock import patch, MagicMock

import pytest

from opencloudcosts.cache import CacheManager
from opencloudcosts.config import Settings
from opencloudcosts.models import CloudProvider, PricingTerm
from opencloudcosts.providers.aws import AWSProvider

# Minimal price list item matching what AWS Pricing API returns
_M5_XLARGE_PRICE_ITEM = {
    "product": {
        "sku": "JRTCKXETXF8Z6NMQ",
        "productFamily": "Compute Instance",
        "attributes": {
            "instanceType": "m5.xlarge",
            "vcpu": "4",
            "memory": "16 GiB",
            "operatingSystem": "Linux",
            "tenancy": "Shared",
            "location": "US East (N. Virginia)",
            "preInstalledSw": "NA",
            "capacitystatus": "Used",
            "networkPerformance": "Up to 10 Gigabit",
            "storage": "EBS only",
        },
    },
    "terms": {
        "OnDemand": {
            "JRTCKXETXF8Z6NMQ.JRTCKXETXF": {
                "priceDimensions": {
                    "JRTCKXETXF8Z6NMQ.JRTCKXETXF.6YS6EN2CT7": {
                        "unit": "Hrs",
                        "pricePerUnit": {"USD": "0.1920000000"},
                        "description": "$0.192 per On Demand Linux m5.xlarge Instance Hour",
                    }
                },
                "termAttributes": {},
            }
        }
    },
}


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


async def test_get_compute_price(aws_provider: AWSProvider):
    with patch.object(aws_provider, "_get_products", return_value=[_M5_XLARGE_PRICE_ITEM]):
        prices = await aws_provider.get_compute_price("m5.xlarge", "us-east-1")

    assert len(prices) == 1
    p = prices[0]
    assert p.provider == CloudProvider.AWS
    assert p.pricing_term == PricingTerm.ON_DEMAND
    assert p.price_per_unit == Decimal("0.1920000000")
    assert p.attributes["instanceType"] == "m5.xlarge"
    assert p.attributes["vcpu"] == "4"
    assert p.region == "us-east-1"


async def test_get_compute_price_cached(aws_provider: AWSProvider):
    """Second call should hit cache — _get_products called exactly once."""
    with patch.object(aws_provider, "_get_products", return_value=[_M5_XLARGE_PRICE_ITEM]) as mock:
        await aws_provider.get_compute_price("m5.xlarge", "us-east-1")
        prices = await aws_provider.get_compute_price("m5.xlarge", "us-east-1")

    assert len(prices) == 1
    mock.assert_called_once()  # API called only on first request


async def test_get_compute_price_no_results(aws_provider: AWSProvider):
    with patch.object(aws_provider, "_get_products", return_value=[]):
        prices = await aws_provider.get_compute_price("m99.fake", "us-east-1")
    assert prices == []


async def test_invalid_region(aws_provider: AWSProvider):
    with pytest.raises(ValueError, match="Unknown AWS region"):
        await aws_provider.get_compute_price("m5.xlarge", "invalid-region-99")


async def test_list_regions(aws_provider: AWSProvider):
    regions = await aws_provider.list_regions("compute")
    assert "us-east-1" in regions
    assert "ap-southeast-2" in regions
    assert len(regions) > 20


async def test_check_availability_true(aws_provider: AWSProvider):
    with patch.object(aws_provider, "_get_products", return_value=[_M5_XLARGE_PRICE_ITEM]):
        available = await aws_provider.check_availability("compute", "m5.xlarge", "us-east-1")
    assert available is True


async def test_check_availability_false(aws_provider: AWSProvider):
    with patch.object(aws_provider, "_get_products", return_value=[]):
        available = await aws_provider.check_availability("compute", "m99.fake", "us-east-1")
    assert available is False


async def test_list_instance_types(aws_provider: AWSProvider):
    with patch.object(aws_provider, "_get_products", return_value=[_M5_XLARGE_PRICE_ITEM]):
        instances = await aws_provider.list_instance_types("us-east-1", family="m5")

    assert len(instances) == 1
    assert instances[0].instance_type == "m5.xlarge"
    assert instances[0].vcpu == 4
    assert instances[0].memory_gb == 16.0


# ------------------------------------------------------------------
# Credential-free bulk API fallback
# ------------------------------------------------------------------

# Minimal bulk index JSON matching the public pricing file format
_BULK_INDEX = {
    "products": {
        "JRTCKXETXF8Z6NMQ": _M5_XLARGE_PRICE_ITEM["product"],
    },
    "terms": {
        "OnDemand": {
            "JRTCKXETXF8Z6NMQ": _M5_XLARGE_PRICE_ITEM["terms"]["OnDemand"],
        }
    },
}


async def test_bulk_fallback_on_no_credentials(aws_provider: AWSProvider):
    """When boto3 raises NoCredentialsError, _get_products falls back to httpx bulk API."""
    import botocore.exceptions
    import httpx

    def raise_no_creds(*args, **kwargs):
        raise botocore.exceptions.NoCredentialsError()

    mock_response = MagicMock()
    mock_response.raise_for_status = MagicMock()
    mock_response.json.return_value = _BULK_INDEX

    with patch.object(aws_provider, "_get_products_boto3", side_effect=raise_no_creds):
        with patch("httpx.get", return_value=mock_response):
            prices = await aws_provider.get_compute_price("m5.xlarge", "us-east-1")

    assert len(prices) == 1
    assert prices[0].price_per_unit == Decimal("0.1920000000")


# ------------------------------------------------------------------
# Spot pricing
# ------------------------------------------------------------------

_SPOT_PRICE_HISTORY_RESPONSE = {
    "SpotPriceHistory": [
        {"AvailabilityZone": "us-east-1a", "SpotPrice": "0.0420", "InstanceType": "m5.xlarge", "ProductDescription": "Linux/UNIX"},
        {"AvailabilityZone": "us-east-1b", "SpotPrice": "0.0380", "InstanceType": "m5.xlarge", "ProductDescription": "Linux/UNIX"},
        {"AvailabilityZone": "us-east-1a", "SpotPrice": "0.0390", "InstanceType": "m5.xlarge", "ProductDescription": "Linux/UNIX"},
    ]
}


async def test_get_spot_price_returns_cheapest_az(aws_provider: AWSProvider):
    """Returns cheapest AZ price; allAZPrices contains all AZ entries."""
    mock_ec2 = MagicMock()
    mock_ec2.describe_spot_price_history.return_value = _SPOT_PRICE_HISTORY_RESPONSE

    with patch("boto3.client", return_value=mock_ec2):
        prices = await aws_provider.get_compute_price("m5.xlarge", "us-east-1", term=PricingTerm.SPOT)

    assert len(prices) == 1
    p = prices[0]
    assert p.pricing_term == PricingTerm.SPOT
    # us-east-1b has the only price 0.0380; us-east-1a min is 0.0390
    assert p.price_per_unit == Decimal("0.0380")
    assert p.attributes["availabilityZone"] == "us-east-1b"
    all_az = json.loads(p.attributes["allAZPrices"])
    assert "us-east-1a" in all_az
    assert "us-east-1b" in all_az
    assert all_az["us-east-1b"] == "0.0380"
    assert all_az["us-east-1a"] == "0.0390"
    assert p.provider == CloudProvider.AWS
    assert p.region == "us-east-1"


async def test_get_spot_price_no_credentials_raises_valueerror(aws_provider: AWSProvider):
    """NoCredentialsError from boto3 becomes a clear ValueError."""
    import botocore.exceptions

    def raise_no_creds(*args, **kwargs):
        raise botocore.exceptions.NoCredentialsError()

    with patch("boto3.client", side_effect=raise_no_creds):
        with pytest.raises(ValueError, match="requires AWS credentials"):
            await aws_provider.get_compute_price("m5.xlarge", "us-east-1", term=PricingTerm.SPOT)


async def test_get_spot_price_empty_response(aws_provider: AWSProvider):
    """Empty SpotPriceHistory returns empty list."""
    mock_ec2 = MagicMock()
    mock_ec2.describe_spot_price_history.return_value = {"SpotPriceHistory": []}

    with patch("boto3.client", return_value=mock_ec2):
        prices = await aws_provider.get_compute_price("m5.xlarge", "us-east-1", term=PricingTerm.SPOT)

    assert prices == []


async def test_bulk_fallback_filters_correctly(aws_provider: AWSProvider):
    """Bulk fallback applies TERM_MATCH filters in-memory."""
    import botocore.exceptions

    # Add a second product that should NOT match (different instance type)
    bulk_with_extra = json.loads(json.dumps(_BULK_INDEX))
    bulk_with_extra["products"]["DIFFERENTSKU"] = {
        "sku": "DIFFERENTSKU",
        "productFamily": "Compute Instance",
        "attributes": {
            "instanceType": "c5.xlarge",   # different — should be filtered out
            "vcpu": "4",
            "memory": "8 GiB",
            "operatingSystem": "Linux",
            "tenancy": "Shared",
            "preInstalledSw": "NA",
            "capacitystatus": "Used",
        },
    }

    def raise_no_creds(*args, **kwargs):
        raise botocore.exceptions.NoCredentialsError()

    mock_response = MagicMock()
    mock_response.raise_for_status = MagicMock()
    mock_response.json.return_value = bulk_with_extra

    with patch.object(aws_provider, "_get_products_boto3", side_effect=raise_no_creds):
        with patch("httpx.get", return_value=mock_response):
            prices = await aws_provider.get_compute_price("m5.xlarge", "us-east-1")

    # Should only return m5.xlarge, not c5.xlarge
    assert len(prices) == 1
    assert prices[0].attributes["instanceType"] == "m5.xlarge"
