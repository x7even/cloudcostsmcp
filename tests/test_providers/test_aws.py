"""Tests for the AWS provider, mocking _get_products to avoid boto3 paginator complexity."""

from __future__ import annotations

import json
from decimal import Decimal
from pathlib import Path
from unittest.mock import MagicMock, patch

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
        {
            "AvailabilityZone": "us-east-1a",
            "SpotPrice": "0.0420",
            "InstanceType": "m5.xlarge",
            "ProductDescription": "Linux/UNIX",
        },
        {
            "AvailabilityZone": "us-east-1b",
            "SpotPrice": "0.0380",
            "InstanceType": "m5.xlarge",
            "ProductDescription": "Linux/UNIX",
        },
        {
            "AvailabilityZone": "us-east-1a",
            "SpotPrice": "0.0390",
            "InstanceType": "m5.xlarge",
            "ProductDescription": "Linux/UNIX",
        },
    ]
}


async def test_get_spot_price_returns_cheapest_az(aws_provider: AWSProvider):
    """Returns cheapest AZ price; allAZPrices contains all AZ entries."""
    mock_ec2 = MagicMock()
    mock_ec2.describe_spot_price_history.return_value = _SPOT_PRICE_HISTORY_RESPONSE

    with patch("boto3.client", return_value=mock_ec2):
        prices = await aws_provider.get_compute_price(
            "m5.xlarge", "us-east-1", term=PricingTerm.SPOT
        )

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


async def test_get_spot_price_no_credentials_returns_empty(aws_provider: AWSProvider):
    """NoCredentialsError from boto3 returns empty list gracefully."""
    import botocore.exceptions

    def raise_no_creds(*args, **kwargs):
        raise botocore.exceptions.NoCredentialsError()

    with patch("boto3.client", side_effect=raise_no_creds):
        prices = await aws_provider.get_compute_price(
            "m5.xlarge", "us-east-1", term=PricingTerm.SPOT
        )
        assert prices == []


async def test_get_spot_price_empty_response(aws_provider: AWSProvider):
    """Empty SpotPriceHistory returns empty list."""
    mock_ec2 = MagicMock()
    mock_ec2.describe_spot_price_history.return_value = {"SpotPriceHistory": []}

    with patch("boto3.client", return_value=mock_ec2):
        prices = await aws_provider.get_compute_price(
            "m5.xlarge", "us-east-1", term=PricingTerm.SPOT
        )

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
            "instanceType": "c5.xlarge",  # different — should be filtered out
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
    with patch.object(aws_provider, "_get_products", return_value=[_CLOUDWATCH_PRICE_ITEM]):
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

    def _gp3_side_effect(service: str, filters: list, **kwargs: object) -> list:
        # Only return the base storage item; IOPS/throughput add-on calls return empty
        if any(f.get("Value") == "Storage" for f in filters):
            return [_GP3_PRICE_ITEM]
        return []

    with patch.object(aws_provider, "_get_products", side_effect=_gp3_side_effect):
        prices = await aws_provider.get_storage_price("gp3", "us-east-1")

    assert len(prices) == 1
    p = prices[0]
    assert p.provider == "aws"
    assert p.region == "us-east-1"
    assert float(p.price_per_unit) == pytest.approx(0.08)


# Minimal NAT Gateway price item matching the AWS bulk pricing JSON shape.
# productFamily is TOP-LEVEL (not inside attributes) and servicecode is AmazonEC2.
_NAT_GW_HOURS_ITEM = {
    "product": {
        "sku": "M2YSHUBETB3JX4M4",
        "productFamily": "NAT Gateway",
        "attributes": {
            "servicecode": "AmazonEC2",
            "location": "US East (N. Virginia)",
            "locationType": "AWS Region",
            "group": "NGW:NatGateway",
            "groupDescription": "Hourly charge for NAT Gateways",
            "usagetype": "NatGateway-Hours",
            "operation": "NatGateway",
            "regionCode": "us-east-1",
            "servicename": "Amazon Elastic Compute Cloud",
        },
    },
    "terms": {
        "OnDemand": {
            "M2YSHUBETB3JX4M4.JRTCKXETXF": {
                "priceDimensions": {
                    "M2YSHUBETB3JX4M4.JRTCKXETXF.6YS6EN2CT7": {
                        "unit": "Hrs",
                        "pricePerUnit": {"USD": "0.0450000000"},
                        "description": "$0.045 per NAT Gateway hour",
                    }
                },
                "termAttributes": {},
            }
        }
    },
}


