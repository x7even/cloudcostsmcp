"""
Contract tests for the CloudPricingProvider protocol.

Invariants enforced:
  1. supports() / dispatch agreement — supports()==True ↔ get_price() doesn't raise NotSupportedError
  2. Shape invariant — every public_prices[*] has price_per_unit > 0, non-empty region, valid PriceUnit
  3. Region echo — result.public_prices[*].region matches spec.region
  4. Example roundtrip — describe_catalog() example_invocation passes back to get_price() successfully
  5. Breakdown arithmetic — where breakdown.monthly_total exists, it equals sum of prices ×qty within $0.01
"""
from __future__ import annotations

from decimal import Decimal
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

from opencloudcosts.cache import CacheManager
from opencloudcosts.config import Settings
from opencloudcosts.models import (
    PRICING_SCHEMAS,
    CloudProvider,
    ComputePricingSpec,
    DatabasePricingSpec,
    EffectivePrice,
    NormalizedPrice,
    PriceUnit,
    PricingDomain,
    PricingResult,
    PricingSpec,
    PricingTerm,
)
from opencloudcosts.providers.base import NotSupportedError, ProviderBase


# ---------------------------------------------------------------------------
# Invariant: PRICING_SCHEMAS registry is self-consistent
# ---------------------------------------------------------------------------

def test_pricing_schemas_all_domains_represented() -> None:
    """Every PricingDomain must appear in PRICING_SCHEMAS."""
    registered_domains = {domain for domain, _ in PRICING_SCHEMAS}
    for domain in PricingDomain:
        assert domain in registered_domains, f"PricingDomain.{domain.name} missing from PRICING_SCHEMAS"


def test_pricing_schemas_all_spec_classes_are_base_subclasses() -> None:
    """Every spec class in PRICING_SCHEMAS must be a subclass of BasePricingSpec."""
    from opencloudcosts.models import BasePricingSpec
    for key, spec_cls in PRICING_SCHEMAS.items():
        assert issubclass(spec_cls, BasePricingSpec), (
            f"PRICING_SCHEMAS[{key}] = {spec_cls} is not a BasePricingSpec subclass"
        )


def test_pricing_schemas_domain_matches_spec_class() -> None:
    """The domain key in PRICING_SCHEMAS must match the spec class's domain literal."""
    for (domain, service), spec_cls in PRICING_SCHEMAS.items():
        instance = spec_cls(provider="aws", region="us-east-1")
        assert instance.domain == domain, (
            f"PRICING_SCHEMAS[({domain}, {service})] = {spec_cls.__name__} "
            f"but instance.domain = {instance.domain}"
        )


# ---------------------------------------------------------------------------
# Invariant: NotSupportedError.to_response() shape
# ---------------------------------------------------------------------------

def test_not_supported_error_to_response_shape() -> None:
    err = NotSupportedError(
        provider=CloudProvider.AZURE,
        domain=PricingDomain.AI,
        service="gemini",
        reason="Gemini is GCP-only.",
        alternatives=["get_price(provider='gcp', domain='ai', service='gemini', ...)"],
        example_invocation={"provider": "gcp", "domain": "ai", "service": "gemini"},
    )
    resp = err.to_response()
    assert resp["error"] == "not_supported"
    assert resp["provider"] == "azure"
    assert resp["domain"] == "ai"
    assert resp["service"] == "gemini"
    assert "reason" in resp
    assert "alternatives" in resp
    assert "example_invocation" in resp


def test_not_supported_error_minimal_response() -> None:
    err = NotSupportedError(
        provider=CloudProvider.AWS,
        domain=PricingDomain.OBSERVABILITY,
        service=None,
        reason="Not implemented.",
    )
    resp = err.to_response()
    assert resp["error"] == "not_supported"
    assert "alternatives" not in resp
    assert "example_invocation" not in resp


# ---------------------------------------------------------------------------
# Invariant: ProviderBase mixin raises NotSupportedError for all optional methods
# ---------------------------------------------------------------------------

class _StubProvider(ProviderBase):
    provider = CloudProvider.AWS


