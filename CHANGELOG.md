# Changelog

All notable changes to OpenCloudCosts are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

---

## [0.8.1] — 2026-04-23

### Added
- HTTP retry/backoff on all upstream pricing API calls (AWS, GCP, Azure) using tenacity:
  exponential back-off 1–30s, 3 attempts, retry on 429/5xx and transient network errors.
- Schema-version gate on startup: cache is only purged when the DB schema changes,
  not on every restart. Pricing data now survives Docker/Claude Desktop/`uv run` restarts.
- Test + lint CI workflow (`.github/workflows/tests.yml`): runs pytest, ruff, and mypy
  across Python 3.11/3.12/3.13 on every push and pull request.
- Docker image published to `ghcr.io/x7even/opencloudcosts` via GHCR workflow.
- PyPI/uvx quickstart in README: `uvx opencloudcosts` and `pip install opencloudcosts`.
- CONTRIBUTING.md, SECURITY.md, GitHub issue templates, PR template.
- CHANGELOG.md (this file).
- Multi-model LLM harness matrix runner (`local-test-harness/run_matrix.py`) with
  `--parallel-models N` flag; 123/123 (100%) pass rate across gemma-4-26b-a4b,
  qwen3.6-35b-a3b, and qwen3.5-122b-a10b@q6_k.

### Changed
- `list_instance_types` response compacted: `network_performance` dropped, GPU fields
  (`gpu_count`, `gpu_type`) only included when non-null. Reduces token use in large regions.

### Fixed
- Structured error envelopes at all tool boundaries: raw exception text (including potential
  boto3 stack frames) no longer leaks into LLM context. Full tracebacks are logged
  server-side.
- 5 provider contract invariant tests were `@pytest.mark.skip` — they now run (35 tests).
- Stale test fixtures for gp3 IOPS add-ons, Azure Premium SSD unit, BigQuery storage
  two-tier format, and Cloud SQL per-instance-size descriptions.
- Duplicate `[dependency-groups].dev` block in `pyproject.toml` with conflicting pytest
  versions removed. Unused `google-cloud-billing` extra dropped.

### Migration from v0.8.0
No tool API changes. Cache will be purged once on first startup (schema version write).

---

## [0.8.0] — 2026-04-21

Breaking change: 31 provider-specific tools consolidated to a 15-tool surface.

### Added
- `get_price(spec)` — unified dispatcher using a discriminated-union `PricingSpec`.
  Replaces 21 domain-specific tools.
- `describe_catalog(provider, domain, service)` — returns the full support matrix with
  `example_invocation` copy-paste snippets for `get_price`.
- Capability-based provider protocol: `supports()`, `get_price()`, `describe_catalog()`
  on each cloud provider — zero provider-branching in the tool layer.
- MAX_TOOL_ROUNDS=30 for agentic BOM/TCO prompts (previously 20).
- 123/123 (100%) harness pass rate on a 123-prompt test suite across AWS, GCP, Azure.

### Removed (migration required)
The following tools no longer exist. Use `get_price(spec={...})` instead:

| Old tool | Replacement |
|----------|-------------|
| `get_compute_price` | `get_price({"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", ...})` |
| `compare_compute_prices` | `compare_prices(spec={...}, regions=[...])` |
| `get_service_price` | `get_price({"provider": "gcp", "domain": "database", "service": "cloud_sql", ...})` |
| `check_availability` | `find_available_regions(spec={...})` |
| 17 others | See `describe_catalog()` for equivalent invocations |

### Fixed
- gp3 EBS add-on pricing (IOPS/throughput tiers correctly returned alongside base rate).
- Azure Premium SSD disk unit corrected to `per_month` (flat tier fee, not per-GB).
- BigQuery active/long-term storage rates now use the paid tier, not the free-quota tier.
- Cloud SQL SKU matching updated to GCP's per-instance-size description format.
- Memorystore Standard SKU: excludes Redis Cluster node SKUs to return correct HA rate.
- Lambda pricing: dedicated `_price_lambda` for request + duration SKUs.
- Monthly estimate shown for `PER_MONTH` unit in tool responses.

---

## [0.7.x] — 2025–2026

Phase-based rollout:
- **Phase 1**: AWS public pricing (EC2, EBS, list instances)
- **Phase 2**: AWS effective pricing (Cost Explorer, Savings Plans, Reserved Instances)
- **Phase 3**: GCP public pricing (Compute Engine, Persistent Disk, CUDs)
- **Phase 4**: Azure public pricing (Retail Prices API); streamable-HTTP transport; Dockerfile
- **Phase 5**: GCP managed services (GKE, Memorystore, BigQuery, Vertex AI, Gemini,
  Cloud LB/CDN/NAT/Armor/Monitoring, Cloud SQL); Azure reserved pricing

[Unreleased]: https://github.com/x7even/cloudcostmcp/compare/v0.8.1...HEAD
[0.8.1]: https://github.com/x7even/cloudcostmcp/releases/tag/v0.8.1
[0.8.0]: https://github.com/x7even/cloudcostmcp/releases/tag/v0.8.0
