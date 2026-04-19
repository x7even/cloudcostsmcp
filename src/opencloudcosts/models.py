from __future__ import annotations

from datetime import datetime
from decimal import Decimal
from enum import Enum
from typing import Annotated, Any, Literal, Union

from pydantic import BaseModel, Field


class CloudProvider(str, Enum):
    AWS = "aws"
    GCP = "gcp"
    AZURE = "azure"


class PricingTerm(str, Enum):
    ON_DEMAND = "on_demand"
    RESERVED_1YR = "reserved_1yr"
    RESERVED_3YR = "reserved_3yr"
    RESERVED_1YR_PARTIAL = "reserved_1yr_partial"
    RESERVED_1YR_ALL = "reserved_1yr_all"
    RESERVED_3YR_PARTIAL = "reserved_3yr_partial"
    RESERVED_3YR_ALL = "reserved_3yr_all"
    SPOT = "spot"
    SAVINGS_PLAN = "savings_plan"       # AWS generic (legacy — prefer specific variants below)
    COMPUTE_SP = "compute_savings_plan"          # AWS Compute Savings Plan
    EC2_INSTANCE_SP = "ec2_instance_savings_plan"  # AWS EC2 Instance Savings Plan
    SAGEMAKER_SP = "sagemaker_savings_plan"      # AWS SageMaker Savings Plan
    CUD_1YR = "cud_1yr"                # GCP Committed Use Discount 1yr
    CUD_3YR = "cud_3yr"                # GCP Committed Use Discount 3yr
    FLEX_CUD = "flex_cud"              # GCP Flexible Committed Use Discount
    SUD = "sud"                        # GCP Sustained Use Discount
    PTU = "provisioned_throughput_units"  # Azure OpenAI provisioned throughput


# ---------------------------------------------------------------------------
# Pricing domain taxonomy — describes WHAT is being priced
# ---------------------------------------------------------------------------

class PricingDomain(str, Enum):
    """High-level domain grouping for pricing queries."""
    COMPUTE = "compute"              # VMs, Fargate, ECS-on-Fargate
    STORAGE = "storage"             # block + object storage
    DATABASE = "database"           # RDS, Cloud SQL, Memorystore, ElastiCache, Azure SQL
    CONTAINER = "container"         # EKS, GKE, AKS control planes
    AI = "ai"                       # Bedrock, Vertex, Gemini, SageMaker, Azure OpenAI
    SERVERLESS = "serverless"       # Lambda, Cloud Run, Cloud Functions, Azure Functions
    ANALYTICS = "analytics"         # BigQuery, Redshift, Athena, Synapse
    NETWORK = "network"             # LB, CDN, NAT, egress, Cloud Armor, WAF
    OBSERVABILITY = "observability" # CloudWatch, Cloud Monitoring, Azure Monitor


class PricingShape(str, Enum):
    """Billing model — how the price accrues."""
    RATE_PER_HOUR = "rate_per_hour"
    RATE_PER_GB_MONTH = "rate_per_gb_month"
    RATE_PER_TOKEN = "rate_per_token"
    RATE_PER_REQUEST = "rate_per_request"
    RATE_PER_QUERY = "rate_per_query"
    TIERED_USAGE = "tiered_usage"
    COMPOSITE = "composite"          # multiple shapes combined (e.g. LB: fixed + data)


class PriceUnit(str, Enum):
    PER_HOUR = "per_hour"
    PER_MONTH = "per_month"
    PER_GB_MONTH = "per_gb_month"
    PER_GB = "per_gb"
    PER_IOPS_MONTH = "per_iops_month"
    PER_REQUEST = "per_request"
    PER_GB_SECOND = "per_gb_second"    # Lambda duration
    PER_QUERY = "per_query"            # Route53, Athena
    PER_UNIT = "per_unit"              # generic fallback