async def test_get_service_price_nat_gateway(aws_provider: AWSProvider):
    """get_service_price with productFamily='NAT Gateway' should return results.

    Regression test: NAT Gateway pricing lives under AmazonEC2 (not AmazonVPC).
    The productFamily filter must match the top-level field, not just attributes.
    """
    with patch.object(aws_provider, "_get_products", return_value=[_NAT_GW_HOURS_ITEM]):
        prices = await aws_provider.get_service_price(
            "AmazonEC2", "us-east-1", {"productFamily": "NAT Gateway"}
        )

    assert len(prices) == 1
    p = prices[0]
    assert p.provider == "aws"
    assert p.region == "us-east-1"
    assert float(p.price_per_unit) == pytest.approx(0.045)
    assert p.product_family == "NAT Gateway"


async def test_search_pricing_nat_gateway(aws_provider: AWSProvider):
    """search_pricing('NAT Gateway') should find NAT Gateway results.

    Regression test: search_pricing previously routed 'NAT Gateway' through
    the EC2 compute instance path (looking for instanceType matches), which
    never matched NAT Gateway products. The query must use a productFamily filter.
    """
    with patch.object(aws_provider, "_get_products", return_value=[_NAT_GW_HOURS_ITEM]):
        prices = await aws_provider.search_pricing("NAT Gateway", region="us-east-1")

    assert len(prices) == 1
    p = prices[0]
    assert p.product_family == "NAT Gateway"
    assert float(p.price_per_unit) == pytest.approx(0.045)


async def test_nat_gateway_alias_resolves_to_ec2():
    """The 'nat_gateway' alias must resolve to AmazonEC2, not AmazonVPC."""
    from opencloudcosts.providers.aws import _resolve_service_code

    assert _resolve_service_code("nat_gateway") == "AmazonEC2"
    assert _resolve_service_code("natgateway") == "AmazonEC2"


# ---------------------------------------------------------------------------
# Inter-region egress tests
# ---------------------------------------------------------------------------


def test_region_continent_known_prefixes(aws_provider: AWSProvider):
    """_region_continent correctly classifies all known AWS region prefix families."""
    rc = aws_provider._region_continent
    assert rc("us-east-1") == "us"
    assert rc("us-west-2") == "us"
    assert rc("ca-central-1") == "us"
    assert rc("eu-west-1") == "eu"
    assert rc("eu-central-1") == "eu"
    assert rc("ap-southeast-1") == "ap"
    assert rc("ap-northeast-1") == "ap"
    assert rc("cn-north-1") == "ap"
    assert rc("sa-east-1") == "sa"
    assert rc("me-south-1") == "me"
    assert rc("il-central-1") == "me"  # Israel classified with Middle East
    assert rc("af-south-1") == "af"
    assert rc("unknown-region-1") == "us"  # unknown defaults to us


def test_egress_rates_symmetric(aws_provider: AWSProvider):
    """_EGRESS_RATES should have entries for both (A, B) and (B, A) for all pairs."""
    rates = aws_provider._EGRESS_RATES
    for a, b in list(rates.keys()):
        if a != b:
            assert (b, a) in rates, f"Missing reverse entry for ({b}, {a})"


def test_egress_static_fallback_known_route(aws_provider: AWSProvider):
    """Fallback for us-east-1 → eu-west-1 returns the published $0.02/GB rate."""
    results = aws_provider._egress_static_fallback("us-east-1", "eu-west-1")
    assert len(results) == 1
    p = results[0]
    assert float(p.price_per_unit) == pytest.approx(0.02)
    assert p.attributes.get("fallback") == "true"
    assert p.attributes.get("fromRegionCode") == "us-east-1"
    assert p.attributes.get("toRegionCode") == "eu-west-1"


