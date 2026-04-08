# T24 · Expand test coverage

**Status:** pending  
**Branch:** task/T24-test-coverage

## Problem
53 tests cover the basics, but several important paths have zero coverage. Key gaps: BoM database items, region analysis tools, cache tools, list_services, GCP compute delegation in get_service_price.

## Files to change
- `tests/test_tools/test_bom.py` (create)
- `tests/test_tools/test_availability.py` (create)
- `tests/test_tools/test_lookup_extended.py` (create)
- `tests/test_cache_extended.py` (create, or extend test_cache.py)

## Tests to implement

### 1. `tests/test_tools/test_bom.py`

#### estimate_bom with service="database" (AWS RDS)
```python
async def test_estimate_bom_database_aws(mock_providers):
    # Mock get_service_price returning a db.t4g.micro price
    result = await call_estimate_bom([{
        "provider": "aws", "service": "database",
        "type": "db.t4g.micro", "region": "us-east-1"
    }])
    assert "errors" not in result or result["errors"] is None
    assert len(result["line_items"]) == 1
    assert "db.t4g.micro" in result["line_items"][0]["description"]
```

#### estimate_bom with service="database" for GCP (graceful error)
```python
async def test_estimate_bom_database_gcp_graceful_error(mock_providers):
    result = await call_estimate_bom([{
        "provider": "gcp", "service": "database",
        "type": "db-n1-standard-2", "region": "us-central1"
    }])
    assert result["errors"] is not None
    assert any("database" in e.lower() for e in result["errors"])
```

#### estimate_bom os field
```python
async def test_estimate_bom_windows_os(mock_providers):
    # Capture os argument passed to get_compute_price
    # Assert it receives "Windows", not "Linux"
```

#### estimate_bom mixed providers
```python
async def test_estimate_bom_mixed_providers(mock_providers):
    result = await call_estimate_bom([
        {"provider": "aws", "service": "compute", "type": "m5.xlarge", "region": "us-east-1"},
        {"provider": "gcp", "service": "compute", "type": "n2-standard-4", "region": "us-central1"},
    ])
    assert len(result["line_items"]) == 2
```

### 2. `tests/test_tools/test_availability.py`

#### find_cheapest_region with baseline_region
```python
async def test_find_cheapest_region_with_baseline(mock_aws_provider):
    # Mock 3 regions with different prices
    # Assert delta_per_hour, delta_monthly, delta_pct present in each entry
    # Assert baseline entry has delta of 0
```

#### find_cheapest_region regions=["all"]
```python
async def test_find_cheapest_region_all_regions(mock_aws_provider):
    # regions=["all"] should query all_available, not _AWS_MAJOR_REGIONS
    # Assert "note" field NOT present (full search, no scoping)
```

#### find_available_regions sorted output and baseline
```python
async def test_find_available_regions_sorted_cheapest_first(mock_aws_provider):
    # Mock 3 regions: prices $0.30, $0.15, $0.20
    # Assert result sorted: $0.15, $0.20, $0.30
    # Assert _sort_price key not present in output (was popped)

async def test_find_available_regions_baseline_deltas(mock_aws_provider):
    result = await call_find_available_regions(..., baseline_region="us-east-1")
    for entry in result["available_regions"]:
        assert "delta_per_hour" in entry
        assert "delta_pct" in entry
```

#### list_services tool
```python
async def test_list_services_returns_services_with_aliases(mock_aws_provider):
    result = await call_list_services(provider="aws")
    assert result["count"] > 0
    ec2 = next(s for s in result["services"] if s["service_code"] == "AmazonEC2")
    assert "ec2" in ec2["aliases"]
```

### 3. `tests/test_tools/test_lookup_extended.py`

#### search_pricing with service param
```python
async def test_search_pricing_cloudwatch_service(mock_aws_provider):
    # Mock _get_products returning cloudwatch item
    result = await call_search_pricing(
        provider="aws", service="cloudwatch", query="metric"
    )
    assert result["count"] > 0
    # Verify it did NOT hit the EC2-specific filter path
```

#### get_service_price GCP compute delegation
```python
async def test_get_service_price_gcp_compute_delegation(mock_gcp_provider):
    # provider="gcp", service="compute", filters={"instanceType": "n2-standard-4"}
    # Should delegate to get_compute_price, not fail with "not supported"
    result = await call_get_service_price(
        provider="gcp", service="compute",
        region="us-central1",
        filters={"instanceType": "n2-standard-4"}
    )
    assert "error" not in result
    assert result["instance_type"] == "n2-standard-4"
```

#### get_service_price no-results tip
```python
async def test_get_service_price_no_results_returns_tip(mock_aws_provider):
    # Mock empty results
    result = await call_get_service_price(
        provider="aws", service="cloudwatch",
        region="us-east-1", filters={"group": "NonExistent"}
    )
    assert result["result"] == "no_results"
    assert "tip" in result
```

### 4. `tests/test_cache_extended.py` (or extend test_cache.py)

#### cache_stats
```python
async def test_cache_stats(cache_manager):
    # Insert some prices
    result = await cache_manager.stats()
    assert "price_entries" in result
    assert "db_size_mb" in result
```

#### refresh_cache clears metadata
```python
async def test_refresh_cache_clears_provider_metadata(cache_manager):
    await cache_manager.set_metadata("aws:services_index", ["AmazonEC2"])
    await cache_manager.set_metadata("gcp:sku_list", ["sku1"])
    await cache_manager.clear_provider("aws")
    assert await cache_manager.get_metadata("aws:services_index") is None
    assert await cache_manager.get_metadata("gcp:sku_list") is not None
```

### 5. `estimate_unit_economics` end-to-end
```python
async def test_estimate_unit_economics_basic(mock_aws_provider):
    result = await call_estimate_unit_economics(
        items=[{"provider": "aws", "service": "compute", "type": "t3.micro", "region": "us-east-1"}],
        units_per_month=10000,
        unit_label="user"
    )
    assert "cost_per_unit" in result
    assert "infrastructure_monthly" in result
    assert result["volume"] == "10,000 users/month"
```

## Fixtures needed
Most tests need a minimal MCP context with mocked providers. Consider a shared `conftest.py` fixture in `tests/test_tools/` that creates a mock context with both AWS and GCP providers that can be selectively mocked.

## Acceptance criteria
- At least 20 new tests passing
- All 10 coverage areas above have at least 1 test
- No existing tests broken
- `uv run pytest tests/` still passes in full
