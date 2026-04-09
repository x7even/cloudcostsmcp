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


# ------------------------------------------------------------------
# Spot price history tests (T30)
# ------------------------------------------------------------------

_SPOT_HISTORY_MULTI_AZ = {
    "SpotPriceHistory": [
        {"AvailabilityZone": "us-east-1a", "SpotPrice": "0.039", "InstanceType": "m5.xlarge"},
        {"AvailabilityZone": "us-east-1a", "SpotPrice": "0.040", "InstanceType": "m5.xlarge"},
        {"AvailabilityZone": "us-east-1b", "SpotPrice": "0.038", "InstanceType": "m5.xlarge"},
        {"AvailabilityZone": "us-east-1b", "SpotPrice": "0.037", "InstanceType": "m5.xlarge"},
    ]
}


async def test_get_spot_history_returns_stats(aws_provider: AWSProvider):
    mock_ec2 = MagicMock()
    mock_ec2.describe_spot_price_history.return_value = _SPOT_HISTORY_MULTI_AZ
    with patch("boto3.client", return_value=mock_ec2):
        result = await aws_provider._get_spot_history("m5.xlarge", "us-east-1")
    assert "stability" in result
    assert "by_availability_zone" in result
    assert "us-east-1a" in result["by_availability_zone"]
    assert "us-east-1b" in result["by_availability_zone"]
    assert result["overall"]["sample_count"] == 4


async def test_get_spot_history_stable(aws_provider: AWSProvider):
    # All same price → stability = "stable"
    mock_ec2 = MagicMock()
    mock_ec2.describe_spot_price_history.return_value = {
        "SpotPriceHistory": [
            {"AvailabilityZone": "us-east-1a", "SpotPrice": "0.039"},
            {"AvailabilityZone": "us-east-1a", "SpotPrice": "0.039"},
        ]
    }
    with patch("boto3.client", return_value=mock_ec2):
        result = await aws_provider._get_spot_history("m5.xlarge", "us-east-1")
    assert result["stability"] == "stable"


async def test_get_spot_history_no_credentials(aws_provider: AWSProvider):
    import botocore.exceptions
    with patch("boto3.client", side_effect=botocore.exceptions.NoCredentialsError()):
        with pytest.raises(ValueError, match="requires AWS credentials"):
            await aws_provider._get_spot_history("m5.xlarge", "us-east-1")


async def test_get_spot_history_empty_returns_empty_dict(aws_provider: AWSProvider):
    mock_ec2 = MagicMock()
    mock_ec2.describe_spot_price_history.return_value = {"SpotPriceHistory": []}
    with patch("boto3.client", return_value=mock_ec2):
        result = await aws_provider._get_spot_history("m5.xlarge", "us-east-1")
    assert result == {}


# ------------------------------------------------------------------
# Partial Upfront and All Upfront Reserved Instance tests
# ------------------------------------------------------------------

_PARTIAL_UPFRONT_ITEM = {
    "product": _M5_XLARGE_PRICE_ITEM["product"],
    "terms": {
        "Reserved": {
            "KEY.PARTIAL": {
                "priceDimensions": {
                    "dim1": {
                        "unit": "Hrs",
                        "pricePerUnit": {"USD": "0.0530000000"},
                        "description": "$0.053 per Reserved Linux m5.xlarge Instance Hour",
                    },
                    "dim2": {
                        "unit": "Quantity",
                        "pricePerUnit": {"USD": "280.0000000000"},
                        "description": "Upfront Fee",
                    },
                },
                "termAttributes": {
                    "LeaseContractLength": "1yr",
                    "PurchaseOption": "Partial Upfront",
                    "OfferingClass": "standard",
                },
            }
        }
    },
}

_ALL_UPFRONT_ITEM = {
    "product": _M5_XLARGE_PRICE_ITEM["product"],
    "terms": {
        "Reserved": {
            "KEY.ALL": {
                "priceDimensions": {
                    "dim1": {
                        "unit": "Hrs",
                        "pricePerUnit": {"USD": "0.0000000000"},
                        "description": "$0.00 per Reserved Linux m5.xlarge Instance Hour",
                    },
                    "dim2": {
                        "unit": "Quantity",
                        "pricePerUnit": {"USD": "560.0000000000"},
                        "description": "Upfront Fee",
                    },
                },
                "termAttributes": {
                    "LeaseContractLength": "1yr",
                    "PurchaseOption": "All Upfront",
                    "OfferingClass": "standard",
                },
            }
        }
    },
}


