"""
AWS cloud pricing provider.

Public pricing: uses boto3 `pricing` client when AWS credentials are available,
otherwise falls back to the public AWS bulk pricing HTTPS endpoints (no credentials
needed) via httpx. All public pricing tools work credential-free.

Effective pricing uses Cost Explorer (`ce` client) — opt-in only due to $0.01/call cost.
"""
from __future__ import annotations

import asyncio
import json
import logging
from datetime import UTC
from decimal import Decimal
from typing import Any

import boto3
import botocore.exceptions
import httpx

from opencloudcosts.cache import CacheManager
from opencloudcosts.config import Settings
from opencloudcosts.models import (
    AiPricingSpec,
    AnalyticsPricingSpec,
    CloudProvider,
    ComputePricingSpec,
    ContainerPricingSpec,
    DatabasePricingSpec,
    EffectivePrice,
    EgressPricingSpec,
    InstanceTypeInfo,
    NetworkPricingSpec,
    NormalizedPrice,
    ObservabilityPricingSpec,
    PriceUnit,
    PricingDomain,
    PricingResult,
    PricingSpec,
    PricingTerm,
    ProviderCatalog,
    ServerlessPricingSpec,
    StoragePricingSpec,
)
from opencloudcosts.providers.base import NotConfiguredError, NotSupportedError, ProviderBase
from opencloudcosts.utils.http_retry import sync_retry
from opencloudcosts.utils.regions import aws_region_to_display, list_aws_regions
from opencloudcosts.utils.units import parse_aws_unit

logger = logging.getLogger(__name__)

# AWS Pricing API only works from these two regions
_PRICING_REGION = "us-east-1"

# Map our PricingTerm enum to AWS offer term keys in the price list
_TERM_KEY: dict[PricingTerm, str] = {
    PricingTerm.ON_DEMAND: "OnDemand",
    PricingTerm.RESERVED_1YR: "Reserved",
    PricingTerm.RESERVED_3YR: "Reserved",
    PricingTerm.RESERVED_1YR_PARTIAL: "Reserved",
    PricingTerm.RESERVED_1YR_ALL: "Reserved",
    PricingTerm.RESERVED_3YR_PARTIAL: "Reserved",
    PricingTerm.RESERVED_3YR_ALL: "Reserved",
    PricingTerm.SAVINGS_PLAN: "Reserved",  # handled separately via SP API
}

# Reserved term qualifiers (LeaseContractLength + PurchaseOption)
_RESERVED_FILTERS: dict[PricingTerm, dict[str, str]] = {
    PricingTerm.RESERVED_1YR:         {"LeaseContractLength": "1yr", "PurchaseOption": "No Upfront"},
    PricingTerm.RESERVED_3YR:         {"LeaseContractLength": "3yr", "PurchaseOption": "No Upfront"},
    PricingTerm.RESERVED_1YR_PARTIAL: {"LeaseContractLength": "1yr", "PurchaseOption": "Partial Upfront"},
    PricingTerm.RESERVED_1YR_ALL:     {"LeaseContractLength": "1yr", "PurchaseOption": "All Upfront"},
    PricingTerm.RESERVED_3YR_PARTIAL: {"LeaseContractLength": "3yr", "PurchaseOption": "Partial Upfront"},
    PricingTerm.RESERVED_3YR_ALL:     {"LeaseContractLength": "3yr", "PurchaseOption": "All Upfront"},
}


# Public bulk pricing — no credentials required
# Per-region index is ~5-15 MB gzipped; we cache parsed results in SQLite.
_BULK_BASE = "https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws"
_BULK_URL = "{base}/{service}/current/{region}/index.json"

# Human-friendly aliases -> canonical AWS service offer codes.
# The bulk URL uses the offer code directly; aliases allow callers to use
# short names like "cloudwatch" or "data_transfer".
_SERVICE_ALIASES: dict[str, str] = {
    # compute / infra
    "ec2": "AmazonEC2",
    "ebs": "AmazonEC2",   # EBS pricing lives under AmazonEC2 service
    "s3": "AmazonS3",
    "rds": "AmazonRDS",
    "cloudwatch": "AmazonCloudWatch",
    "lambda": "AWSLambda",
    "data_transfer": "AWSDataTransfer",
    "transfer": "AWSDataTransfer",
    "elb": "AWSELB",
    "load_balancer": "AWSELB",
    "cloudfront": "AmazonCloudFront",
    "cdn": "AmazonCloudFront",
    "route53": "AmazonRoute53",
    "dns": "AmazonRoute53",
    "dynamodb": "AmazonDynamoDB",
    "efs": "AmazonEFS",
    "fsx": "AmazonFSx",
    "elasticache": "AmazonElastiCache",
    "redis": "AmazonElastiCache",
    "sqs": "AmazonSQS",
    "sns": "AmazonSNS",
    "redshift": "AmazonRedshift",
    "cloudtrail": "AWSCloudTrail",
    "config": "AWSConfig",
    "backup": "AWSBackup",
    "ecs": "AmazonECS",
    "eks": "AmazonEKS",
    "ecr": "AmazonECR",
    "secretsmanager": "AWSSecretsManager",
    "kms": "awskms",
    "waf": "awswaf",
    "shield": "AWSShield",
    "kinesis": "AmazonKinesis",
    "glue": "AWSGlue",
    "athena": "AmazonAthena",
    "sagemaker": "AmazonSageMaker",
    "fargate": "AmazonECS",
    "bedrock": "AmazonBedrock",
    # NAT Gateway pricing lives under AmazonEC2, not AmazonVPC
    "nat_gateway": "AmazonEC2",
    "natgateway": "AmazonEC2",
    "vpc": "AmazonVPC",
}

# Index of services available in the AWS bulk pricing API (cached after first fetch)
_AWS_SERVICE_INDEX_URL = f"{_BULK_BASE}/index.json"

# Bedrock model aliases — short names → AWS catalog names used in the pricing API
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

# Capability matrix: (domain.value, service | None) → supported
_AWS_CAPABILITIES: dict[tuple[str, str | None], bool] = {
    (PricingDomain.COMPUTE.value, None): True,
    (PricingDomain.COMPUTE.value, "ec2"): True,
    (PricingDomain.COMPUTE.value, "fargate"): True,
    (PricingDomain.STORAGE.value, None): True,
    (PricingDomain.STORAGE.value, "ebs"): True,
    (PricingDomain.STORAGE.value, "s3"): True,
    (PricingDomain.DATABASE.value, None): True,
    (PricingDomain.DATABASE.value, "rds"): True,
    (PricingDomain.DATABASE.value, "elasticache"): True,
    (PricingDomain.AI.value, None): True,
    (PricingDomain.AI.value, "bedrock"): True,
    (PricingDomain.AI.value, "sagemaker"): True,
    (PricingDomain.SERVERLESS.value, None): True,
    (PricingDomain.SERVERLESS.value, "lambda"): True,
    (PricingDomain.ANALYTICS.value, None): True,
    (PricingDomain.ANALYTICS.value, "redshift"): True,
    (PricingDomain.ANALYTICS.value, "athena"): True,
    (PricingDomain.NETWORK.value, None): True,
    (PricingDomain.NETWORK.value, "lb"): True,
    (PricingDomain.NETWORK.value, "cloud_lb"): True,    # GCP-style alias
    (PricingDomain.NETWORK.value, "cdn"): True,
    (PricingDomain.NETWORK.value, "cloud_cdn"): True,   # GCP-style alias
    (PricingDomain.NETWORK.value, "cloudfront"): True,  # direct service name
    (PricingDomain.NETWORK.value, "nat"): True,
    (PricingDomain.NETWORK.value, "cloud_nat"): True,   # GCP-style alias
    (PricingDomain.NETWORK.value, "waf"): True,
    (PricingDomain.NETWORK.value, "data_transfer"): True,
    (PricingDomain.NETWORK.value, "egress"): True,
    (PricingDomain.OBSERVABILITY.value, None): True,
    (PricingDomain.OBSERVABILITY.value, "cloudwatch"): True,
    (PricingDomain.CONTAINER.value, None): True,
    (PricingDomain.CONTAINER.value, "eks"): True,
    (PricingDomain.INTER_REGION_EGRESS.value, None): True,
}


def _resolve_service_code(service: str) -> str:
    """Resolve alias or pass through as-is (AWS offer codes are case-sensitive)."""
    return _SERVICE_ALIASES.get(service.lower(), service)


def _boto_session(settings: Settings) -> boto3.Session:
    kwargs: dict[str, Any] = {}
    if settings.aws_profile:
        kwargs["profile_name"] = settings.aws_profile
    return boto3.Session(**kwargs)