class NormalizedPrice(BaseModel):
    """Provider-agnostic pricing entry — the core data model."""
    provider: CloudProvider
    service: str                    # e.g. "compute", "storage", "database"
    sku_id: str                     # provider-native SKU ID
    product_family: str             # e.g. "Compute Instance", "Storage"
    description: str                # human-readable
    region: str                     # normalized region code (us-east-1, us-east1)
    attributes: dict[str, str] = Field(default_factory=dict)
    # ^ instance_type, vcpu, memory_gb, os, storage_type, engine, etc.
    pricing_term: PricingTerm
    price_per_unit: Decimal
    unit: PriceUnit
    currency: str = "USD"
    effective_date: datetime | None = None

    @property
    def monthly_cost(self) -> Decimal:
        """Convenience: monthly cost assuming 730 hrs/month."""
        if self.unit == PriceUnit.PER_HOUR:
            return self.price_per_unit * Decimal("730")
        if self.unit == PriceUnit.PER_MONTH:
            return self.price_per_unit
        return self.price_per_unit

    @property
    def hourly_cost(self) -> Decimal:
        if self.unit == PriceUnit.PER_HOUR:
            return self.price_per_unit
        if self.unit == PriceUnit.PER_MONTH:
            return self.price_per_unit / Decimal("730")
        return self.price_per_unit

    def summary(self) -> dict[str, Any]:
        """Compact dict for LLM consumption."""
        from opencloudcosts.utils.regions import region_display_name
        return {
            "provider": self.provider.value,
            "description": self.description,
            "region": self.region,
            "region_name": region_display_name(self.provider.value, self.region),
            "term": self.pricing_term.value,
            "price": (
                # Use 2-significant-figure scientific notation for sub-microprice SKUs
                # (e.g. Lambda per-request at $0.0000002 would show as "$0.000000" at 6dp).
                # Standard 6dp for everything else.
                f"${float(self.price_per_unit):.2e} {self.unit.value}"
                if self.price_per_unit > 0 and self.price_per_unit < Decimal("0.0000005")
                else f"${self.price_per_unit:.6f} {self.unit.value}"
            ),
            "monthly_estimate": f"${self.monthly_cost:.2f}/mo" if self.unit == PriceUnit.PER_HOUR else None,
            **{k: v for k, v in self.attributes.items() if k in ("instanceType", "vcpu", "memory", "operatingSystem", "storage_type", "volumeType")},
        }


class PriceComparison(BaseModel):
    """Result of a cross-region or cross-provider price comparison."""
    query_description: str
    results: list[NormalizedPrice]
    cheapest: NormalizedPrice | None = None
    most_expensive: NormalizedPrice | None = None
    price_delta_pct: float | None = None   # (most_expensive - cheapest) / cheapest * 100

    @classmethod
    def from_results(cls, query: str, results: list[NormalizedPrice]) -> "PriceComparison":
        if not results:
            return cls(query_description=query, results=[])
        sorted_results = sorted(results, key=lambda r: r.price_per_unit)
        cheapest = sorted_results[0]
        most_expensive = sorted_results[-1]
        delta = None
        if cheapest.price_per_unit > 0:
            delta = float(
                (most_expensive.price_per_unit - cheapest.price_per_unit)
                / cheapest.price_per_unit * 100
            )
        return cls(
            query_description=query,
            results=sorted_results,
            cheapest=cheapest,
            most_expensive=most_expensive,
            price_delta_pct=round(delta, 2) if delta is not None else None,
        )


