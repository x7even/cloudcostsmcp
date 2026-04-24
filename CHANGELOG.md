# Changelog

All notable changes to OpenCloudCosts are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

---

## [0.8.5] — 2026-04-24

### Added
- `src/opencloudcosts/utils/spec_infer.py` — `fill_domain(spec)` infers the required
  `domain` field from `service`, `storage_type`, and `resource_type` before
  discriminated-union validation, eliminating the most common `invalid_spec` failure class.
  Covers: service-keyed lookup (rds→database, bigquery→analytics, gke→container, etc.),
  `storage_type` present → storage, `db.`/`cache.` resource_type prefix → database,
  dotted/dashed/Standard_/Basic_/Premium_ instance names → compute.
- `spec_error_response(err, spec)` in `spec_infer.py` — structured `invalid_spec` error
  dict with targeted hints: lists valid `domain` values when discriminator tag is missing,
  lists valid `PricingTerm` strings when a bad term is supplied.
- `get_price` docstring extended with `INTER_REGION_EGRESS` example invocation.
- Azure `describe_catalog` decision_matrix now includes an explicit "NOT SUPPORTED" note
  for database domain, stopping models from looping on futile database queries.

### Changed
- `fill_domain` applied in all tool entry points (`get_price`, `compare_prices`,
  `get_spot_history`, `find_cheapest_region`, `find_available_regions`, `estimate_bom`,
  `estimate_unit_economics`) before `validate_python`.
- AWS spot pricing: `NoCredentialsError` / `PartialCredentialsError` now returns an empty
  `public_prices` list instead of raising, so the tool layer reports "no pricing found"
  rather than an `upstream_failure`.
- GCP Cloud Monitoring breakdown rates (`tier1_rate_per_mib`, `tier2_rate_per_mib`,
  `tier3_rate_per_mib`, `estimated_monthly_cost`) now formatted as `"$X.XXXX/MiB"` strings
  so the grounding regex in `analyse.py` can extract them from tool output.

### Fixed
- Harness: 123/123 (100%) on gemma-4-26b-a4b, up from 104/123 (84.6%) in v0.8.4.
  Eliminated: invalid_spec from domain omission on BOM/service specs, false hallucination
  flags on Cloud Monitoring tier rates, Azure database 12-round futile search, AWS spot
  upstream_failure.

---

## [0.8.4] — 2026-04-23

### Added
- `src/opencloudcosts/utils/money.py` — `_price(amount, unit)` and `_money(amount, label)`
  helpers that return structured dicts `{amount, unit, currency, display}` instead of
  formatted strings. Consumers can now do arithmetic on `price.amount` without parsing.

### Changed
- All string-money `f"${p.price_per_unit:.6f}/{p.unit.value}"` formatting eliminated from
  the tool layer (`lookup.py`, `availability.py`, `bom.py`) and `NormalizedPrice.summary()`.
  Every price field in tool responses is now a structured dict with a `display` key for
  human readability (e.g. `"$0.192000/per_hour"`) alongside the numeric `amount`.
- `apply_baseline_deltas` (`utils/baseline.py`) now accepts both string `"$X.XX/unit"`
  and new dict `{amount: X.XX}` values for `price_per_unit` / `monthly_estimate`.
- Harness `analyse.py` requires no changes: the embedded `display` field keeps existing
  `$` regex grounding checks working on serialised tool output.

---

## [0.8.3] — 2026-04-23

### Added
- Trust metadata on `NormalizedPrice`: `fetched_at`, `source_url`, `cache_age_seconds`
  fields (all optional). Populated from cache hits via `_apply_cache_trust()` on
  `ProviderBase`; emitted in `NormalizedPrice.summary()` when set.
- `CacheManager.get_prices_with_meta()` — returns `(data, fetched_at_datetime)` so
  providers can compute `cache_age_seconds` at read time without breaking existing callers.
- `INTER_REGION_EGRESS` pricing domain and `EgressPricingSpec` discriminated union variant
  (`source_region`, `dest_region`, `data_gb`).
- AWS inter-region egress pricing via `AWSDataTransfer` bulk pricing SKUs, filterable
  by `fromRegionCode` / `toRegionCode`. Exposed through `get_price` with the new domain.
- Egress entries added to `AWSProvider.describe_catalog()`.

---

## [0.8.2] — 2026-04-23

### Added
- Three new `ProviderBase` protocol methods with sensible defaults:
  - `major_regions() -> list[str]` — provider's curated region shortlist for fan-out tools
  - `default_region() -> str` — provider's primary region when none is specified
  - `bom_advisories(services, sample_region) -> list[dict]` — provider-specific BOM
    advisory rows (data transfer, support costs, etc.)
- All three providers implement the new methods; AWS `bom_advisories` carries the
  60-line advisory block previously hard-coded in `bom.py`.

### Changed
- All `if provider == "aws"` / provider-string conditionals removed from the tool layer.
  `find_cheapest_region` and `find_available_regions` call `pvdr.major_regions()`;
  `get_price` uses `pvdr.default_region()`; `estimate_bom` calls `pvdr.bom_advisories()`.
- `get_spot_history` tool is now provider-agnostic: removed the hard AWS guard; catches
  `NotSupportedError` from `pvdr.get_spot_history()` and returns a structured envelope.
- `_AWS_MAJOR_REGIONS` / `_GCP_MAJOR_REGIONS` module-level constants removed from
  `availability.py`; `_DEFAULT_REGIONS` dict removed from `lookup.py`.

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

[Unreleased]: https://github.com/x7even/cloudcostmcp/compare/v0.8.4...HEAD
[0.8.4]: https://github.com/x7even/cloudcostmcp/compare/v0.8.3...v0.8.4
[0.8.3]: https://github.com/x7even/cloudcostmcp/compare/v0.8.2...v0.8.3
[0.8.2]: https://github.com/x7even/cloudcostmcp/compare/v0.8.1...v0.8.2
[0.8.1]: https://github.com/x7even/cloudcostmcp/releases/tag/v0.8.1
[0.8.0]: https://github.com/x7even/cloudcostmcp/releases/tag/v0.8.0
