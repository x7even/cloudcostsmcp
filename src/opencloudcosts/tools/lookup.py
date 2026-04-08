"""Price lookup MCP tools."""
from __future__ import annotations

import asyncio
import logging
from typing import Any

from mcp.server.fastmcp import Context

from opencloudcosts.models import PriceComparison, PricingTerm
from opencloudcosts.providers.base import NotConfiguredError

logger = logging.getLogger(__name__)


def _providers(ctx: Context) -> dict[str, Any]:
    return ctx.request_context.lifespan_context["providers"]


def _settings(ctx: Context) -> Any:
    return ctx.request_context.lifespan_context["settings"]


def register_lookup_tools(mcp: Any) -> None:
    """Register all lookup tools onto the FastMCP app."""

    @mcp.tool()
    async def get_compute_price(
        ctx: Context,
        provider: str,
        instance_type: str,
        region: str,
        os: str = "Linux",
        term: str = "on_demand",
    ) -> dict[str, Any]:
        """
        Get the price for a specific compute instance type in a cloud region.

        Returns pricing details including per-hour cost, monthly estimate, vCPU, and memory.
        Supports both public on-demand and reserved pricing terms.

        Args:
            provider: Cloud provider — "aws" or "gcp"
            instance_type: Instance type, e.g. "m5.xlarge" (AWS) or "n2-standard-4" (GCP)
            region: Region code, e.g. "us-east-1" (AWS) or "us-east1" (GCP)
            os: Operating system — "Linux" (default) or "Windows"
            term: Pricing term — "on_demand" (default), "reserved_1yr", "reserved_3yr", "spot"
        """
        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' is not configured. Available: {list(_providers(ctx))}"}

        try:
            pricing_term = PricingTerm(term)
        except ValueError:
            return {"error": f"Unknown term '{term}'. Valid: {[t.value for t in PricingTerm]}"}

        try:
            prices = await pvdr.get_compute_price(instance_type, region, os, pricing_term)
        except ValueError as e:
            return {"error": str(e)}
        except Exception as e:
            logger.error("get_compute_price error: %s", e)
            return {"error": f"API error: {e}"}

        if not prices:
            return {
                "result": "no_prices_found",
                "message": f"No pricing found for {instance_type} in {region} ({provider}). "
                           "Check instance type spelling or try search_pricing.",
            }

        return {
            "provider": provider,
            "instance_type": instance_type,
            "region": region,
            "os": os,
            "term": term,
            "prices": [p.summary() for p in prices],
            "count": len(prices),
        }

    @mcp.tool()
    async def compare_compute_prices(
        ctx: Context,
        provider: str,
        instance_type: str,
        regions: list[str],
        os: str = "Linux",
        term: str = "on_demand",
        baseline_region: str = "",
    ) -> dict[str, Any]:
        """
        Compare the price of the same compute instance type across multiple regions.

        Fetches all regions concurrently. Returns prices sorted cheapest first, plus
        the % price delta between cheapest and most expensive regions.

        Optionally compares every region against a baseline (e.g. baseline_region="us-east-1")
        to show delta $/% vs that reference point.

        Args:
            provider: Cloud provider — "aws" or "gcp"
            instance_type: Instance type, e.g. "m5.xlarge"
            regions: List of region codes to compare, e.g. ["us-east-1", "ap-southeast-2"]
            os: Operating system — "Linux" (default) or "Windows"
            term: Pricing term — "on_demand" (default), "reserved_1yr", "reserved_3yr"
            baseline_region: Optional region code for delta comparison, e.g. "us-east-1".
                             Adds delta_per_hour, delta_monthly, delta_pct to each result.
        """
        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            pricing_term = PricingTerm(term)
        except ValueError:
            return {"error": f"Unknown term '{term}'."}

        # Fan out concurrently instead of sequentially
        semaphore = asyncio.Semaphore(10)

        async def fetch_one(region: str) -> tuple[str, list, str | None]:
            async with semaphore:
                try:
                    prices = await pvdr.get_compute_price(instance_type, region, os, pricing_term)
                    return region, prices, None
                except Exception as e:
                    return region, [], str(e)

        raw = await asyncio.gather(*[fetch_one(r) for r in regions])

        all_prices = []
        errors = {}
        for region, prices, err in raw:
            if err:
                errors[region] = err
            elif prices:
                all_prices.extend(prices)
            else:
                errors[region] = "no pricing found"

        if not all_prices:
            return {
                "result": "no_prices_found",
                "message": f"No pricing found for {instance_type} in any of the specified regions.",
                "errors": errors,
            }

        comparison = PriceComparison.from_results(
            f"{instance_type} across regions", all_prices
        )

        prices_by_region = [p.summary() for p in comparison.results]

        if baseline_region:
            # summary() uses "price" key not "price_per_hour" — build uniform dicts
            from opencloudcosts.utils.baseline import apply_baseline_deltas
            uniform = [
                {
                    "region": p.region,
                    "region_name": p.summary().get("region_name", p.region),
                    "price_per_hour": f"${p.price_per_unit:.6f}",
                    "monthly_estimate": f"${p.monthly_cost:.2f}/mo",
                    **{k: v for k, v in p.summary().items()
                       if k in ("vcpu", "memory", "instanceType", "operatingSystem")},
                }
                for p in comparison.results
            ]
            try:
                apply_baseline_deltas(uniform, baseline_region)
            except ValueError as e:
                return {"error": str(e)}
            prices_by_region = uniform

        result: dict[str, Any] = {
            "provider": provider,
            "instance_type": instance_type,
            "term": term,
            "prices_by_region": prices_by_region,
            "cheapest_region": comparison.cheapest.region if comparison.cheapest else None,
            "most_expensive_region": comparison.most_expensive.region if comparison.most_expensive else None,
            "price_delta_pct": comparison.price_delta_pct,
        }
        if baseline_region:
            result["baseline_region"] = baseline_region
        if errors:
            result["errors"] = errors
        return result

    @mcp.tool()
    async def get_storage_price(
        ctx: Context,
        provider: str,
        storage_type: str,
        region: str,
        size_gb: float = 100.0,
    ) -> dict[str, Any]:
        """
        Get the price for cloud storage in a given region.

        AWS storage types: gp3, gp2, io1, io2, st1, sc1, standard (EBS), or s3 (object storage).
        GCP storage types: pd-ssd, pd-balanced, pd-standard, pd-extreme.

        Args:
            provider: Cloud provider — "aws" or "gcp"
            storage_type: Storage type, e.g. "gp3", "io2", "pd-ssd"
            region: Region code, e.g. "us-east-1"
            size_gb: Storage size in GB for monthly cost estimation (default 100)
        """
        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            prices = await pvdr.get_storage_price(storage_type, region, size_gb)
        except ValueError as e:
            return {"error": str(e)}
        except Exception as e:
            logger.error("get_storage_price error: %s", e)
            return {"error": f"API error: {e}"}

        if not prices:
            return {"result": "no_prices_found", "message": f"No storage pricing for {storage_type} in {region}."}

        result_prices = [p.summary() for p in prices]
        # Add monthly cost estimate for the requested size
        for i, p in enumerate(prices):
            from decimal import Decimal
            monthly = p.price_per_unit * Decimal(str(size_gb))
            result_prices[i]["monthly_estimate_for_size"] = f"${monthly:.2f}/mo for {size_gb} GB"

        return {
            "provider": provider,
            "storage_type": storage_type,
            "region": region,
            "size_gb": size_gb,
            "prices": result_prices,
        }

    @mcp.tool()
    async def get_service_price(
        ctx: Context,
        provider: str,
        service: str,
        region: str,
        filters: dict[str, str] | None = None,
    ) -> dict[str, Any]:
        """
        Get pricing for a specific AWS/GCP service by service code and optional filters.

        Use this to look up pricing for non-compute services such as CloudWatch, RDS,
        Lambda, S3, etc. The service code must be a valid AWS service code (e.g.
        "AmazonCloudWatch", "AmazonRDS") or a recognised alias (e.g. "cloudwatch", "rds").

        Args:
            provider: Cloud provider — "aws" or "gcp"
            service: Service code or alias, e.g. "cloudwatch", "AmazonRDS", "lambda"
            region: Region code, e.g. "us-east-1"
            filters: Optional dict of attribute filters, e.g. {"group": "Metrics"}
        """
        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' is not configured. Available: {list(_providers(ctx))}"}

        # Alias resolution for common service names
        _SERVICE_ALIASES: dict[str, str] = {
            "cloudwatch": "AmazonCloudWatch",
            "rds": "AmazonRDS",
            "s3": "AmazonS3",
            "ec2": "AmazonEC2",
            "lambda": "AWSLambda",
            "dynamodb": "AmazonDynamoDB",
            "sns": "AmazonSNS",
            "sqs": "AmazonSQS",
            "elb": "AWSELB",
            "route53": "AmazonRoute53",
        }
        resolved_service = _SERVICE_ALIASES.get(service.lower(), service)

        try:
            display_name = __import__(
                "opencloudcosts.utils.regions", fromlist=["aws_region_to_display"]
            ).aws_region_to_display(region)
        except (ValueError, Exception):
            display_name = region

        filter_list: list[dict[str, str]] = [
            {"Field": "location", "Value": display_name},
        ]
        if filters:
            for k, v in filters.items():
                filter_list.append({"Field": k, "Value": v})

        try:
            raw = await pvdr._get_products(resolved_service, filter_list, max_results=20)
        except Exception as e:
            logger.error("get_service_price error: %s", e)
            return {"error": f"API error: {e}"}

        prices = []
        for item in raw:
            p = pvdr._item_to_price(item, region, __import__(
                "opencloudcosts.models", fromlist=["PricingTerm"]
            ).PricingTerm.ON_DEMAND, service)
            if p:
                prices.append(p)

        if not prices:
            no_results: dict[str, Any] = {
                "result": "no_results",
                "provider": provider,
                "service": service,
                "region": region,
                "filters_applied": filters or {},
                "message": (
                    f"No pricing found for service '{service}' in {region} "
                    "with the provided filters."
                ),
                "tip": (
                    f"Try search_pricing(provider='{provider}', service='{service}', query='...') "
                    "to explore available products and valid filter attribute names. "
                    "Use list_services() to verify the service code exists."
                ),
            }
            if resolved_service != service:
                no_results["resolved_service_code"] = resolved_service
            return no_results

        return {
            "provider": provider,
            "service": service,
            "resolved_service_code": resolved_service if resolved_service != service else service,
            "region": region,
            "filters_applied": filters or {},
            "count": len(prices),
            "prices": [p.summary() for p in prices],
        }

    @mcp.tool()
    async def search_pricing(
        ctx: Context,
        provider: str,
        query: str,
        service: str = "",
        region: str = "",
        service: str = "",
        max_results: int = 10,
    ) -> dict[str, Any]:
        """
        Search the cloud pricing catalog by keyword across any service.

        For AWS, defaults to EC2 compute search. Set service to search other
        AWS services (e.g. "cloudwatch", "rds", "data_transfer", "lambda").
        For GCP, searches compute instance types.

        Args:
            provider: Cloud provider — "aws" or "gcp"
            query: Search keyword, e.g. "m5", "metric", "MySQL", "egress"
            region: Optional region code to filter results
            service: AWS service to search (default: "ec2"). Use list_services()
                     to discover available service codes.
            max_results: Maximum results to return (default 10, max 50)
        """
        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        max_results = min(max_results, 50)
        try:
            kwargs: dict[str, Any] = {"query": query, "region": region or None,
                                      "max_results": max_results}
            if service and provider == "aws":
                kwargs["service_code"] = service
            prices = await pvdr.search_pricing(**kwargs)
        except Exception as e:
            logger.error("search_pricing error: %s", e)
            return {"error": f"API error: {e}"}

        results = [p.summary() for p in prices]

        if not results:
            tip = (
                "Check the service code with list_services(). "
                "Try a broader query (e.g. the product family name). "
            )
            if service:
                tip += f"Verify that '{service}' is a valid service code or alias."
            return {
                "result": "no_results",
                "provider": provider,
                "service": service or "ec2",
                "query": query,
                "region": region or "any",
                "message": (
                    f"No pricing found matching '{query}'"
                    + (f" in service '{service}'" if service else "")
                    + "."
                ),
                "tip": tip,
            }

        return {
            "provider": provider,
            "query": query,
            "service": service or "ec2",
            "region": region or "all",
            "count": len(results),
            "results": results,
        }

    @mcp.tool()
    async def get_database_price(
        ctx: Context,
        provider: str,
        instance_type: str,
        region: str,
        engine: str = "MySQL",
        deployment: str = "single-az",
        term: str = "on_demand",
    ) -> dict[str, Any]:
        """
        Get the price for a managed database instance (RDS on AWS).

        A dedicated, LLM-friendly tool for database pricing. Handles engine name
        normalization, Multi-AZ vs Single-AZ mapping, and RDS filter attribute names
        automatically — no need to know AWS RDS filter keys.

        Supported engines: MySQL, PostgreSQL, MariaDB, Oracle, SQLServer,
        Aurora-MySQL, Aurora-PostgreSQL.

        Args:
            provider: Cloud provider — "aws" (GCP Cloud SQL planned for Phase 4)
            instance_type: DB instance type, e.g. "db.r5.large", "db.t4g.micro"
            region: Region code, e.g. "us-east-1"
            engine: Database engine — "MySQL" (default), "PostgreSQL", "MariaDB",
                    "Oracle", "SQLServer", "Aurora-MySQL", "Aurora-PostgreSQL"
            deployment: "single-az" (default) or "multi-az"
            term: Pricing term — "on_demand" (default), "reserved_1yr", "reserved_3yr"
        """
        _RDS_ENGINE_MAP = {
            "mysql": "MySQL",
            "postgresql": "PostgreSQL",
            "postgres": "PostgreSQL",
            "mariadb": "MariaDB",
            "oracle": "Oracle",
            "sqlserver": "SQL Server",
            "aurora-mysql": "Aurora MySQL",
            "aurora-postgresql": "Aurora PostgreSQL",
            "aurora-postgres": "Aurora PostgreSQL",
        }

        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        if provider == "gcp":
            return {
                "error": (
                    "GCP database pricing (Cloud SQL) is planned for Phase 4. "
                    "For GCP compute-equivalent sizing, use get_compute_price(provider='gcp', ...)."
                )
            }

        if not hasattr(pvdr, "get_service_price"):
            return {"error": f"Provider '{provider}' does not support database pricing."}

        engine_normalized = _RDS_ENGINE_MAP.get(engine.lower(), engine)
        deployment_option = "Multi-AZ" if deployment.lower() == "multi-az" else "Single-AZ"

        filters: dict[str, str] = {
            "instanceType": instance_type,
            "databaseEngine": engine_normalized,
            "deploymentOption": deployment_option,
        }

        if term.startswith("reserved"):
            filters["termType"] = "Reserved"
            filters["leaseContractLength"] = "1yr" if "1yr" in term else "3yr"
            filters["purchaseOption"] = "No Upfront"

        try:
            prices = await pvdr.get_service_price("rds", region, filters, max_results=5)
        except Exception as e:
            logger.error("get_database_price error: %s", e)
            return {"error": f"API error: {e}"}

        if not prices:
            return {
                "result": "no_prices_found",
                "message": (
                    f"No pricing found for {instance_type} ({engine_normalized}, "
                    f"{deployment_option}) in {region}."
                ),
                "tip": (
                    "Check instance_type format (e.g. 'db.r5.large'), "
                    "engine name, and region."
                ),
            }

        from opencloudcosts.utils.regions import region_display_name as _rdn
        p = prices[0]
        return {
            "provider": provider,
            "instance_type": instance_type,
            "engine": engine_normalized,
            "deployment": deployment,
            "term": term,
            "region": region,
            "region_name": _rdn(provider, region),
            "price_per_hour": f"${p.price_per_unit:.6f}",
            "monthly_estimate": f"${p.monthly_cost:.2f}/mo",
        }

    @mcp.tool()
    async def get_service_price(
        ctx: Context,
        provider: str,
        service: str,
        region: str,
        filters: dict[str, str] | None = None,
        max_results: int = 20,
    ) -> dict[str, Any]:
        """
        Get pricing for any cloud service by service code and attribute filters.

        This is the generic pricing tool — use it for services not covered by
        get_compute_price or get_storage_price: CloudWatch, data transfer, RDS,
        Lambda, ELB, CloudFront, Route53, DynamoDB, EFS, ElastiCache, etc.

        Use list_services() to discover available service codes.
        Use search_pricing(service=...) to explore available products and their
        filter attributes before calling this tool.

        Common service aliases (AWS):
          cloudwatch, data_transfer, rds, lambda, elb, cloudfront, route53,
          dynamodb, efs, elasticache, sqs, sns, redshift, cloudtrail, backup

        Example filters:
          CloudWatch metrics: {"group": "Metric"}
          Data transfer (egress to internet): {"transferType": "AWS Outbound"}
          Data transfer between regions: {"fromRegionCode": "us-east-1",
                                          "toRegionCode": "eu-west-1"}
          RDS MySQL: {"databaseEngine": "MySQL", "instanceType": "db.r5.large"}
          Lambda: {"group": "AWS-Lambda-Duration"}

        Args:
            provider: Cloud provider — "aws" (GCP generic service pricing coming soon)
            service: Service code or alias, e.g. "cloudwatch", "AmazonCloudWatch"
            region: Region code, e.g. "us-east-1"
            filters: Attribute key/value pairs to narrow results (optional)
            max_results: Maximum results to return (default 20)
        """
        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        # GCP compute: delegate to get_compute_price using instanceType filter
        if provider == "gcp" and service in ("compute", "AmazonEC2", "ec2"):
            instance_type = (filters or {}).get("instanceType", "")
            if not instance_type:
                return {
                    "error": (
                        "For GCP compute pricing, provide filters={'instanceType': '<type>'} "
                        "e.g. 'n2-standard-4', 'a3-highgpu-8g'. "
                        "Or use get_compute_price(provider='gcp', instance_type=..., region=...) directly."
                    )
                }
            try:
                from opencloudcosts.models import PricingTerm
                term_str = (filters or {}).get("term", "on_demand")
                os_str = (filters or {}).get("os", "Linux")
                pricing_term = PricingTerm(term_str)
                prices = await pvdr.get_compute_price(instance_type, region, os_str, pricing_term)
            except Exception as e:
                logger.error("get_service_price GCP compute delegation error: %s", e)
                return {"error": f"API error: {e}"}
            if not prices:
                return {
                    "result": "no_prices_found",
                    "message": f"No pricing found for GCP instance '{instance_type}' in {region}.",
                    "tip": "Use list_instance_types(provider='gcp', region=...) to discover available types.",
                }
            return {
                "provider": provider,
                "service": "compute",
                "region": region,
                "instance_type": instance_type,
                "count": len(prices),
                "results": [p.summary() for p in prices],
            }

        if not hasattr(pvdr, "get_service_price"):
            return {
                "error": (
                    f"Provider '{provider}' does not support generic service pricing yet. "
                    "For GCP compute use get_compute_price(provider='gcp', instance_type=..., region=...). "
                    "For GCP storage use get_storage_price(provider='gcp', storage_type=..., region=...)."
                )
            }

        try:
            prices = await pvdr.get_service_price(
                service, region, filters or {}, max_results
            )
        except Exception as e:
            logger.error("get_service_price error: %s", e)
            return {"error": f"API error: {e}"}

        if not prices:
            return {
                "result": "no_prices_found",
                "message": (
                    f"No pricing found for service '{service}' in {region} "
                    f"with filters {filters}. "
                    "Try search_pricing() to explore available products and attributes."
                ),
            }

        return {
            "provider": provider,
            "service": service,
            "region": region,
            "filters_applied": filters or {},
            "count": len(prices),
            "results": [p.summary() for p in prices],
        }

    @mcp.tool()
    async def get_effective_price(
        ctx: Context,
        provider: str,
        service: str,
        instance_type: str,
        region: str,
    ) -> dict[str, Any]:
        """
        Get effective/bespoke pricing for an instance type, reflecting actual account discounts
        (Reserved Instances, Savings Plans, Committed Use Discounts, EDP).

        Requires cloud provider credentials with Cost Explorer (AWS) or billing export (GCP) access.
        For AWS, also requires OCC_AWS_ENABLE_COST_EXPLORER=true (note: costs $0.01/call).

        Args:
            provider: Cloud provider — "aws" or "gcp"
            service: Service type — "compute", "storage", "database"
            instance_type: Instance type, e.g. "m5.xlarge"
            region: Region code, e.g. "us-east-1"
        """
        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            effective = await pvdr.get_effective_price(service, instance_type, region)
        except NotConfiguredError as e:
            return {"error": str(e), "hint": "Configure credentials to enable effective pricing."}
        except Exception as e:
            logger.error("get_effective_price error: %s", e)
            return {"error": f"API error: {e}"}

        if not effective:
            return {"message": "No effective pricing data found for the given parameters."}

        return {
            "provider": provider,
            "instance_type": instance_type,
            "region": region,
            "effective_prices": [
                {
                    "on_demand_rate": f"${ep.base_price.price_per_unit:.6f}/{ep.base_price.unit.value}",
                    "effective_rate": f"${ep.effective_price_per_unit:.6f}/{ep.base_price.unit.value}",
                    "discount_type": ep.discount_type,
                    "discount_pct": f"{ep.discount_pct:.1f}%",
                    "savings": f"${ep.savings_vs_on_demand:.6f}/{ep.base_price.unit.value}",
                    "commitment": ep.commitment_term,
                    "source": ep.source,
                }
                for ep in effective
            ],
        }

    @mcp.tool()
    async def get_discount_summary(
        ctx: Context,
        provider: str = "aws",
    ) -> dict[str, Any]:
        """
        Return a summary of all active cloud discounts for the authenticated account.

        For AWS: lists active Savings Plans (type, commitment $/hr, utilization %) and
        active Reserved Instances (instance type, count, offering type, days remaining),
        plus utilization metrics from Cost Explorer for the previous month.

        Requires credentials and OCC_AWS_ENABLE_COST_EXPLORER=true for AWS.

        Args:
            provider: Cloud provider — "aws" (GCP support coming in Phase 4)
        """
        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            return await pvdr.get_discount_summary()
        except NotConfiguredError as e:
            return {"error": str(e), "hint": "Set OCC_AWS_ENABLE_COST_EXPLORER=true and configure AWS credentials."}
        except Exception as e:
            logger.error("get_discount_summary error: %s", e)
            return {"error": f"API error: {e}"}

    @mcp.tool()
    async def get_prices_batch(
        ctx: Context,
        provider: str,
        instance_types: list[str],
        region: str,
        os: str = "Linux",
        term: str = "on_demand",
    ) -> dict[str, Any]:
        """
        Get prices for multiple instance types in a single region in one call.

        Fetches all prices concurrently. Useful for comparing a shortlist of
        candidate instance types (e.g. m5.xlarge vs c5.xlarge vs r5.xlarge)
        without making separate tool calls.

        Args:
            provider: Cloud provider — "aws" or "gcp"
            instance_types: List of instance types, e.g. ["m5.xlarge", "c5.xlarge", "r5.large"]
            region: Region code, e.g. "us-east-1" or "us-central1"
            os: Operating system — "Linux" (default) or "Windows"
            term: Pricing term — "on_demand" (default), "reserved_1yr", "reserved_3yr", "spot"
        """
        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            pricing_term = PricingTerm(term)
        except ValueError:
            return {"error": f"Unknown term '{term}'. Valid: {[t.value for t in PricingTerm]}"}

        async def fetch_one(itype: str) -> tuple[str, list | str]:
            try:
                prices = await pvdr.get_compute_price(itype, region, os, pricing_term)
                return itype, prices
            except Exception as e:
                return itype, str(e)

        raw = await asyncio.gather(*[fetch_one(t) for t in instance_types])

        results = []
        errors = {}
        for itype, outcome in raw:
            if isinstance(outcome, str):
                errors[itype] = outcome
            elif not outcome:
                errors[itype] = "no pricing found"
            else:
                p = outcome[0]
                results.append({
                    "instance_type": itype,
                    "price_per_hour": f"${p.price_per_unit:.6f}",
                    "monthly_estimate": f"${p.monthly_cost:.2f}/mo",
                    **{k: v for k, v in p.summary().items()
                       if k in ("vcpu", "memory", "description")},
                })

        # Sort by price ascending
        results.sort(key=lambda x: x["price_per_hour"])

        out: dict[str, Any] = {
            "provider": provider,
            "region": region,
            "os": os,
            "term": term,
            "count": len(results),
            "results": results,
        }
        if errors:
            out["errors"] = errors
        return out

    @mcp.tool()
    async def refresh_cache(
        ctx: Context,
        provider: str = "",
    ) -> dict[str, Any]:
        """
        Invalidate the pricing cache to force fresh data on next request.

        Args:
            provider: Provider to clear ("aws", "gcp"), or empty string to clear all.
        """
        cache = ctx.request_context.lifespan_context["cache"]
        if provider:
            counts = await cache.clear_provider(provider)
            return {
                "message": f"Cache cleared for provider: {provider}",
                "prices_deleted": counts["prices_deleted"],
                "metadata_deleted": counts["metadata_deleted"],
            }
        else:
            deleted = await cache.purge_expired()
            stats = await cache.stats()
            return {"message": f"Purged {deleted} expired entries", "cache_stats": stats}
