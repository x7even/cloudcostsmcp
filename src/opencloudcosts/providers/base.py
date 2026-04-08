"""Abstract provider interface — all cloud providers implement this Protocol."""
from __future__ import annotations

from typing import Protocol, runtime_checkable

from opencloudcosts.models import (
    CloudProvider,
    EffectivePrice,
    InstanceTypeInfo,
    NormalizedPrice,
    PricingTerm,
)


class NotConfiguredError(Exception):
    """Raised when a provider requires credentials that have not been supplied."""


@runtime_checkable
class CloudPricingProvider(Protocol):
    """Protocol every provider must satisfy."""

    provider: CloudProvider

    async def get_compute_price(
        self,
        instance_type: str,
        region: str,
        os: str = "Linux",
        term: PricingTerm = PricingTerm.ON_DEMAND,
    ) -> list[NormalizedPrice]:
        """Return pricing for a specific compute instance type in a region."""
        ...

    async def get_storage_price(
        self,
        storage_type: str,
        region: str,
        size_gb: float | None = None,
    ) -> list[NormalizedPrice]:
        """Return pricing for block/object storage."""
        ...

    async def search_pricing(
        self,
        query: str,
        region: str | None = None,
        max_results: int = 20,
    ) -> list[NormalizedPrice]:
        """Free-text search across the pricing catalog."""
        ...

    async def list_regions(self, service: str = "compute") -> list[str]:
        """Return all regions where a service is available."""
        ...

    async def list_instance_types(
        self,
        region: str,
        family: str | None = None,
        min_vcpus: int | None = None,
        min_memory_gb: float | None = None,
        gpu: bool = False,
    ) -> list[InstanceTypeInfo]:
        """Return available instance types matching the given filters."""
        ...

    async def check_availability(
        self,
        service: str,
        sku_or_type: str,
        region: str,
    ) -> bool:
        """Return True if the given SKU/instance type is available in the region."""
        ...

    async def get_effective_price(
        self,
        service: str,
        instance_type: str,
        region: str,
    ) -> list[EffectivePrice]:
        """
        Return effective/bespoke pricing reflecting account-level discounts.
        Raises NotConfiguredError if the provider is not authenticated.
        """
        ...
