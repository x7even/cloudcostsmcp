"""Tests for normalized pricing models."""
from __future__ import annotations

from decimal import Decimal

from cloudcostmcp.models import (
    BomEstimate,
    BomLineItem,
    CloudProvider,
    NormalizedPrice,
    PriceComparison,
    PriceUnit,
    PricingTerm,
)


def make_price(region: str = "us-east-1", price: str = "0.192") -> NormalizedPrice:
    return NormalizedPrice(
        provider=CloudProvider.AWS,
        service="compute",
        sku_id="TEST123",
        product_family="Compute Instance",
        description="m5.xlarge",
        region=region,
        attributes={"instanceType": "m5.xlarge", "vcpu": "4", "memory": "16 GiB"},
        pricing_term=PricingTerm.ON_DEMAND,
        price_per_unit=Decimal(price),
        unit=PriceUnit.PER_HOUR,
    )


def test_normalized_price_monthly():
    p = make_price(price="0.192")
    assert p.monthly_cost == Decimal("0.192") * Decimal("730")


def test_normalized_price_summary():
    p = make_price()
    s = p.summary()
    assert s["provider"] == "aws"
    assert "0.192" in s["price"]
    assert s["instanceType"] == "m5.xlarge"


def test_price_comparison_sorted():
    p1 = make_price("us-east-1", "0.192")
    p2 = make_price("ap-southeast-2", "0.278")
    p3 = make_price("eu-west-1", "0.214")

    comp = PriceComparison.from_results("m5.xlarge comparison", [p1, p2, p3])
    assert comp.cheapest.region == "us-east-1"
    assert comp.most_expensive.region == "ap-southeast-2"
    assert comp.price_delta_pct is not None
    assert comp.price_delta_pct > 0
    # Sydney is ~44.8% more expensive than N. Virginia
    assert abs(comp.price_delta_pct - 44.79) < 1.0


def test_price_comparison_single():
    comp = PriceComparison.from_results("single", [make_price()])
    assert comp.cheapest is not None
    assert comp.price_delta_pct == 0.0


def test_price_comparison_empty():
    comp = PriceComparison.from_results("empty", [])
    assert comp.cheapest is None
    assert comp.price_delta_pct is None


def test_bom_estimate():
    p = make_price(price="0.192")
    item = BomLineItem.from_price("3x m5.xlarge", p, quantity=3, hours_per_month=730.0)
    assert item.monthly_cost == Decimal("0.192") * Decimal("730") * 3

    estimate = BomEstimate.from_items([item])
    assert estimate.total_monthly == item.monthly_cost
    assert estimate.total_annual == item.monthly_cost * 12
