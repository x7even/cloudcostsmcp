# T23 · Explicit error messages for Phase 4 features

**Status:** pending  
**Branch:** task/T23-phase4-errors

## Problem
`get_effective_price(provider="gcp")` and `get_discount_summary(provider="gcp")` fail at runtime with opaque errors deep in the GCP provider. The LLM gets no guidance on what to use instead.

## Files to change
- `src/opencloudcosts/tools/lookup.py` — add early-exit checks
- `docs/tools.md` — annotate AWS-only tools

## Implementation

### 1. `get_effective_price` tool (lookup.py)
After the provider lookup, add:
```python
if provider == "gcp":
    return {
        "error": (
            "GCP effective pricing via BigQuery billing export is planned for Phase 4 "
            "and not yet available. "
            "For committed-use discount list prices, use: "
            "get_compute_price(provider='gcp', term='cud_1yr') or term='cud_3yr'."
        )
    }
```

### 2. `get_discount_summary` tool (lookup.py)
```python
if provider == "gcp":
    return {
        "error": (
            "GCP discount summary via BigQuery billing export is planned for Phase 4 "
            "and not yet available. "
            "GCP committed-use discounts (CUDs) are available as list prices via "
            "get_compute_price(term='cud_1yr' or 'cud_3yr')."
        )
    }
```

### 3. docs/tools.md
In the **Effective & Discount Pricing** section, add a note under both tools:
> **AWS only.** GCP effective pricing (BigQuery billing export) is planned for Phase 4.

## Tests to add
```python
async def test_get_effective_price_gcp_returns_helpful_error():
    result = await call_tool("get_effective_price", provider="gcp", ...)
    assert "error" in result
    assert "Phase 4" in result["error"]
    assert "cud_1yr" in result["error"]  # points to alternative

async def test_get_discount_summary_gcp_returns_helpful_error():
    result = await call_tool("get_discount_summary", provider="gcp")
    assert "error" in result
    assert "Phase 4" in result["error"]
```

## Acceptance criteria
- Both tools return a structured `{"error": "..."}` immediately for GCP, not a runtime exception
- Error message mentions Phase 4 and points to an available alternative
- AWS path unchanged
