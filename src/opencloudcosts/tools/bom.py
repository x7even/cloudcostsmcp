"""Bill of Materials (BoM) and unit-economics MCP tools — v0.8.0."""
from __future__ import annotations

import logging
from decimal import Decimal
from typing import Any

from mcp.server.fastmcp import Context
from pydantic import TypeAdapter

from opencloudcosts.models import BomEstimate, BomLineItem, PricingSpec
from opencloudcosts.providers.base import NotSupportedError

logger = logging.getLogger(__name__)

_SPEC_ADAPTER: TypeAdapter[PricingSpec] = TypeAdapter(PricingSpec)


def register_bom_tools(mcp: Any) -> None:

    @mcp.tool()
    async def estimate_bom(
        ctx: Context,
        items: list[dict[str, Any]],
    ) -> dict[str, Any]:
        """
        Use this tool for total infrastructure cost, TCO, monthly spend for a multi-resource
        stack, or cost comparison between architectures.

        Handles compute + storage + database + AI together in a single call — do NOT call
        get_price individually for multi-resource questions; use this tool instead.

        Returns per-item and total monthly/annual costs with real public pricing data,
        plus a not_included list with follow-up get_price calls for hidden costs
        (egress, load balancers, NAT Gateway, monitoring, backups).

        Each item should be a PricingSpec dict PLUS a quantity field:
          - provider: "aws" | "gcp" | "azure"
          - domain: "compute" | "storage" | "database" | "ai" | ...
          - region: region code
          - quantity: number of units (default 1)
          - hours_per_month: hours/month for compute (default 730 = always-on)
          - description: optional label for this line item
          Plus domain-specific fields (see get_price or describe_catalog for details).

        Examples:
          Compute + database + storage on AWS:
          [
            {"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge",   "region": "us-east-1", "quantity": 3},
            {"provider": "aws", "domain": "database", "service": "rds", "resource_type": "db.r6g.large", "engine": "MySQL", "deployment": "single-az", "region": "us-east-1"},
            {"provider": "aws", "domain": "storage",  "storage_type": "gp3", "size_gb": 500, "region": "us-east-1"}
          ]

          Mixed cloud:
          [
            {"provider": "gcp",   "domain": "compute", "resource_type": "n1-standard-4", "region": "us-central1", "quantity": 2},
            {"provider": "azure", "domain": "compute", "resource_type": "Standard_D4s_v3", "region": "eastus", "quantity": 1}
          ]
        """
        providers = ctx.request_context.lifespan_context["providers"]
        line_items: list[BomLineItem] = []
        errors: list[str] = []

        for idx, item in enumerate(items):
            label = f"Item {idx + 1}"
            try:
                quantity = int(item.get("quantity", 1))
                hours_per_month = float(item.get("hours_per_month", 730.0))
                size_gb = float(item.get("size_gb", 100.0))
                description = item.get("description")

                # Build a clean spec dict (remove BoM-only fields)
                spec_dict = {k: v for k, v in item.items()
                             if k not in ("quantity", "hours_per_month", "description")}
                # Fill hours_per_month into spec for compute
                if spec_dict.get("domain") == "compute" and "hours_per_month" not in spec_dict:
                    spec_dict["hours_per_month"] = hours_per_month
                if spec_dict.get("domain") == "storage" and "size_gb" not in spec_dict and size_gb:
                    spec_dict["size_gb"] = size_gb

                try:
                    parsed = _SPEC_ADAPTER.validate_python(spec_dict)
                except Exception as e:
                    errors.append(f"{label}: invalid spec — {e}")
                    continue

                pvdr = providers.get(parsed.provider.value)
                if pvdr is None:
                    errors.append(f"{label}: provider '{parsed.provider.value}' not configured")
                    continue

                if not pvdr.supports(parsed.domain, parsed.service):
                    errors.append(
                        f"{label}: {parsed.provider.value} does not support "
                        f"{parsed.domain.value}/{parsed.service}"
                    )
                    continue

                result = await pvdr.get_price(parsed)

                if not result.public_prices:
                    errors.append(f"{label}: no pricing found for spec {spec_dict}")
                    continue

                if not description:
                    resource_id = (
                        getattr(parsed, "resource_type", None)
                        or getattr(parsed, "storage_type", None)
                        or getattr(parsed, "model", None)
                        or parsed.service
                        or parsed.domain.value
                    )
                    description = f"{resource_id} ({parsed.region})"

                bom_item = BomLineItem.from_price(
                    description=description,
                    price=result.public_prices[0],
                    quantity=quantity,
                    hours_per_month=hours_per_month,
                    size_gb=size_gb,
                )
                line_items.append(bom_item)

            except NotSupportedError as e:
                errors.append(f"{label}: {e.reason}")
            except Exception as e:
                errors.append(f"{label}: {e}")

        if not line_items:
            return {"error": "No valid line items. Errors: " + "; ".join(errors)}

        estimate = BomEstimate.from_items(line_items)

        services_in_bom = {li.service for li in estimate.items}
        providers_in_bom = {li.provider.value for li in estimate.items}

        not_included: list[dict[str, str]] = []
        for pname in providers_in_bom:
            pvdr_advisory = providers.get(pname)
            if pvdr_advisory is None:
                continue
            pvdr_regions = [li.region for li in estimate.items if li.provider.value == pname]
            sample_region = pvdr_regions[0] if pvdr_regions else pvdr_advisory.default_region()
            not_included.extend(pvdr_advisory.bom_advisories(services_in_bom, sample_region))

        return {
            "line_items": [
                {
                    "description": li.description,
                    "provider": li.provider.value,
                    "service": li.service,
                    "region": li.region,
                    "quantity": li.quantity,
                    "unit_price": f"${li.unit_price.price_per_unit:.6f}/{li.unit_price.unit.value}",
                    "monthly_cost": f"${li.monthly_cost:.2f}",
                    "annual_cost": f"${li.annual_cost:.2f}",
                }
                for li in estimate.items
            ],
            "totals": {
                "monthly": f"${estimate.total_monthly:.2f}",
                "annual": f"${estimate.total_annual:.2f}",
                "currency": estimate.currency,
            },
            "not_included": not_included or None,
            "errors": errors or None,
        }

    @mcp.tool()
    async def estimate_unit_economics(
        ctx: Context,
        items: list[dict[str, Any]],
        units_per_month: float,
        unit_label: str = "user",
    ) -> dict[str, Any]:
        """
        Estimate per-unit economics (cost per user, per request, per transaction) given
        a Bill of Materials and expected monthly usage volume.

        Args:
            items: Same format as estimate_bom — list of cloud resource PricingSpec dicts
                   plus quantity field. See estimate_bom for full item format.
            units_per_month: Monthly volume being measured (e.g. 10000 users)
            unit_label: What the unit represents — "user", "request", "transaction", etc.
        """
        providers = ctx.request_context.lifespan_context["providers"]
        line_items: list[BomLineItem] = []
        errors: list[str] = []

        for idx, item in enumerate(items):
            label = f"Item {idx + 1}"
            try:
                quantity = int(item.get("quantity", 1))
                hours_per_month = float(item.get("hours_per_month", 730.0))
                size_gb = float(item.get("size_gb", 100.0))
                description = item.get("description")

                spec_dict = {k: v for k, v in item.items()
                             if k not in ("quantity", "hours_per_month", "description")}
                if spec_dict.get("domain") == "compute" and "hours_per_month" not in spec_dict:
                    spec_dict["hours_per_month"] = hours_per_month

                try:
                    parsed = _SPEC_ADAPTER.validate_python(spec_dict)
                except Exception as e:
                    errors.append(f"{label}: invalid spec — {e}")
                    continue

                pvdr = providers.get(parsed.provider.value)
                if pvdr is None:
                    errors.append(f"{label}: provider '{parsed.provider.value}' not configured")
                    continue

                if not pvdr.supports(parsed.domain, parsed.service):
                    errors.append(
                        f"{label}: {parsed.provider.value} does not support "
                        f"{parsed.domain.value}/{parsed.service}"
                    )
                    continue

                result = await pvdr.get_price(parsed)
                if not result.public_prices:
                    errors.append(f"{label}: no pricing found")
                    continue

                if not description:
                    resource_id = (
                        getattr(parsed, "resource_type", None)
                        or getattr(parsed, "storage_type", None)
                        or parsed.domain.value
                    )
                    description = f"{resource_id} ({parsed.region})"

                line_items.append(BomLineItem.from_price(
                    description=description,
                    price=result.public_prices[0],
                    quantity=quantity,
                    hours_per_month=hours_per_month,
                    size_gb=size_gb,
                ))

            except NotSupportedError as e:
                errors.append(f"{label}: {e.reason}")
            except Exception as e:
                errors.append(f"{label}: {e}")

        if not line_items:
            return {"error": "No valid items. Errors: " + "; ".join(errors)}

        estimate = BomEstimate.from_items(line_items)
        cost_per_unit = (
            estimate.total_monthly / Decimal(str(units_per_month))
            if units_per_month > 0 else Decimal("0")
        )
        sample_region = estimate.items[0].region if estimate.items else "us-east-1"

        return {
            "pricing_region": sample_region,
            "infrastructure_monthly": f"${estimate.total_monthly:.2f}",
            "infrastructure_annual": f"${estimate.total_annual:.2f}",
            "volume": f"{units_per_month:,.0f} {unit_label}s/month",
            "cost_per_unit": f"${cost_per_unit:.4f} per {unit_label}",
            "cost_per_unit_annual": f"${cost_per_unit * 12:.4f} per {unit_label}/year",
            "errors": errors or None,
            "important": (
                f"Prices are for {sample_region} — always state the region in your answer "
                "as unit economics vary significantly by region."
            ),
        }