async def test_reserved_1yr_partial_upfront(aws_provider: AWSProvider):
    with patch.object(aws_provider, "_get_products", return_value=[_PARTIAL_UPFRONT_ITEM]):
        prices = await aws_provider.get_compute_price(
            "m5.xlarge", "us-east-1", term=PricingTerm.RESERVED_1YR_PARTIAL
        )
    assert len(prices) == 1
    p = prices[0]
    assert p.pricing_term == PricingTerm.RESERVED_1YR_PARTIAL
    # Effective hourly: $0.053/hr + $280/8760 ≈ $0.0850/hr — must be > raw $0.053
    assert p.price_per_unit > Decimal("0.05")
    # upfront_cost stored in attributes for transparency
    assert "upfront_cost" in p.attributes
    assert Decimal(p.attributes["upfront_cost"]) == Decimal("280.0000000000")


async def test_reserved_1yr_all_upfront_normalised(aws_provider: AWSProvider):
    with patch.object(aws_provider, "_get_products", return_value=[_ALL_UPFRONT_ITEM]):
        prices = await aws_provider.get_compute_price(
            "m5.xlarge", "us-east-1", term=PricingTerm.RESERVED_1YR_ALL
        )
    assert len(prices) == 1
    p = prices[0]
    assert p.pricing_term == PricingTerm.RESERVED_1YR_ALL
    # $560 / 8760 ≈ $0.0639/hr  (not $0)
    expected = Decimal("560") / Decimal("8760")
    assert abs(p.price_per_unit - expected) < Decimal("0.000001")
    assert "upfront_cost" in p.attributes
    assert Decimal(p.attributes["upfront_cost"]) == Decimal("560.0000000000")


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


# ------------------------------------------------------------------
# Generic service pricing
# ------------------------------------------------------------------

_CLOUDWATCH_PRICE_ITEM = {
    "product": {
        "sku": "CW123456",
        "productFamily": "Amazon CloudWatch Metrics",
        "attributes": {
            "group": "Metric",
            "groupDescription": "Custom and cross-account metrics",
            "location": "US East (N. Virginia)",
        },
    },
    "terms": {
        "OnDemand": {
            "CW123456.JRTCKXETXF": {
                "priceDimensions": {
                    "CW123456.JRTCKXETXF.6YS6EN2CT7": {
                        "unit": "Metrics",
                        "pricePerUnit": {"USD": "0.3000000000"},
                        "description": "$0.30 per metric per month",
                    }
                },
                "termAttributes": {},
            }
        }
    },
}


async def test_get_service_price_cloudwatch(aws_provider: AWSProvider):
    """get_service_price resolves 'cloudwatch' alias and returns normalized prices."""
    with patch.object(aws_provider, "_get_products", return_value=[_CLOUDWATCH_PRICE_ITEM]):
        prices = await aws_provider.get_service_price(
            "cloudwatch", "us-east-1", {"group": "Metric"}
        )

    assert len(prices) == 1
    p = prices[0]
    assert p.price_per_unit == Decimal("0.3000000000")
    # description should fall back to groupDescription or group
    assert p.description


async def test_get_service_price_alias_resolution(aws_provider: AWSProvider):
    """Alias 'cloudwatch' should resolve to 'AmazonCloudWatch' before calling _get_products."""
    captured_args = {}

    async def mock_get_products(service_code, filters, max_results=100):
        captured_args["service_code"] = service_code
        return [_CLOUDWATCH_PRICE_ITEM]

    with patch.object(aws_provider, "_get_products", side_effect=mock_get_products):
        await aws_provider.get_service_price("cloudwatch", "us-east-1", {})

    assert captured_args["service_code"] == "AmazonCloudWatch"


