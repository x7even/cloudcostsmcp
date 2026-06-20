"""Tests for opencloudcosts.utils.spec_infer."""
from __future__ import annotations

import pytest

from opencloudcosts.utils.spec_infer import fill_domain, spec_error_response

# ---------------------------------------------------------------------------
# fill_domain — service-keyed lookup
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("service,expected_domain", [
    ("rds", "database"),
    ("cloud_sql", "database"),
    ("elasticache", "database"),
    ("memorystore", "database"),
    ("sql", "database"),
    ("cosmos", "database"),
    ("bigquery", "analytics"),
    ("cloud_nat", "network"),
    ("cloud_lb", "network"),
    ("cloud_cdn", "network"),
    ("nat", "network"),
    ("lb", "network"),
    ("cdn", "network"),
    ("cloud_armor", "observability"),
    ("cloudwatch", "observability"),
    ("cloud_monitoring", "observability"),
    ("bedrock", "ai"),
    ("gemini", "ai"),
    ("vertex", "ai"),
    ("openai", "ai"),
    ("sagemaker", "ai"),
    ("lambda", "serverless"),
    ("functions", "serverless"),
    ("azure_functions", "serverless"),
    ("cloud_functions", "serverless"),
    ("cloud_run", "serverless"),
    ("gke", "container"),
    ("eks", "container"),
    ("aks", "container"),
    ("data_transfer", "inter_region_egress"),
    ("egress", "inter_region_egress"),
])
def test_fill_domain_from_service(service, expected_domain):
    spec = {"service": service, "provider": "aws"}
    result = fill_domain(spec)
    assert result["domain"] == expected_domain


def test_fill_domain_service_case_insensitive():
    spec = {"service": "RDS", "provider": "aws"}
    result = fill_domain(spec)
    assert result["domain"] == "database"


def test_fill_domain_does_not_mutate_original_spec():
    spec = {"service": "rds", "provider": "aws"}
    result = fill_domain(spec)
    assert "domain" not in spec
    assert result is not spec


# ---------------------------------------------------------------------------
# fill_domain — domain already present (no-op)
# ---------------------------------------------------------------------------


def test_fill_domain_preserves_existing_domain():
    spec = {"service": "rds", "domain": "compute", "provider": "aws"}
    result = fill_domain(spec)
    assert result["domain"] == "compute"
    assert result is spec  # returned as-is


# ---------------------------------------------------------------------------
# fill_domain — storage_type fallback
# ---------------------------------------------------------------------------


def test_fill_domain_storage_type_infers_storage():
    spec = {"storage_type": "gp3", "provider": "aws"}
    result = fill_domain(spec)
    assert result["domain"] == "storage"


def test_fill_domain_storage_type_takes_precedence_over_unknown_service():
    spec = {"service": "unknown_svc", "storage_type": "ssd", "provider": "aws"}
    result = fill_domain(spec)
    assert result["domain"] == "storage"


# ---------------------------------------------------------------------------
# fill_domain — resource_type patterns
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("resource_type", ["db.m5.large", "db.r6g.xlarge"])
def test_fill_domain_db_resource_type_infers_database(resource_type):
    spec = {"resource_type": resource_type, "provider": "aws"}
    result = fill_domain(spec)
    assert result["domain"] == "database"


@pytest.mark.parametrize("resource_type", ["cache.r6g.large", "cache.t3.medium"])
def test_fill_domain_cache_resource_type_infers_database(resource_type):
    spec = {"resource_type": resource_type, "provider": "aws"}
    result = fill_domain(spec)
    assert result["domain"] == "database"


@pytest.mark.parametrize("resource_type", [
    "m5.xlarge", "c6i.2xlarge", "n2-standard-4", "e2-medium",
    "Standard_D4s_v3", "Basic_A1", "Premium_P1",
])
def test_fill_domain_compute_resource_type_patterns(resource_type):
    spec = {"resource_type": resource_type, "provider": "aws"}
    result = fill_domain(spec)
    assert result["domain"] == "compute"


def test_fill_domain_unknown_resource_type_returns_spec_unchanged():
    spec = {"resource_type": "unknown", "provider": "aws"}
    result = fill_domain(spec)
    assert "domain" not in result


# ---------------------------------------------------------------------------
# fill_domain — nothing to infer
# ---------------------------------------------------------------------------


def test_fill_domain_empty_spec_returns_unchanged():
    spec: dict = {}
    result = fill_domain(spec)
    assert "domain" not in result


def test_fill_domain_unknown_service_no_storage_or_resource_type():
    spec = {"service": "mysuperservice", "provider": "aws"}
    result = fill_domain(spec)
    assert "domain" not in result


# ---------------------------------------------------------------------------
# spec_error_response — structure
# ---------------------------------------------------------------------------


def test_spec_error_response_has_required_keys():
    result = spec_error_response(ValueError("some pydantic error"), {})
    assert "error" in result
    assert result["error"] == "invalid_spec"
    assert "reason" in result
    assert "hint" in result


def test_spec_error_response_reason_is_str_of_exception():
    exc = ValueError("boom boom")
    result = spec_error_response(exc, {})
    assert result["reason"] == "boom boom"


# ---------------------------------------------------------------------------
# spec_error_response — discriminator / domain hint
# ---------------------------------------------------------------------------


def test_spec_error_response_missing_domain_sets_fix():
    exc = Exception("unable to extract tag using discriminator 'domain'")
    result = spec_error_response(exc, {})
    assert "fix" in result
    assert "domain" in result["fix"]
    assert "compute" in result["fix"]


def test_spec_error_response_discriminator_error_with_no_domain_in_spec():
    exc = Exception("value error: discriminator field required")
    spec = {"service": "rds"}  # no domain
    result = spec_error_response(exc, spec)
    assert "fix" in result
    assert "domain" in result["fix"]


def test_spec_error_response_discriminator_error_with_domain_present_no_fix():
    """When domain IS in the spec, the discriminator branch should not fire."""
    exc = Exception("some discriminator error")
    spec = {"domain": "compute"}
    result = spec_error_response(exc, spec)
    # fix might or might not be present — only check it's not the domain fix
    if "fix" in result:
        assert "The 'domain' field" not in result["fix"]


# ---------------------------------------------------------------------------
# spec_error_response — bad term hint
# ---------------------------------------------------------------------------


def test_spec_error_response_bad_term_lists_valid_terms():
    exc = Exception("value error: .term input should be one of on_demand, spot ...")
    result = spec_error_response(exc, {"domain": "compute"})
    assert "fix" in result
    assert "on_demand" in result["fix"]
    assert "spot" in result["fix"]


# ---------------------------------------------------------------------------
# spec_error_response — missing provider hint
# ---------------------------------------------------------------------------


def test_spec_error_response_missing_provider_sets_fix():
    exc = Exception("provider field missing")
    result = spec_error_response(exc, {"domain": "compute"})
    assert "fix" in result
    assert "provider" in result["fix"]
    assert "aws" in result["fix"]


# ---------------------------------------------------------------------------
# spec_error_response — fallback (unrecognised error)
# ---------------------------------------------------------------------------


def test_spec_error_response_unrecognised_error_no_fix():
    exc = Exception("something completely unexpected")
    result = spec_error_response(exc, {"domain": "compute", "provider": "aws"})
    # Should still return the base keys; fix may or may not be present
    assert result["error"] == "invalid_spec"
    assert "describe_catalog" in result["hint"]
