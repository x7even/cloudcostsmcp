from __future__ import annotations

from datetime import datetime
from decimal import Decimal
from enum import Enum
from typing import Any

from pydantic import BaseModel, Field


class CloudProvider(str, Enum):
    AWS = "aws"
    GCP = "gcp"
    AZURE = "azure"


class PricingTerm(str, Enum):
    ON_DEMAND = "on_demand"
    RESERVED_1YR = "reserved_1yr"
    RESERVED_3YR = "reserved_3yr"
    RESERVED_1YR_PARTIAL = "reserved_1yr_partial"
    RESERVED_1YR_ALL = "reserved_1yr_all"
    RESERVED_3YR_PARTIAL = "reserved_3yr_partial"
    RESERVED_3YR_ALL = "reserved_3yr_all"
    SPOT = "spot"
    SAVINGS_PLAN = "savings_plan"  # AWS
    CUD_1YR = "cud_1yr"           # GCP Committed Use Discount 1yr
    CUD_3YR = "cud_3yr"           # GCP Committed Use Discount 3yr
    SUD = "sud"                   # GCP Sustained Use Discount


class PriceUnit(str, Enum):
    PER_HOUR = "per_hour"
    PER_MONTH = "per_month"
    PER_GB_MONTH = "per_gb_month"
    PER_GB = "per_gb"
    PER_IOPS_MONTH = "per_iops_month"
    PER_REQUEST = "per_request"
    PER_GB_SECOND = "per_gb_second"    # Lambda duration
    PER_QUERY = "per_query"            # Route53, Athena
    PER_UNIT = "per_unit"              # generic fallback


class NormalizedPrice(BaseModel):
    """Provider-agnostic pricing entry — the core data model."""
    provider: CloudProvider
    service: str                    # e.g. "compute", "storage", "database"
    sku_id: str                     # provider-native SKU ID
    product_family: str             # e.g. "Compute Instance", "Storage"
    description: str                # human-readable
    region: str                     # normalized region code (us-east-1, us-east1)
    attributes: dict[str, str] = Field(default_factory=dict)
    # ^ instance_type, vcpu, memory_gb, os, storage_type, engine, etc.
    pricing_term: PricingTerm
    price_per_unit: Decimal
    unit: PriceUnit
    currency: str = "USD"
    effective_date: datetime | None = None

    @property
    def monthly_cost(self) -> Decimal:
        """Convenience: monthly cost assuming 730 hrs/month."""
        if self.unit == PriceUnit.PER_HOUR:
            return self.price_per_unit * Decimal("730")
        if self.unit == PriceUnit.PER_MONTH:
            return self.price_per_unit
        return self.price_per_unit

    @property
    def hourly_cost(self) -> Decimal:
        if self.unit == PriceUnit.PER_HOUR:
            return self.price_per_unit
        if self.unit == PriceUnit.PER_MONTH:
            return self.price_per_unit / Decimal("730")
        return self.price_per_unit

    def summary(self) -> dict[str, Any]:
        """Compact dict for LLM consumption."""
        from opencloudcosts.utils.regions import region_display_name
        return {
            "provider": self.provider.value,
            "description": self.description,
            "region": self.region,
            "region_name": region_display_name(self.provider.value, self.region),
            "term": self.pricing_term.value,
            "price": f"${self.price_per_unit:.6f} {self.unit.value}",
            "monthly_estimate": f"${self.monthly_cost:.2f}/mo" if self.unit == PriceUnit.PER_HOUR else None,
            **{k: v for k, v in self.attributes.items() if k in ("instanceType", "vcpu", "memory", "operatingSystem", "storage_type", "volumeType")},
        }


class PriceComparison(BaseModel):
    """Result of a cross-region or cross-provider price comparison."""
    query_description: str
    results: list[NormalizedPrice]
    cheapest: NormalizedPrice | None = None
    most_expensive: NormalizedPrice | None = None
    price_delta_pct: float | None = None   # (most_expensive - cheapest) / cheapest * 100

    @classmethod
    def from_results(cls, query: str, results: list[NormalizedPrice]) -> "PriceComparison":
        if not results:
            return cls(query_description=query, results=[])
        sorted_results = sorted(results, key=lambda r: r.price_per_unit)
        cheapest = sorted_results[0]
        most_expensive = sorted_results[-1]
        delta = None
        if cheapest.price_per_unit > 0:
            delta = float(
                (most_expensive.price_per_unit - cheapest.price_per_unit)
                / cheapest.price_per_unit * 100
            )
        return cls(
            query_description=query,
            results=sorted_results,
            cheapest=cheapest,
            most_expensive=most_expensive,
            price_delta_pct=round(delta, 2) if delta is not None else None,
        )


class BomLineItem(BaseModel):
    """A single line in a Bill of Materials."""
    description: str
    service: str
    provider: CloudProvider
    region: str
    quantity: int
    hours_per_month: float = 730.0
    unit_price: NormalizedPrice
    monthly_cost: Decimal
    annual_cost: Decimal

    @classmethod
    def from_price(
        cls,
        description: str,
        price: NormalizedPrice,
        quantity: int,
        hours_per_month: float = 730.0,
    ) -> "BomLineItem":
        if price.unit == PriceUnit.PER_HOUR:
            monthly = price.price_per_unit * Decimal(str(hours_per_month)) * quantity
        elif price.unit == PriceUnit.PER_MONTH:
            monthly = price.price_per_unit * quantity
        else:
            monthly = price.price_per_unit * quantity
        return cls(
            description=description,
            service=price.service,
            provider=price.provider,
            region=price.region,
            quantity=quantity,
            hours_per_month=hours_per_month,
            unit_price=price,
            monthly_cost=monthly,
            annual_cost=monthly * 12,
        )


class BomEstimate(BaseModel):
    """Total cost of ownership for a Bill of Materials."""
    items: list[BomLineItem]
    total_monthly: Decimal
    total_annual: Decimal
    currency: str = "USD"

    @classmethod
    def from_items(cls, items: list[BomLineItem], currency: str = "USD") -> "BomEstimate":
        total_monthly = sum(i.monthly_cost for i in items)
        return cls(
            items=items,
            total_monthly=total_monthly,
            total_annual=total_monthly * 12,
            currency=currency,
        )


class EffectivePrice(BaseModel):
    """Bespoke pricing reflecting actual account discounts."""
    base_price: NormalizedPrice          # public on-demand price
    effective_price_per_unit: Decimal    # actual rate after discounts
    discount_type: str                   # "RI", "SP", "CUD", "EDP", "SUD"
    discount_pct: float                  # e.g. 35.0 for 35% off
    commitment_term: str | None = None   # "1yr", "3yr", None for SUD/EDP
    source: str = ""                     # "cost_explorer", "savings_plans_api", "billing_export"

    @property
    def savings_vs_on_demand(self) -> Decimal:
        return self.base_price.price_per_unit - self.effective_price_per_unit


class InstanceTypeInfo(BaseModel):
    """Metadata about a compute instance type."""
    provider: CloudProvider
    instance_type: str
    vcpu: int
    memory_gb: float
    gpu_count: int = 0
    gpu_type: str | None = None
    network_performance: str | None = None
    storage: str | None = None
    region: str
    available: bool = True