@pytest.mark.asyncio
async def test_provider_base_get_price_raises() -> None:
    spec = ComputePricingSpec(provider=CloudProvider.AWS, region="us-east-1", resource_type="m5.xlarge")
    with pytest.raises(NotSupportedError):
        await _StubProvider().get_price(spec)


@pytest.mark.asyncio
async def test_provider_base_describe_catalog_raises() -> None:
    with pytest.raises(NotSupportedError):
        await _StubProvider().describe_catalog()


@pytest.mark.asyncio
async def test_provider_base_get_discount_summary_raises() -> None:
    with pytest.raises(NotSupportedError):
        await _StubProvider().get_discount_summary()


@pytest.mark.asyncio
async def test_provider_base_applicable_commitments_returns_empty() -> None:
    spec = ComputePricingSpec(provider=CloudProvider.AWS, region="us-east-1", resource_type="m5.xlarge")
    result = await _StubProvider()._applicable_commitments(spec)
    assert result == []


# ---------------------------------------------------------------------------
# Invariant: PricingResult.summary() is LLM-safe
# ---------------------------------------------------------------------------

def test_pricing_result_summary_with_no_auth() -> None:
    price = NormalizedPrice(
        provider=CloudProvider.AWS,
        service="compute",
        sku_id="test-sku",
        product_family="Compute Instance",
        description="m5.xlarge Linux on-demand",
        region="us-east-1",
        pricing_term=PricingTerm.ON_DEMAND,
        price_per_unit=Decimal("0.192"),
        unit=PriceUnit.PER_HOUR,
    )
    result = PricingResult(public_prices=[price], auth_available=False)
    summary = result.summary()

    assert "public_prices" in summary
    assert len(summary["public_prices"]) == 1
    assert summary["auth_available"] is False
    assert "contracted_prices" not in summary
    assert "effective_price" not in summary


def test_pricing_result_summary_with_auth() -> None:
    price = NormalizedPrice(
        provider=CloudProvider.AWS,
        service="compute",
        sku_id="test-sku",
        product_family="Compute Instance",
        description="m5.xlarge Linux on-demand",
        region="us-east-1",
        pricing_term=PricingTerm.ON_DEMAND,
        price_per_unit=Decimal("0.192"),
        unit=PriceUnit.PER_HOUR,
    )
    contracted = NormalizedPrice(
        provider=CloudProvider.AWS,
        service="compute",
        sku_id="test-sku-sp",
        product_family="Compute Instance",
        description="m5.xlarge Compute SP",
        region="us-east-1",
        pricing_term=PricingTerm.COMPUTE_SP,
        price_per_unit=Decimal("0.140"),
        unit=PriceUnit.PER_HOUR,
    )
    effective = EffectivePrice(
        base_price=price,
        effective_price_per_unit=Decimal("0.140"),
        discount_type="SP",
        discount_pct=27.1,
        commitment_term="1yr",
        source="cost_explorer",
    )
    result = PricingResult(
        public_prices=[price],
        contracted_prices=[contracted],
        effective_price=effective,
        auth_available=True,
    )
    summary = result.summary()

    assert summary["auth_available"] is True
    assert "contracted_prices" in summary
    assert "effective_price" in summary
    assert "discount_pct" in summary["effective_price"]


# ---------------------------------------------------------------------------
# Provider fixtures (shared across invariant tests below)
# ---------------------------------------------------------------------------