class BomLineItem(BaseModel):
    """A single line in a Bill of Materials."""
    description: str
    service: str
    provider: CloudProvider
    region: str
    quantity: int
    hours_per_month: float = 730.0
    unit_price: NormalizedPrice
    monthly_cost: Decimal
    annual_cost: Decimal

    @classmethod
    def from_price(
        cls,
        description: str,
        price: NormalizedPrice,
        quantity: int,
        hours_per_month: float = 730.0,
        size_gb: float = 1.0,
    ) -> "BomLineItem":
        if price.unit == PriceUnit.PER_HOUR:
            monthly = price.price_per_unit * Decimal(str(hours_per_month)) * quantity
        elif price.unit == PriceUnit.PER_GB_MONTH:
            monthly = price.price_per_unit * Decimal(str(size_gb)) * quantity
        elif price.unit == PriceUnit.PER_MONTH:
            monthly = price.price_per_unit * quantity
        else:
            monthly = price.price_per_unit * quantity
        return cls(
            description=description,
            service=price.service,
            provider=price.provider,
            region=price.region,
            quantity=quantity,
            hours_per_month=hours_per_month,
            unit_price=price,
            monthly_cost=monthly,
            annual_cost=monthly * 12,
        )


class BomEstimate(BaseModel):
    """Total cost of ownership for a Bill of Materials."""
    items: list[BomLineItem]
    total_monthly: Decimal
    total_annual: Decimal
    currency: str = "USD"

    @classmethod
    def from_items(cls, items: list[BomLineItem], currency: str = "USD") -> "BomEstimate":
        total_monthly = sum(i.monthly_cost for i in items)
        return cls(
            items=items,
            total_monthly=total_monthly,
            total_annual=total_monthly * 12,
            currency=currency,
        )


class EffectivePrice(BaseModel):
    """Bespoke pricing reflecting actual account discounts."""
    base_price: NormalizedPrice          # public on-demand price
    effective_price_per_unit: Decimal    # actual rate after discounts
    discount_type: str                   # "RI", "SP", "CUD", "EDP", "SUD"
    discount_pct: float                  # e.g. 35.0 for 35% off
    commitment_term: str | None = None   # "1yr", "3yr", None for SUD/EDP
    source: str = ""                     # "cost_explorer", "savings_plans_api", "billing_export"

    @property
    def savings_vs_on_demand(self) -> Decimal:
        return self.base_price.price_per_unit - self.effective_price_per_unit


class InstanceTypeInfo(BaseModel):
    """Metadata about a compute instance type."""
    provider: CloudProvider
    instance_type: str
    vcpu: int
    memory_gb: float
    gpu_count: int = 0
    gpu_type: str | None = None
    network_performance: str | None = None
    storage: str | None = None
    region: str
    available: bool = True


# ---------------------------------------------------------------------------
# Typed pricing specs — one class per PricingDomain
# Discriminated union allows providers to dispatch on domain cleanly.
# ---------------------------------------------------------------------------

class BasePricingSpec(BaseModel):
    """Common fields shared by all pricing specs."""
    provider: CloudProvider
    domain: PricingDomain
    service: str | None = None   # e.g. "rds", "bedrock", "gke", "bigquery"
    region: str
    term: PricingTerm = PricingTerm.ON_DEMAND
    schema_version: Literal["1"] = "1"

    def cache_key(self) -> str:
        """Stable, hashable cache key. Excludes fields that don't affect price."""
        parts = [
            self.provider.value,
            self.domain.value,
            self.service or "",
            self.region,
            self.term.value,
        ]
        return ":".join(parts)


class ComputePricingSpec(BasePricingSpec):
    """VMs, bare metal, Fargate tasks — anything priced per vCPU-hour + mem-hour."""
    domain: Literal[PricingDomain.COMPUTE] = PricingDomain.COMPUTE
    resource_type: str | None = None   # instance type (m5.xlarge, n1-standard-4)
    os: str = "Linux"
    vcpu: float | None = None          # Fargate-style sizing
    memory_gb: float | None = None
    hours_per_month: float = 730.0

    def cache_key(self) -> str:
        base = super().cache_key()
        return f"{base}:{self.resource_type or ''}:{self.os}:{self.vcpu or ''}:{self.memory_gb or ''}"


class StoragePricingSpec(BasePricingSpec):
    """Block and object storage."""
    domain: Literal[PricingDomain.STORAGE] = PricingDomain.STORAGE
    storage_type: str = "gp3"          # gp3, pd-ssd, standard, nearline, azure-blob-hot, …
    size_gb: float | None = None
    iops: int | None = None

    def cache_key(self) -> str:
        base = super().cache_key()
        return f"{base}:{self.storage_type}:{self.size_gb or ''}:{self.iops or ''}"


