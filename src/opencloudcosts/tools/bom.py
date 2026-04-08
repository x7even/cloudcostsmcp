"""Bill of Materials (BoM) and TCO estimation MCP tools."""
from __future__ import annotations

import logging
from decimal import Decimal
from typing import Any

from mcp.server.fastmcp import Context

from opencloudcosts.models import BomEstimate, BomLineItem, CloudProvider, PricingTerm

logger = logging.getLogger(__name__)


def register_bom_tools(mcp: Any) -> None:

    @mcp.tool()
    async def estimate_bom(
        ctx: Context,
        items: list[dict[str, Any]],
    ) -> dict[str, Any]:
        """
        Calculate the Total Cost of Ownership (TCO) for a Bill of Materials.

        Each item in the BoM specifies a cloud resource. Returns per-item and total
        monthly/annual costs using real public pricing data.

        Each item should be a dict with:
          - provider: "aws" or "gcp"
          - service: "compute", "storage", or "database"
          - type: instance type or storage type, e.g. "m5.xlarge", "gp3"
          - region: region code, e.g. "us-east-1"
          - quantity: number of units (default 1)
          - hours_per_month: hours/month for compute (default 730 = always-on)
          - term: pricing term (default "on_demand")
          - description: optional label for this line item
          - os: "Linux" (default) or "Windows"

        Example items:
          [
            {"provider": "aws", "service": "compute", "type": "m5.xlarge", "region": "us-east-1", "quantity": 3},
            {"provider": "aws", "service": "storage", "type": "gp3", "region": "us-east-1", "quantity": 1, "size_gb": 500}
          ]
        """
        providers = ctx.request_context.lifespan_context["providers"]
        line_items: list[BomLineItem] = []
        errors: list[str] = []

        for idx, item in enumerate(items):
            label = f"Item {idx + 1}"
            try:
                provider_name = item.get("provider", "aws")
                service = item.get("service", "compute")
                resource_type = item.get("type", "")
                region = item.get("region", "us-east-1")
                quantity = int(item.get("quantity", 1))
                hours_per_month = float(item.get("hours_per_month", 730.0))
                term_str = item.get("term", "on_demand")
                description = item.get("description") or f"{resource_type} ({region})"
                size_gb = float(item.get("size_gb", 100.0))
                os_type = item.get("os", "Linux")

                pvdr = providers.get(provider_name)
                if pvdr is None:
                    errors.append(f"{label}: provider '{provider_name}' not configured")
                    continue

                pricing_term = PricingTerm(term_str)

                if service == "compute":
                    prices = await pvdr.get_compute_price(resource_type, region, os_type, pricing_term)
                elif service == "storage":
                    prices = await pvdr.get_storage_price(resource_type, region, size_gb)
                elif service == "database":
                    if not hasattr(pvdr, "get_service_price"):
                        errors.append(f"{label}: database pricing not supported for provider '{provider_name}'")
                        continue
                    db_filters: dict[str, str] = {"instanceType": resource_type}
                    # Infer deployment option from term (default Single-AZ)
                    if "multi" in term_str.lower():
                        db_filters["deploymentOption"] = "Multi-AZ"
                    else:
                        db_filters["deploymentOption"] = "Single-AZ"
                    prices = await pvdr.get_service_price("rds", region, db_filters, max_results=5)
                    if not prices and db_filters.get("deploymentOption"):
                        # Retry without deploymentOption filter
                        del db_filters["deploymentOption"]
                        prices = await pvdr.get_service_price("rds", region, db_filters, max_results=5)
                else:
                    errors.append(f"{label}: unsupported service '{service}'")
                    continue

                if not prices:
                    errors.append(f"{label}: no pricing found for {resource_type} in {region}")
                    continue

                bom_item = BomLineItem.from_price(
                    description=description,
                    price=prices[0],
                    quantity=quantity,
                    hours_per_month=hours_per_month,
                )
                line_items.append(bom_item)

            except (ValueError, KeyError) as e:
                errors.append(f"{label}: {e}")

        if not line_items:
            return {"error": "No valid line items. Errors: " + "; ".join(errors)}

        estimate = BomEstimate.from_items(line_items)

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
            "errors": errors if errors else None,
        }

    @mcp.tool()
    async def estimate_unit_economics(
        ctx: Context,
        items: list[dict[str, Any]],
        units_per_month: float,
        unit_label: str = "user",
    ) -> dict[str, Any]:
        """
        Estimate per-unit economics (e.g. cost per user, per request, per transaction)
        given a Bill of Materials and expected monthly usage volume.

        Args:
            items: Same format as estimate_bom — list of cloud resource items.
                Each compute item may include os: "Linux" (default) or "Windows".
            units_per_month: Monthly volume of the unit being measured (e.g. 10000 users)
            unit_label: What the unit represents — "user", "request", "transaction", etc.

        Returns:
            Total infrastructure cost plus cost per unit at the given volume.
        """
        providers = ctx.request_context.lifespan_context["providers"]
        line_items: list[BomLineItem] = []
        errors: list[str] = []

        for idx, item in enumerate(items):
            label = f"Item {idx + 1}"
            try:
                provider_name = item.get("provider", "aws")
                service = item.get("service", "compute")
                resource_type = item.get("type", "")
                region = item.get("region", "us-east-1")
                quantity = int(item.get("quantity", 1))
                hours_per_month = float(item.get("hours_per_month", 730.0))
                term_str = item.get("term", "on_demand")
                description = item.get("description") or f"{resource_type} ({region})"
                size_gb = float(item.get("size_gb", 100.0))
                os_type = item.get("os", "Linux")

                pvdr = providers.get(provider_name)
                if pvdr is None:
                    errors.append(f"{label}: provider '{provider_name}' not configured")
                    continue

                pricing_term = PricingTerm(term_str)

                if service == "compute":
                    prices = await pvdr.get_compute_price(resource_type, region, os_type, pricing_term)
                elif service == "storage":
                    prices = await pvdr.get_storage_price(resource_type, region, size_gb)
                elif service == "database":
                    if not hasattr(pvdr, "get_service_price"):
                        errors.append(f"{label}: database pricing not supported for provider '{provider_name}'")
                        continue
                    db_filters = {"instanceType": resource_type, "deploymentOption": "Single-AZ"}
                    prices = await pvdr.get_service_price("rds", region, db_filters, max_results=5)
                    if not prices:
                        del db_filters["deploymentOption"]
                        prices = await pvdr.get_service_price("rds", region, db_filters, max_results=5)
                else:
                    errors.append(f"{label}: unsupported service '{service}'")
                    continue

                if not prices:
                    errors.append(f"{label}: no pricing found")
                    continue

                line_items.append(BomLineItem.from_price(description, prices[0], quantity, hours_per_month))

            except (ValueError, KeyError) as e:
                errors.append(f"{label}: {e}")

        if not line_items:
            return {"error": "No valid items. Errors: " + "; ".join(errors)}

        estimate = BomEstimate.from_items(line_items)
        cost_per_unit = estimate.total_monthly / Decimal(str(units_per_month)) if units_per_month > 0 else Decimal("0")

        return {
            "infrastructure_monthly": f"${estimate.total_monthly:.2f}",
            "infrastructure_annual": f"${estimate.total_annual:.2f}",
            "volume": f"{units_per_month:,.0f} {unit_label}s/month",
            "cost_per_unit": f"${cost_per_unit:.4f} per {unit_label}",
            "cost_per_unit_annual": f"${cost_per_unit * 12:.4f} per {unit_label}/year",
            "errors": errors if errors else None,
        }
