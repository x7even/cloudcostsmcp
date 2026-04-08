# T27 · Dedicated `get_database_price` tool

**Status:** pending  
**Branch:** task/T27-get-database-price

## Problem
Database pricing works through `get_service_price("rds", ...)` but requires knowing RDS filter attribute names (`databaseEngine`, `instanceType`, `deploymentOption`). A dedicated tool is far more LLM-friendly and enables future GCP Cloud SQL support.

## Files to change
- `src/opencloudcosts/tools/lookup.py` — add `get_database_price` tool
- `src/opencloudcosts/tools/bom.py` — delegate database items to `get_database_price`
- `docs/tools.md` — add tool documentation under Pricing Lookup

## Tool signature
```python
async def get_database_price(
    ctx: Context,
    provider: str,           # "aws" (gcp in Phase 4)
    instance_type: str,      # "db.r5.large", "db.t4g.micro"
    region: str,             # "us-east-1"
    engine: str = "MySQL",   # MySQL, PostgreSQL, MariaDB, Oracle, SQLServer (AWS)
    deployment: str = "single-az",  # "single-az", "multi-az"
    term: str = "on_demand"  # "on_demand", "reserved_1yr", "reserved_3yr"
) -> dict[str, Any]:
```

## Implementation

### AWS path
Build RDS filters from params and call `pvdr.get_service_price("rds", region, filters)`:
```python
filters = {
    "instanceType": instance_type,
    "databaseEngine": engine,
    "deploymentOption": "Multi-AZ" if deployment == "multi-az" else "Single-AZ",
}
# For reserved terms, add:
if term.startswith("reserved"):
    filters["termType"] = "Reserved"
    filters["leaseContractLength"] = "1yr" if "1yr" in term else "3yr"
    filters["purchaseOption"] = "No Upfront"
```

### Response format (matching `get_compute_price` style)
```json
{
  "provider": "aws",
  "instance_type": "db.r5.large",
  "engine": "MySQL",
  "deployment": "single-az",
  "term": "on_demand",
  "region": "us-east-1",
  "region_name": "US East (N. Virginia)",
  "price_per_hour": "$0.240000",
  "monthly_estimate": "$175.20/mo"
}
```

### GCP path (placeholder)
```python
if provider == "gcp":
    return {
        "error": (
            "GCP database pricing (Cloud SQL) is planned for Phase 4. "
            "For GCP compute-equivalent sizing, use get_compute_price(provider='gcp', ...)."
        )
    }
```

### Update `estimate_bom` 
Replace the inline RDS lookup in `bom.py` with a call to the provider-level `get_service_price`, keeping the existing logic (already works) but ensuring parity with `get_database_price` defaults (engine="MySQL", deployment="Single-AZ").

Actually, `estimate_bom` can remain as-is for now since it already works. `get_database_price` is a new user-facing tool, not a refactor of the BoM path.

## Supported engines (AWS)
| Value | RDS databaseEngine filter |
|-------|--------------------------|
| `MySQL` | `MySQL` |
| `PostgreSQL` | `PostgreSQL` |
| `MariaDB` | `MariaDB` |
| `Oracle` | `Oracle` |
| `SQLServer` | `SQL Server` |
| `Aurora-MySQL` | `Aurora MySQL` |
| `Aurora-PostgreSQL` | `Aurora PostgreSQL` |

## docs/tools.md entry
Add under **Pricing Lookup** section:

### `get_database_price`
Get pricing for a managed database instance.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"` (GCP in Phase 4) |
| `instance_type` | string | ✓ | e.g. `"db.r5.large"`, `"db.t4g.micro"` |
| `region` | string | ✓ | Region code |
| `engine` | string | | `"MySQL"` (default), `"PostgreSQL"`, `"MariaDB"`, `"Oracle"`, `"SQLServer"`, `"Aurora-MySQL"`, `"Aurora-PostgreSQL"` |
| `deployment` | string | | `"single-az"` (default) or `"multi-az"` |
| `term` | string | | `"on_demand"` (default), `"reserved_1yr"`, `"reserved_3yr"` |

## Tests
```python
async def test_get_database_price_mysql(mock_aws_provider):
    # Mock get_service_price returning RDS item
    result = await call_get_database_price(
        provider="aws", instance_type="db.r5.large",
        region="us-east-1", engine="MySQL"
    )
    assert "price_per_hour" in result
    assert result["engine"] == "MySQL"
    assert result["deployment"] == "single-az"

async def test_get_database_price_multi_az_more_expensive(mock_aws_provider):
    # Multi-AZ should cost ~2x Single-AZ
    single = await call_get_database_price(..., deployment="single-az")
    multi = await call_get_database_price(..., deployment="multi-az")
    # multi price > single price

async def test_get_database_price_gcp_returns_phase4_error():
    result = await call_get_database_price(provider="gcp", ...)
    assert "Phase 4" in result["error"]
```

## Acceptance criteria
- Tool returns structured price response for all 7 supported engines
- Multi-AZ returns the multi-AZ price (not single-AZ)
- GCP returns a clear Phase 4 error with an alternative suggestion
- `docs/tools.md` updated with full parameter table