class AWSProvider(ProviderBase):
    provider = CloudProvider.AWS

    def __init__(self, settings: Settings, cache: CacheManager) -> None:
        self._settings = settings
        self._cache = cache
        session = _boto_session(settings)
        # Pricing client — only used when credentials are present.
        # Falls back to httpx bulk API automatically on NoCredentialsError.
        self._pricing = session.client("pricing", region_name=_PRICING_REGION)
        self._ce: Any = None          # lazy — only create if Cost Explorer enabled
        self._sp: Any = None          # Savings Plans API client
        self._ec2: Any = None         # EC2 client for Reserved Instance queries
        if settings.aws_enable_cost_explorer:
            self._ce = session.client("ce", region_name="us-east-1")
            self._sp = session.client("savingsplans", region_name="us-east-1")
            self._ec2 = session.client("ec2", region_name=settings.aws_region)

    # ------------------------------------------------------------------
    # Internal helpers
    # ------------------------------------------------------------------

    async def _get_products(
        self,
        service_code: str,
        filters: list[dict[str, str]],
        max_results: int = 100,
    ) -> list[dict[str, Any]]:
        """
        Return matching price list items, trying boto3 first.
        Falls back to the public bulk pricing HTTPS endpoint if credentials
        are absent — so public pricing works with zero AWS configuration.

        Both paths run in a thread executor so they don't block the event loop,
        enabling genuine concurrency when fan-out tools (find_cheapest_region)
        call this across many regions simultaneously.
        """
        try:
            return await asyncio.to_thread(
                self._get_products_boto3, service_code, filters, max_results
            )
        except (
            botocore.exceptions.NoCredentialsError,
            botocore.exceptions.PartialCredentialsError,
        ):
            logger.info(
                "No AWS credentials — falling back to public bulk pricing API for %s",
                service_code,
            )
            # Extract region from filters for the bulk URL
            region = next(
                (f["Value"] for f in filters if f["Field"] == "location"), None
            )
            if region is None:
                return []
            # Bulk API uses region codes, not display names
            from opencloudcosts.utils.regions import AWS_DISPLAY_REGION
            region_code = AWS_DISPLAY_REGION.get(region, region)
            return await asyncio.to_thread(
                self._get_products_bulk, service_code, region_code, filters, max_results
            )

    def _get_products_boto3(
        self,
        service_code: str,
        filters: list[dict[str, str]],
        max_results: int,
    ) -> list[dict[str, Any]]:
        """Fetch via boto3 pricing client (requires credentials)."""
        paginator = self._pricing.get_paginator("get_products")
        boto_filters = [
            {"Type": "TERM_MATCH", "Field": f["Field"], "Value": f["Value"]}
            for f in filters
        ]
        results: list[dict[str, Any]] = []
        for page in paginator.paginate(
            ServiceCode=service_code,
            Filters=boto_filters,
            FormatVersion="aws_v1",
            PaginationConfig={"MaxItems": max_results, "PageSize": min(max_results, 100)},
        ):
            for item_str in page.get("PriceList", []):
                results.append(json.loads(item_str))
                if len(results) >= max_results:
                    return results
        return results

    def _get_products_bulk(
        self,
        service_code: str,
        region_code: str,
        filters: list[dict[str, str]],
        max_results: int,
    ) -> list[dict[str, Any]]:
        """
        Fetch from the public bulk pricing JSON (no credentials needed).
        Downloads the per-region index, then filters in-memory to match
        the same fields that would have been used in TERM_MATCH filters.
        """
        url = _BULK_URL.format(
            base=_BULK_BASE,
            service=_resolve_service_code(service_code),
            region=region_code,
        )
        logger.debug("Bulk pricing fetch: %s", url)
        try:
            for attempt in sync_retry():
                with attempt:
                    resp = httpx.get(url, timeout=60, follow_redirects=True)
                    resp.raise_for_status()
            data = resp.json()
        except httpx.HTTPError as e:
            logger.error("Bulk pricing HTTP error for %s/%s: %s", service_code, region_code, e)
            return []

        products = data.get("products", {})
        terms_data = data.get("terms", {})

        # Build filter map from non-location fields (location is implicit from URL)
        field_filters = {
            f["Field"]: f["Value"]
            for f in filters
            if f["Field"] != "location"
        }

        results: list[dict[str, Any]] = []
        for sku, product in products.items():
            attrs = product.get("attributes", {})
            # Apply all TERM_MATCH filters.
            # productFamily and productGroup sit at the product top level, not inside
            # attributes — check both so filters like productFamily="Storage" work.
            if not all(attrs.get(k, product.get(k)) == v for k, v in field_filters.items()):
                continue

            # Reconstruct the same shape get_products returns
            item: dict[str, Any] = {"product": product, "terms": {}}
            for term_type in ("OnDemand", "Reserved"):
                term_skus = terms_data.get(term_type, {}).get(sku, {})
                if term_skus:
                    item["terms"][term_type] = term_skus

            results.append(item)
            if len(results) >= max_results:
                break

        return results

    @staticmethod
    def _extract_on_demand_price(
        item: dict[str, Any],
    ) -> tuple[Decimal, str] | None:
        """Return (price_per_unit, unit_str) from an OnDemand term, or None."""
        terms = item.get("terms", {}).get("OnDemand", {})
        for _offer_term in terms.values():
            for dim in _offer_term.get("priceDimensions", {}).values():
                usd = dim.get("pricePerUnit", {}).get("USD", "0")
                if usd and usd != "0.0000000000":
                    return Decimal(usd), dim.get("unit", "Hrs")
        return None

    @staticmethod
    def _extract_reserved_price(
        item: dict[str, Any],
        term: PricingTerm,
    ) -> tuple[Decimal, str, Decimal] | None:
        """Return (effective_price_per_unit, unit_str, upfront_cost) for a Reserved term, or None.

        For Partial Upfront and All Upfront terms the priceDimensions contain both
        an "Hrs" dimension (recurring hourly) and a "Quantity" dimension (one-time
        upfront payment). We normalise the upfront to an effective hourly rate so
        that all terms are comparable on a per-hour basis:

            effective_hourly = recurring_hourly + upfront / (8760 * term_years)

        upfront_cost is the raw one-time payment (for transparency in attributes).
        """
        qualifiers = _RESERVED_FILTERS.get(term, {})
        terms = item.get("terms", {}).get("Reserved", {})
        for offer_term in terms.values():
            attrs = offer_term.get("termAttributes", {})
            if (
                attrs.get("LeaseContractLength") == qualifiers.get("LeaseContractLength")
                and attrs.get("PurchaseOption") == qualifiers.get("PurchaseOption")
            ):
                price_dims = offer_term.get("priceDimensions", {})
                hourly = Decimal("0")
                upfront = Decimal("0")
                for dim in price_dims.values():
                    unit = dim.get("unit", "")
                    usd = Decimal(dim.get("pricePerUnit", {}).get("USD", "0"))
                    if unit == "Hrs":
                        hourly = usd
                    elif unit == "Quantity":
                        upfront = usd

                # Normalise upfront to effective hourly rate
                if upfront > 0:
                    years = 3 if "3yr" in str(term.value) else 1
                    term_hours = Decimal(str(8760 * years))
                    hourly += upfront / term_hours

                if hourly > 0:
                    return hourly, "Hrs", upfront
        return None

    def _item_to_price(
        self,
        item: dict[str, Any],
        region: str,
        term: PricingTerm,
        service: str,
    ) -> NormalizedPrice | None:
        product = item.get("product", {})
        attrs = product.get("attributes", {})
        sku = product.get("sku", "")

        upfront_cost: Decimal | None = None
        if term == PricingTerm.ON_DEMAND:
            result = self._extract_on_demand_price(item)
            if result is None:
                return None
            price_decimal, unit_str = result
        else:
            reserved_result = self._extract_reserved_price(item, term)
            if reserved_result is None:
                return None
            price_decimal, unit_str, upfront_cost = reserved_result

        extra_attrs: dict[str, str] = {}
        if upfront_cost is not None and upfront_cost > 0:
            extra_attrs["upfront_cost"] = str(upfront_cost)

        # Build a meaningful description from whichever attribute is most specific.
        # Fallback chain covers compute, storage, database, and generic services.
        _desc_keys = (
            "instanceType", "databaseEngine", "groupDescription",
            "group", "transferType", "volumeType",
        )
        description = next(
            (attrs[k] for k in _desc_keys if k in attrs),
            product.get("productFamily") or sku,
        )

        # Pass all attributes through, stripping only the high-noise keys that
        # duplicate information already present at the response level.
        _noise = {"location", "locationType", "servicecode", "servicename",
                  "regionCode", "usagetype"}

        return NormalizedPrice(
            provider=CloudProvider.AWS,
            service=service,
            sku_id=sku,
            product_family=product.get("productFamily", ""),
            description=description,
            region=region,
            attributes={**{k: v for k, v in attrs.items() if k not in _noise}, **extra_attrs},
            pricing_term=term,
            price_per_unit=price_decimal,
            unit=parse_aws_unit(unit_str),
        )

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    async def _get_spot_price(
        self, instance_type: str, region: str, os: str = "Linux"
    ) -> list[NormalizedPrice]:
        product_desc = "Linux/UNIX" if os == "Linux" else "Windows"

        def _fetch():
            ec2 = boto3.client("ec2", region_name=region)
            return ec2.describe_spot_price_history(
                InstanceTypes=[instance_type],
                ProductDescriptions=[product_desc],
                MaxResults=20,
            )

        try:
            resp = await asyncio.to_thread(_fetch)
        except (
            botocore.exceptions.NoCredentialsError,
            botocore.exceptions.PartialCredentialsError,
        ):
            raise ValueError(
                "Spot pricing requires AWS credentials. "
                "Set AWS_PROFILE or AWS_ACCESS_KEY_ID. "
                "Public spot pricing data is not available."
            )

        items = resp.get("SpotPriceHistory", [])
        if not items:
            return []

        by_az: dict[str, Decimal] = {}
        for item in items:
            az = item["AvailabilityZone"]
            price = Decimal(item["SpotPrice"])
            if az not in by_az or price < by_az[az]:
                by_az[az] = price

        cheapest_az = min(by_az, key=lambda az: by_az[az])
        price = by_az[cheapest_az]

        return [NormalizedPrice(
            provider=CloudProvider.AWS,
            service="compute",
            sku_id=f"{instance_type}-spot",
            product_family="Compute Instance",
            region=region,
            pricing_term=PricingTerm.SPOT,
            price_per_unit=price,
            unit=PriceUnit.PER_HOUR,
            description=f"{instance_type} spot ({cheapest_az})",
            attributes={
                "instanceType": instance_type,
                "availabilityZone": cheapest_az,
                "allAZPrices": json.dumps({az: str(p) for az, p in by_az.items()}),
                "operatingSystem": os,
            },
        )]

    async def _get_spot_history(
        self,
        instance_type: str,
        region: str,
        os: str = "Linux",
        availability_zone: str = "",
        hours: int = 24,
    ) -> dict:
        from collections import defaultdict
        from datetime import datetime, timedelta

        start_time = datetime.now(UTC) - timedelta(hours=min(hours, 720))
        product_desc = "Linux/UNIX" if os == "Linux" else "Windows"

        def _fetch():
            kwargs: dict = {
                "InstanceTypes": [instance_type],
                "ProductDescriptions": [product_desc],
                "StartTime": start_time,
                "MaxResults": 300,
            }
            if availability_zone:
                kwargs["AvailabilityZone"] = availability_zone
            ec2 = boto3.client("ec2", region_name=region)
            resp = ec2.describe_spot_price_history(**kwargs)
            return resp.get("SpotPriceHistory", [])

        try:
            items = await asyncio.to_thread(_fetch)
        except (
            botocore.exceptions.NoCredentialsError,
            botocore.exceptions.PartialCredentialsError,
        ):
            raise ValueError(
                "Spot price history requires AWS credentials. "
                "Set AWS_PROFILE or AWS_ACCESS_KEY_ID."
            )

        if not items:
            return {}

        # Group prices by AZ
        by_az: dict[str, list[Decimal]] = defaultdict(list)
        for item in items:
            by_az[item["AvailabilityZone"]].append(Decimal(item["SpotPrice"]))

        all_prices = [p for ps in by_az.values() for p in ps]
        overall_avg = sum(all_prices) / len(all_prices)
        overall_min = min(all_prices)
        overall_max = max(all_prices)

        # Volatility: stddev / mean
        if len(all_prices) > 1:
            variance = sum((p - overall_avg) ** 2 for p in all_prices) / len(all_prices)
            stddev = variance ** Decimal("0.5")
        else:
            stddev = Decimal("0")
        volatility_ratio = stddev / overall_avg if overall_avg > 0 else Decimal("0")

        if volatility_ratio < Decimal("0.05"):
            stability = "stable"
            recommendation = "Low interruption risk. Good candidate for fault-tolerant batch workloads."
        elif volatility_ratio < Decimal("0.15"):
            stability = "moderate"
            recommendation = "Moderate price variation. Use with checkpointing for long-running jobs."
        else:
            stability = "volatile"
            recommendation = "High volatility. Consider on-demand or reserved instances for reliable workloads."

        az_stats = {}
        for az, prices in sorted(by_az.items()):
            az_avg = sum(prices) / len(prices)
            az_stats[az] = {
                "current": f"${prices[0]:.6f}",   # most recent (API returns newest first)
                "min": f"${min(prices):.6f}",
                "max": f"${max(prices):.6f}",
                "avg": f"${az_avg:.6f}",
                "sample_count": len(prices),
            }

        return {
            "instance_type": instance_type,
            "region": region,
            "os": os,
            "lookback_hours": hours,
            "stability": stability,
            "volatility_ratio": f"{volatility_ratio:.4f}",
            "overall": {
                "min": f"${overall_min:.6f}",
                "max": f"${overall_max:.6f}",
                "avg": f"${overall_avg:.6f}",
                "sample_count": len(all_prices),
            },
            "by_availability_zone": az_stats,
            "recommendation": recommendation,
        }

    async def get_compute_price(
        self,
        instance_type: str,
        region: str,
        os: str = "Linux",
        term: PricingTerm = PricingTerm.ON_DEMAND,
    ) -> list[NormalizedPrice]:
        if term == PricingTerm.SPOT:
            return await self._get_spot_price(instance_type, region, os)

        cache_extras = {"instance_type": instance_type, "os": os, "term": term.value}
        cached_meta = await self._cache.get_prices_with_meta("aws", "compute", region, cache_extras)
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            return self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at, self._SOURCE_URL,
            )

        try:
            display_name = aws_region_to_display(region)
        except ValueError as e:
            raise ValueError(str(e)) from e

        filters = [
            {"Field": "instanceType", "Value": instance_type},
            {"Field": "location", "Value": display_name},
            {"Field": "operatingSystem", "Value": os},
            {"Field": "tenancy", "Value": "Shared"},
            {"Field": "preInstalledSw", "Value": "NA"},
            {"Field": "capacitystatus", "Value": "Used"},
        ]

        try:
            raw = await self._get_products("AmazonEC2", filters, max_results=10)
        except botocore.exceptions.ClientError as e:
            logger.error("AWS Pricing API error: %s", e)
            raise

        prices = []
        for item in raw:
            p = self._item_to_price(item, region, term, "compute")
            if p:
                prices.append(p)

        await self._cache.set_prices(
            "aws", "compute", region, cache_extras,
            [p.model_dump(mode="json") for p in prices],
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return prices

    async def get_storage_price(
        self,
        storage_type: str,
        region: str,
        size_gb: float | None = None,
        iops: int | None = None,
    ) -> list[NormalizedPrice]:
        cache_extras = {"storage_type": storage_type}
        cached_meta = await self._cache.get_prices_with_meta("aws", "storage", region, cache_extras)
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            return self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at, self._SOURCE_URL,
            )

        display_name = aws_region_to_display(region)

        # EBS volume types
        # volumeApiName disambiguates gp2 vs gp3 (both share volumeType="General Purpose")
        _EBS_API_NAMES = {"gp2", "gp3", "io1", "io2", "st1", "sc1", "standard"}
        if storage_type.lower() in _EBS_API_NAMES:
            filters = [
                {"Field": "volumeType", "Value": self._map_ebs_type(storage_type)},
                {"Field": "location", "Value": display_name},
                {"Field": "productFamily", "Value": "Storage"},
            ]
            # gp2 and gp3 share volumeType="General Purpose" — use volumeApiName to
            # get only the specific type's storage-per-GB SKU (not IOPS/throughput add-ons)
            if storage_type.lower() in {"gp2", "gp3"}:
                filters.append({"Field": "volumeApiName", "Value": storage_type.lower()})
            raw = await self._get_products("AmazonEC2", filters, max_results=5)
        else:
            # S3 standard storage as fallback
            filters = [
                {"Field": "location", "Value": display_name},
                {"Field": "storageClass", "Value": "General Purpose"},
                {"Field": "volumeType", "Value": "Standard"},
            ]
            raw = await self._get_products("AmazonS3", filters, max_results=5)

        prices = []
        for item in raw:
            p = self._item_to_price(item, region, PricingTerm.ON_DEMAND, "storage")
            if p:
                # Override unit based on product family
                if p.unit == PriceUnit.PER_UNIT:
                    p = p.model_copy(update={"unit": PriceUnit.PER_GB_MONTH})
                prices.append(p)

        # For gp3, also fetch provisioned IOPS and throughput add-on rates.
        # gp3 baseline (3000 IOPS, 125 MB/s) is included; provisioning above
        # those baselines costs extra: $0.005/IOPS-month and $0.04/MB/s-month.
        if storage_type.lower() == "gp3":
            gp3_iops_filters = [
                {"Field": "productFamily", "Value": "System Operation"},
                {"Field": "group", "Value": "EBS IOPS"},
                {"Field": "volumeApiName", "Value": "gp3"},
                {"Field": "location", "Value": display_name},
            ]
            iops_raw = await self._get_products("AmazonEC2", gp3_iops_filters, max_results=5)
            for item in iops_raw:
                p = self._item_to_price(item, region, PricingTerm.ON_DEMAND, "storage-iops")
                if p:
                    if p.unit == PriceUnit.PER_UNIT:
                        p = p.model_copy(update={"unit": PriceUnit.PER_IOPS_MONTH})
                    prices.append(p)
            gp3_tp_filters = [
                {"Field": "productFamily", "Value": "System Operation"},
                {"Field": "group", "Value": "EBS Throughput"},
                {"Field": "volumeApiName", "Value": "gp3"},
                {"Field": "location", "Value": display_name},
            ]
            tp_raw = await self._get_products("AmazonEC2", gp3_tp_filters, max_results=5)
            for item in tp_raw:
                p = self._item_to_price(item, region, PricingTerm.ON_DEMAND, "storage-throughput")
                if p:
                    if p.unit == PriceUnit.PER_UNIT:
                        p = p.model_copy(update={"unit": PriceUnit.PER_MBPS_MONTH})
                    prices.append(p)

        # For provisioned IOPS types, also fetch the per-IOPS-month rate
        if storage_type.lower() in {"io1", "io2"}:
            iops_filters = [
                {"Field": "productFamily", "Value": "System Operation"},
                {"Field": "group", "Value": "EBS IOPS"},
                {"Field": "volumeApiName", "Value": storage_type.lower()},
                {"Field": "location", "Value": display_name},
            ]
            iops_raw = await self._get_products("AmazonEC2", iops_filters, max_results=5)
            for item in iops_raw:
                p = self._item_to_price(item, region, PricingTerm.ON_DEMAND, "storage-iops")
                if p:
                    # Override unit if not already recognised as per-IOPS
                    if p.unit == PriceUnit.PER_UNIT:
                        p = p.model_copy(update={"unit": PriceUnit.PER_IOPS_MONTH})
                    prices.append(p)

        # Only cache non-empty results — empty likely means a transient failure
        # or a bug (like the productFamily filter issue) that we don't want to bake in.
        if prices:
            await self._cache.set_prices(
                "aws", "storage", region, cache_extras,
                [p.model_dump(mode="json") for p in prices],
                ttl_hours=self._settings.cache_ttl_hours,
            )
        return prices

    @staticmethod
    def _map_ebs_type(storage_type: str) -> str:
        mapping = {
            "gp2": "General Purpose",
            "gp3": "General Purpose",
            "io1": "Provisioned IOPS",
            "io2": "Provisioned IOPS",
            "st1": "Throughput Optimized HDD",
            "sc1": "Cold HDD",
            "standard": "Magnetic",
        }
        return mapping.get(storage_type.lower(), storage_type)

    async def get_service_price(
        self,
        service: str,
        region: str,
        filters: dict[str, str],
        max_results: int = 20,
        term: PricingTerm = PricingTerm.ON_DEMAND,
    ) -> list[NormalizedPrice]:
        """
        Generic pricing lookup for any AWS service by service code or alias.

        service: canonical offer code (e.g. "AmazonCloudWatch") or alias
                 (e.g. "cloudwatch", "data_transfer", "rds").
        filters: product attribute key/value pairs to narrow results, e.g.
                 {"group": "Metric"} for CloudWatch,
                 {"fromRegionCode": "us-east-1", "toRegionCode": "eu-west-1"}
                 for data transfer.
        term: pricing term — controls which term bucket is extracted from the response.
              Term-level attributes (termType, leaseContractLength, purchaseOption) must
              NOT be passed in filters; they are derived from the term parameter.
        """
        # Term-level attributes are not product attributes — strip them so they
        # don't corrupt the TERM_MATCH filter sent to the pricing API.
        _TERM_ATTRS = {"termType", "leaseContractLength", "purchaseOption"}
        product_filters = {k: v for k, v in filters.items() if k not in _TERM_ATTRS}

        service_code = _resolve_service_code(service)
        try:
            display_name = aws_region_to_display(region)
        except ValueError:
            display_name = region  # pass through unknown regions

        filter_list: list[dict[str, str]] = [
            {"Field": "location", "Value": display_name}
        ]
        for k, v in product_filters.items():
            filter_list.append({"Field": k, "Value": v})

        raw = await self._get_products(service_code, filter_list, max_results=max_results)
        prices = []
        for item in raw:
            p = self._item_to_price(item, region, term, service.lower())
            if p:
                prices.append(p)
        return prices

    async def list_services(self) -> list[dict[str, str]]:
        """Return all available AWS pricing services with their offer codes and aliases."""
        cache_key = "aws:service_index"
        cached = await self._cache.get_metadata(cache_key)
        if cached is not None:
            return cached

        try:
            resp = await asyncio.to_thread(
                httpx.get, _AWS_SERVICE_INDEX_URL, timeout=30, follow_redirects=True
            )
            resp.raise_for_status()
            data = resp.json()
            offers = data.get("offers", {})
        except Exception as e:
            logger.warning("Could not fetch AWS service index: %s", e)
            # Fall back to known aliases
            offers = {v: {} for v in set(_SERVICE_ALIASES.values())}

        # Reverse-map aliases for display
        alias_for: dict[str, list[str]] = {}
        for alias, code in _SERVICE_ALIASES.items():
            alias_for.setdefault(code, []).append(alias)

        services = [
            {
                "service_code": code,
                "aliases": alias_for.get(code, []),
            }
            for code in sorted(offers.keys())
        ]
        await self._cache.set_metadata(
            cache_key, services, ttl_hours=self._settings.metadata_ttl_days * 24
        )
        return services

    async def search_pricing(
        self,
        query: str,
        region: str | None = None,
        max_results: int = 20,
        service_code: str = "AmazonEC2",
    ) -> list[NormalizedPrice]:
        """
        Search pricing catalog by keyword across any AWS service.

        For EC2 (default), applies compute-specific filters so results are
        meaningful instance-type matches. For other services, performs a
        broad text search across all product attributes.
        """
        resolved = _resolve_service_code(service_code)
        filters: list[dict[str, str]] = []
        if region:
            try:
                display_name = aws_region_to_display(region)
                filters.append({"Field": "location", "Value": display_name})
            except ValueError:
                pass

        _EBS_TYPES = {"gp2", "gp3", "io1", "io2", "st1", "sc1", "standard"}
        # Known EC2 product families that are not compute instances.
        # Queries matching these are routed to a productFamily filter search instead
        # of the compute instance search path.
        _EC2_NON_COMPUTE_FAMILIES = {"nat gateway", "storage snapshot", "system operation"}
        if resolved == "AmazonEC2" and query.lower() in _EBS_TYPES:
            # EBS volume type query — use storage-specific filters, not compute filters
            volume_type = self._map_ebs_type(query.lower())
            filters += [
                {"Field": "productFamily", "Value": "Storage"},
                {"Field": "volumeType", "Value": volume_type},
            ]
            raw = await self._get_products(resolved, filters, max_results=max_results)
            prices = []
            for item in raw:
                p = self._item_to_price(item, region or "us-east-1", PricingTerm.ON_DEMAND, "storage")
                if p:
                    if p.unit == PriceUnit.PER_UNIT:
                        p = p.model_copy(update={"unit": PriceUnit.PER_GB_MONTH})
                    prices.append(p)
                if len(prices) >= max_results:
                    break
            return prices
        elif resolved == "AmazonEC2" and query.lower() in _EC2_NON_COMPUTE_FAMILIES:
            # Non-compute EC2 product family search (e.g. "NAT Gateway", "Storage Snapshot").
            # productFamily sits at the product top level — use it as a direct filter.
            filters.append({"Field": "productFamily", "Value": query})
            raw = await self._get_products(resolved, filters, max_results=max_results)
            prices = []
            for item in raw:
                p = self._item_to_price(item, region or "us-east-1", PricingTerm.ON_DEMAND, "nat_gateway")
                if p:
                    prices.append(p)
                if len(prices) >= max_results:
                    break
            return prices
        elif resolved == "AmazonEC2":
            # Keep existing compute-specific filters for backward compatibility
            filters += [
                {"Field": "tenancy", "Value": "Shared"},
                {"Field": "operatingSystem", "Value": "Linux"},
                {"Field": "preInstalledSw", "Value": "NA"},
                {"Field": "capacitystatus", "Value": "Used"},
            ]
            if "." in query and not query.startswith("gpu"):
                filters.append({"Field": "instanceType", "Value": query})

            raw = await self._get_products(resolved, filters, max_results=max_results * 3)
            prices = []
            query_lower = query.lower()
            for item in raw:
                attrs = item.get("product", {}).get("attributes", {})
                instance_type = attrs.get("instanceType", "")
                if "gpu" in query_lower:
                    if attrs.get("gpu", "0") == "0":
                        continue
                elif query_lower not in instance_type.lower():
                    continue
                p = self._item_to_price(item, region or "us-east-1", PricingTerm.ON_DEMAND, "compute")
                if p:
                    prices.append(p)
                if len(prices) >= max_results:
                    break
            return prices
        else:
            # Generic service search: fetch up to max_results * 5 products and
            # text-match query against all attribute values AND top-level product fields
            # (productFamily sits at the product top level, not inside attributes).
            raw = await self._get_products(resolved, filters, max_results=max_results * 5)
            prices = []
            query_lower = query.lower()
            svc_label = service_code.lower().replace("amazon", "").replace("aws", "").strip()
            for item in raw:
                product = item.get("product", {})
                attrs = product.get("attributes", {})
                # Include top-level product fields (productFamily, productGroup) in haystack
                top_level = " ".join(
                    str(product.get(k, ""))
                    for k in ("productFamily", "productGroup")
                )
                haystack = (top_level + " " + " ".join(str(v) for v in attrs.values())).lower()
                if query_lower not in haystack:
                    continue
                p = self._item_to_price(item, region or "us-east-1", PricingTerm.ON_DEMAND, svc_label)
                if p:
                    prices.append(p)
                if len(prices) >= max_results:
                    break
            return prices

    async def list_regions(self, service: str = "compute") -> list[str]:
        return list_aws_regions()

    async def list_instance_types(
        self,
        region: str,
        family: str | None = None,
        min_vcpus: int | None = None,
        min_memory_gb: float | None = None,
        gpu: bool = False,
    ) -> list[InstanceTypeInfo]:
        cache_key = f"aws:instance_types:{region}:{family}:{min_vcpus}:{min_memory_gb}:{gpu}"
        cached = await self._cache.get_metadata(cache_key)
        if cached is not None:
            return [InstanceTypeInfo.model_validate(i) for i in cached]

        display_name = aws_region_to_display(region)
        filters: list[dict[str, str]] = [
            {"Field": "location", "Value": display_name},
            {"Field": "tenancy", "Value": "Shared"},
            {"Field": "operatingSystem", "Value": "Linux"},
            {"Field": "preInstalledSw", "Value": "NA"},
            {"Field": "capacitystatus", "Value": "Used"},
        ]
        if family:
            # family prefix like "m5", "c6g"
            pass  # we filter post-fetch

        raw = await self._get_products("AmazonEC2", filters, max_results=500)

        instances: list[InstanceTypeInfo] = []
        seen: set[str] = set()
        for item in raw:
            attrs = item.get("product", {}).get("attributes", {})
            itype = attrs.get("instanceType", "")
            if not itype or itype in seen:
                continue
            seen.add(itype)

            # Family filter
            if family and not itype.startswith(family):
                continue

            vcpu_str = attrs.get("vcpu", "0").replace(",", "")
            try:
                vcpu = int(vcpu_str)
            except ValueError:
                vcpu = 0

            mem_str = attrs.get("memory", "0 GiB").replace(" GiB", "").replace(",", "")
            try:
                mem_gb = float(mem_str)
            except ValueError:
                mem_gb = 0.0

            gpu_count_str = attrs.get("gpu", "0")
            try:
                gpu_count = int(gpu_count_str)
            except ValueError:
                gpu_count = 0

            if min_vcpus and vcpu < min_vcpus:
                continue
            if min_memory_gb and mem_gb < min_memory_gb:
                continue
            if gpu and gpu_count == 0:
                continue

            instances.append(InstanceTypeInfo(
                provider=CloudProvider.AWS,
                instance_type=itype,
                vcpu=vcpu,
                memory_gb=mem_gb,
                gpu_count=gpu_count,
                gpu_type=attrs.get("gpuMemory"),
                network_performance=attrs.get("networkPerformance"),
                storage=attrs.get("storage"),
                region=region,
            ))

        await self._cache.set_metadata(
            cache_key,
            [i.model_dump(mode="json") for i in instances],
            ttl_hours=self._settings.metadata_ttl_days * 24,
        )
        return instances

    async def check_availability(
        self,
        service: str,
        sku_or_type: str,
        region: str,
    ) -> bool:
        if service == "compute":
            prices = await self.get_compute_price(sku_or_type, region)
            return len(prices) > 0
        if service == "storage":
            prices = await self.get_storage_price(sku_or_type, region)
            return len(prices) > 0
        return False

    async def get_effective_price(
        self,
        service: str,
        instance_type: str,
        region: str,
    ) -> list[EffectivePrice]:
        if not self._settings.aws_enable_cost_explorer or self._ce is None:
            raise NotConfiguredError(
                "Cost Explorer is disabled. Set OCC_AWS_ENABLE_COST_EXPLORER=true "
                "to enable effective pricing (note: each API call costs $0.01)."
            )

        from datetime import date, timedelta
        end = date.today().replace(day=1)
        start = (end - timedelta(days=1)).replace(day=1)

        try:
            resp = self._ce.get_cost_and_usage(
                TimePeriod={"Start": str(start), "End": str(end)},
                Granularity="MONTHLY",
                Metrics=["NetAmortizedCost", "UsageQuantity"],
                Filter={
                    "And": [
                        {"Dimensions": {"Key": "SERVICE", "Values": [self._service_to_ce(service)]}},
                        {"Dimensions": {"Key": "REGION", "Values": [region]}},
                        {"Dimensions": {"Key": "INSTANCE_TYPE", "Values": [instance_type]}},
                    ]
                },
            )
        except botocore.exceptions.ClientError as e:
            logger.error("Cost Explorer error: %s", e)
            raise

        base_prices = await self.get_compute_price(instance_type, region)
        if not base_prices:
            return []

        base = base_prices[0]
        results = []
        for result_by_time in resp.get("ResultsByTime", []):
            total = result_by_time.get("Total", {})
            net_cost = Decimal(total.get("NetAmortizedCost", {}).get("Amount", "0"))
            usage = Decimal(total.get("UsageQuantity", {}).get("Amount", "1") or "1")
            if usage > 0:
                effective_rate = net_cost / usage
                discount_pct = float(
                    (base.price_per_unit - effective_rate) / base.price_per_unit * 100
                ) if base.price_per_unit > 0 else 0.0
                results.append(EffectivePrice(
                    base_price=base,
                    effective_price_per_unit=effective_rate,
                    discount_type="Blended (RI/SP/EDP)",
                    discount_pct=round(discount_pct, 2),
                    source="cost_explorer",
                ))
        return results

    @staticmethod
    def _service_to_ce(service: str) -> str:
        mapping = {
            "compute": "Amazon Elastic Compute Cloud - Compute",
            "storage": "Amazon Elastic Block Store",
            "database": "Amazon Relational Database Service",
            "s3": "Amazon Simple Storage Service",
        }
        return mapping.get(service, service)

    # ------------------------------------------------------------------
    # Savings Plans API
    # ------------------------------------------------------------------

    def _require_auth(self) -> None:
        if not self._settings.aws_enable_cost_explorer or self._sp is None:
            raise NotConfiguredError(
                "Savings Plans / Cost Explorer APIs require credentials. "
                "Set OCC_AWS_ENABLE_COST_EXPLORER=true and ensure "
                "AWS credentials are configured."
            )

    def get_active_savings_plans(self) -> list[dict[str, Any]]:
        """Return all active Savings Plans in the account."""
        self._require_auth()
        resp = self._sp.describe_savings_plans(states=["active"])
        return resp.get("savingsPlans", [])

    def get_savings_plan_rates(self, savings_plan_id: str) -> list[dict[str, Any]]:
        """Return the discounted rates provided by a specific Savings Plan."""
        self._require_auth()
        resp = self._sp.describe_savings_plan_rates(
            savingsPlanId=savings_plan_id,
        )
        return resp.get("searchResults", [])

    def get_active_reserved_instances(self) -> list[dict[str, Any]]:
        """Return all active EC2 Reserved Instances in the configured region."""
        self._require_auth()
        resp = self._ec2.describe_reserved_instances(
            Filters=[{"Name": "state", "Values": ["active"]}]
        )
        return resp.get("ReservedInstances", [])

    # ------------------------------------------------------------------
    # v0.8.0 capability protocol
    # ------------------------------------------------------------------

    def supports(self, domain: PricingDomain, service: str | None = None) -> bool:
        return _AWS_CAPABILITIES.get((domain.value, service), False)

    def supported_terms(
        self, domain: PricingDomain, service: str | None = None
    ) -> list[PricingTerm]:
        base = [PricingTerm.ON_DEMAND]
        if domain == PricingDomain.COMPUTE:
            base += [
                PricingTerm.SPOT,
                PricingTerm.RESERVED_1YR, PricingTerm.RESERVED_1YR_PARTIAL,
                PricingTerm.RESERVED_1YR_ALL, PricingTerm.RESERVED_3YR,
                PricingTerm.RESERVED_3YR_PARTIAL, PricingTerm.RESERVED_3YR_ALL,
                PricingTerm.COMPUTE_SP, PricingTerm.EC2_INSTANCE_SP,
            ]
        elif domain == PricingDomain.DATABASE:
            base += [
                PricingTerm.RESERVED_1YR, PricingTerm.RESERVED_3YR,
                PricingTerm.RESERVED_1YR_PARTIAL, PricingTerm.RESERVED_1YR_ALL,
            ]
        elif domain == PricingDomain.AI and service == "bedrock":
            base = [PricingTerm.ON_DEMAND, PricingTerm.SAVINGS_PLAN]
        return base

    _MAJOR_REGIONS = [
        "us-east-1", "us-east-2", "us-west-1", "us-west-2",
        "ca-central-1", "eu-west-1", "eu-west-2", "eu-central-1",
        "ap-southeast-1", "ap-southeast-2", "ap-northeast-1", "ap-south-1",
    ]
    _SOURCE_URL = "https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/index.json"

    def major_regions(self) -> list[str]:
        return self._MAJOR_REGIONS

    def default_region(self) -> str:
        return "us-east-1"

    def bom_advisories(
        self, services: set[str], sample_region: str
    ) -> list[dict[str, str]]:
        advisories: list[dict[str, str]] = []
        if "compute" in services or "database" in services:
            advisories.append({
                "item": "Data transfer (egress)",
                "why": "Outbound traffic to the internet or cross-region — varies by workload",
                "how_to_price": (
                    f'get_price(spec={{"provider": "aws", "domain": "network", "service": "data_transfer", '
                    f'"region": "{sample_region}"}})'
                ),
                "price": "unknown — use the how_to_price call above to get the real figure",
            })
            advisories.append({
                "item": "Load balancer (ALB/NLB)",
                "why": "Typically needed in front of compute clusters",
                "how_to_price": (
                    f'get_price(spec={{"provider": "aws", "domain": "network", "service": "lb", '
                    f'"region": "{sample_region}"}})'
                ),
                "price": "unknown — use the how_to_price call above to get the real figure",
            })
            advisories.append({
                "item": "NAT Gateway",
                "why": "Required if EC2 instances are in private subnets",
                "how_to_price": (
                    f'get_price(spec={{"provider": "aws", "domain": "network", "service": "nat", '
                    f'"region": "{sample_region}"}})'
                ),
                "price": "unknown — use the how_to_price call above to get the real figure",
            })
        advisories.append({
            "item": "CloudWatch monitoring",
            "why": "Logs, metrics, alarms — scales with number of instances and log volume",
            "how_to_price": (
                f'get_price(spec={{"provider": "aws", "domain": "observability", "service": "cloudwatch", '
                f'"region": "{sample_region}"}})'
            ),
            "price": "unknown — use the how_to_price call above to get the real figure",
        })
        if "database" in services:
            advisories.append({
                "item": "RDS automated backups",
                "why": "Free for storage equal to DB size; extra storage charged beyond that",
                "how_to_price": (
                    f'search_pricing(provider="aws", query="RDS Storage Snapshot", region="{sample_region}")'
                ),
                "price": "unknown — use the how_to_price call above to get the real figure",
            })
        if "storage" in services:
            advisories.append({
                "item": "EBS snapshots",
                "why": "Point-in-time backups stored in S3 — charged per GB-month",
                "how_to_price": (
                    f'search_pricing(provider="aws", query="EBS Storage Snapshot", region="{sample_region}")'
                ),
                "price": "unknown — use the how_to_price call above to get the real figure",
            })
        return advisories

    async def get_spot_history(
        self, spec: PricingSpec, hours: int = 24, availability_zone: str = ""
    ) -> dict:
        from opencloudcosts.models import ComputePricingSpec
        if not isinstance(spec, ComputePricingSpec):
            from opencloudcosts.providers.base import NotSupportedError
            raise NotSupportedError(
                provider=self.provider,
                domain=spec.domain,
                service=spec.service,
                reason="get_spot_history requires domain='compute'.",
            )
        return await self._get_spot_history(
            instance_type=spec.resource_type or "",
            region=spec.region,
            os=getattr(spec, "os", "Linux") or "Linux",
            availability_zone=availability_zone,
            hours=hours,
        )

    async def get_price(self, spec: PricingSpec) -> PricingResult:
        """Unified dispatcher — routes spec to the appropriate internal method."""
        if not self.supports(spec.domain, spec.service):
            raise NotSupportedError(
                provider=self.provider,
                domain=spec.domain,
                service=spec.service,
                reason=f"AWS does not support domain='{spec.domain.value}', service='{spec.service}'.",
                alternatives=["Use describe_catalog(provider='aws') to see supported combinations."],
            )

        public_prices = await self._dispatch_public(spec)
        result = PricingResult(public_prices=public_prices, source="catalog")

        # Enrich with effective pricing when auth is configured
        commitments = await self._applicable_commitments(spec)
        if commitments:
            result.auth_available = True
            result.contracted_prices = [c.base_price for c in commitments]
            result.effective_price = commitments[0] if commitments else None
        elif self._settings.aws_enable_cost_explorer:
            result.auth_available = True

        return result

    async def _dispatch_public(self, spec: PricingSpec) -> list[NormalizedPrice]:
        """Route spec to the appropriate public-pricing internal method."""
        if isinstance(spec, ComputePricingSpec):
            return await self._price_compute(spec)
        if isinstance(spec, StoragePricingSpec):
            return await self._price_storage(spec)
        if isinstance(spec, DatabasePricingSpec):
            return await self._price_database(spec)
        if isinstance(spec, AiPricingSpec):
            return await self._price_ai(spec)
        if isinstance(spec, ServerlessPricingSpec):
            return await self._price_serverless(spec)
        if isinstance(spec, AnalyticsPricingSpec):
            return await self._price_analytics(spec)
        if isinstance(spec, NetworkPricingSpec):
            return await self._price_network(spec)
        if isinstance(spec, ObservabilityPricingSpec):
            return await self._price_observability(spec)
        if isinstance(spec, ContainerPricingSpec):
            return await self._price_container(spec)
        if isinstance(spec, EgressPricingSpec):
            return await self._price_egress(spec)
        raise NotSupportedError(
            provider=self.provider,
            domain=spec.domain,
            service=spec.service,
            reason=f"Unhandled domain '{spec.domain.value}'.",
        )

    async def _price_egress(self, spec: EgressPricingSpec) -> list[NormalizedPrice]:
        """Inter-region data transfer pricing from AWS Bulk Pricing (AWSDataTransfer)."""
        src = spec.source_region or spec.region
        dst = spec.dest_region

        filters: list[dict[str, str]] = [
            {"Field": "transferType", "Value": "AWS Inter-Region Outbound"},
        ]
        if src:
            try:
                filters.append({"Field": "fromRegionCode", "Value": src})
            except Exception:
                pass

        raw = await self._get_products("AWSDataTransfer", filters, max_results=30)
        prices = []
        for item in raw:
            attrs = item.get("product", {}).get("attributes", {})
            to_region = attrs.get("toRegionCode", "")
            # If caller specified dest_region, filter to exact match; otherwise accept all
            if dst and to_region and to_region != dst:
                continue
            p = self._item_to_price(item, src or "us-east-1", PricingTerm.ON_DEMAND, "inter_region_egress")
            if p:
                p = p.model_copy(update={
                    "source_url": self._SOURCE_URL,
                    "attributes": {
                        **p.attributes,
                        "fromRegionCode": attrs.get("fromRegionCode", src),
                        "toRegionCode": to_region,
                        "transferType": attrs.get("transferType", ""),
                    },
                })
                prices.append(p)
            if len(prices) >= 10:
                break

        return prices

    async def _price_compute(self, spec: ComputePricingSpec) -> list[NormalizedPrice]:
        if spec.service == "fargate" or (spec.vcpu is not None and spec.memory_gb is not None and not spec.resource_type):
            return await self._price_fargate(spec)
        if not spec.resource_type:
            raise NotSupportedError(
                provider=self.provider, domain=spec.domain, service=spec.service,
                reason="ComputePricingSpec requires resource_type (instance type) or vcpu+memory_gb (Fargate).",
                example_invocation={"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", "region": spec.region},
            )
        return await self.get_compute_price(spec.resource_type, spec.region, spec.os, spec.term)

    async def _price_fargate(self, spec: ComputePricingSpec) -> list[NormalizedPrice]:
        vcpu = spec.vcpu or 1.0
        memory_gb = spec.memory_gb or 2.0
        vcpu_filters: dict[str, str] = {"cputype": "perCPU"}
        mem_filters: dict[str, str] = {"memorytype": "perGB"}
        if spec.os.lower() == "windows":
            vcpu_filters["operatingSystem"] = "Windows"
        vcpu_prices = await self.get_service_price("fargate", spec.region, vcpu_filters, max_results=5)
        mem_prices = await self.get_service_price("fargate", spec.region, mem_filters, max_results=5)
        prices = []
        if vcpu_prices:
            prices.append(vcpu_prices[0].model_copy(update={
                "description": f"Fargate vCPU ({vcpu} vCPU × ${vcpu_prices[0].price_per_unit:.6f}/hr)",
                "attributes": {**vcpu_prices[0].attributes, "vcpu": str(vcpu), "os": spec.os},
            }))
        if mem_prices:
            prices.append(mem_prices[0].model_copy(update={
                "description": f"Fargate memory ({memory_gb} GB × ${mem_prices[0].price_per_unit:.6f}/GB-hr)",
                "attributes": {**mem_prices[0].attributes, "memory_gb": str(memory_gb)},
            }))
        return prices

    async def _price_storage(self, spec: StoragePricingSpec) -> list[NormalizedPrice]:
        return await self.get_storage_price(
            spec.storage_type, spec.region, spec.size_gb, spec.iops
        )

    async def _price_database(self, spec: DatabasePricingSpec) -> list[NormalizedPrice]:
        svc = spec.service or "rds"
        if svc in ("rds", "database", None):
            _RDS_ENGINE_MAP = {
                "mysql": "MySQL", "postgresql": "PostgreSQL", "postgres": "PostgreSQL",
                "mariadb": "MariaDB", "oracle": "Oracle", "sqlserver": "SQL Server",
                "aurora-mysql": "Aurora MySQL", "aurora-postgresql": "Aurora PostgreSQL",
                "aurora-postgres": "Aurora PostgreSQL",
            }
            engine_normalized = _RDS_ENGINE_MAP.get(spec.engine.lower(), spec.engine)
            deployment_option = "Multi-AZ" if spec.deployment.lower() in ("multi-az", "ha", "multi-zone") else "Single-AZ"
            filters: dict[str, str] = {
                "instanceType": spec.resource_type,
                "databaseEngine": engine_normalized,
                "deploymentOption": deployment_option,
            }
            return await self.get_service_price("rds", spec.region, filters, max_results=5, term=spec.term)
        if svc == "elasticache":
            filters = {"instanceType": spec.resource_type}
            return await self.get_service_price("elasticache", spec.region, filters, max_results=5)
        raise NotSupportedError(
            provider=self.provider, domain=spec.domain, service=svc,
            reason=f"AWS database service '{svc}' not recognised. Use 'rds' or 'elasticache'.",
            alternatives=["service='rds'", "service='elasticache'"],
        )

    async def _price_ai(self, spec: AiPricingSpec) -> list[NormalizedPrice]:
        svc = spec.service or "bedrock"
        if svc == "bedrock":
            catalog_name = _BEDROCK_MODEL_ALIASES.get((spec.model or "").lower(), spec.model or "")
            mode = spec.mode or "on_demand"
            if mode == "batch":
                in_type, out_type = "input tokens batch", "output tokens batch"
            else:
                in_type, out_type = "Input tokens", "Output tokens"
            in_prices = await self.get_service_price(
                "bedrock", spec.region, {"model": catalog_name, "inferenceType": in_type}, max_results=5
            )
            out_prices = await self.get_service_price(
                "bedrock", spec.region, {"model": catalog_name, "inferenceType": out_type}, max_results=5
            )
            prices = [p for p in in_prices + out_prices if p]

            # When token counts are provided, compute and prepend a total-cost NormalizedPrice.
            # Bedrock prices are per individual token; expose per-million-token rates too.
            if prices and (spec.input_tokens or spec.output_tokens):
                in_rate = next((p.price_per_unit for p in prices if "input" in p.description.lower()), None)
                out_rate = next((p.price_per_unit for p in prices if "output" in p.description.lower()), None)
                total = Decimal("0")
                components: list[str] = []
                if in_rate is not None and spec.input_tokens:
                    in_cost = in_rate * Decimal(str(spec.input_tokens))
                    total += in_cost
                    per_m = in_rate * Decimal("1000000")
                    components.append(
                        f"{spec.input_tokens:,} input tokens × ${per_m:.4f}/1M = ${in_cost:.4f}"
                    )
                if out_rate is not None and spec.output_tokens:
                    out_cost = out_rate * Decimal(str(spec.output_tokens))
                    total += out_cost
                    per_m = out_rate * Decimal("1000000")
                    components.append(
                        f"{spec.output_tokens:,} output tokens × ${per_m:.4f}/1M = ${out_cost:.4f}"
                    )
                if total > 0:
                    prices.insert(0, NormalizedPrice(
                        provider=CloudProvider.AWS,
                        service="bedrock",
                        sku_id=f"aws:bedrock:{catalog_name}:total",
                        product_family="AI",
                        description="Bedrock total: " + "; ".join(components),
                        region=spec.region,
                        pricing_term=spec.term,
                        price_per_unit=total,
                        unit=PriceUnit.PER_UNIT,
                        attributes={
                            "billing_dimension": "workload_total",
                            "model": catalog_name,
                            "input_tokens": str(spec.input_tokens or 0),
                            "output_tokens": str(spec.output_tokens or 0),
                        },
                    ))
            return prices
        if svc == "sagemaker":
            filters: dict[str, str] = {}
            if spec.machine_type:
                filters["instanceType"] = spec.machine_type
            return await self.get_service_price("sagemaker", spec.region, filters, max_results=10)
        raise NotSupportedError(
            provider=self.provider, domain=spec.domain, service=svc,
            reason=f"AWS AI service '{svc}' not recognised. Use 'bedrock' or 'sagemaker'.",
            alternatives=["service='bedrock'", "service='sagemaker'"],
        )

    async def _price_serverless(self, spec: ServerlessPricingSpec) -> list[NormalizedPrice]:
        svc = (spec.service or "lambda").lower()
        if svc == "lambda":
            return await self._price_lambda(spec)
        return await self.get_service_price(svc, spec.region, {}, max_results=10)

    async def _price_lambda(self, spec: ServerlessPricingSpec) -> list[NormalizedPrice]:
        region = spec.region or "us-east-1"
        cache_key_extras = {"service": "lambda"}
        cached_meta = await self._cache.get_prices_with_meta("aws", "lambda", region, cache_key_extras)
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            return self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at, self._SOURCE_URL,
            )

        try:
            display_name = aws_region_to_display(region)
        except ValueError:
            display_name = region

        raw = await asyncio.to_thread(
            self._get_products_bulk,
            "AWSLambda", region,
            [{"Field": "location", "Value": display_name}],
            max_results=500,
        )

        request_price: Decimal | None = None
        duration_price: Decimal | None = None
        for item in raw:
            attrs = item.get("product", {}).get("attributes", {})
            group = attrs.get("group", "")
            usagetype = attrs.get("usagetype", "")
            extracted = self._extract_on_demand_price(item)
            if not extracted or extracted[0] == 0:
                continue
            price_val, unit_str = extracted
            # Standard x86 request rate (not ARM, not Edge, not managed instances)
            if (group == "AWS-Lambda-Requests" and "ARM" not in usagetype
                    and "Edge" not in usagetype and request_price is None):
                request_price = price_val
            # Standard x86 duration rate (first/highest tier at startUsageAmount=0)
            elif (group == "AWS-Lambda-Duration" and usagetype == "Lambda-GB-Second"
                  and duration_price is None):
                duration_price = price_val

        prices = []
        if request_price:
            per_million = request_price * 1_000_000
            prices.append(NormalizedPrice(
                provider=CloudProvider.AWS, service="serverless",
                sku_id=f"aws:lambda:{region}:requests",
                product_family="AWS Lambda",
                description=f"Lambda requests — ${per_million:.2f} per 1M requests",
                region=region, pricing_term=PricingTerm.ON_DEMAND,
                price_per_unit=request_price, unit=PriceUnit.PER_REQUEST,
                attributes={"billing_dimension": "requests",
                            "per_million_requests": f"${per_million:.4f}"},
            ))
        if duration_price:
            prices.append(NormalizedPrice(
                provider=CloudProvider.AWS, service="serverless",
                sku_id=f"aws:lambda:{region}:duration",
                product_family="AWS Lambda",
                description="Lambda duration (per GB-second)",
                region=region, pricing_term=PricingTerm.ON_DEMAND,
                price_per_unit=duration_price, unit=PriceUnit.PER_GB_SECOND,
                attributes={"billing_dimension": "gb_second"},
            ))

        if prices:
            await self._cache.set_prices(
                "aws", "lambda", region, cache_key_extras,
                [p.model_dump(mode="json") for p in prices],
                ttl_hours=self._settings.cache_ttl_hours,
            )
        return prices

    async def _price_analytics(self, spec: AnalyticsPricingSpec) -> list[NormalizedPrice]:
        svc = spec.service or "redshift"
        return await self.get_service_price(svc, spec.region, {}, max_results=10)

    async def _price_network(self, spec: NetworkPricingSpec) -> list[NormalizedPrice]:
        _NET_SERVICE_MAP = {
            "lb": "elb", "load_balancer": "elb", "elb": "elb",
            "cloud_lb": "elb",                              # GCP-style alias
            "cdn": "cloudfront", "cloudfront": "cloudfront", "cloud_cdn": "cloudfront",
            "nat": "nat_gateway", "nat_gateway": "nat_gateway",
            "cloud_nat": "nat_gateway",                     # GCP-style alias
            "waf": "waf",
            "data_transfer": "data_transfer", "egress": "data_transfer",
        }
        svc = _NET_SERVICE_MAP.get(spec.service or "lb", spec.service or "elb")
        return await self.get_service_price(svc, spec.region, {}, max_results=10)

    async def _price_observability(self, spec: ObservabilityPricingSpec) -> list[NormalizedPrice]:
        svc = spec.service or "cloudwatch"
        return await self.get_service_price(svc, spec.region, {}, max_results=10)

    async def _price_container(self, spec: ContainerPricingSpec) -> list[NormalizedPrice]:
        svc = spec.service or "eks"
        return await self.get_service_price(svc, spec.region, {}, max_results=10)

    async def _applicable_commitments(self, spec: PricingSpec) -> list[EffectivePrice]:
        """Return account commitments applicable to this spec when Cost Explorer auth is present."""
        if not self._settings.aws_enable_cost_explorer or self._ce is None:
            return []
        if not isinstance(spec, ComputePricingSpec) or not spec.resource_type:
            return []
        try:
            return await self.get_effective_price("compute", spec.resource_type, spec.region)
        except (NotConfiguredError, Exception):
            return []

    async def describe_catalog(self) -> ProviderCatalog:
        return ProviderCatalog(
            provider=CloudProvider.AWS,
            domains=list({PricingDomain(d) for d, _ in _AWS_CAPABILITIES}),
            services={
                "compute": ["ec2", "fargate"],
                "storage": ["ebs", "s3"],
                "database": ["rds", "elasticache"],
                "ai": ["bedrock", "sagemaker"],
                "serverless": ["lambda"],
                "analytics": ["redshift", "athena"],
                "network": ["lb", "cdn", "nat", "waf", "data_transfer"],
                "observability": ["cloudwatch"],
                "container": ["eks"],
                "inter_region_egress": [],
            },
            supported_terms={
                "compute": [t.value for t in self.supported_terms(PricingDomain.COMPUTE)],
                "database": [t.value for t in self.supported_terms(PricingDomain.DATABASE)],
                "ai/bedrock": [t.value for t in self.supported_terms(PricingDomain.AI, "bedrock")],
            },
            filter_hints={
                "compute": {"resource_type": "EC2 instance type, e.g. 'm5.xlarge'", "os": "'Linux' or 'Windows'", "term": "pricing term"},
                "compute/fargate": {"vcpu": "vCPU count (0.25–16)", "memory_gb": "memory in GB", "os": "'Linux' or 'Windows'"},
                "storage": {"storage_type": "'gp3', 'io1', 'st1', 'sc1', 's3-standard'", "size_gb": "size for monthly estimate"},
                "database/rds": {"resource_type": "DB instance type e.g. 'db.r5.large'", "engine": "MySQL/PostgreSQL/MariaDB/Oracle/SQLServer", "deployment": "'single-az' or 'multi-az'"},
                "database/elasticache": {"resource_type": "cache.r6g.large", "service": "elasticache"},
                "ai/bedrock": {"model": "e.g. 'claude-3-5-sonnet', 'nova-pro', 'llama-3-1-70b'", "mode": "'on_demand' or 'batch'"},
                "ai/sagemaker": {"machine_type": "ml instance type e.g. 'ml.g5.xlarge'"},
                "serverless/lambda": {"service": "lambda"},
                "analytics/redshift": {"service": "redshift"},
                "analytics/athena": {"service": "athena"},
                "network/lb": {"service": "lb", "note": "also accepts 'cloud_lb'"},
                "network/cdn": {"service": "cdn"},
                "network/nat": {"service": "nat", "note": "also accepts 'cloud_nat'"},
                "network/data_transfer": {"service": "data_transfer"},
                "observability/cloudwatch": {"service": "cloudwatch"},
                "container/eks": {"service": "eks"},
                "inter_region_egress": {"source_region": "origin region e.g. 'us-east-1'", "dest_region": "destination region e.g. 'eu-west-1'; empty = internet egress"},
            },
            example_invocations={
                "compute": {"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", "region": "us-east-1", "os": "Linux", "term": "on_demand"},
                "compute/fargate": {"provider": "aws", "domain": "compute", "service": "fargate", "vcpu": 2.0, "memory_gb": 4.0, "region": "us-east-1"},
                "storage": {"provider": "aws", "domain": "storage", "storage_type": "gp3", "region": "us-east-1", "size_gb": 100},
                "database/rds": {"provider": "aws", "domain": "database", "service": "rds", "resource_type": "db.r5.large", "engine": "MySQL", "deployment": "single-az", "region": "us-east-1"},
                "ai/bedrock": {"provider": "aws", "domain": "ai", "service": "bedrock", "model": "claude-3-5-sonnet", "region": "us-east-1", "input_tokens": 1000000, "output_tokens": 1000000},
                "serverless/lambda": {"provider": "aws", "domain": "serverless", "service": "lambda", "region": "us-east-1"},
                "observability/cloudwatch": {"provider": "aws", "domain": "observability", "service": "cloudwatch", "region": "us-east-1"},
                "container/eks": {"provider": "aws", "domain": "container", "service": "eks", "region": "us-east-1"},
                "inter_region_egress": {"provider": "aws", "domain": "inter_region_egress", "source_region": "us-east-1", "dest_region": "eu-west-1"},
            },
            decision_matrix={
                "ECS on Fargate": "compute/fargate — use vcpu + memory_gb params",
                "ECS tasks (Fargate launch type)": "compute/fargate",
                "EC2 instances": "compute/ec2",
                "Lambda functions": "serverless/lambda",
                "EBS volumes": "storage/ebs",
                "S3 buckets": "storage/s3",
                "RDS instances": "database/rds",
                "ElastiCache clusters": "database/elasticache",
                "Bedrock inference": "ai/bedrock",
                "SageMaker endpoints": "ai/sagemaker",
                "EKS clusters": "container/eks",
                "CloudWatch metrics": "observability/cloudwatch",
                "Application Load Balancer": "network/lb (also: service='cloud_lb')",
                "NAT Gateway": "network/nat (also: service='cloud_nat')",
                "CloudFront CDN": "network/cdn — AWS-ONLY; for GCP Cloud CDN use provider='gcp', service='cloud_cdn'",
                "AWS WAF": "network/waf",
                "Data transfer / egress": "network/data_transfer",
                "Inter-region data transfer (region-to-region)": "inter_region_egress — use source_region + dest_region",
            },
        )

    async def get_discount_summary(self) -> dict[str, Any]:
        """
        Aggregate summary of all active discounts: Savings Plans + Reserved Instances.
        Returns structured data ready for MCP tool consumption.
        """
        self._require_auth()

        # Savings Plans
        sp_list = self.get_active_savings_plans()
        savings_plans_summary = []
        for sp in sp_list:
            savings_plans_summary.append({
                "id": sp.get("savingsPlanId", ""),
                "type": sp.get("savingsPlanType", ""),       # Compute, EC2Instance, SageMaker
                "payment_option": sp.get("paymentOption", ""),
                "commitment_usd_per_hour": sp.get("commitment", ""),
                "term_years": "3" if sp.get("termDurationInSeconds", 0) > 94_000_000 else "1",
                "start": sp.get("start", ""),
                "end": sp.get("end", ""),
                "state": sp.get("state", ""),
                "utilization_pct": sp.get("utilizationPercentage", "N/A"),
            })

        # Reserved Instances
        ri_list = self.get_active_reserved_instances()
        ri_summary = []
        for ri in ri_list:
            end_dt = ri.get("End")
            days_remaining = None
            if end_dt:
                from datetime import datetime
                now = datetime.now(UTC)
                end_aware = end_dt if end_dt.tzinfo else end_dt.replace(tzinfo=UTC)
                days_remaining = max(0, (end_aware - now).days)
            ri_summary.append({
                "instance_type": ri.get("InstanceType", ""),
                "region": self._settings.aws_region,
                "count": ri.get("InstanceCount", 0),
                "offering_type": ri.get("OfferingType", ""),  # No Upfront, Partial, All
                "duration_years": "3" if ri.get("Duration", 0) > 94_000_000 else "1",
                "days_remaining": days_remaining,
                "fixed_price": ri.get("FixedPrice", 0),
                "usage_price": ri.get("UsagePrice", 0),
                "product_description": ri.get("ProductDescription", ""),
                "state": ri.get("State", ""),
            })

        # Cost Explorer: get SP + RI utilization for last full month
        utilization: dict[str, Any] = {}
        try:
            from datetime import date, timedelta
            end = date.today().replace(day=1)
            start = (end - timedelta(days=1)).replace(day=1)
            period = {"Start": str(start), "End": str(end)}

            sp_util = self._ce.get_savings_plans_utilization(TimePeriod=period)
            total_sp = sp_util.get("Total", {}).get("Utilization", {})
            utilization["savings_plans"] = {
                "utilized_hours": total_sp.get("TotalCommitment", ""),
                "unused_hours": total_sp.get("UnusedCommitment", ""),
                "utilization_pct": total_sp.get("UtilizationPercentage", ""),
                "net_savings": sp_util.get("Total", {}).get("Savings", {}).get("NetSavings", ""),
            }

            ri_util = self._ce.get_reservation_utilization(
                TimePeriod=period, Granularity="MONTHLY"
            )
            total_ri = ri_util.get("Total", {}).get("UtilizationsByTime", [{}])
            if total_ri:
                utilization["reserved_instances"] = {
                    "utilization_pct": ri_util.get("Total", {}).get("UtilizationPercentage", ""),
                    "on_demand_cost_covered": ri_util.get("Total", {}).get("OnDemandCostOfRIHoursUsed", ""),
                    "unrealized_savings": ri_util.get("Total", {}).get("UnrealizedSavings", ""),
                }
        except botocore.exceptions.ClientError as e:
            utilization["error"] = str(e)

        return {
            "savings_plans": savings_plans_summary,
            "reserved_instances": ri_summary,
            "utilization": utilization,
            "sp_count": len(savings_plans_summary),
            "ri_count": len(ri_summary),
        }
