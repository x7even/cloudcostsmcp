"""Abstract provider interface — all cloud providers implement this Protocol."""
from __future__ import annotations

from datetime import UTC, datetime
from typing import Protocol, runtime_checkable

from opencloudcosts.models import (
    CloudProvider,
    EffectivePrice,
    InstanceTypeInfo,
    NormalizedPrice,
    PricingDomain,
    PricingResult,
    PricingSpec,
    PricingTerm,
    ProviderCatalog,
)


class NotConfiguredError(Exception):
    """Raised when a provider requires credentials that have not been supplied."""


class NotSupportedError(Exception):
    """
    Raised when a provider does not support the requested domain/service combination.
    Always convertible to a structured LLM-readable response via to_response().
    """

    def __init__(
        self,
        provider: CloudProvider,
        domain: PricingDomain,
        service: str | None,
        reason: str,
        alternatives: list[str] | None = None,
        example_invocation: dict | None = None,
    ) -> None:
        super().__init__(reason)
        self.provider = provider
        self.domain = domain
        self.service = service
        self.reason = reason
        self.alternatives = alternatives or []
        self.example_invocation = example_invocation or {}

    def to_response(self) -> dict:
        """Structured response the tool layer returns verbatim to the LLM."""
        resp: dict = {
            "error": "not_supported",
            "provider": self.provider.value,
            "domain": self.domain.value,
            "service": self.service,
            "reason": self.reason,
        }
        if self.alternatives:
            resp["alternatives"] = self.alternatives
        if self.example_invocation:
            resp["example_invocation"] = self.example_invocation
        return resp


class ProviderBase:
    """
    Mixin providing default NotSupportedError-raising implementations for
    optional Protocol methods. Providers inherit this and override what they support.
    """

    provider: CloudProvider

    def supports(self, domain: PricingDomain, service: str | None = None) -> bool:
        return False

    def supported_terms(
        self, domain: PricingDomain, service: str | None = None
    ) -> list[PricingTerm]:
        return [PricingTerm.ON_DEMAND]

    async def get_price(self, spec: PricingSpec) -> PricingResult:
        raise NotSupportedError(
            provider=self.provider,
            domain=spec.domain,
            service=spec.service,
            reason=f"{self.provider.value} does not implement get_price yet.",
        )

    async def describe_catalog(self) -> ProviderCatalog:
        raise NotSupportedError(
            provider=self.provider,
            domain=PricingDomain.COMPUTE,
            service=None,
            reason=f"{self.provider.value} does not implement describe_catalog yet.",
        )

    async def get_discount_summary(self) -> dict:
        raise NotSupportedError(
            provider=self.provider,
            domain=PricingDomain.COMPUTE,
            service=None,
            reason=f"{self.provider.value} does not support discount summaries.",
            alternatives=["Use get_price with the relevant term (reserved_1yr, cud_1yr, etc.)"],
        )

    async def get_spot_history(
        self, spec: PricingSpec, hours: int = 24, availability_zone: str = ""
    ) -> dict:
        raise NotSupportedError(
            provider=self.provider,
            domain=spec.domain,
            service=spec.service,
            reason=f"{self.provider.value} does not support spot price history.",
            alternatives=["Use get_price with term='spot' for current spot rate."],
        )

    def bom_advisories(
        self, services: set[str], sample_region: str
    ) -> list[dict[str, str]]:
        """Return provider-specific BOM advisory rows for services not included in estimate_bom."""
        return []

    def major_regions(self) -> list[str]:
        """Return the provider's curated major-region list for fan-out tools."""
        return []

    def default_region(self) -> str:
        """Return the provider's default region when none is specified."""
        return ""

    async def _applicable_commitments(self, spec: PricingSpec) -> list[EffectivePrice]:
        """Override to return account commitments applicable to this spec when auth is present."""
        return []

    def _apply_cache_trust(
        self,
        prices: list[NormalizedPrice],
        fetched_at: datetime,
        source_url: str = "",
    ) -> list[NormalizedPrice]:
        """Stamp trust metadata onto prices returned from a cache hit."""
        age = (datetime.now(UTC) - fetched_at).total_seconds()
        return [
            p.model_copy(update={
                "fetched_at": fetched_at,
                "cache_age_seconds": age,
                "source_url": source_url or p.source_url,
            })
            for p in prices
        ]


# ---------------------------------------------------------------------------
# Current Protocol — implemented by all three providers (AWS, GCP, Azure).
# This will be replaced by the v2 Protocol as providers are migrated on the
# v0.8.0-prep branch. Both coexist during transition.
# ---------------------------------------------------------------------------

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