def test_egress_static_fallback_high_rate_route(aws_provider: AWSProvider):
    """Fallback for me-south-1 → eu-west-1 returns the $0.16/GB rate (not $0.09 default)."""
    results = aws_provider._egress_static_fallback("me-south-1", "eu-west-1")
    assert len(results) == 1
    assert float(results[0].price_per_unit) == pytest.approx(0.16)


def test_egress_static_fallback_africa(aws_provider: AWSProvider):
    """Fallback for af-south-1 → us-east-1 returns Africa rates, not US intra-continent."""
    results = aws_provider._egress_static_fallback("af-south-1", "us-east-1")
    assert len(results) == 1
    assert float(results[0].price_per_unit) == pytest.approx(0.16)


def test_egress_static_fallback_no_dest(aws_provider: AWSProvider):
    """Fallback with no dest returns one entry per destination continent."""
    results = aws_provider._egress_static_fallback("us-east-1", None)
    assert len(results) > 1
    to_codes = {r.attributes.get("toRegionCode", "") for r in results}
    assert any("eu" in c for c in to_codes)
    assert any("ap" in c for c in to_codes)


async def test_price_egress_uses_correct_filter(aws_provider: AWSProvider):
    """_price_egress must use 'InterRegion Outbound' not 'AWS Inter-Region Outbound'."""
    from opencloudcosts.models import EgressPricingSpec

    spec = EgressPricingSpec(
        provider="aws",
        region="us-east-1",
        source_region="us-east-1",
        dest_region="eu-west-1",
    )
    captured_filters: list = []

    async def _capture(*args, **kwargs):
        captured_filters.extend(args[1] if len(args) > 1 else [])
        return []  # return empty to trigger fallback

    with patch.object(aws_provider, "_get_products", side_effect=_capture):
        await aws_provider._price_egress(spec)

    transfer_type_filter = next(
        (f for f in captured_filters if f.get("Field") == "transferType"), None
    )
    assert transfer_type_filter is not None
    assert transfer_type_filter["Value"] == "InterRegion Outbound"


async def test_price_egress_falls_back_when_api_empty(aws_provider: AWSProvider):
    """When _get_products returns empty, _price_egress falls back to static rates."""
    from opencloudcosts.models import EgressPricingSpec

    spec = EgressPricingSpec(
        provider="aws",
        region="us-east-1",
        source_region="us-east-1",
        dest_region="eu-west-1",
    )
    with patch.object(aws_provider, "_get_products", return_value=[]):
        prices = await aws_provider._price_egress(spec)

    assert len(prices) == 1
    assert prices[0].attributes.get("fallback") == "true"
    assert float(prices[0].price_per_unit) == pytest.approx(0.02)


# ---------------------------------------------------------------------------
# network/egress (tiered internet egress path)
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_network_egress_internet_returns_breakdown(aws_provider: AWSProvider):
    """get_price with domain=network service=egress returns a PricingResult with breakdown."""
    from opencloudcosts.models import NetworkPricingSpec, PricingDomain

    spec = NetworkPricingSpec(
        provider="aws",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="us-east-1",
        destination_type="internet",
        data_gb_per_month=1000.0,
    )
    result = await aws_provider.get_price(spec)

    assert len(result.public_prices) == 1
    p = result.public_prices[0]
    assert p.provider.value == "aws"
    assert p.service == "egress"
    # Should have breakdown with tier info
    assert "tiers" in result.breakdown
    assert result.breakdown["data_gb"] == pytest.approx(1000.0)
    # 100 GB free, then 900 GB at $0.09 = $81.00
    assert float(result.breakdown["total_cost"]) == pytest.approx(81.0, rel=1e-3)


@pytest.mark.asyncio
async def test_network_egress_cross_region_returns_flat_rate(aws_provider: AWSProvider):
    """Cross-region egress via network/egress uses continent-pair lookup."""
    from opencloudcosts.models import NetworkPricingSpec, PricingDomain

    spec = NetworkPricingSpec(
        provider="aws",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="us-east-1",
        destination_type="cross_region",
        destination_region="eu-west-1",
        data_gb_per_month=1024.0,
    )
    result = await aws_provider.get_price(spec)

    assert len(result.public_prices) == 1
    p = result.public_prices[0]
    assert p.service == "egress"
    # us → eu = $0.02/GB
    assert float(p.price_per_unit) == pytest.approx(0.02, rel=1e-3)


