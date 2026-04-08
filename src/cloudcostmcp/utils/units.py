"""Helpers for normalising price units from provider APIs."""
from __future__ import annotations

from decimal import Decimal

from cloudcostmcp.models import PriceUnit

# AWS Pricing API unit strings -> our PriceUnit enum
AWS_UNIT_MAP: dict[str, PriceUnit] = {
    "Hrs": PriceUnit.PER_HOUR,
    "Hours": PriceUnit.PER_HOUR,
    "hr": PriceUnit.PER_HOUR,
    "GB-Mo": PriceUnit.PER_GB_MONTH,
    "GB-month": PriceUnit.PER_GB_MONTH,
    "GB": PriceUnit.PER_GB,
    "IOPS-Mo": PriceUnit.PER_IOPS_MONTH,
    "Requests": PriceUnit.PER_REQUEST,
    "Request": PriceUnit.PER_REQUEST,
}

# GCP Billing Catalog API usageUnit strings -> our PriceUnit enum
GCP_UNIT_MAP: dict[str, PriceUnit] = {
    "h": PriceUnit.PER_HOUR,
    "hour": PriceUnit.PER_HOUR,
    "GiBy.mo": PriceUnit.PER_GB_MONTH,
    "GBy.mo": PriceUnit.PER_GB_MONTH,
    "GBy": PriceUnit.PER_GB,
    "GiBy": PriceUnit.PER_GB,
    "count": PriceUnit.PER_REQUEST,
    "mo": PriceUnit.PER_MONTH,
}


def parse_aws_unit(unit_str: str) -> PriceUnit:
    return AWS_UNIT_MAP.get(unit_str, PriceUnit.PER_UNIT)


def parse_gcp_unit(unit_str: str) -> PriceUnit:
    return GCP_UNIT_MAP.get(unit_str, PriceUnit.PER_UNIT)


def gcp_money_to_decimal(units: str | int, nanos: int) -> Decimal:
    """Convert GCP Money proto (units + nanos) to a Decimal price."""
    return Decimal(str(units)) + Decimal(nanos) / Decimal("1000000000")


def hours_to_monthly(price_per_hour: Decimal, hours_per_month: float = 730.0) -> Decimal:
    return price_per_hour * Decimal(str(hours_per_month))


def monthly_to_hourly(price_per_month: Decimal, hours_per_month: float = 730.0) -> Decimal:
    return price_per_month / Decimal(str(hours_per_month))
