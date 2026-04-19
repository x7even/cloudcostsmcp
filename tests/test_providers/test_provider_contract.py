"""
Contract tests for the CloudPricingProvider protocol.

Phase 1 (current): skeleton only — verifies the test infrastructure works
and that existing providers can be instantiated. The full parameterised
invariant suite activates on the v0.8.0-prep branch once providers implement
get_price(), supports(), and describe_catalog().

Invariants enforced in the full suite (see roadmap_0_8_0.md):
  1. supports() / dispatch agreement
  2. Shape invariant (price_per_unit > 0, non-empty region, valid PriceUnit)
  3. Region echo (result region matches spec region)
  4. Example actually works (describe_catalog example passes back to get_price)
  5. Breakdown arithmetic (monthly_total matches sum of prices within $0.01)
"""
from __future__ import annotations

import pytest

from opencloudcosts.models import (
    PRICING_SCHEMAS,
    PricingDomain,
    PricingResult,
    PricingSpec,
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
    from opencloudcosts.models import CloudProvider
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
    """Without optional fields, to_response() must still be valid."""
    from opencloudcosts.models import CloudProvider
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
    from opencloudcosts.models import CloudProvider
    provider = CloudProvider.AWS


@pytest.mark.asyncio
async def test_provider_base_get_price_raises() -> None:
    from opencloudcosts.models import ComputePricingSpec, CloudProvider
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
    from opencloudcosts.models import ComputePricingSpec, CloudProvider
    spec = ComputePricingSpec(provider=CloudProvider.AWS, region="us-east-1", resource_type="m5.xlarge")
    result = await _StubProvider()._applicable_commitments(spec)
    assert result == []


# ---------------------------------------------------------------------------
# Invariant: PricingResult.summary() is LLM-safe
# ---------------------------------------------------------------------------

def test_pricing_result_summary_with_no_auth() -> None:
    from opencloudcosts.models import (
        CloudProvider, NormalizedPrice, PricingTerm, PriceUnit
    )
    from decimal import Decimal
    from datetime import datetime

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
    from opencloudcosts.models import (
        CloudProvider, NormalizedPrice, PricingTerm, PriceUnit, EffectivePrice
    )
    from decimal import Decimal

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
# Placeholder: full parameterised invariant suite
# Activated on v0.8.0-prep once providers implement the v2 Protocol.
# ---------------------------------------------------------------------------

@pytest.mark.skip(reason="Activates on v0.8.0-prep after provider migration")
def test_supports_dispatch_agreement() -> None:
    """supports() == True iff get_price() does not raise NotSupportedError."""
    pass


@pytest.mark.skip(reason="Activates on v0.8.0-prep after provider migration")
def test_shape_invariant() -> None:
    """Every public_prices[*] has price_per_unit > 0, non-empty region, valid PriceUnit."""
    pass


@pytest.mark.skip(reason="Activates on v0.8.0-prep after provider migration")
def test_region_echo() -> None:
    """result.public_prices[*].region matches spec.region."""
    pass


@pytest.mark.skip(reason="Activates on v0.8.0-prep after provider migration")
def test_describe_catalog_example_roundtrip() -> None:
    """describe_catalog(p, d, s).example_invocation passes back to get_price successfully."""
    pass


@pytest.mark.skip(reason="Activates on v0.8.0-prep after provider migration")
def test_breakdown_arithmetic() -> None:
    """Where breakdown.monthly_total exists, it equals sum(prices) × quantity within $0.01."""
    pass