@pytest.mark.asyncio
async def test_network_egress_cross_az(aws_provider: AWSProvider):
    """Cross-AZ egress via network/egress uses $0.01/GB flat rate."""
    from opencloudcosts.models import NetworkPricingSpec, PricingDomain

    spec = NetworkPricingSpec(
        provider="aws",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="us-east-1",
        destination_type="cross_az",
        data_gb_per_month=500.0,
    )
    result = await aws_provider.get_price(spec)

    assert len(result.public_prices) == 1
    p = result.public_prices[0]
    assert float(p.price_per_unit) == pytest.approx(0.01, rel=1e-3)
    assert result.breakdown["data_gb"] == pytest.approx(500.0)


@pytest.mark.asyncio
async def test_network_egress_100gb_all_free(aws_provider: AWSProvider):
    """Exactly 100 GB hits the free tier fully, total cost should be $0."""
    from opencloudcosts.models import NetworkPricingSpec, PricingDomain

    spec = NetworkPricingSpec(
        provider="aws",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="us-east-1",
        destination_type="internet",
        data_gb_per_month=100.0,
    )
    result = await aws_provider.get_price(spec)

    assert result.breakdown["total_cost"] == "0.0000"


@pytest.mark.asyncio
async def test_network_egress_supports_check(aws_provider: AWSProvider):
    """supports() returns True for NETWORK/egress."""
    from opencloudcosts.models import PricingDomain

    assert aws_provider.supports(PricingDomain.NETWORK, "egress")


# ---------------------------------------------------------------------------
# ijson streaming bulk path tests (_get_products_bulk)
# ---------------------------------------------------------------------------

import gzip as _gzip
import json as _json
import time as _time


def _make_bulk_json(products: dict, on_demand: dict | None = None, reserved: dict | None = None) -> bytes:
    """Build a minimal AWS bulk pricing JSON and return it gzip-compressed.

    This matches the format _fetch_bulk_compressed returns, which is what
    _get_products_bulk feeds to its three ijson passes.
    All attribute values must be strings to match the real AWS format.
    """
    data = {
        "formatVersion": "aws_v1",
        "products": products,
        "terms": {
            "OnDemand": on_demand or {},
            "Reserved": reserved or {},
        },
    }
    return _gzip.compress(_json.dumps(data).encode(), compresslevel=1)


# Minimal product + term structures for bulk tests — mirrors _M5_XLARGE_PRICE_ITEM shape
_BULK_SKU = "JRTCKXETXF8Z6NMQ"
_BULK_PRODUCTS = {
    _BULK_SKU: {
        "sku": _BULK_SKU,
        "productFamily": "Compute Instance",
        "attributes": {
            "instanceType": "m5.xlarge",
            "vcpu": "4",
            "memory": "16 GiB",
            "operatingSystem": "Linux",
            "tenancy": "Shared",
            "preInstalledSw": "NA",
            "capacitystatus": "Used",
        },
    }
}
_BULK_ON_DEMAND = {
    _BULK_SKU: {
        _BULK_SKU + ".JRTCKXETXF": {
            "priceDimensions": {
                _BULK_SKU + ".JRTCKXETXF.6YS6EN2CT7": {
                    "unit": "Hrs",
                    "pricePerUnit": {"USD": "0.1920000000"},
                    "description": "$0.192 per On Demand Linux m5.xlarge Instance Hour",
                }
            },
            "termAttributes": {},
        }
    }
}