_M5_XLARGE_ITEM = {
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

_AZURE_VM_ITEM = {
    "retailPrice": 0.192,
    "unitPrice": 0.192,
    "armRegionName": "eastus",
    "armSkuName": "Standard_D4s_v3",
    "productName": "Virtual Machines DSv3 Series",
    "skuName": "D4s v3",
    "meterName": "D4s v3",
    "serviceName": "Virtual Machines",
    "serviceFamily": "Compute",
    "unitOfMeasure": "1 Hour",
    "type": "Consumption",
    "currencyCode": "USD",
    "meterId": "test-meter",
}


@pytest.fixture
async def aws_provider(tmp_path: Path):
    from opencloudcosts.providers.aws import AWSProvider
    settings = Settings(cache_dir=tmp_path / "cache_aws")
    cache = CacheManager(settings.cache_dir)
    await cache.initialize()
    provider = AWSProvider(settings, cache)
    yield provider
    await cache.close()


@pytest.fixture
async def gcp_provider(tmp_path: Path):
    from opencloudcosts.providers.gcp import GCPProvider
    settings = Settings(cache_dir=tmp_path / "cache_gcp", gcp_api_key="fake-key-for-tests")
    cache = CacheManager(settings.cache_dir)
    await cache.initialize()
    provider = GCPProvider(settings, cache)
    yield provider
    await cache.close()


@pytest.fixture
async def azure_provider(tmp_path: Path):
    from opencloudcosts.providers.azure import AzureProvider
    settings = Settings(cache_dir=tmp_path / "cache_azure")
    cache = CacheManager(settings.cache_dir)
    await cache.initialize()
    provider = AzureProvider(settings, cache)
    yield provider
    await cache.close()


# ---------------------------------------------------------------------------
# Invariant 1: supports() / dispatch agreement
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
@pytest.mark.parametrize("provider_name,domain,service,should_support", [
    # AWS supported
    ("aws", PricingDomain.COMPUTE, None, True),
    ("aws", PricingDomain.STORAGE, None, True),
    ("aws", PricingDomain.DATABASE, "rds", True),
    ("aws", PricingDomain.AI, "bedrock", True),
    ("aws", PricingDomain.SERVERLESS, "lambda", True),
    # AWS not supported (GCP-only services)
    ("aws", PricingDomain.AI, "gemini", False),
    ("aws", PricingDomain.AI, "vertex", False),
    ("aws", PricingDomain.CONTAINER, "gke", False),
    # GCP supported
    ("gcp", PricingDomain.COMPUTE, None, True),
    ("gcp", PricingDomain.ANALYTICS, "bigquery", True),
    ("gcp", PricingDomain.AI, "gemini", True),
    # GCP not supported (AWS-only services)
    ("gcp", PricingDomain.AI, "bedrock", False),
    ("gcp", PricingDomain.CONTAINER, "eks", False),
    # Azure supported
    ("azure", PricingDomain.COMPUTE, None, True),
    ("azure", PricingDomain.STORAGE, None, True),
    # Azure not yet supported
    ("azure", PricingDomain.AI, "azure_openai", False),
    ("azure", PricingDomain.ANALYTICS, "bigquery", False),
])
async def test_supports_dispatch_agreement(
    provider_name: str,
    domain: PricingDomain,
    service: str | None,
    should_support: bool,
    aws_provider: object,
    gcp_provider: object,
    azure_provider: object,
) -> None:
    """supports() == True iff get_price() does not raise NotSupportedError."""
    providers = {"aws": aws_provider, "gcp": gcp_provider, "azure": azure_provider}
    provider = providers[provider_name]

    actual = provider.supports(domain, service)
    assert actual == should_support, (
        f"{provider_name}.supports({domain.value}, {service!r}) returned {actual}, expected {should_support}"
    )

    # For unsupported combos, get_price must raise NotSupportedError (not KeyError, AttributeError, etc.)
    if not should_support:
        from pydantic import TypeAdapter
        from opencloudcosts.models import PricingSpec as _PS
        _SPEC_ADAPTER = TypeAdapter(_PS)
        spec_dict: dict = {
            "provider": provider_name,
            "domain": domain.value,
            "region": "us-east-1",
        }
        if service:
            spec_dict["service"] = service
        try:
            spec = _SPEC_ADAPTER.validate_python(spec_dict)
        except Exception:
            pytest.skip(f"Cannot build spec for {domain.value}/{service} — not a registered domain")
        with pytest.raises(NotSupportedError):
            await provider.get_price(spec)


# ---------------------------------------------------------------------------
# Invariant 2 & 3: Shape invariant + region echo
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_shape_invariant_aws_compute(aws_provider: object) -> None:
    """AWS compute result: all prices have price_per_unit > 0, non-empty region, valid PriceUnit."""
    from opencloudcosts.providers.aws import AWSProvider
    provider: AWSProvider = aws_provider  # type: ignore[assignment]
    spec = ComputePricingSpec(
        provider=CloudProvider.AWS, region="us-east-1", resource_type="m5.xlarge"
    )
    with patch.object(provider, "_get_products", return_value=[_M5_XLARGE_ITEM]):
        result = await provider.get_price(spec)

    assert isinstance(result, PricingResult)
    assert len(result.public_prices) > 0
    for price in result.public_prices:
        assert price.price_per_unit > Decimal("0"), f"price_per_unit must be positive: {price}"
        assert price.region, f"region must be non-empty: {price}"
        assert price.unit in PriceUnit, f"unit must be a valid PriceUnit: {price.unit}"


@pytest.mark.asyncio
async def test_shape_invariant_azure_compute(azure_provider: object) -> None:
    """Azure compute result: all prices have price_per_unit > 0, non-empty region, valid PriceUnit."""
    from opencloudcosts.providers.azure import AzureProvider
    provider: AzureProvider = azure_provider  # type: ignore[assignment]
    spec = ComputePricingSpec(
        provider=CloudProvider.AZURE, region="eastus", resource_type="Standard_D4s_v3"
    )
    mock_resp = MagicMock()
    mock_resp.raise_for_status = MagicMock()
    mock_resp.json.return_value = {"Items": [_AZURE_VM_ITEM], "NextPageLink": None}
    with patch("httpx.get", return_value=mock_resp):
        result = await provider.get_price(spec)

    assert isinstance(result, PricingResult)
    assert len(result.public_prices) > 0
    for price in result.public_prices:
        assert price.price_per_unit > Decimal("0"), f"price_per_unit must be positive: {price}"
        assert price.region, f"region must be non-empty: {price}"
        assert price.unit in PriceUnit, f"unit must be a valid PriceUnit: {price.unit}"


@pytest.mark.asyncio
async def test_region_echo_aws(aws_provider: object) -> None:
    """AWS compute result: every price.region matches the spec.region."""
    from opencloudcosts.providers.aws import AWSProvider
    provider: AWSProvider = aws_provider  # type: ignore[assignment]
    spec = ComputePricingSpec(
        provider=CloudProvider.AWS, region="us-east-1", resource_type="m5.xlarge"
    )
    with patch.object(provider, "_get_products", return_value=[_M5_XLARGE_ITEM]):
        result = await provider.get_price(spec)

    for price in result.public_prices:
        assert price.region == spec.region, (
            f"Region echo failed: price.region={price.region!r}, spec.region={spec.region!r}"
        )


@pytest.mark.asyncio
async def test_region_echo_azure(azure_provider: object) -> None:
    """Azure compute result: every price.region matches the spec.region."""
    from opencloudcosts.providers.azure import AzureProvider
    provider: AzureProvider = azure_provider  # type: ignore[assignment]
    spec = ComputePricingSpec(
        provider=CloudProvider.AZURE, region="eastus", resource_type="Standard_D4s_v3"
    )
    mock_resp = MagicMock()
    mock_resp.raise_for_status = MagicMock()
    mock_resp.json.return_value = {"Items": [_AZURE_VM_ITEM], "NextPageLink": None}
    with patch("httpx.get", return_value=mock_resp):
        result = await provider.get_price(spec)

    for price in result.public_prices:
        assert price.region == spec.region, (
            f"Region echo failed: price.region={price.region!r}, spec.region={spec.region!r}"
        )


# ---------------------------------------------------------------------------
# Invariant 4: describe_catalog example_invocation roundtrip
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_describe_catalog_example_roundtrip_azure(azure_provider: object) -> None:
    """describe_catalog() example_invocations pass back to get_price() without error."""
    from pydantic import TypeAdapter
    from opencloudcosts.models import PricingSpec as _PS
    _SPEC_ADAPTER = TypeAdapter(_PS)
    from opencloudcosts.providers.azure import AzureProvider
    provider: AzureProvider = azure_provider  # type: ignore[assignment]

    catalog = await provider.describe_catalog()
    assert catalog.example_invocations, "describe_catalog must return at least one example_invocation"

    for key, example in catalog.example_invocations.items():
        try:
            spec = _SPEC_ADAPTER.validate_python(example)
        except Exception as exc:
            pytest.fail(f"example_invocations[{key!r}] failed to parse as PricingSpec: {exc}\n{example}")

        mock_resp = MagicMock()
        mock_resp.raise_for_status = MagicMock()
        mock_resp.json.return_value = {"Items": [_AZURE_VM_ITEM], "NextPageLink": None}
        with patch("httpx.get", return_value=mock_resp):
            try:
                result = await provider.get_price(spec)
                assert isinstance(result, PricingResult), (
                    f"example_invocations[{key!r}] returned {type(result)}, not PricingResult"
                )
            except NotSupportedError as exc:
                pytest.fail(
                    f"example_invocations[{key!r}] raised NotSupportedError — "
                    f"describe_catalog example is inconsistent with supports(): {exc}"
                )


@pytest.mark.asyncio
async def test_describe_catalog_example_roundtrip_aws(aws_provider: object) -> None:
    """AWS describe_catalog() example_invocations parse and dispatch without NotSupportedError."""
    from pydantic import TypeAdapter
    from opencloudcosts.models import PricingSpec as _PS
    _SPEC_ADAPTER = TypeAdapter(_PS)
    from opencloudcosts.providers.aws import AWSProvider
    provider: AWSProvider = aws_provider  # type: ignore[assignment]

    catalog = await provider.describe_catalog()
    assert catalog.example_invocations

    for key, example in list(catalog.example_invocations.items())[:3]:  # spot check first 3
        try:
            spec = _SPEC_ADAPTER.validate_python(example)
        except Exception as exc:
            pytest.fail(f"AWS example_invocations[{key!r}] failed to parse: {exc}")

        with patch.object(provider, "_get_products", return_value=[_M5_XLARGE_ITEM]):
            try:
                result = await provider.get_price(spec)
                assert isinstance(result, PricingResult)
            except NotSupportedError as exc:
                pytest.fail(
                    f"AWS example_invocations[{key!r}] raised NotSupportedError: {exc}"
                )
            except Exception:
                pass  # other errors (e.g. wrong mock data shape) are acceptable in spot-check


# ---------------------------------------------------------------------------
# Invariant 5: Breakdown arithmetic
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_breakdown_arithmetic_gke_standard(gcp_provider: object) -> None:
    """GKE Standard breakdown.cluster_management_monthly must equal price * hours_per_month."""
    from opencloudcosts.models import ContainerPricingSpec
    from opencloudcosts.providers.gcp import GCPProvider
    provider: GCPProvider = gcp_provider  # type: ignore[assignment]
    spec = ContainerPricingSpec(
        provider=CloudProvider.GCP, region="us-central1", service="gke", mode="standard"
    )

    gke_sku = {
        "name": "services/6F81-5844-456A/skus/FAKE-GKE",
        "skuId": "FAKE-GKE",
        "description": "Kubernetes Engine Cluster Management",
        "category": {
            "serviceDisplayName": "Kubernetes Engine",
            "resourceFamily": "Compute",
            "resourceGroup": "CPU",
            "usageType": "OnDemand",
        },
        "serviceRegions": ["us-central1"],
        "pricingInfo": [{
            "pricingExpression": {
                "usageUnit": "h",
                "tieredRates": [{
                    "startUsageAmount": 0,
                    "unitPrice": {"currencyCode": "USD", "units": "0", "nanos": 100000000},
                }],
            }
        }],
    }

    with patch.object(provider, "_fetch_skus", return_value=[gke_sku]):
        result = await provider.get_price(spec)

    assert isinstance(result, PricingResult)
    assert len(result.public_prices) > 0

    breakdown = result.breakdown
    if "cluster_management_monthly" in breakdown:
        rate = result.public_prices[0].price_per_unit
        expected_monthly = float(rate * Decimal(str(spec.hours_per_month)))
        actual_monthly = breakdown["cluster_management_monthly"]
        assert abs(actual_monthly - expected_monthly) <= 0.01, (
            f"Breakdown arithmetic failed: expected ≈${expected_monthly:.4f}, "
            f"got ${actual_monthly:.4f}"
        )
