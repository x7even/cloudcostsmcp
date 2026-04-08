# T19 · AWS Reserved Instance upfront options (Partial/All Upfront)

**Status:** pending  
**Branch:** task/T19-ri-upfront-options

## Problem
`_RESERVED_FILTERS` hardcodes `"PurchaseOption": "No Upfront"`. Partial and All Upfront RIs are cheaper on a monthly basis (in exchange for an upfront payment) and are commonly used in FinOps analysis.

## Files to change
- `src/opencloudcosts/models.py` — add new `PricingTerm` enum values
- `src/opencloudcosts/providers/aws.py` — extend `_RESERVED_FILTERS` dict
- `docs/tools.md` — update `term` parameter table

## Implementation

### 1. New PricingTerm values (models.py)
```python
class PricingTerm(str, Enum):
    ON_DEMAND = "on_demand"
    SPOT = "spot"
    RESERVED_1YR = "reserved_1yr"          # No Upfront (existing)
    RESERVED_3YR = "reserved_3yr"          # No Upfront (existing)
    RESERVED_1YR_PARTIAL = "reserved_1yr_partial"
    RESERVED_1YR_ALL = "reserved_1yr_all"
    RESERVED_3YR_PARTIAL = "reserved_3yr_partial"
    RESERVED_3YR_ALL = "reserved_3yr_all"
    CUD_1YR = "cud_1yr"
    CUD_3YR = "cud_3yr"
```

### 2. Extend `_RESERVED_FILTERS` (aws.py)
```python
_RESERVED_FILTERS: dict[PricingTerm, dict[str, str]] = {
    PricingTerm.RESERVED_1YR:         {"LeaseContractLength": "1yr", "PurchaseOption": "No Upfront"},
    PricingTerm.RESERVED_3YR:         {"LeaseContractLength": "3yr", "PurchaseOption": "No Upfront"},
    PricingTerm.RESERVED_1YR_PARTIAL: {"LeaseContractLength": "1yr", "PurchaseOption": "Partial Upfront"},
    PricingTerm.RESERVED_1YR_ALL:     {"LeaseContractLength": "1yr", "PurchaseOption": "All Upfront"},
    PricingTerm.RESERVED_3YR_PARTIAL: {"LeaseContractLength": "3yr", "PurchaseOption": "Partial Upfront"},
    PricingTerm.RESERVED_3YR_ALL:     {"LeaseContractLength": "3yr", "PurchaseOption": "All Upfront"},
}
```

### 3. Handle "All Upfront" pricing dimension
All Upfront RIs have a one-time `Quantity` price dimension (not `Hrs`). The existing `_extract_reserved_price()` may not handle this. Options:
- A) Normalise to hourly: `upfront_cost / (hours_in_term)` and add to any recurring hourly component.
- B) Return both: add `upfront_cost` field to the response alongside `price_per_unit` (the recurring hourly, which is $0 for All Upfront).

**Recommendation: Option A** for compatibility with `estimate_bom` which expects `price_per_unit` to be the hourly rate. Calculate `effective_hourly = upfront / (8760 * years)`. Include `upfront_cost` in `attributes` for transparency.

### 4. `_extract_reserved_price()` update (aws.py)
Inspect the `priceDimensions` for both `Hrs` and `Quantity` units:
```python
hourly = Decimal("0")
upfront = Decimal("0")
for dim in price_dims.values():
    unit = dim.get("unit", "")
    usd = Decimal(dim["pricePerUnit"].get("USD", "0"))
    if unit == "Hrs":
        hourly = usd
    elif unit == "Quantity":
        upfront = usd

if upfront > 0:
    term_hours = 8760 * (3 if "3yr" in term_key else 1)
    hourly += upfront / term_hours
    # Store upfront_cost in attributes
```

### 5. docs/tools.md
Update the `term` parameter description in `get_compute_price`:
| Value | Description |
|-------|-------------|
| `on_demand` | On-demand (default) |
| `reserved_1yr` | 1-year No Upfront RI |
| `reserved_1yr_partial` | 1-year Partial Upfront RI |
| `reserved_1yr_all` | 1-year All Upfront RI (normalised to hourly equivalent) |
| `reserved_3yr` | 3-year No Upfront RI |
| `reserved_3yr_partial` | 3-year Partial Upfront RI |
| `reserved_3yr_all` | 3-year All Upfront RI (normalised to hourly equivalent) |
| `spot` | Spot instance (requires credentials) |

## Tests to add
```python
_PARTIAL_UPFRONT_ITEM = {
    # product: same as _M5_XLARGE_PRICE_ITEM
    "terms": {
        "Reserved": {
            "...: {
                "priceDimensions": {
                    "dim1": {"unit": "Hrs", "pricePerUnit": {"USD": "0.0530"}},
                    "dim2": {"unit": "Quantity", "pricePerUnit": {"USD": "280.00"}},
                },
                "termAttributes": {
                    "LeaseContractLength": "1yr",
                    "PurchaseOption": "Partial Upfront",
                },
            }
        }
    }
}

async def test_reserved_1yr_partial_upfront(aws_provider):
    with patch.object(aws_provider, "_get_products", return_value=[_PARTIAL_UPFRONT_ITEM]):
        prices = await aws_provider.get_compute_price(
            "m5.xlarge", "us-east-1", term=PricingTerm.RESERVED_1YR_PARTIAL
        )
    assert len(prices) == 1
    # Effective hourly should be > No Upfront hourly
    # (partial: lower hourly + upfront)

async def test_reserved_1yr_all_upfront_normalised(aws_provider):
    # All Upfront: $0 hourly + lump sum
    # Effective hourly = upfront / 8760
```

## Acceptance criteria
- All 6 new term values return prices for m5.xlarge
- All Upfront terms normalise to effective hourly rate (not $0)
- `upfront_cost` present in attributes for partial/all upfront
- Existing `reserved_1yr` and `reserved_3yr` behaviour unchanged
