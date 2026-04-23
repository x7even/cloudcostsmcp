"""Spec pre-processing helpers — domain inference and error enrichment."""
from __future__ import annotations

from typing import Any

# Maps known service identifiers to their canonical PricingDomain value.
# Used to infer a missing "domain" field before discriminated-union validation.
_SERVICE_TO_DOMAIN: dict[str, str] = {
    # database
    "rds": "database",
    "cloud_sql": "database",
    "memorystore": "database",
    # analytics
    "bigquery": "analytics",
    # network
    "cloud_nat": "network",
    "cloud_lb": "network",
    "cloud_cdn": "network",
    "nat": "network",
    "lb": "network",
    "cdn": "network",
    # observability
    "cloud_armor": "observability",
    "cloudwatch": "observability",
    "cloud_monitoring": "observability",
    # ai
    "bedrock": "ai",
    "gemini": "ai",
    "vertex": "ai",
    # serverless
    "lambda": "serverless",
    "functions": "serverless",
    # container
    "gke": "container",
    "eks": "container",
    # egress / data transfer
    "data_transfer": "inter_region_egress",
    "egress": "inter_region_egress",
}

# All valid PricingTerm values — shown in error hints when the model sends a bad term.
_VALID_TERMS = (
    "on_demand, spot, reserved_1yr, reserved_1yr_partial, reserved_1yr_all, "
    "reserved_3yr, reserved_3yr_partial, reserved_3yr_all, "
    "cud_1yr, cud_3yr, sud, compute_savings_plan, ec2_instance_savings_plan"
)


def fill_domain(spec: dict[str, Any]) -> dict[str, Any]:
    """Return spec with 'domain' added if it can be inferred from 'service', 'storage_type', or 'resource_type'."""
    if "domain" in spec:
        return spec

    # 1. Service-keyed lookup (highest precision)
    service = str(spec.get("service", "")).lower()
    if service and service in _SERVICE_TO_DOMAIN:
        return {**spec, "domain": _SERVICE_TO_DOMAIN[service]}

    # 2. storage_type present → storage
    if spec.get("storage_type"):
        return {**spec, "domain": "storage"}

    # 3. resource_type prefix patterns
    rt = str(spec.get("resource_type", "")).lower()
    if rt:
        if rt.startswith("db.") or rt.startswith("cache."):
            return {**spec, "domain": "database"}
        # Compute: AWS (contains dot, e.g. m5.xlarge), GCP (dash-separated families),
        # Azure (starts with Standard_ / Basic_ / Premium_)
        if ("." in rt or "-" in rt
                or rt.startswith("standard_")
                or rt.startswith("basic_")
                or rt.startswith("premium_")):
            return {**spec, "domain": "compute"}

    return spec


def spec_error_response(err: Exception, spec: dict[str, Any]) -> dict[str, Any]:
    """
    Return a structured invalid_spec error dict with targeted hints.

    - Missing domain/discriminator  → tells model 'domain' is required with valid values.
    - Bad term value                → lists valid PricingTerm strings.
    - Anything else                 → returns raw Pydantic message with describe_catalog hint.
    """
    msg = str(err)
    resp: dict[str, Any] = {
        "error": "invalid_spec",
        "reason": msg,
        "hint": (
            "Call describe_catalog(provider, domain, service) to get a valid "
            "example_invocation for your provider/domain/service combination."
        ),
    }

    if "unable to extract tag" in msg.lower() or (
        "domain" not in spec and "discriminator" in msg.lower()
    ):
        resp["fix"] = (
            "The 'domain' field is required and must be one of: "
            "compute, storage, database, ai, container, serverless, "
            "analytics, network, observability, inter_region_egress"
        )
    elif ".term" in msg and "input should be" in msg.lower():
        resp["fix"] = f"Valid term values: {_VALID_TERMS}"
    elif "provider" not in spec:
        resp["fix"] = "The 'provider' field is required: 'aws', 'gcp', or 'azure'."

    return resp