def test_get_products_bulk_streaming_match(aws_provider: AWSProvider):
    """_get_products_bulk returns matching product with OnDemand terms via ijson streaming."""
    compressed = _make_bulk_json(_BULK_PRODUCTS, _BULK_ON_DEMAND)
    filters = [
        {"Field": "location", "Value": "US East (N. Virginia)"},
        {"Field": "instanceType", "Value": "m5.xlarge"},
        {"Field": "operatingSystem", "Value": "Linux"},
    ]

    with patch("opencloudcosts.providers.aws._fetch_bulk_compressed", return_value=compressed):
        results = aws_provider._get_products_bulk("AmazonEC2", "us-east-1", filters, max_results=10)

    assert len(results) == 1
    item = results[0]
    assert item["product"]["sku"] == _BULK_SKU
    assert item["product"]["attributes"]["instanceType"] == "m5.xlarge"
    # OnDemand terms should be present; the value is the offer-term dict keyed by offer-term code
    assert "OnDemand" in item["terms"]
    on_demand_value = item["terms"]["OnDemand"]
    # The offer-term key has the format SKU.OFFERCODE — we verify the dict is non-empty
    assert len(on_demand_value) > 0
    # The outer offer-term key contains the SKU as a prefix
    first_term_key = next(iter(on_demand_value))
    assert first_term_key.startswith(_BULK_SKU)


def test_get_products_bulk_no_match(aws_provider: AWSProvider):
    """_get_products_bulk returns [] when no product matches the filter."""
    compressed = _make_bulk_json(_BULK_PRODUCTS, _BULK_ON_DEMAND)
    filters = [
        {"Field": "location", "Value": "US East (N. Virginia)"},
        {"Field": "instanceType", "Value": "c99.nonexistent"},
    ]

    with patch("opencloudcosts.providers.aws._fetch_bulk_compressed", return_value=compressed):
        results = aws_provider._get_products_bulk("AmazonEC2", "us-east-1", filters, max_results=10)

    assert results == []


def test_get_products_bulk_max_results_early_exit(aws_provider: AWSProvider):
    """_get_products_bulk stops collecting products once max_results is reached."""
    # Build a bulk JSON with 5 distinct SKUs that all match the filter
    products = {}
    on_demand = {}
    for i in range(5):
        sku = f"SKU{i:04d}"
        products[sku] = {
            "sku": sku,
            "productFamily": "Compute Instance",
            "attributes": {
                "instanceType": f"m5.{i}xlarge",
                "operatingSystem": "Linux",
                "tenancy": "Shared",
                "preInstalledSw": "NA",
                "capacitystatus": "Used",
            },
        }
        on_demand[sku] = {
            sku + ".TERM": {
                "priceDimensions": {
                    sku + ".DIM": {
                        "unit": "Hrs",
                        "pricePerUnit": {"USD": f"0.{i + 1}000000000"},
                        "description": f"${i + 1} per hour",
                    }
                },
                "termAttributes": {},
            }
        }

    compressed = _make_bulk_json(products, on_demand)
    # Only operatingSystem filter — all 5 match
    filters = [
        {"Field": "location", "Value": "US East (N. Virginia)"},
        {"Field": "operatingSystem", "Value": "Linux"},
    ]

    with patch("opencloudcosts.providers.aws._fetch_bulk_compressed", return_value=compressed):
        results = aws_provider._get_products_bulk("AmazonEC2", "us-east-1", filters, max_results=2)

    # Should stop at max_results=2, not return all 5
    assert len(results) == 2


def test_get_products_bulk_productfamily_filter(aws_provider: AWSProvider):
    """productFamily filter works because _get_products_bulk checks product top-level fields."""
    products = {
        "STORAGE_SKU": {
            "sku": "STORAGE_SKU",
            "productFamily": "Storage",
            "attributes": {
                "volumeApiName": "gp3",
                "operatingSystem": "Linux",  # not used for storage, but present
            },
        },
        "COMPUTE_SKU": {
            "sku": "COMPUTE_SKU",
            "productFamily": "Compute Instance",
            "attributes": {
                "instanceType": "m5.xlarge",
                "operatingSystem": "Linux",
            },
        },
    }
    on_demand = {
        "STORAGE_SKU": {
            "STORAGE_SKU.TERM": {
                "priceDimensions": {
                    "STORAGE_SKU.DIM": {
                        "unit": "GB-Mo",
                        "pricePerUnit": {"USD": "0.0800000000"},
                        "description": "$0.08 per GB-month",
                    }
                },
                "termAttributes": {},
            }
        }
    }

    compressed = _make_bulk_json(products, on_demand)
    filters = [
        {"Field": "location", "Value": "US East (N. Virginia)"},
        {"Field": "productFamily", "Value": "Storage"},
    ]

    with patch("opencloudcosts.providers.aws._fetch_bulk_compressed", return_value=compressed):
        results = aws_provider._get_products_bulk("AmazonEC2", "us-east-1", filters, max_results=10)

    assert len(results) == 1
    assert results[0]["product"]["sku"] == "STORAGE_SKU"
    assert results[0]["product"]["productFamily"] == "Storage"


