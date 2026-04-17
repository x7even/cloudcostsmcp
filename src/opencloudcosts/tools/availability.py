"""SKU and region availability MCP tools."""
from __future__ import annotations

import asyncio
import logging
from decimal import Decimal
from typing import Any

from mcp.server.fastmcp import Context

from opencloudcosts.models import PriceComparison, PricingTerm
from opencloudcosts.providers.base import NotConfiguredError
from opencloudcosts.utils.baseline import apply_baseline_deltas
from opencloudcosts.utils.regions import region_display_name

logger = logging.getLogger(__name__)

# Default region set for AWS fan-out tools when no regions are specified.
# Querying all ~30 AWS regions cold (no cache) requires downloading ~30 bulk
# pricing files and will time out. This curated list covers the major
# commercial regions. Pass regions=["all"] to search the full set.
_AWS_MAJOR_REGIONS = [
    "us-east-1",       # N. Virginia
    "us-east-2",       # Ohio
    "us-west-1",       # N. California
    "us-west-2",       # Oregon
    "ca-central-1",    # Canada
    "eu-west-1",       # Ireland
    "eu-west-2",       # London
    "eu-central-1",    # Frankfurt
    "ap-southeast-1",  # Singapore
    "ap-southeast-2",  # Sydney
    "ap-northeast-1",  # Tokyo
    "ap-south-1",      # Mumbai
]

# Default region set for GCP fan-out tools when no regions are specified.
# Querying all 30+ GCP regions cold (no cache) requires many API calls and
# will time out. This curated list covers the major commercial regions.
# Pass regions=["all"] to search the full set.
_GCP_MAJOR_REGIONS = [
    "us-central1",          # Iowa
    "us-east1",             # South Carolina
    "us-west1",             # Oregon
    "us-west2",             # Los Angeles
    "europe-west1",         # Belgium
    "europe-west2",         # London
    "europe-west3",         # Frankfurt
    "europe-west4",         # Netherlands
    "asia-east1",           # Taiwan
    "asia-northeast1",      # Tokyo
    "asia-southeast1",      # Singapore
    "australia-southeast1", # Sydney
]