async def test_get_service_price_passthrough_unknown_alias(aws_provider: AWSProvider):
    """Unknown service codes should be passed through as-is."""
    captured_args = {}

    async def mock_get_products(service_code, filters, max_results=100):
        captured_args["service_code"] = service_code
        return []

    with patch.object(aws_provider, "_get_products", side_effect=mock_get_products):
        await aws_provider.get_service_price("MyCustomServiceCode", "us-east-1", {})

    assert captured_args["service_code"] == "MyCustomServiceCode"


async def test_get_service_price_generic_attributes(aws_provider: AWSProvider):
    """_item_to_price should include all non-noise attributes in the result."""
    with patch.object(aws_provider, "_get_products", return_value=[_CLOUDWATCH_PRICE_ITEM]):
        prices = await aws_provider.get_service_price("cloudwatch", "us-east-1", {})

    assert len(prices) == 1
    attrs = prices[0].attributes
    # group should be present (not a noise key)
    assert "group" in attrs
    # location should be removed (noise key)
    assert "location" not in attrs


async def test_list_services(aws_provider: AWSProvider):
    """list_services should fetch the index and return service codes with aliases."""
    mock_index = {
        "offers": {
            "AmazonEC2": {"currentVersionUrl": "/offers/v1.0/aws/AmazonEC2/current/index.json"},
            "AmazonCloudWatch": {"currentVersionUrl": "..."},
            "AWSLambda": {"currentVersionUrl": "..."},
        }
    }

    import httpx

    mock_response = MagicMock()
    mock_response.raise_for_status = MagicMock()
    mock_response.json.return_value = mock_index

    with patch("httpx.get", return_value=mock_response):
        services = await aws_provider.list_services()

    # Should return list of service entries
    assert isinstance(services, list)
    assert len(services) > 0
    # Check that AmazonEC2 appears
    codes = [s["service_code"] for s in services]
    assert "AmazonEC2" in codes
    # Check that aliases are populated for known services
    ec2_entry = next(s for s in services if s["service_code"] == "AmazonEC2")
    assert "aliases" in ec2_entry
    assert "ec2" in ec2_entry["aliases"]


async def test_search_pricing_generic_service(aws_provider: AWSProvider):
    """search_pricing with service='cloudwatch' should search via get_service_price."""
    with patch.object(
        aws_provider, "_get_products", return_value=[_CLOUDWATCH_PRICE_ITEM]
    ):
        results = await aws_provider.search_pricing("metric", service_code="AmazonCloudWatch")

    assert isinstance(results, list)
    assert len(results) >= 1


# Minimal EBS gp3 price item matching AWS bulk pricing JSON shape.
# productFamily is a TOP-LEVEL product field, NOT inside attributes.
_GP3_PRICE_ITEM = {
    "product": {
        "sku": "ABC123GP3",
        "productFamily": "Storage",
        "attributes": {
            "volumeType": "General Purpose",
            "volumeApiName": "gp3",
            "location": "US East (N. Virginia)",
            "storageMedia": "SSD-backed",
            "usagetype": "EBS:VolumeUsage.gp3",
        },
    },
    "terms": {
        "OnDemand": {
            "ABC123GP3.JRTCKXETXF": {
                "priceDimensions": {
                    "ABC123GP3.JRTCKXETXF.6YS6EN2CT7": {
                        "unit": "GB-Mo",
                        "pricePerUnit": {"USD": "0.0800000000"},
                        "description": "$0.08 per GB-month of General Purpose (gp3) provisioned storage",
                    }
                },
                "termAttributes": {},
            }
        }
    },
}


async def test_get_storage_price_gp3(aws_provider: AWSProvider):
    """get_storage_price gp3 should return results.

    Regression test: productFamily is a top-level product field in AWS bulk
    pricing JSON, not inside attributes. _get_products_bulk previously only
    checked attrs, causing productFamily='Storage' filter to always fail.
    """
    with patch.object(aws_provider, "_get_products", return_value=[_GP3_PRICE_ITEM]):
        prices = await aws_provider.get_storage_price("gp3", "us-east-1")

    assert len(prices) == 1
    p = prices[0]
    assert p.provider == "aws"
    assert p.region == "us-east-1"
    assert float(p.price_per_unit) == pytest.approx(0.08)