def test_get_products_bulk_fetch_error_returns_empty(aws_provider: AWSProvider):
    """When _fetch_bulk_compressed raises RuntimeError, _get_products_bulk returns []."""
    filters = [{"Field": "location", "Value": "US East (N. Virginia)"}]

    with patch(
        "opencloudcosts.providers.aws._fetch_bulk_compressed",
        side_effect=RuntimeError("HTTP 503 for us-east-1"),
    ):
        results = aws_provider._get_products_bulk("AmazonEC2", "us-east-1", filters, max_results=10)

    assert results == []


def test_get_products_bulk_oserror_returns_empty(aws_provider: AWSProvider):
    """When _fetch_bulk_compressed raises OSError (network error), returns []."""
    filters = [{"Field": "location", "Value": "US East (N. Virginia)"}]

    with patch(
        "opencloudcosts.providers.aws._fetch_bulk_compressed",
        side_effect=OSError("Connection refused"),
    ):
        results = aws_provider._get_products_bulk("AmazonEC2", "us-east-1", filters, max_results=10)

    assert results == []


def test_get_products_bulk_reserved_terms_collected(aws_provider: AWSProvider):
    """_get_products_bulk collects Reserved terms in pass 3."""
    reserved = {
        _BULK_SKU: {
            _BULK_SKU + ".RESERVED": {
                "priceDimensions": {
                    _BULK_SKU + ".R.DIM": {
                        "unit": "Hrs",
                        "pricePerUnit": {"USD": "0.0500000000"},
                        "description": "Reserved hourly",
                    }
                },
                "termAttributes": {
                    "LeaseContractLength": "1yr",
                    "PurchaseOption": "No Upfront",
                    "OfferingClass": "standard",
                },
            }
        }
    }

    compressed = _make_bulk_json(_BULK_PRODUCTS, None, reserved)
    filters = [
        {"Field": "location", "Value": "US East (N. Virginia)"},
        {"Field": "instanceType", "Value": "m5.xlarge"},
    ]

    with patch("opencloudcosts.providers.aws._fetch_bulk_compressed", return_value=compressed):
        results = aws_provider._get_products_bulk("AmazonEC2", "us-east-1", filters, max_results=10)

    assert len(results) == 1
    item = results[0]
    # Pass 2 (OnDemand) returned nothing since on_demand is empty; Pass 3 should get Reserved
    assert "Reserved" in item["terms"]
    reserved_value = item["terms"]["Reserved"]
    assert len(reserved_value) > 0
    # Reserved offer-term key has format SKU.OFFERCODE — verify the dict is non-empty
    # and the key starts with the SKU prefix
    first_reserved_key = next(iter(reserved_value))
    assert first_reserved_key.startswith(_BULK_SKU)