class DatabasePricingSpec(BasePricingSpec):
    """Managed relational DBs, in-memory caches (Memorystore / ElastiCache)."""
    domain: Literal[PricingDomain.DATABASE] = PricingDomain.DATABASE
    resource_type: str = ""            # db instance type — required in practice
    engine: str = "MySQL"
    deployment: str = "single-az"     # single-az / multi-az / ha / regional
    storage_gb: float | None = None
    capacity_gb: float | None = None  # for in-memory services (Memorystore, ElastiCache)
    hours_per_month: float = 730.0

    def cache_key(self) -> str:
        base = super().cache_key()
        return f"{base}:{self.resource_type}:{self.engine}:{self.deployment}"


class ContainerPricingSpec(BasePricingSpec):
    """Managed Kubernetes / container orchestration control planes."""
    domain: Literal[PricingDomain.CONTAINER] = PricingDomain.CONTAINER
    # service: "gke" | "eks" | "aks" (on BasePricingSpec)
    mode: str = "standard"             # standard / autopilot / fargate
    node_type: str | None = None       # worker node instance type
    node_count: int = 3
    vcpu: float | None = None          # Autopilot per-pod sizing
    memory_gb: float | None = None
    hours_per_month: float = 730.0

    def cache_key(self) -> str:
        base = super().cache_key()
        return f"{base}:{self.mode}:{self.node_type or ''}:{self.node_count}"


class AiPricingSpec(BasePricingSpec):
    """AI/ML inference and training — Bedrock, Vertex, Gemini, SageMaker, Azure OpenAI."""
    domain: Literal[PricingDomain.AI] = PricingDomain.AI
    # service: "bedrock" | "vertex" | "gemini" | "sagemaker" | "openai"
    model: str | None = None           # model ID for inference (claude-3-5-sonnet, gemini-1.5-flash)
    machine_type: str | None = None    # VM type for training/prediction (n1-standard-8)
    task: str = "inference"            # inference / training / prediction
    input_tokens: int | None = None
    output_tokens: int | None = None
    training_hours: float | None = None
    mode: str = "on_demand"            # on_demand / batch (Bedrock batch = 50% off)

    def cache_key(self) -> str:
        base = super().cache_key()
        return f"{base}:{self.model or ''}:{self.machine_type or ''}:{self.task}:{self.mode}"


class ServerlessPricingSpec(BasePricingSpec):
    """Function-as-a-service — Lambda, Cloud Functions, Cloud Run, Azure Functions."""
    domain: Literal[PricingDomain.SERVERLESS] = PricingDomain.SERVERLESS
    # service: "lambda" | "cloud_functions" | "cloud_run" | "azure_functions"
    gb_seconds: float | None = None         # execution duration × memory
    requests_millions: float | None = None
    hours_per_month: float | None = None    # for min-instance / concurrency pricing

    def cache_key(self) -> str:
        base = super().cache_key()
        return f"{base}:{self.gb_seconds or ''}:{self.requests_millions or ''}"


class AnalyticsPricingSpec(BasePricingSpec):
    """Data warehouse / analytics query engines."""
    domain: Literal[PricingDomain.ANALYTICS] = PricingDomain.ANALYTICS
    # service: "bigquery" | "redshift" | "athena" | "synapse"
    query_tb: float | None = None
    active_storage_gb: float | None = None
    longterm_storage_gb: float | None = None
    streaming_gb: float | None = None       # BigQuery streaming inserts

    def cache_key(self) -> str:
        base = super().cache_key()
        return f"{base}:{self.query_tb or ''}:{self.active_storage_gb or ''}"


