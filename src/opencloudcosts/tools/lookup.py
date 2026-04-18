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

        For multi-resource stacks (compute + storage + database together), use
        `estimate_bom` instead — it handles all services in one call and surfaces
        hidden costs like egress, load balancers, and monitoring.

        Args:
            provider: Cloud provider — "aws", "gcp", or "azure"
            instance_type: Instance type, e.g. "m5.xlarge" (AWS), "n2-standard-4" (GCP),
                           or "Standard_D4s_v3" (Azure)
            region: Region code, e.g. "us-east-1" (AWS), "us-east1" (GCP),
                    or "eastus" (Azure)
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

        result: dict[str, Any] = {
            "provider": provider,
            "instance_type": instance_type,
            "region": region,
            "os": os,
            "term": term,
            "prices": [p.summary() for p in prices],
            "count": len(prices),
        }

        # When a reserved term is returned, surface the sibling payment options so
        # the LLM knows to fetch them for a proper upfront vs no-upfront comparison.
        _TERM_LABEL = {
            "reserved_1yr":         "No Upfront",
            "reserved_1yr_partial": "Partial Upfront",
            "reserved_1yr_all":     "All Upfront",
            "reserved_3yr":         "No Upfront",
            "reserved_3yr_partial": "Partial Upfront",
            "reserved_3yr_all":     "All Upfront",
        }
        _RESERVED_SIBLINGS: dict[str, list[str]] = {
            "reserved_1yr":         ["reserved_1yr_partial", "reserved_1yr_all"],
            "reserved_1yr_partial": ["reserved_1yr", "reserved_1yr_all"],
            "reserved_1yr_all":     ["reserved_1yr", "reserved_1yr_partial"],
            "reserved_3yr":         ["reserved_3yr_partial", "reserved_3yr_all"],
            "reserved_3yr_partial": ["reserved_3yr", "reserved_3yr_all"],
            "reserved_3yr_all":     ["reserved_3yr", "reserved_3yr_partial"],
        }
        if term in _RESERVED_SIBLINGS and provider == "aws":
            siblings = _RESERVED_SIBLINGS[term]
            result["see_also"] = (
                f"This is the {_TERM_LABEL[term]} rate. "
                f"To compare all payment options call get_compute_price with "
                f"term='{siblings[0]}' ({_TERM_LABEL[siblings[0]]}) and "
                f"term='{siblings[1]}' ({_TERM_LABEL[siblings[1]]}). "
                f"Each returns an effective hourly rate with the upfront cost normalised in."
            )

        return result

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
            provider: Cloud provider — "aws", "gcp", or "azure"
            instance_type: Instance type, e.g. "m5.xlarge" (AWS), "n2-standard-4" (GCP),
                           or "Standard_D4s_v3" (Azure)
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

        # Detect cross-geography comparisons and surface data transfer pricing hint.
        # AWS region prefixes map to broad geographies; different prefixes = cross-geo.
        if provider == "aws" and len(regions) >= 2:
            def _geo(r: str) -> str:
                if r.startswith("us-") or r.startswith("ca-"):
                    return "americas"
                if r.startswith("eu-") or r.startswith("me-") or r.startswith("af-") or r.startswith("il-"):
                    return "emea"
                if r.startswith("ap-") or r.startswith("sa-"):
                    return "apac"
                return r  # unknown — treat as unique

            geos = {_geo(r) for r in regions}
            if len(geos) > 1:
                # Pick the two regions to suggest for data transfer lookup
                src = baseline_region if baseline_region and baseline_region in regions else regions[0]
                dst = next(r for r in regions if r != src)
                result["migration_note"] = (
                    f"These regions span different geographies. "
                    f"Cross-region data egress adds to your total cost — "
                    f"use get_service_price(provider=\"aws\", service=\"data_transfer\", "
                    f"region=\"{src}\", filters={{\"fromRegionCode\": \"{src}\", "
                    f"\"toRegionCode\": \"{dst}\"}}) to price outbound transfer."
                )

        return result

    @mcp.tool()
    async def get_storage_price(
        ctx: Context,
        provider: str,
        storage_type: str,
        region: str,
        size_gb: float = 100.0,
        iops: int | None = None,
    ) -> dict[str, Any]:
        """
        Get the price for cloud storage in a given region.

        AWS storage types: gp3, gp2, io1, io2, st1, sc1, standard (EBS), or s3 (object storage).
        GCP storage types: pd-ssd, pd-balanced, pd-standard, pd-extreme (Persistent Disk),
            or standard, nearline, coldline, archive (GCS object storage).
        Azure storage types: premium-ssd, standard-ssd, standard-hdd, ultra-ssd, blob.

        GCS examples:
            get_storage_price(provider="gcp", storage_type="standard", region="us-central1")
            get_storage_price(provider="gcp", storage_type="nearline", region="us-central1")
            get_storage_price(provider="gcp", storage_type="coldline", region="us-central1")
            get_storage_price(provider="gcp", storage_type="archive", region="us-central1")

        For multi-resource stacks (compute + storage + database together), use
        `estimate_bom` instead — it handles all services in one call and surfaces
        hidden costs like egress, load balancers, and monitoring.

        For io1/io2 EBS volumes, pass the `iops` parameter to get the per-IOPS-month
        rate alongside the per-GB-month rate, and a combined monthly estimate.

        Args:
            provider: Cloud provider — "aws", "gcp", or "azure"
            storage_type: Storage type, e.g. "gp3" (AWS), "pd-ssd" (GCP), "premium-ssd" (Azure)
            region: Region code, e.g. "us-east-1"
            size_gb: Storage size in GB for monthly cost estimation (default 100)
            iops: Provisioned IOPS count (for io1/io2 EBS only). When provided, the
                  per-IOPS-month rate is returned and included in the monthly estimate.
        """
        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            prices = await pvdr.get_storage_price(storage_type, region, size_gb, iops)
        except TypeError:
            # Provider doesn't accept iops parameter (e.g. GCP/Azure) — call without it
            prices = await pvdr.get_storage_price(storage_type, region, size_gb)
        except ValueError as e:
            return {"error": str(e)}
        except Exception as e:
            logger.error("get_storage_price error: %s", e)
            return {"error": f"API error: {e}"}

        if not prices:
            return {"result": "no_prices_found", "message": f"No storage pricing for {storage_type} in {region}."}

        from decimal import Decimal
        from opencloudcosts.models import PriceUnit as _PriceUnit

        result_prices = [p.summary() for p in prices]
        # Compute monthly cost estimate for the requested size.
        # For PER_GB_MONTH SKUs the summary() leaves monthly_estimate=null;
        # fill it in here so the LLM always receives a concrete dollar figure.
        storage_monthly: Decimal | None = None
        iops_monthly: Decimal | None = None
        for i, p in enumerate(prices):
            if p.unit == _PriceUnit.PER_GB_MONTH:
                monthly = p.price_per_unit * Decimal(str(size_gb))
                storage_monthly = monthly
                result_prices[i]["monthly_estimate"] = f"${monthly:.2f}/mo"
                result_prices[i]["monthly_estimate_for_size"] = f"${monthly:.2f}/mo for {size_gb} GB"
            elif p.unit == _PriceUnit.PER_IOPS_MONTH:
                if iops is not None and iops > 0:
                    monthly = p.price_per_unit * Decimal(str(iops))
                    iops_monthly = monthly
                    result_prices[i]["monthly_estimate"] = f"${monthly:.2f}/mo"
                    result_prices[i]["monthly_estimate_for_iops"] = f"${monthly:.2f}/mo for {iops} IOPS"
            elif p.unit == _PriceUnit.PER_HOUR:
                # hourly-billed storage (unusual but possible): leave monthly_estimate as-is
                # but still add the per-size helper key so the response is consistent
                monthly = p.price_per_unit * Decimal("730")
                result_prices[i]["monthly_estimate_for_size"] = f"${monthly:.2f}/mo for {size_gb} GB"
            else:
                monthly = p.price_per_unit * Decimal(str(size_gb))
                result_prices[i]["monthly_estimate_for_size"] = f"${monthly:.2f}/mo for {size_gb} GB"

        response: dict[str, Any] = {
            "provider": provider,
            "storage_type": storage_type,
            "region": region,
            "size_gb": size_gb,
            "prices": result_prices,
        }

        # For io1/io2 with IOPS specified, add a combined estimate and explanatory note
        if iops is not None and iops > 0 and storage_monthly is not None and iops_monthly is not None:
            combined = storage_monthly + iops_monthly
            response["iops"] = iops
            response["monthly_estimate"] = f"${combined:.2f}/mo"
            response["note"] = (
                "For io2/io1, total monthly cost = storage cost + IOPS cost. "
                "Both components are shown above."
            )
        elif iops is not None and iops > 0 and storage_monthly is not None:
            response["iops"] = iops

        return response

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

        Lambda pricing requires two separate calls (each has its own filter group):
            Duration: get_service_price(provider="aws", service="lambda", region="us-east-1",
                        filters={"group": "AWS-Lambda-Duration"})  → per GB-second rate
            Requests: get_service_price(provider="aws", service="lambda", region="us-east-1",
                        filters={"group": "AWS-Lambda-Requests"})  → per request rate
            Free tier: first 1M requests/month and 400,000 GB-seconds/month.

        Data transfer between regions:
            get_service_price(provider="aws", service="data_transfer", region="us-east-1",
                              filters={"fromRegionCode": "us-east-1", "toRegionCode": "eu-west-1"})
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
            "gcs": "CloudStorage",
            "cloud-storage": "CloudStorage",
            "cloud-sql": "CloudSQL",
            "cloudsql": "CloudSQL",
            "gke": "KubernetesEngine",
            "kubernetes": "KubernetesEngine",
            "k8s": "KubernetesEngine",
            "memorystore": "MemorystoreRedis",
            "redis": "MemorystoreRedis",
            "memorystore-redis": "MemorystoreRedis",
            "bigquery": "BigQuery",
            "bq": "BigQuery",
            "vertex": "VertexAI",
            "vertex-ai": "VertexAI",
            "vertexai": "VertexAI",
            "gemini": "Gemini",
            "load-balancer": "CloudLoadBalancing",
            "loadbalancer": "CloudLoadBalancing",
            "lb": "CloudLoadBalancing",
            "cloud-lb": "CloudLoadBalancing",
            "cdn": "CloudCDN",
            "cloud-cdn": "CloudCDN",
            "nat": "CloudNAT",
            "cloud-nat": "CloudNAT",
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

        if not hasattr(pvdr, '_get_products'):
            return {
                "error": f"get_service_price is not supported for {provider} — "
                         f"use get_compute_price or get_storage_price instead. "
                         f"Azure service-level pricing is not available via this tool.",
                "provider": provider,
                "service": service,
            }

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
        max_results: int = 10,
    ) -> dict[str, Any]:
        """
        Search the cloud pricing catalog by keyword across any service.

        For AWS, defaults to EC2 compute search. Set service to search other
        AWS services (e.g. "cloudwatch", "rds", "data_transfer", "lambda").
        For GCP, searches compute instance types.

        Args:
            provider: Cloud provider — "aws", "gcp", or "azure"
            query: Search keyword, e.g. "m5", "D4s", "Standard_D", "egress"
            region: Optional region code to filter results
            service: AWS service to search (default: "ec2"). Use list_services()
                     to discover available service codes. Ignored for GCP and Azure.
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

        response: dict[str, Any] = {
            "provider": provider,
            "query": query,
            "service": service or "ec2",
            "region": region or "all",
            "count": len(results),
            "results": results,
        }

        # If any result looks like a data transfer SKU, add a hint so the LLM
        # knows the correct tool call pattern for inter-region egress pricing.
        if any(
            "transfer" in (r.get("service", "") + r.get("description", "")).lower()
            for r in results
        ):
            response["data_transfer_tip"] = (
                "For inter-region data transfer pricing, use: "
                "get_service_price(provider='aws', service='data_transfer', region='<source-region>', "
                "filters={'fromRegionCode': '<source>', 'toRegionCode': '<dest>'})"
            )

        return response

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

        For multi-resource stacks (compute + storage + database together), use
        `estimate_bom` instead — it handles all services in one call and surfaces
        hidden costs like egress, load balancers, and monitoring.

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
            ha = deployment.lower() in ("multi-az", "ha", "regional", "multi-zone")
            engine_norm = {
                "mysql": "MySQL",
                "postgresql": "PostgreSQL",
                "postgres": "PostgreSQL",
                "sqlserver": "SQL Server",
            }.get(engine.lower(), engine)
            if not hasattr(pvdr, "get_cloud_sql_price"):
                return {"error": "GCP Cloud SQL pricing not available."}
            try:
                prices = await pvdr.get_cloud_sql_price(instance_type, region, engine_norm, ha)
            except ValueError as e:
                return {"error": str(e)}
            except Exception as e:
                logger.error("get_database_price GCP error: %s", e)
                return {"error": f"API error: {e}"}
            if not prices:
                return {"error": f"No Cloud SQL pricing found for {instance_type} in {region}."}
            p = prices[0]
            return {
                "provider": "gcp",
                "service": "cloud_sql",
                "instance_type": instance_type,
                "engine": engine_norm,
                "ha": ha,
                "region": p.region,
                "pricing_term": p.pricing_term.value,
                "price_per_hour": f"${p.price_per_unit:.6f}",
                "monthly_estimate": f"${p.monthly_cost:.2f}/mo",
            }

        if not hasattr(pvdr, "get_service_price"):
            return {"error": f"Provider '{provider}' does not support database pricing."}

        engine_normalized = _RDS_ENGINE_MAP.get(engine.lower(), engine)
        deployment_option = "Multi-AZ" if deployment.lower() == "multi-az" else "Single-AZ"

        try:
            pricing_term = PricingTerm(term)
        except ValueError:
            return {"error": f"Unknown term '{term}'. Valid: {[t.value for t in PricingTerm]}"}

        # Only product-level attributes go into filters; term routing is handled by
        # the `term` parameter so that _item_to_price reads the right bucket.
        filters: dict[str, str] = {
            "instanceType": instance_type,
            "databaseEngine": engine_normalized,
            "deploymentOption": deployment_option,
        }

        try:
            prices = await pvdr.get_service_price("rds", region, filters, max_results=5, term=pricing_term)
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
          dynamodb, efs, elasticache, sqs, sns, redshift, cloudtrail, backup,
          sagemaker, fargate, ecs, bedrock

        Example filters:
          CloudWatch metrics: {"group": "Metric"}
          Data transfer (egress to internet): {"transferType": "AWS Outbound"}
          Data transfer between regions: {"fromRegionCode": "us-east-1",
                                          "toRegionCode": "eu-west-1"}
          RDS MySQL: {"databaseEngine": "MySQL", "instanceType": "db.r5.large"}
          Lambda: {"group": "AWS-Lambda-Duration"}
          SageMaker training: {"instanceType": "ml.m5.xlarge-Training"}
          SageMaker by component: {"component": "Training"}

        Data transfer (inter-region egress):
          get_service_price(provider="aws", service="data_transfer", region="us-east-1",
                            filters={"fromRegionCode": "us-east-1", "toRegionCode": "eu-west-1"})
          # Returns tiered egress rates (first 10TB, next 40TB, etc.)
          # Note: AWS charges based on the source region; set region= to the source.

        SageMaker instance pricing:
          get_service_price(provider="aws", service="sagemaker", region="us-east-1",
                            filters={"instanceType": "ml.m5.xlarge-Training"})
          # Suffix -Training, -Hosting, or -Notebook must be appended to instanceType.
          # Or use: filters={"component": "Training"} to list all training prices.

        Args:
            provider: Cloud provider — "aws" (GCP generic service pricing coming soon)
            service: Service code or alias, e.g. "cloudwatch", "AmazonCloudWatch"
            region: Region code, e.g. "us-east-1"
            filters: Attribute key/value pairs to narrow results (optional).
                For data transfer: use {"fromRegionCode": "us-east-1", "toRegionCode": "eu-west-1"}.
                The "region" param should be the source (egress) region.
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

        if provider == "gcp":
            return {
                "error": (
                    "GCP effective pricing via BigQuery billing export is planned for Phase 4 "
                    "and not yet available. "
                    "For committed-use discount list prices, use: "
                    "get_compute_price(provider='gcp', term='cud_1yr') or term='cud_3yr'."
                )
            }

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

        if provider == "gcp":
            return {
                "error": (
                    "GCP discount summary via BigQuery billing export is planned for Phase 4 "
                    "and not yet available. "
                    "GCP committed-use discounts (CUDs) are available as list prices via "
                    "get_compute_price(provider='gcp', term='cud_1yr' or 'cud_3yr')."
                )
            }

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
    async def get_spot_history(
        ctx: Context,
        provider: str,
        instance_type: str,
        region: str,
        availability_zone: str = "",
        os: str = "Linux",
        hours: int = 24,
    ) -> dict[str, Any]:
        """
        Get spot price history and stability analysis for an AWS instance type.

        Returns per-AZ spot price statistics (current, min, max, avg, sample count),
        overall volatility ratio, a stability label (stable/moderate/volatile), and
        an actionable recommendation. Requires AWS credentials.

        Args:
            provider: Cloud provider — must be "aws"
            instance_type: Instance type, e.g. "m5.xlarge"
            region: Region code, e.g. "us-east-1"
            availability_zone: Filter to a specific AZ, e.g. "us-east-1a". Empty = all AZs.
            os: Operating system — "Linux" (default) or "Windows"
            hours: Lookback window in hours (default 24, max 720)
        """
        if provider != "aws":
            return {
                "error": (
                    f"get_spot_history only supports provider='aws'. "
                    f"Got '{provider}'."
                )
            }

        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' is not configured. Available: {list(_providers(ctx))}"}

        try:
            result = await pvdr._get_spot_history(
                instance_type=instance_type,
                region=region,
                os=os,
                availability_zone=availability_zone,
                hours=hours,
            )
        except ValueError as e:
            return {"error": str(e)}
        except Exception as e:
            logger.error("get_spot_history error: %s", e)
            return {"error": f"API error: {e}"}

        if not result:
            from opencloudcosts.utils.regions import region_display_name as _rdn
            return {
                "result": "no_data",
                "message": (
                    f"No spot price history found for {instance_type} in {region}. "
                    "Check instance type spelling or try a different region."
                ),
                "provider": provider,
                "region_name": _rdn(provider, region),
            }

        from opencloudcosts.utils.regions import region_display_name as _rdn
        result["provider"] = provider
        result["region_name"] = _rdn(provider, region)
        return result

    # -----------------------------------------------------------------------
    # Fargate pricing
    # -----------------------------------------------------------------------

    # Bedrock model alias map — slug -> AWS catalog name
    _BEDROCK_MODEL_ALIASES: dict[str, str] = {
        "claude-3-5-sonnet": "Claude 3.5 Sonnet",
        "claude-3-5-haiku": "Claude 3.5 Haiku",
        "claude-3-7-sonnet": "Claude 3.7 Sonnet",
        "claude-3-opus": "Claude 3 Opus",
        "claude-3-sonnet": "Claude 3 Sonnet",
        "claude-3-haiku": "Claude 3 Haiku",
        "nova-pro": "Nova Pro",
        "nova-lite": "Nova Lite",
        "nova-micro": "Nova Micro",
        "llama-3-1-70b": "Llama 3.1 70B",
        "llama-3-1-8b": "Llama 3.1 8B",
        "llama-3-2-90b": "Llama 3.2 90B",
        "llama-3-2-11b": "Llama 3.2 11B",
        "llama-3-2-3b": "Llama 3.2 3B",
        "llama-3-2-1b": "Llama 3.2 1B",
        "mistral-large": "Mistral Large",
        "mistral-small": "Mistral Small",
        "titan-text-express": "Titan Text G1 - Express",
        "titan-text-lite": "Titan Text G1 - Lite",
        "titan-text-premier": "Titan Text Premier",
        "titan-embeddings": "Titan Embeddings G1 - Text",
        "cohere-command-r": "Cohere Command R",
        "cohere-command-r-plus": "Cohere Command R+",
        "gemma-2-27b": "Gemma 2 27B IT",
        "gemma-2-9b": "Gemma 2 9B IT",
        "jamba-1-5-large": "AI21 Jamba 1.5 Large",
    }

    @mcp.tool()
    async def get_fargate_price(
        ctx: Context,
        region: str,
        vcpu: float,
        memory_gb: float,
        os: str = "Linux",
        hours_per_month: float = 730.0,
    ) -> dict[str, Any]:
        """
        Get AWS Fargate task pricing for a given vCPU + memory configuration.

        Fargate bills per-vCPU-hour and per-GB-hour separately. This tool fetches
        both rates and combines them into a total hourly and monthly cost.

        Valid task sizes (vCPU -> memory range):
          0.25 vCPU: 0.5-2 GB | 0.5 vCPU: 1-4 GB | 1 vCPU: 2-8 GB
          2 vCPU: 4-16 GB | 4 vCPU: 8-30 GB | 8 vCPU: 16-60 GB | 16 vCPU: 32-120 GB

        Args:
            region: AWS region code, e.g. "us-east-1"
            vcpu: Task vCPU count (e.g. 0.25, 0.5, 1, 2, 4, 8, 16)
            memory_gb: Task memory in GB (e.g. 0.5, 2, 4, 8, 16, 30)
            os: "Linux" (default) or "Windows"
            hours_per_month: Hours per month the task runs (default 730 = always-on)
        """
        pvdr = _providers(ctx).get("aws")
        if pvdr is None:
            return {"error": "AWS provider is not configured."}

        if not hasattr(pvdr, "get_service_price"):
            return {"error": "AWS provider does not support get_service_price."}

        # Build vCPU rate filters
        vcpu_filters: dict[str, str] = {"cputype": "perCPU"}
        mem_filters: dict[str, str] = {"memorytype": "perGB"}
        if os.lower() == "windows":
            vcpu_filters["operatingSystem"] = "Windows"

        try:
            vcpu_prices = await pvdr.get_service_price(
                "fargate", region, vcpu_filters, max_results=5
            )
            mem_prices = await pvdr.get_service_price(
                "fargate", region, mem_filters, max_results=5
            )
        except Exception as e:
            logger.error("get_fargate_price error: %s", e)
            return {"error": f"API error: {e}"}

        if not vcpu_prices:
            return {
                "result": "no_prices_found",
                "message": f"No Fargate vCPU pricing found in {region} for os={os}.",
                "tip": "Check region code and os parameter ('Linux' or 'Windows').",
            }
        if not mem_prices:
            return {
                "result": "no_prices_found",
                "message": f"No Fargate memory pricing found in {region}.",
                "tip": "Check region code.",
            }

        from decimal import Decimal as _D
        vcpu_rate = vcpu_prices[0].price_per_unit
        mem_rate = mem_prices[0].price_per_unit

        hourly_total = vcpu_rate * _D(str(vcpu)) + mem_rate * _D(str(memory_gb))
        monthly_total = hourly_total * _D(str(hours_per_month))

        return {
            "provider": "aws",
            "region": region,
            "os": os,
            "vcpu": vcpu,
            "memory_gb": memory_gb,
            "hours_per_month": hours_per_month,
            "vcpu_rate_per_hour": f"${vcpu_rate:.6f}",
            "memory_rate_per_gb_hour": f"${mem_rate:.6f}",
            "hourly_total": f"${hourly_total:.6f}",
            "monthly_total": f"${monthly_total:.2f}",
            "formula": (
                f"hourly = {vcpu} vCPU * ${vcpu_rate:.6f}/vCPU-hr "
                f"+ {memory_gb} GB * ${mem_rate:.6f}/GB-hr"
            ),
        }

    @mcp.tool()
    async def get_bedrock_price(
        ctx: Context,
        model: str,
        region: str = "us-east-1",
        input_tokens: int = 1000000,
        output_tokens: int = 1000000,
        mode: str = "on_demand",
    ) -> dict[str, Any]:
        """
        Get AWS Bedrock model inference pricing.

        Bedrock bills per 1,000 tokens (input and output separately). Fetches both
        rates and calculates total cost for the given monthly token volumes.

        Args:
            model: Model identifier — canonical name or alias:
                   Anthropic Claude: "claude-3-5-sonnet", "claude-3-5-haiku",
                     "claude-3-7-sonnet", "claude-3-opus", "claude-3-sonnet", "claude-3-haiku"
                   Amazon Nova: "nova-pro", "nova-lite", "nova-micro"
                   Meta Llama: "llama-3-1-70b", "llama-3-1-8b", "llama-3-2-90b"
                   Mistral: "mistral-large", "mistral-small"
                   Pass the full AWS catalog name (e.g. "Claude 3.5 Sonnet") to skip alias lookup.
            region: AWS region (default "us-east-1"). Not all models are in all regions.
            input_tokens: Monthly input token volume (default 1,000,000 = 1M tokens)
            output_tokens: Monthly output token volume (default 1,000,000 = 1M tokens)
            mode: "on_demand" (default) or "batch" (50% discount, async only)
        """
        pvdr = _providers(ctx).get("aws")
        if pvdr is None:
            return {"error": "AWS provider is not configured."}

        if not hasattr(pvdr, "get_service_price"):
            return {"error": "AWS provider does not support get_service_price."}

        # Resolve alias
        catalog_name = _BEDROCK_MODEL_ALIASES.get(model.lower(), model)

        if mode == "batch":
            input_inference_type = "input tokens batch"
            output_inference_type = "output tokens batch"
        else:
            input_inference_type = "Input tokens"
            output_inference_type = "Output tokens"

        input_filters: dict[str, str] = {
            "model": catalog_name,
            "inferenceType": input_inference_type,
        }
        output_filters: dict[str, str] = {
            "model": catalog_name,
            "inferenceType": output_inference_type,
        }

        try:
            input_prices = await pvdr.get_service_price(
                "bedrock", region, input_filters, max_results=5
            )
            output_prices = await pvdr.get_service_price(
                "bedrock", region, output_filters, max_results=5
            )
        except Exception as e:
            logger.error("get_bedrock_price error: %s", e)
            return {"error": f"API error: {e}"}

        if not input_prices:
            return {
                "result": "no_prices_found",
                "model": catalog_name,
                "region": region,
                "mode": mode,
                "message": (
                    f"No Bedrock input token pricing found for model '{catalog_name}' "
                    f"in {region} (mode={mode})."
                ),
                "tip": (
                    "Check model name and region. Not all models are available in all regions. "
                    "Pass the full AWS catalog name (e.g. 'Claude 3.5 Sonnet') to bypass alias lookup."
                ),
            }
        if not output_prices:
            return {
                "result": "no_prices_found",
                "model": catalog_name,
                "region": region,
                "mode": mode,
                "message": (
                    f"No Bedrock output token pricing found for model '{catalog_name}' "
                    f"in {region} (mode={mode})."
                ),
            }

        from decimal import Decimal as _D
        input_rate = input_prices[0].price_per_unit   # price per 1k tokens
        output_rate = output_prices[0].price_per_unit

        input_cost = input_rate * _D(str(input_tokens)) / _D("1000")
        output_cost = output_rate * _D(str(output_tokens)) / _D("1000")
        total_cost = input_cost + output_cost

        return {
            "model": catalog_name,
            "region": region,
            "mode": mode,
            "input_rate_per_1k_tokens": f"${input_rate:.6f}",
            "output_rate_per_1k_tokens": f"${output_rate:.6f}",
            "input_tokens": input_tokens,
            "output_tokens": output_tokens,
            "monthly_input_cost": f"${input_cost:.4f}",
            "monthly_output_cost": f"${output_cost:.4f}",
            "monthly_total_cost": f"${total_cost:.4f}",
            "note": "Prices are per 1,000 tokens. Batch mode is 50% cheaper but async-only.",
        }

    @mcp.tool()
    async def get_gke_price(
        ctx: Context,
        region: str,
        mode: str = "standard",
        node_count: int = 3,
        node_type: str = "n1-standard-4",
        vcpu: float = 0.0,
        memory_gb: float = 0.0,
        hours_per_month: float = 730.0,
    ) -> dict[str, Any]:
        """
        Get GKE (Google Kubernetes Engine) pricing.

        Two billing modes:

        Standard mode (default):
          - Fixed cluster management fee per hour (regardless of node count/size)
          - Worker nodes are billed as regular Compute Engine VMs — use get_compute_price
            for node costs and multiply by node_count.
          - Example: get_gke_price(region="us-central1", mode="standard", node_count=3,
              node_type="n2-standard-4")

        Autopilot mode:
          - No cluster management fee — pay only for pod resource requests
          - Billed per vCPU-hour and per GiB-RAM-hour for pod requests
          - Example: get_gke_price(region="us-central1", mode="autopilot",
              vcpu=4.0, memory_gb=16.0)

        Args:
            region: GCP region, e.g. "us-central1", "europe-west1"
            mode: "standard" (node-based) or "autopilot" (pod-based)
            node_count: Number of worker nodes (standard mode only, for hint)
            node_type: VM type for worker nodes (standard mode only, for hint)
            vcpu: vCPUs requested by pods (autopilot mode only)
            memory_gb: Memory in GB requested by pods (autopilot mode only)
            hours_per_month: Hours per month (default 730 = always-on)
        """
        pvdr = _providers(ctx).get("gcp")
        if pvdr is None:
            return {"error": "GCP provider not configured."}
        if not hasattr(pvdr, "get_gke_price"):
            return {"error": "GKE pricing not available."}
        if mode not in ("standard", "autopilot"):
            return {"error": f"Unknown mode '{mode}'. Use 'standard' or 'autopilot'."}
        try:
            result = await pvdr.get_gke_price(
                region=region, mode=mode, node_count=node_count,
                node_type=node_type, vcpu=vcpu, memory_gb=memory_gb,
                hours_per_month=hours_per_month,
            )
            return result
        except NotConfiguredError as e:
            return {"error": str(e)}
        except Exception as e:
            logger.error("get_gke_price error: %s", e)
            return {"error": f"Pricing lookup failed: {e}"}

    @mcp.tool()
    async def get_memorystore_price(
        ctx: Context,
        capacity_gb: float,
        region: str,
        tier: str = "standard",
        hours_per_month: float = 730.0,
    ) -> dict[str, Any]:
        """
        Get GCP Memorystore for Redis pricing.

        Memorystore is billed per GiB-hour of provisioned capacity. Two tiers:
          - basic: single zone, no HA, lowest cost
          - standard: HA with cross-zone replication (recommended for production)

        Standard tier typically costs ~1.3-2x more than Basic depending on region.

        Args:
            capacity_gb: Provisioned memory capacity in GB (e.g. 5.0, 10.0, 100.0)
            region: GCP region, e.g. "us-central1", "europe-west1"
            tier: "basic" (single zone) or "standard" (HA, recommended)
            hours_per_month: Hours per month (default 730 = always-on)

        Example:
            get_memorystore_price(capacity_gb=10.0, region="us-central1", tier="standard")
            get_memorystore_price(capacity_gb=5.0, region="europe-west1", tier="basic")
        """
        pvdr = _providers(ctx).get("gcp")
        if pvdr is None:
            return {"error": "GCP provider is not configured."}

        if not hasattr(pvdr, "get_memorystore_price"):
            return {"error": "Memorystore pricing not available."}

        try:
            prices = await pvdr.get_memorystore_price(
                capacity_gb=capacity_gb,
                region=region,
                tier=tier,
                hours_per_month=hours_per_month,
            )
        except ValueError as e:
            return {"error": str(e)}
        except Exception as e:
            logger.error("get_memorystore_price error: %s", e)
            return {"error": f"API error: {e}"}

        if not prices:
            return {
                "result": "no_prices_found",
                "message": f"No Memorystore pricing found for tier={tier} in {region}.",
            }

        from decimal import Decimal
        p = prices[0]
        raw_rate = Decimal(p.attributes.get("rate_per_gib_hr", "0"))

        return {
            "provider": "gcp",
            "service": "memorystore_redis",
            "tier": tier,
            "capacity_gb": capacity_gb,
            "region": p.region,
            "rate_per_gib_hr": f"${raw_rate:.6f}/GiB-hr",
            "hourly_cost": f"${p.price_per_unit:.4f}/hr",
            "monthly_cost": f"${p.monthly_cost:.2f}/mo",
            "note": "Memorystore Standard includes HA (2-zone replication). Basic is single-zone.",
        }

    @mcp.tool()
    async def get_bigquery_price(
        ctx: Context,
        region: str = "us",
        query_tb: float | None = None,
        active_storage_gb: float | None = None,
        longterm_storage_gb: float | None = None,
        streaming_gb: float | None = None,
    ) -> dict[str, Any]:
        """Get BigQuery pricing for storage, analysis queries, and streaming inserts.

        BigQuery uses on-demand (per-TiB) query pricing plus per-GiB-month
        storage pricing. Multi-region locations ("us", "eu") are cheaper for
        storage than single-region. Pass quantities to get cost estimates.

        Args:
            region: BigQuery location — multi-region "us" (default) or "eu",
                    or single region e.g. "us-central1", "europe-west1".
                    Single-region queries fall back to multi-region rates if
                    no single-region SKU is found.
            query_tb: TiB of data scanned per month (on-demand query cost).
                      Omit to return rates only.
            active_storage_gb: GB of active storage (data modified in last 90 days).
            longterm_storage_gb: GB of long-term storage (data not modified > 90 days,
                                 roughly half the active storage rate).
            streaming_gb: GB of streaming inserts per month.

        Examples:
            get_bigquery_price(region="us")
            get_bigquery_price(region="us", query_tb=10.0, active_storage_gb=500.0)
            get_bigquery_price(region="europe-west1", longterm_storage_gb=1000.0)
        """
        pvdr = _providers(ctx).get("gcp")
        if pvdr is None:
            return {"error": "GCP provider is not configured."}

        if not hasattr(pvdr, "get_bigquery_price"):
            return {"error": "BigQuery pricing not available."}

        try:
            result = await pvdr.get_bigquery_price(
                region=region,
                query_tb=query_tb,
                active_storage_gb=active_storage_gb,
                longterm_storage_gb=longterm_storage_gb,
                streaming_gb=streaming_gb,
            )
        except Exception as e:
            logger.error("get_bigquery_price error: %s", e)
            return {"error": f"API error: {e}"}

        return result

    @mcp.tool()
    async def get_vertex_price(
        ctx: Context,
        machine_type: str,
        region: str = "us-central1",
        hours: float = 730.0,
        task: str = "training",
    ) -> dict[str, Any]:
        """
        Get Vertex AI custom training / prediction compute pricing.

        Returns per-vCPU-hour and per-GiB-RAM-hour rates for the given machine
        type family, looked up from the Vertex AI SKU catalog.

        Vertex AI bills custom training and prediction jobs by machine type.
        Rates are returned as unit prices — multiply by vCPU count and RAM GiB
        for the actual machine type to get total cost.

        Args:
            machine_type: GCP machine type, e.g. "n1-standard-4", "a2-highgpu-1g"
            region: GCP region, e.g. "us-central1", "europe-west4"
            hours: Estimated runtime hours (default 730 = one month always-on)
            task: "training" (default) or "prediction"

        Examples:
            get_vertex_price(machine_type="n1-standard-4", region="us-central1", task="training")
            get_vertex_price(machine_type="a2-highgpu-1g", region="us-central1", task="prediction")
        """
        pvdr = _providers(ctx).get("gcp")
        if pvdr is None:
            return {"error": "GCP provider not configured."}
        if not hasattr(pvdr, "get_vertex_price"):
            return {"error": "Vertex AI pricing not available."}
        if task not in ("training", "prediction"):
            return {"error": f"Unknown task '{task}'. Use 'training' or 'prediction'."}
        try:
            result = await pvdr.get_vertex_price(
                machine_type=machine_type,
                region=region,
                hours=hours,
                task=task,
            )
            return result
        except NotConfiguredError as e:
            return {"error": str(e)}
        except Exception as e:
            logger.error("get_vertex_price error: %s", e)
            return {"error": f"Pricing lookup failed: {e}"}

    @mcp.tool()
    async def get_gemini_price(
        ctx: Context,
        model: str = "gemini-1.5-flash",
        region: str = "us-central1",
    ) -> dict[str, Any]:
        """
        Get Vertex AI Gemini generative model token / character pricing.

        Returns input and output pricing rates for the specified Gemini model
        as found in the Vertex AI SKU catalog. Rates may be per-character or
        per-token depending on the model generation.

        Args:
            model: Gemini model name substring, e.g. "gemini-1.5-flash",
                   "gemini-1.0-pro", "gemini-1.5-pro"
            region: GCP region, e.g. "us-central1" (most Gemini SKUs are global
                    or us-central1 — try "us-central1" if another region returns nothing)

        Examples:
            get_gemini_price(model="gemini-1.5-flash", region="us-central1")
            get_gemini_price(model="gemini-1.0-pro")
        """
        pvdr = _providers(ctx).get("gcp")
        if pvdr is None:
            return {"error": "GCP provider not configured."}
        if not hasattr(pvdr, "get_gemini_price"):
            return {"error": "Gemini pricing not available."}
        try:
            result = await pvdr.get_gemini_price(
                model=model,
                region=region,
            )
            return result
        except NotConfiguredError as e:
            return {"error": str(e)}
        except Exception as e:
            logger.error("get_gemini_price error: %s", e)
            return {"error": f"Pricing lookup failed: {e}"}

    @mcp.tool()
    async def get_cloud_lb_price(
        ctx: Context,
        region: str = "us-central1",
        lb_type: str = "https",
        rule_count: int = 1,
        data_gb: float = 0.0,
        hours_per_month: float = 730.0,
    ) -> dict[str, Any]:
        """Get GCP Cloud Load Balancing pricing for forwarding rules and data processed.

        Returns per-rule hourly rates and per-GB data-processed rates for the
        specified load balancer type. Pass quantities to get monthly cost estimates.

        Args:
            region: GCP region, e.g. "us-central1" (default), "europe-west1".
            lb_type: LB type — "https" (External HTTP(S), default), "tcp" (TCP Proxy),
                     "ssl" (SSL Proxy), "network" (L4 Network LB), "internal" (Internal LB).
            rule_count: Number of forwarding rules (default 1).
            data_gb: GB of data processed per month (omit for rule cost only).
            hours_per_month: Hours active per month (default 730 = always-on).

        Examples:
            get_cloud_lb_price(region="us-central1", lb_type="https", rule_count=2)
            get_cloud_lb_price(region="us-central1", lb_type="network", rule_count=3, data_gb=1000.0)
            get_cloud_lb_price(region="europe-west1", lb_type="tcp", hours_per_month=730)
        """
        pvdr = _providers(ctx).get("gcp")
        if pvdr is None:
            return {"error": "GCP provider is not configured."}
        if not hasattr(pvdr, "get_cloud_lb_price"):
            return {"error": "Cloud Load Balancing pricing not available."}
        try:
            result = await pvdr.get_cloud_lb_price(
                region=region,
                lb_type=lb_type,
                rule_count=rule_count,
                data_gb=data_gb,
                hours_per_month=hours_per_month,
            )
        except Exception as e:
            logger.error("get_cloud_lb_price error: %s", e)
            return {"error": f"Pricing lookup failed: {e}"}
        return result

    @mcp.tool()
    async def get_cloud_cdn_price(
        ctx: Context,
        region: str = "us-central1",
        egress_gb: float = 0.0,
        cache_fill_gb: float = 0.0,
    ) -> dict[str, Any]:
        """Get GCP Cloud CDN pricing for cache egress and cache fill.

        Returns egress and cache fill rates. Pass quantities to get monthly cost
        estimates. Egress rates vary by destination region.

        Args:
            region: GCP region for destination / CDN PoP, e.g. "us-central1" (default).
            egress_gb: GB egressed from CDN to end users per month.
            cache_fill_gb: GB filled into CDN from origin per month.

        Examples:
            get_cloud_cdn_price(region="us-central1")
            get_cloud_cdn_price(region="us-central1", egress_gb=5000.0, cache_fill_gb=500.0)
            get_cloud_cdn_price(region="europe-west1", egress_gb=1000.0)
        """
        pvdr = _providers(ctx).get("gcp")
        if pvdr is None:
            return {"error": "GCP provider is not configured."}
        if not hasattr(pvdr, "get_cloud_cdn_price"):
            return {"error": "Cloud CDN pricing not available."}
        try:
            result = await pvdr.get_cloud_cdn_price(
                region=region,
                egress_gb=egress_gb,
                cache_fill_gb=cache_fill_gb,
            )
        except Exception as e:
            logger.error("get_cloud_cdn_price error: %s", e)
            return {"error": f"Pricing lookup failed: {e}"}
        return result

    @mcp.tool()
    async def get_cloud_nat_price(
        ctx: Context,
        region: str = "us-central1",
        gateway_count: int = 1,
        data_gb: float = 0.0,
        hours_per_month: float = 730.0,
    ) -> dict[str, Any]:
        """Get GCP Cloud NAT pricing for gateway uptime and data processed.

        Returns per-gateway hourly rates and per-GB data-processed rates.
        Pass quantities to get monthly cost estimates.

        Args:
            region: GCP region, e.g. "us-central1" (default), "europe-west1".
            gateway_count: Number of Cloud NAT gateways (default 1).
            data_gb: GB processed through NAT per month.
            hours_per_month: Hours active per month (default 730 = always-on).

        Examples:
            get_cloud_nat_price(region="us-central1")
            get_cloud_nat_price(region="us-central1", gateway_count=2, data_gb=500.0)
            get_cloud_nat_price(region="europe-west1", gateway_count=1, data_gb=100.0, hours_per_month=730)
        """
        pvdr = _providers(ctx).get("gcp")
        if pvdr is None:
            return {"error": "GCP provider is not configured."}
        if not hasattr(pvdr, "get_cloud_nat_price"):
            return {"error": "Cloud NAT pricing not available."}
        try:
            result = await pvdr.get_cloud_nat_price(
                region=region,
                gateway_count=gateway_count,
                data_gb=data_gb,
                hours_per_month=hours_per_month,
            )
        except Exception as e:
            logger.error("get_cloud_nat_price error: %s", e)
            return {"error": f"Pricing lookup failed: {e}"}
        return result

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