# ---------------------------------------------------------------------------
# Singleflight lock tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_singleflight_serialises_concurrent_downloads(aws_provider: AWSProvider, tmp_path):
    """Concurrent _get_products calls with same (service, region) are serialised by the lock.

    The lock prevents *simultaneous* downloads of the same bulk file. However it does NOT
    deduplicate: N callers still do N downloads (just sequentially, not concurrently).
    This test confirms: (a) lock serialises, (b) bulk download is called once per caller
    (i.e. call_count == N, not 1 — it's a mutex, not a singleflight with shared result).
    """
    import asyncio
    import botocore.exceptions
    from opencloudcosts.providers import aws as aws_module

    # Clear module-level lock cache so this test starts clean
    aws_module._bulk_fetch_locks.clear()

    call_count = 0
    call_times: list[float] = []

    def slow_bulk(service_code, region_code, filters, max_results):
        nonlocal call_count
        call_count += 1
        call_times.append(_time.monotonic())
        _time.sleep(0.05)  # blocking sleep in thread — correct for asyncio.to_thread
        return []

    filters = [
        {"Field": "location", "Value": "US East (N. Virginia)"},
        {"Field": "instanceType", "Value": "m5.xlarge"},
    ]

    def raise_no_creds(*a, **kw):
        raise botocore.exceptions.NoCredentialsError()

    with patch.object(aws_provider, "_get_products_boto3", side_effect=raise_no_creds):
        with patch.object(aws_provider, "_get_products_bulk", side_effect=slow_bulk):
            tasks = [
                aws_provider._get_products("AmazonEC2", filters, 10),
                aws_provider._get_products("AmazonEC2", filters, 10),
            ]
            results = await asyncio.gather(*tasks)

    # Both calls return (empty) results
    assert results[0] == []
    assert results[1] == []

    # Lock serialises: call_count == 2 (no dedup — the lock is a mutex, not a once-cell).
    # If the lock were NOT present, both could run concurrently (still count==2 but overlap);
    # the sequential ordering of call_times proves serialisation.
    assert call_count == 2, (
        f"Expected 2 sequential calls (mutex semantics), got {call_count}. "
        "Design note: the singleflight lock serialises concurrent downloads of the same "
        "bulk file but does NOT deduplicate results — N callers still trigger N downloads."
    )

    # With serialisation the second call starts after the first finishes:
    # gap between start times should be >= sleep duration (0.05 s)
    if len(call_times) == 2:
        gap = call_times[1] - call_times[0]
        assert gap >= 0.04, (
            f"Call start gap {gap:.3f}s < 0.04s — calls appear to have overlapped, "
            "suggesting the singleflight lock is not working."
        )


@pytest.mark.asyncio
async def test_singleflight_different_regions_run_concurrently(aws_provider: AWSProvider):
    """Calls for DIFFERENT (service, region) pairs are NOT blocked by the same lock."""
    import asyncio
    import botocore.exceptions
    from opencloudcosts.providers import aws as aws_module

    aws_module._bulk_fetch_locks.clear()

    started: list[float] = []

    def slow_bulk(service_code, region_code, filters, max_results):
        started.append(_time.monotonic())
        _time.sleep(0.05)
        return []

    def raise_no_creds(*a, **kw):
        raise botocore.exceptions.NoCredentialsError()

    filters_east = [{"Field": "location", "Value": "US East (N. Virginia)"}]
    filters_west = [{"Field": "location", "Value": "US West (Oregon)"}]

    with patch.object(aws_provider, "_get_products_boto3", side_effect=raise_no_creds):
        with patch.object(aws_provider, "_get_products_bulk", side_effect=slow_bulk):
            t0 = _time.monotonic()
            await asyncio.gather(
                aws_provider._get_products("AmazonEC2", filters_east, 10),
                aws_provider._get_products("AmazonEC2", filters_west, 10),
            )
            elapsed = _time.monotonic() - t0

    # Two separate locks — they can run in parallel, so total time should be
    # much closer to 0.05 s (one sleep) than 0.10 s (two sequential sleeps).
    # We allow up to 0.09 s to avoid flakiness on slow CI.
    assert elapsed < 0.09, (
        f"Different-region calls took {elapsed:.3f}s — expected near-concurrent execution "
        f"(~0.05s), but got nearly sequential time, suggesting over-serialisation."
    )


@pytest.mark.asyncio
async def test_singleflight_no_credentials_missing_location_returns_empty(aws_provider: AWSProvider):
    """_get_products returns [] when no credentials and filters have no location field.

    The bulk path requires a location filter to determine the region code.
    Without it, _get_products should bail early and return [].
    """
    import botocore.exceptions

    def raise_no_creds(*a, **kw):
        raise botocore.exceptions.NoCredentialsError()

    # No location field in filters
    filters = [{"Field": "instanceType", "Value": "m5.xlarge"}]

    with patch.object(aws_provider, "_get_products_boto3", side_effect=raise_no_creds):
        result = await aws_provider._get_products("AmazonEC2", filters, 10)

    assert result == []