def register_availability_tools(mcp: Any) -> None:

    @mcp.tool()
    async def list_regions(
        ctx: Context,
        provider: str,
        service: str = "compute",
    ) -> dict[str, Any]:
        """
        List all regions where a cloud service is available for the given provider.

        Returns region codes and their friendly display names.

        Args:
            provider: Cloud provider — "aws", "gcp", or "azure"
            service: Service type — "compute" (default), "storage", "database"
        """
        pvdr = ctx.request_context.lifespan_context["providers"].get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            regions = await pvdr.list_regions(service)
        except Exception as e:
            logger.error("list_regions error: %s", e)
            return {"error": str(e)}

        return {
            "provider": provider,
            "service": service,
            "regions": [
                {"code": r, "name": region_display_name(provider, r)}
                for r in regions
            ],
            "count": len(regions),
        }

    @mcp.tool()
    async def list_instance_types(
        ctx: Context,
        provider: str,
        region: str,
        family: str = "",
        min_vcpu: int | None = None,
        min_memory_gb: float | None = None,
        gpu: bool = False,
        max_results: int = 50,
    ) -> dict[str, Any]:
        """
        List available compute instance types in a region, with optional filters.

        Useful for discovering what instances are available before fetching pricing,
        or for finding instances that meet specific vCPU/memory/GPU requirements.

        Args:
            provider: Cloud provider — "aws", "gcp", or "azure"
            region: Region code, e.g. "us-east-1" (AWS), "us-central1" (GCP), "eastus" (Azure)
            family: Optional instance family prefix, e.g. "m5", "c6g" (AWS), "n2", "c2" (GCP),
                    or "Standard_D", "Standard_E" (Azure)
            min_vcpu: Filter to instances with at least this many vCPUs.
            min_memory_gb: Filter to instances with at least this much memory (GB).
            gpu: If true, only return GPU instances
            max_results: Maximum number of results (default 50)
        """
        pvdr = ctx.request_context.lifespan_context["providers"].get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        # Azure Retail Prices API does not expose vCPU/memory metadata.
        # Filtering by min_vcpu or min_memory_gb would silently return
        # zero-spec instances, which misleads the LLM. Return a clear error.
        if provider == "azure" and (min_vcpu is not None or min_memory_gb is not None):
            return {
                "result": "specs_unavailable",
                "provider": "azure",
                "message": (
                    "Azure vCPU/memory specs are not available via the Retail Prices API. "
                    "Use the 'family' filter with ARM SKU name prefixes instead."
                ),
                "suggestion": (
                    "Try family='Standard_D4s' (4 vCPU general purpose), "
                    "'Standard_D8s' (8 vCPU), "
                    "'Standard_E4s' (4 vCPU memory-optimised), "
                    "'Standard_F4s' (4 vCPU compute-optimised)"
                ),
                "docs": "https://learn.microsoft.com/en-us/azure/virtual-machines/sizes",
            }

        try:
            instances = await pvdr.list_instance_types(
                region,
                family=family or None,
                min_vcpus=min_vcpu,
                min_memory_gb=min_memory_gb,
                gpu=gpu,
            )
        except Exception as e:
            logger.error("list_instance_types error: %s", e)
            return {"error": str(e)}

        instances.sort(key=lambda i: (i.vcpu, i.memory_gb))

        if min_vcpu is not None:
            instances = [i for i in instances if i.vcpu >= min_vcpu]
        if min_memory_gb is not None:
            instances = [i for i in instances if i.memory_gb >= min_memory_gb]

        total_found = len(instances)

        # Spec-filtered queries: auto-expand the cap so a broad filter like
        # "min_vcpu=4" doesn't silently drop hundreds of valid candidates.
        # The worst outcome is a missed cheaper option due to truncation.
        effective_max = max_results
        if (min_vcpu is not None or min_memory_gb is not None) and effective_max < 200:
            effective_max = 200

        truncated = total_found > effective_max
        instances = instances[:effective_max]

        # Suggest a get_prices_batch call so the LLM can immediately price the results.
        # Cap the suggested batch at 10 types to keep the follow-up call fast.
        _PRICE_BATCH_CAP = 10
        batch_types = [i.instance_type for i in instances[:_PRICE_BATCH_CAP]]
        _prices_batch_hint = (
            f'get_prices_batch(provider="{provider}", instance_types={batch_types}, '
            f'region="{region}") — price these instances sorted cheapest first'
        )
        next_steps: list[str] = []
        if truncated:
            spec_filters_applied = min_vcpu is not None or min_memory_gb is not None
            if spec_filters_applied:
                # Pick a concrete example instance type based on provider and min_vcpu
                if provider == "aws":
                    if min_vcpu is not None and min_vcpu <= 4:
                        _example_type = "m5.xlarge"
                        _example_desc = "4vCPU/16GB"
                    elif min_vcpu is not None and min_vcpu <= 8:
                        _example_type = "m5.2xlarge"
                        _example_desc = "8vCPU/32GB"
                    else:
                        _example_type = "r6i.xlarge"
                        _example_desc = "4vCPU/32GB"
                elif provider == "gcp":
                    if min_vcpu is not None and min_vcpu <= 4:
                        _example_type = "n2-standard-4"
                        _example_desc = "4vCPU/16GB"
                    else:
                        _example_type = "n2-standard-8"
                        _example_desc = "8vCPU/32GB"
                else:
                    if min_vcpu is not None and min_vcpu <= 4:
                        _example_type = "Standard_D4s_v3"
                        _example_desc = "4vCPU/16GB"
                    else:
                        _example_type = "Standard_D8s_v3"
                        _example_desc = "8vCPU/32GB"
                next_steps.append(
                    f"Spec filters applied — {total_found} total matches, showing {effective_max}. "
                    f"There are {total_found - effective_max} more instances not shown. "
                    f"To see all: re-call with max_results={total_found}. "
                    f"To narrow: add family filter (e.g. family='m5') or tighten min_vcpu/min_memory_gb. "
                    f"To price this sample immediately: call get_prices_batch with the instance_types above. "
                    f"Or go direct: get_compute_price(provider='{provider}', "
                    f"instance_type='{_example_type}', region='{region}') for a known {_example_desc} type."
                )
            else:
                # No spec filters — guide the LLM to narrow by family first
                if provider == "aws":
                    _example_family = family if family else "m5"
                    _family_hint = (
                        f'family="{_example_family}" (AWS general-purpose) or '
                        f'"c6g" (ARM compute-optimised) or "r6g" (memory-optimised)'
                    )
                elif provider == "gcp":
                    _example_family = family if family else "n2-standard"
                    _family_hint = (
                        f'family="{_example_family}" (GCP general-purpose) or '
                        f'"c2-standard" (compute-optimised) or "m2-ultramem" (memory-optimised)'
                    )
                else:
                    _example_family = family if family else "Standard_D"
                    _family_hint = f'family="{_example_family}"'
                next_steps.append(
                    f"Result truncated: returned {effective_max} of {total_found} matches. "
                    f"Narrow by family — e.g. {_family_hint} — or add "
                    f"min_vcpu/min_memory_gb spec filters. "
                    f"To retrieve all {total_found} results: re-call with max_results={total_found}."
                )
                next_steps.append(_prices_batch_hint)
        else:
            next_steps.append(_prices_batch_hint)

        response: dict[str, Any] = {
            "provider": provider,
            "region": region,
            "region_name": region_display_name(provider, region),
            "filters": {
                "family": family or None,
                "min_vcpu": min_vcpu,
                "min_memory_gb": min_memory_gb,
                "gpu": gpu,
            },
            "count": len(instances),
            "total_found": total_found,
            "truncated": truncated,
            "instance_types": [
                {
                    "instance_type": i.instance_type,
                    "vcpu": i.vcpu,
                    "memory_gb": i.memory_gb,
                    "gpu_count": i.gpu_count if i.gpu_count else None,
                    "gpu_type": i.gpu_type,
                    "network": i.network_performance,
                    "storage": i.storage,
                }
                for i in instances
            ],
            "next_steps": next_steps,
        }
        if provider == "azure":
            response["specs_note"] = (
                "Azure does not expose vCPU/memory specs via the Retail Prices API. "
                "Filter by family name prefix instead "
                "(e.g. family='Standard_D4s' for 4-vCPU D-series). "
                "See https://learn.microsoft.com/en-us/azure/virtual-machines/sizes for specs."
            )
        return response

    @mcp.tool()
    async def check_availability(
        ctx: Context,
        provider: str,
        service: str,
        sku_or_type: str,
        region: str,
    ) -> dict[str, Any]:
        """
        Check whether a specific instance type or SKU is available in a given region.

        Args:
            provider: Cloud provider — "aws", "gcp", or "azure"
            service: Service type — "compute", "storage"
            sku_or_type: Instance type or SKU ID, e.g. "m5.xlarge" (AWS), "n2-standard-4" (GCP),
                         "Standard_D4s_v3" (Azure), or "gp3" (storage)
            region: Region code, e.g. "us-east-1" (AWS), "us-central1" (GCP), "eastus" (Azure)
        """
        pvdr = ctx.request_context.lifespan_context["providers"].get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            available = await pvdr.check_availability(service, sku_or_type, region)
        except Exception as e:
            logger.error("check_availability error: %s", e)
            return {"error": str(e)}

        return {
            "provider": provider,
            "service": service,
            "sku_or_type": sku_or_type,
            "region": region,
            "region_name": region_display_name(provider, region),
            "available": available,
        }

    @mcp.tool()
    async def find_cheapest_region(
        ctx: Context,
        provider: str,
        instance_type: str,
        os: str = "Linux",
        term: str = "on_demand",
        regions: list[str] | None = None,
        baseline_region: str = "",
    ) -> dict[str, Any]:
        """
        Find the cheapest region for a given compute instance type across all (or specified) regions.

        Queries pricing concurrently across regions and returns results sorted cheapest first,
        with the price delta between cheapest and most expensive regions.

        Optionally compares every region against a baseline (e.g. baseline_region="us-east-1")
        to show delta $/% vs that reference point.

        Args:
            provider: Cloud provider — "aws", "gcp", or "azure"
            instance_type: Instance type, e.g. "m5.xlarge" (AWS), "n2-standard-4" (GCP),
                           or "Standard_D4s_v3" (Azure)
            os: Operating system — "Linux" (default) or "Windows"
            term: Pricing term — "on_demand" (default), "reserved_1yr", "reserved_3yr"
            regions: List of region codes to compare. Omit for major regions (faster).
                     Pass ["all"] to search every available region (slow on first run without cache).
            baseline_region: Optional region code to use as comparison baseline, e.g. "us-east-1".
                             Each result will include delta_per_hour, delta_monthly, delta_pct.
        """
        pvdr = ctx.request_context.lifespan_context["providers"].get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            pricing_term = PricingTerm(term)
        except ValueError:
            return {"error": f"Unknown term '{term}'."}

        all_regions_requested = regions == ["all"]
        if not regions or all_regions_requested:
            all_available = await pvdr.list_regions("compute")
            if provider == "aws" and not all_regions_requested:
                regions = _AWS_MAJOR_REGIONS
                scoped_search = True
            elif provider == "gcp" and not all_regions_requested:
                regions = _GCP_MAJOR_REGIONS
                scoped_search = True
            else:
                regions = all_available
                scoped_search = False
        else:
            scoped_search = False

        semaphore = asyncio.Semaphore(10)
        _fcr_config_errors: list[str] = []

        async def fetch_one(region: str) -> tuple[str, list]:
            async with semaphore:
                try:
                    prices = await pvdr.get_compute_price(instance_type, region, os, pricing_term)
                    return region, prices
                except NotConfiguredError as e:
                    if not _fcr_config_errors:
                        _fcr_config_errors.append(str(e))
                    return region, []
                except Exception:
                    return region, []

        results_raw = await asyncio.gather(*[fetch_one(r) for r in regions])

        all_prices = []
        not_available = []
        for region, prices in results_raw:
            if prices:
                all_prices.extend(prices)
            else:
                not_available.append(region)

        if not all_prices:
            if _fcr_config_errors:
                return {
                    "result": "provider_not_configured",
                    "provider": provider,
                    "instance_type": instance_type,
                    "error": _fcr_config_errors[0],
                }
            return {
                "result": "no_prices_found",
                "message": f"No pricing found for {instance_type} in any region.",
            }

        comparison = PriceComparison.from_results(
            f"{instance_type} cheapest region ({provider})", all_prices
        )

        sorted_entries = [
            {
                "region": p.region,
                "region_name": region_display_name(provider, p.region),
                "price_per_hour": f"${p.price_per_unit:.6f}",
                "monthly_estimate": f"${p.monthly_cost:.2f}/mo",
            }
            for p in comparison.results
        ]

        if baseline_region:
            try:
                apply_baseline_deltas(sorted_entries, baseline_region)
            except ValueError as e:
                return {"error": str(e)}

        cheapest = comparison.cheapest
        most_exp = comparison.most_expensive
        result: dict[str, Any] = {
            "provider": provider,
            "instance_type": instance_type,
            "os": os,
            "term": term,
            "cheapest_region": cheapest.region if cheapest else None,
            "cheapest_region_name": region_display_name(provider, cheapest.region) if cheapest else None,
            "cheapest_price": f"${cheapest.price_per_unit:.6f}/hr" if cheapest else None,
            "most_expensive_region": most_exp.region if most_exp else None,
            "most_expensive_region_name": region_display_name(provider, most_exp.region) if most_exp else None,
            "most_expensive_price": f"${most_exp.price_per_unit:.6f}/hr" if most_exp else None,
            "price_delta_pct": comparison.price_delta_pct,
            "all_regions_sorted": sorted_entries,
            "not_available_in": not_available if not_available else None,
        }
        if baseline_region:
            result["baseline_region"] = baseline_region
            result["baseline_region_name"] = region_display_name(provider, baseline_region)
        if scoped_search:
            result["note"] = (
                f"Searched {len(regions)} major {provider.upper()} regions. "
                "Pass regions=['all'] to search all available regions (slower on first run)."
            )
        return result

    @mcp.tool()
    async def find_available_regions(
        ctx: Context,
        provider: str,
        instance_type: str,
        os: str = "Linux",
        term: str = "on_demand",
        include_prices: bool = True,
        regions: list[str] | None = None,
        baseline_region: str = "",
    ) -> dict[str, Any]:
        """
        Find all regions where a specific compute instance type is available.

        Queries regions concurrently and returns the ones where pricing exists,
        sorted cheapest first. Includes friendly region names, vCPU/memory specs,
        and optional baseline delta comparison.

        For AWS, defaults to major regions to avoid cold-cache timeouts.
        Pass regions=["all"] to search every available region.

        Args:
            provider: Cloud provider — "aws", "gcp", or "azure"
            instance_type: Instance type, e.g. "c6a.xlarge" (AWS), "n2-standard-4" (GCP),
                           or "Standard_D4s_v3" (Azure)
            os: Operating system — "Linux" (default) or "Windows"
            term: Pricing term — "on_demand" (default), "reserved_1yr", "spot"
            include_prices: If true (default), include per-hour price and monthly estimate
            regions: Specific region codes to search. Pass ["all"] for every region.
                     Omit to use major regions for AWS (faster on first run).
            baseline_region: Region to use as comparison baseline, e.g. "us-east-1".
                             Adds delta_per_hour, delta_monthly, delta_pct to each result.
        """
        pvdr = ctx.request_context.lifespan_context["providers"].get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            pricing_term = PricingTerm(term)
        except ValueError:
            return {"error": f"Unknown term '{term}'."}

        all_regions_requested = regions == ["all"]
        if not regions or all_regions_requested:
            all_available = await pvdr.list_regions("compute")
            if provider == "aws" and not all_regions_requested:
                search_regions = _AWS_MAJOR_REGIONS
                scoped = True
            elif provider == "gcp" and not all_regions_requested:
                search_regions = _GCP_MAJOR_REGIONS
                scoped = True
            else:
                search_regions = all_available
                scoped = False
        else:
            search_regions = regions
            scoped = False

        semaphore = asyncio.Semaphore(10)
        _config_errors: list[str] = []

        async def probe(region: str) -> tuple[str, list]:
            async with semaphore:
                try:
                    prices = await pvdr.get_compute_price(
                        instance_type, region, os, pricing_term
                    )
                    return region, prices
                except NotConfiguredError as e:
                    if not _config_errors:
                        _config_errors.append(str(e))
                    return region, []
                except Exception:
                    return region, []

        raw = await asyncio.gather(*[probe(r) for r in search_regions])

        available = []
        for region, prices in raw:
            if prices:
                p = prices[0]
                entry: dict[str, Any] = {
                    "region": region,
                    "region_name": region_display_name(provider, region),
                }
                if include_prices:
                    entry["price_per_hour"] = f"${p.price_per_unit:.6f}"
                    entry["monthly_estimate"] = f"${p.monthly_cost:.2f}/mo"
                    if "vcpu" in p.attributes:
                        entry["vcpu"] = p.attributes["vcpu"]
                    if "memory" in p.attributes:
                        entry["memory"] = p.attributes["memory"]
                    entry["_sort_price"] = p.price_per_unit
                available.append(entry)

        if include_prices:
            available.sort(key=lambda x: x.pop("_sort_price", Decimal("0")))

        if baseline_region and include_prices:
            try:
                apply_baseline_deltas(available, baseline_region)
            except ValueError as e:
                return {"error": str(e)}

        # If no results were found due to provider configuration error, surface it clearly
        # so the LLM knows to answer with available data rather than keep searching.
        if not available and _config_errors:
            return {
                "result": "provider_not_configured",
                "provider": provider,
                "instance_type": instance_type,
                "available_region_count": 0,
                "available_regions": [],
                "error": _config_errors[0],
            }

        result: dict[str, Any] = {
            "provider": provider,
            "instance_type": instance_type,
            "term": term,
            "available_region_count": len(available),
            "available_regions": available,
        }
        if baseline_region:
            result["baseline_region"] = baseline_region
            result["baseline_region_name"] = region_display_name(provider, baseline_region)
        if scoped:
            result["note"] = (
                f"Searched {len(search_regions)} major {provider.upper()} regions. "
                "Pass regions=['all'] to search all available regions (slower on first run)."
            )
        return result

    @mcp.tool()
    async def list_services(
        ctx: Context,
        provider: str = "aws",
    ) -> dict[str, Any]:
        """
        List all cloud services that have pricing data available.

        For AWS, returns all 260+ services in the AWS bulk pricing catalog
        (EC2, RDS, CloudWatch, Lambda, data transfer, ELB, Route53, etc.)
        along with short aliases you can use with get_service_price().

        Use this to discover what services are available before calling
        get_service_price() or search_pricing(service=...).

        Args:
            provider: Cloud provider — "aws" (default)
        """
        pvdr = ctx.request_context.lifespan_context["providers"].get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        if not hasattr(pvdr, "list_services"):
            return {"error": f"Provider '{provider}' does not support list_services yet."}

        try:
            services = await pvdr.list_services()
        except Exception as e:
            logger.error("list_services error: %s", e)
            return {"error": str(e)}

        # Annotate known services with usage hints to help LLM tool selection.
        _SERVICE_HINTS: dict[str, str] = {
            "AWSDataTransfer": (
                "use get_service_price(service='data_transfer', region='<source-region>', "
                "filters={'fromRegionCode': '<source>', 'toRegionCode': '<dest>'}) "
                "for inter-region egress pricing"
            ),
            "AWSLambda": (
                "Lambda has two separate pricing dimensions — use both filters: "
                "(1) Duration/compute: get_service_price(service='lambda', region='us-east-1', "
                "filters={'group': 'AWS-Lambda-Duration'}) → returns per-GB-second rate. "
                "(2) Requests: get_service_price(service='lambda', region='us-east-1', "
                "filters={'group': 'AWS-Lambda-Requests'}) → returns per-request rate. "
                "Free tier: first 1M requests/month and 400,000 GB-seconds/month are free."
            ),
            "AmazonSageMaker": (
                "SageMaker instance pricing uses instanceType with a category suffix: "
                "Training: get_service_price(service='sagemaker', region='us-east-1', filters={'instanceType': 'ml.m5.xlarge-Training'}) "
                "Hosting/inference: filters={'instanceType': 'ml.p3.2xlarge-Hosting'} "
                "Notebook: filters={'instanceType': 'ml.t3.medium-Notebook'} "
                "Or use component filter: filters={'component': 'Training'} to list all training instance prices."
            ),
            "CloudStorage": (
                "GCS storage class pricing — use get_storage_price(provider='gcp', storage_type=..., region=...):\n"
                "  Standard: get_storage_price(provider='gcp', storage_type='standard', region='us-central1')\n"
                "  Nearline: get_storage_price(provider='gcp', storage_type='nearline', region='us-central1')\n"
                "  Coldline: get_storage_price(provider='gcp', storage_type='coldline', region='us-central1')\n"
                "  Archive: get_storage_price(provider='gcp', storage_type='archive', region='us-central1')\n"
                "Storage classes from cheapest to most expensive: Archive < Coldline < Nearline < Standard.\n"
                "Operations (reads/writes) have separate per-operation costs not included here."
            ),
            "CloudSQL": (
                "Cloud SQL instance pricing (per-vCPU + per-GB-RAM/hour) — use get_database_price(provider='gcp', ...):\n"
                "  get_database_price(provider='gcp', instance_type='db-n1-standard-4', region='us-central1', engine='MySQL')\n"
                "  get_database_price(provider='gcp', instance_type='db-n1-highmem-8', region='us-central1', engine='PostgreSQL', deployment='ha')\n"
                "  deployment='ha' enables Regional (high-availability) pricing (~2x cost).\n"
                "  Supported engines: MySQL, PostgreSQL, SQLServer.\n"
                "  Common instance types: db-n1-standard-1/2/4/8/16, db-n1-highmem-2/4/8/16"
            ),
            "KubernetesEngine": (
                "GKE has two billing modes:\n"
                "  Standard: flat $0.10/hr cluster management fee + Compute Engine VM costs for nodes.\n"
                "    get_gke_price(region='us-central1', mode='standard', node_count=3, node_type='n2-standard-4')\n"
                "  Autopilot: pay per pod resource request (vCPU-hr + GiB-RAM-hr), no node management.\n"
                "    get_gke_price(region='us-central1', mode='autopilot', vcpu=4.0, memory_gb=16.0)\n"
                "For node costs in Standard mode, also call:\n"
                "    get_compute_price(provider='gcp', instance_type='n2-standard-4', region='us-central1')"
            ),
            "MemorystoreRedis": (
                "Memorystore for Redis pricing — use get_memorystore_price(provider='gcp', ...):\n"
                "  Standard HA: get_memorystore_price(capacity_gb=10.0, region='us-central1', tier='standard')\n"
                "  Basic (no HA): get_memorystore_price(capacity_gb=10.0, region='us-central1', tier='basic')\n"
                "  Standard tier includes cross-zone HA replication and costs ~1.3-2x Basic.\n"
                "  Pricing is per GiB-hour of provisioned capacity × capacity_gb × hours."
            ),
            "BigQuery": (
                "BigQuery pricing — use get_bigquery_price(region=..., ...):\n"
                "  Rates only: get_bigquery_price(region='us')\n"
                "  With query estimate: get_bigquery_price(region='us', query_tb=10.0)\n"
                "  Storage + query: get_bigquery_price(region='us', query_tb=5.0, active_storage_gb=500.0, longterm_storage_gb=1000.0)\n"
                "  EU multi-region: get_bigquery_price(region='eu', query_tb=2.0)\n"
                "  Single region: get_bigquery_price(region='us-central1', active_storage_gb=200.0)\n"
                "  Free tier: first 1 TiB/month queries free; first 10 GiB/month active storage free.\n"
                "  Long-term storage (data unchanged >90 days) is ~50% cheaper than active storage."
            ),
            "VertexAI": (
                "Vertex AI custom training / prediction compute pricing — billed per vCPU-hour and GiB-RAM-hour:\n"
                "  get_vertex_price(machine_type='n1-standard-4', region='us-central1', task='training')\n"
                "  get_vertex_price(machine_type='a2-highgpu-1g', region='us-central1', task='prediction')\n"
                "  task: 'training' (default) or 'prediction'.\n"
                "  Returns per-vCPU-hr and per-GiB-RAM-hr rates; multiply by machine specs for total cost."
            ),
            "Gemini": (
                "Vertex AI Gemini generative model token / character pricing:\n"
                "  get_gemini_price(model='gemini-1.5-flash', region='us-central1')\n"
                "  get_gemini_price(model='gemini-1.0-pro')\n"
                "  get_gemini_price(model='gemini-1.5-pro')\n"
                "  Returns input and output rates per character or per token.\n"
                "  Most Gemini SKUs are global or in us-central1 — try that region if others return nothing."
            ),
        }
        annotated_services = []
        for svc in services:
            entry = dict(svc)
            hint = _SERVICE_HINTS.get(entry.get("service_code", ""))
            if hint:
                entry["usage_example"] = hint
            annotated_services.append(entry)

        return {
            "provider": provider,
            "count": len(annotated_services),
            "services": annotated_services,
            "tip": (
                "Use service_code or any alias with get_service_price() or "
                "search_pricing(service=...). "
                "Example: get_service_price(service='cloudwatch', region='us-east-1', "
                "filters={'group': 'Metric'})"
            ),
        }

    @mcp.tool()
    async def cache_stats(ctx: Context) -> dict[str, Any]:
        """Return statistics about the local pricing cache (entry counts, DB size)."""
        cache = ctx.request_context.lifespan_context["cache"]
        return await cache.stats()