class NetworkPricingSpec(BasePricingSpec):
    """Load balancers, CDN, NAT, egress, WAF, Cloud Armor."""
    domain: Literal[PricingDomain.NETWORK] = PricingDomain.NETWORK
    # service: "lb" | "cdn" | "nat" | "egress" | "cloud_armor" | "waf"
    lb_type: str | None = None              # https / tcp / ssl / application / network
    rule_count: int = 1
    data_gb: float = 0.0
    gateway_count: int = 1
    egress_gb: float = 0.0
    cache_fill_gb: float = 0.0
    policy_count: int = 1                   # Cloud Armor / WAF policies
    monthly_requests_millions: float = 0.0
    hours_per_month: float = 730.0

    def cache_key(self) -> str:
        base = super().cache_key()
        return f"{base}:{self.lb_type or ''}:{self.rule_count}:{self.gateway_count}"


class ObservabilityPricingSpec(BasePricingSpec):
    """Metrics, logs, traces — CloudWatch, Cloud Monitoring, Azure Monitor."""
    domain: Literal[PricingDomain.OBSERVABILITY] = PricingDomain.OBSERVABILITY
    # service: "cloudwatch" | "cloud_monitoring" | "azure_monitor"
    ingestion_mib: float = 0.0
    metrics_count: int = 0
    log_gb: float = 0.0

    def cache_key(self) -> str:
        base = super().cache_key()
        return f"{base}:{self.ingestion_mib}:{self.metrics_count}:{self.log_gb}"


# Discriminated union — Pydantic dispatches on the `domain` field.
PricingSpec = Annotated[
    Union[
        ComputePricingSpec,
        StoragePricingSpec,
        DatabasePricingSpec,
        ContainerPricingSpec,
        AiPricingSpec,
        ServerlessPricingSpec,
        AnalyticsPricingSpec,
        NetworkPricingSpec,
        ObservabilityPricingSpec,
    ],
    Field(discriminator="domain"),
]

# Registry: (domain, service | None) → spec class
# Used by describe_catalog and validation to surface required fields per cell.
PRICING_SCHEMAS: dict[tuple[PricingDomain, str | None], type[BasePricingSpec]] = {
    (PricingDomain.COMPUTE, None): ComputePricingSpec,
    (PricingDomain.STORAGE, None): StoragePricingSpec,
    (PricingDomain.DATABASE, None): DatabasePricingSpec,
    (PricingDomain.DATABASE, "rds"): DatabasePricingSpec,
    (PricingDomain.DATABASE, "cloud_sql"): DatabasePricingSpec,
    (PricingDomain.DATABASE, "memorystore"): DatabasePricingSpec,
    (PricingDomain.DATABASE, "elasticache"): DatabasePricingSpec,
    (PricingDomain.CONTAINER, None): ContainerPricingSpec,
    (PricingDomain.CONTAINER, "gke"): ContainerPricingSpec,
    (PricingDomain.CONTAINER, "eks"): ContainerPricingSpec,
    (PricingDomain.CONTAINER, "aks"): ContainerPricingSpec,
    (PricingDomain.AI, None): AiPricingSpec,
    (PricingDomain.AI, "bedrock"): AiPricingSpec,
    (PricingDomain.AI, "vertex"): AiPricingSpec,
    (PricingDomain.AI, "gemini"): AiPricingSpec,
    (PricingDomain.AI, "sagemaker"): AiPricingSpec,
    (PricingDomain.AI, "openai"): AiPricingSpec,
    (PricingDomain.SERVERLESS, None): ServerlessPricingSpec,
    (PricingDomain.SERVERLESS, "lambda"): ServerlessPricingSpec,
    (PricingDomain.SERVERLESS, "cloud_functions"): ServerlessPricingSpec,
    (PricingDomain.SERVERLESS, "cloud_run"): ServerlessPricingSpec,
    (PricingDomain.SERVERLESS, "azure_functions"): ServerlessPricingSpec,
    (PricingDomain.ANALYTICS, None): AnalyticsPricingSpec,
    (PricingDomain.ANALYTICS, "bigquery"): AnalyticsPricingSpec,
    (PricingDomain.ANALYTICS, "redshift"): AnalyticsPricingSpec,
    (PricingDomain.ANALYTICS, "athena"): AnalyticsPricingSpec,
    (PricingDomain.ANALYTICS, "synapse"): AnalyticsPricingSpec,
    (PricingDomain.NETWORK, None): NetworkPricingSpec,
    (PricingDomain.NETWORK, "lb"): NetworkPricingSpec,
    (PricingDomain.NETWORK, "cdn"): NetworkPricingSpec,
    (PricingDomain.NETWORK, "nat"): NetworkPricingSpec,
    (PricingDomain.NETWORK, "cloud_armor"): NetworkPricingSpec,
    (PricingDomain.NETWORK, "waf"): NetworkPricingSpec,
    (PricingDomain.OBSERVABILITY, None): ObservabilityPricingSpec,
    (PricingDomain.OBSERVABILITY, "cloudwatch"): ObservabilityPricingSpec,
    (PricingDomain.OBSERVABILITY, "cloud_monitoring"): ObservabilityPricingSpec,
    (PricingDomain.OBSERVABILITY, "azure_monitor"): ObservabilityPricingSpec,
}


