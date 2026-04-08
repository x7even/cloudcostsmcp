# T25 · `search_pricing` and `get_service_price`: helpful no-results response

**Status:** pending  
**Branch:** task/T25-no-results-hints

## Problem
When `search_pricing` or `get_service_price` returns no results (invalid service code, wrong filter keys, typo), the response is an empty list or a generic "no_prices_found" message with no guidance. LLMs retry with the same call or give up.

## Files to change
- `src/opencloudcosts/tools/lookup.py` — enrich no-results responses in both tools

## Implementation

### 1. Standardise no-results shape
Define the response structure:
```json
{
  "result": "no_results",
  "provider": "aws",
  "service": "cloudwatch",
  "region": "us-east-1",
  "filters_applied": {"group": "Metrik"},
  "message": "No pricing found for service 'cloudwatch' in us-east-1 with the provided filters.",
  "tip": "Use search_pricing(provider='aws', service='cloudwatch', query='metric') to explore available products and their filter attribute names. Use list_services() to verify the service code is valid."
}
```

### 2. `get_service_price` no-results block (lookup.py ~line 343)
Replace:
```python
if not prices:
    return {
        "result": "no_prices_found",
        "message": (...)
    }
```
With:
```python
if not prices:
    return {
        "result": "no_results",
        "provider": provider,
        "service": service,
        "region": region,
        "filters_applied": filters or {},
        "message": f"No pricing found for service '{service}' in {region} with filters {filters}.",
        "tip": (
            f"Try: search_pricing(provider='{provider}', service='{service}', query='...') "
            "to explore available products and valid filter attribute names. "
            "Use list_services() to verify the service code exists."
        ),
    }
```

### 3. `search_pricing` no-results block (lookup.py)
After the results check, add:
```python
if not results:
    return {
        "result": "no_results",
        "provider": provider,
        "service": service or "ec2",
        "query": query,
        "region": region or "any",
        "message": f"No pricing found matching '{query}' in service '{service or 'ec2'}'.",
        "tip": (
            "Check the service code with list_services(). "
            "Try a broader query (e.g. the product family name). "
            f"If searching a non-compute service, ensure service='{service}' is correct."
        ),
    }
```

### 4. Include resolved service code
When an alias is used (e.g. `"cloudwatch"` → `"AmazonCloudWatch"`), include `"resolved_service_code": "AmazonCloudWatch"` in the no-results response so the LLM can verify the alias resolution was correct.

## Tests to add
```python
async def test_get_service_price_no_results_returns_tip():
    # Mock _get_products returning []
    result = await aws_provider.get_service_price("cloudwatch", "us-east-1", {"group": "Typo"})
    # At tool layer, verify result has "tip" field

async def test_search_pricing_no_results_returns_tip():
    result = ...  # mock empty return
    assert result["result"] == "no_results"
    assert "tip" in result
    assert "list_services" in result["tip"]
```

## Acceptance criteria
- No-results response always includes `result`, `message`, and `tip` fields
- `tip` mentions `list_services()` and/or `search_pricing()` as next steps
- Resolved service code included when alias was used
- Existing non-empty results paths unchanged
