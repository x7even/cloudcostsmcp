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
from decimal import Decimal
from typing import Any

import boto3
import botocore.exceptions
import httpx

from opencloudcosts.cache import CacheManager
from opencloudcosts.config import Settings
from opencloudcosts.models import (
    CloudProvider,
    EffectivePrice,
    InstanceTypeInfo,
    NormalizedPrice,
    PriceUnit,
    PricingTerm,
)
from opencloudcosts.providers.base import NotConfiguredError
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
}

# Index of services available in the AWS bulk pricing API (cached after first fetch)
_AWS_SERVICE_INDEX_URL = f"{_BULK_BASE}/index.json"


def _resolve_service_code(service: str) -> str:
    """Resolve alias or pass through as-is (AWS offer codes are case-sensitive)."""
    return _SERVICE_ALIASES.get(service.lower(), service)


def _boto_session(settings: Settings) -> boto3.Session:
    kwargs: dict[str, Any] = {}
    if settings.aws_profile:
        kwargs["profile_name"] = settings.aws_profile
    return boto3.Session(**kwargs)


class AWSProvider:
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
            # Apply all TERM_MATCH filters
            if not all(attrs.get(k) == v for k, v in field_filters.items()):
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
        cached = await self._cache.get_prices("aws", "compute", region, cache_extras)
        if cached is not None:
            return [NormalizedPrice.model_validate(p) for p in cached]

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
    ) -> list[NormalizedPrice]:
        cache_extras = {"storage_type": storage_type}
        cached = await self._cache.get_prices("aws", "storage", region, cache_extras)
        if cached is not None:
            return [NormalizedPrice.model_validate(p) for p in cached]

        display_name = aws_region_to_display(region)

        # EBS volume types
        ebs_types = {"gp2", "gp3", "io1", "io2", "st1", "sc1", "standard"}
        if storage_type.lower() in ebs_types:
            filters = [
                {"Field": "volumeType", "Value": self._map_ebs_type(storage_type)},
                {"Field": "location", "Value": display_name},
                {"Field": "productFamily", "Value": "Storage"},
            ]
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
    ) -> list[NormalizedPrice]:
        """
        Generic pricing lookup for any AWS service by service code or alias.

        service: canonical offer code (e.g. "AmazonCloudWatch") or alias
                 (e.g. "cloudwatch", "data_transfer", "rds").
        filters: attribute key/value pairs to narrow results, e.g.
                 {"group": "Metric"} for CloudWatch,
                 {"fromRegionCode": "us-east-1", "toRegionCode": "eu-west-1"}
                 for data transfer.
        """
        service_code = _resolve_service_code(service)
        try:
            display_name = aws_region_to_display(region)
        except ValueError:
            display_name = region  # pass through unknown regions

        filter_list: list[dict[str, str]] = [
            {"Field": "location", "Value": display_name}
        ]
        for k, v in filters.items():
            filter_list.append({"Field": k, "Value": v})

        raw = await self._get_products(service_code, filter_list, max_results=max_results)
        prices = []
        for item in raw:
            p = self._item_to_price(item, region, PricingTerm.ON_DEMAND, service.lower())
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

        if resolved == "AmazonEC2":
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
            # text-match query against all attribute values
            raw = await self._get_products(resolved, filters, max_results=max_results * 5)
            prices = []
            query_lower = query.lower()
            svc_label = service_code.lower().replace("amazon", "").replace("aws", "").strip()
            for item in raw:
                attrs = item.get("product", {}).get("attributes", {})
                haystack = " ".join(str(v) for v in attrs.values()).lower()
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
            from datetime import timezone
            end_dt = ri.get("End")
            days_remaining = None
            if end_dt:
                from datetime import datetime
                now = datetime.now(timezone.utc)
                end_aware = end_dt if end_dt.tzinfo else end_dt.replace(tzinfo=timezone.utc)
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