# ---------------------------------------------------------------------------
# Unified pricing result — returned by every get_price call
# ---------------------------------------------------------------------------

class PricingResult(BaseModel):
    """
    Unified response from get_price. Returns progressively richer data
    depending on what credentials are available.
    """
    public_prices: list[NormalizedPrice]              # always — catalog list rates
    contracted_prices: list[NormalizedPrice] = Field(default_factory=list)
    # ^ applicable SP/RI/CUD rates for this spec, when auth is present
    effective_price: EffectivePrice | None = None     # blended rate, when auth is present
    auth_available: bool = False                      # whether contracted/effective were attempted
    breakdown: dict[str, Any] = Field(default_factory=dict)
    # ^ composite pricing math: {"vcpu_rate": ..., "memory_rate": ..., "monthly_total": ...}
    note: str | None = None
    source: Literal["catalog", "fallback", "mixed"] = "catalog"
    schema_version: Literal["1"] = "1"

    def summary(self) -> dict[str, Any]:
        """Compact form for LLM consumption."""
        out: dict[str, Any] = {
            "public_prices": [p.summary() for p in self.public_prices],
            "auth_available": self.auth_available,
            "source": self.source,
        }
        if self.contracted_prices:
            out["contracted_prices"] = [p.summary() for p in self.contracted_prices]
        if self.effective_price is not None:
            out["effective_price"] = {
                "price": f"${self.effective_price.effective_price_per_unit:.6f}",
                "discount_type": self.effective_price.discount_type,
                "discount_pct": self.effective_price.discount_pct,
                "savings_vs_on_demand": f"${self.effective_price.savings_vs_on_demand:.6f}",
            }
        if self.breakdown:
            out["breakdown"] = self.breakdown
        if self.note:
            out["note"] = self.note
        return out


# ---------------------------------------------------------------------------
# Provider catalog — returned by describe_catalog()
# ---------------------------------------------------------------------------

class ProviderCatalog(BaseModel):
    """
    Describes what a provider supports and how to call get_price for each cell.
    Returned by describe_catalog() — the LLM's O(1) discovery tool.
    """
    provider: CloudProvider
    domains: list[PricingDomain]
    services: dict[str, list[str]]                   # domain.value → [service, ...]
    supported_terms: dict[str, list[str]]             # "domain/service" → [term.value, ...]
    filter_hints: dict[str, dict[str, Any]]           # "domain/service" → {field: description}
    example_invocations: dict[str, dict[str, Any]]    # "domain/service" → ready-to-use spec dict
    decision_matrix: dict[str, str] = Field(default_factory=dict)
    # ^ "Cloud Run" → "serverless/cloud_run", "ECS on Fargate" → "compute/fargate"
