# T31 · GCP Windows pricing support

**Status:** pending  
**Branch:** task/T31-gcp-windows-pricing

## Problem
`get_compute_price(provider="gcp", os="Windows")` is accepted but silently returns Linux pricing. GCP charges an additional per-vCPU/hour Windows Server license fee on top of base compute.

## Investigation required (do this first)
Before coding, call the GCP Cloud Billing Catalog API and search for Windows SKUs:
```
GET https://cloudbilling.googleapis.com/v1/services/6F81-5844-456A/skus?key={API_KEY}
```
Look for SKUs where `description` contains "Windows". Expected pattern based on GCP docs:
- `"N2 Instance Core running Windows"` — per vCPU/hour
- `"N2 Instance Ram running Windows"` — per GB RAM/hour

This is separate from the base N2 SKUs (`"N2 Instance Core"`, `"N2 Instance Ram"`).

## Files to change
- `src/opencloudcosts/providers/gcp.py` — add Windows SKU lookup
- `tests/test_providers/test_gcp.py` — add Windows tests

## Design (pending investigation)

### Assumption: GCP Windows = base compute + Windows license
```
windows_total = base_vcpu_price + base_ram_price + windows_vcpu_license + windows_ram_license
```

### Implementation sketch
In `get_compute_price()`, after computing the base Linux price:
```python
if os == "Windows":
    win_cpu_price = await self._lookup_price(
        service_id, region, f"{family} Instance Core running Windows"
    )
    win_ram_price = await self._lookup_price(
        service_id, region, f"{family} Instance Ram running Windows"
    )
    if win_cpu_price is None or win_ram_price is None:
        # Windows not available for this family/region
        return {"error": f"Windows pricing not available for {instance_type} in {region}"}
    
    windows_license = (vcpus * win_cpu_price) + (memory_gb * win_ram_price)
    total_price = base_price + windows_license
```

### SKU description pattern variations
GCP SKU descriptions vary by instance family. Handle at minimum:
- N1 family: `"N1 Predefined Instance Core running Windows"`
- N2 family: `"N2 Instance Core running Windows"`  
- E2 family: May not have Windows support — return clear error
- C2 family: `"Compute optimized Core running Windows"`

The investigation step above will confirm exact patterns.

### Caching
Windows SKU lookups should be cached with the same TTL as Linux pricing.

## Edge cases
- **E2 instances**: E2 (cost-optimised) does not support Windows. Return `{"error": "Windows not supported for E2 instances. Use N2 or N1."}`.
- **Preemptible/Spot + Windows**: GCP does not support preemptible Windows. If `os="Windows"` and `term="spot"`, return an error.
- **Unknown family**: If no Windows SKU found for the family, return error rather than silently returning Linux price.

## Tests to add
```python
_GCP_N2_WINDOWS_SKU = {
    # Mock SKU for N2 Windows Core license
    "name": "services/6F81.../skus/...",
    "description": "N2 Instance Core running Windows",
    "pricingInfo": [{
        "pricingExpression": {
            "tieredRates": [{"unitPrice": {"units": "0", "nanos": 45000000}}]
        }
    }]
}

async def test_get_compute_price_gcp_windows(gcp_provider):
    # Mock _lookup_price to return both Linux and Windows SKU prices
    prices = await gcp_provider.get_compute_price(
        "n2-standard-4", "us-central1", os="Windows"
    )
    assert len(prices) == 1
    linux_prices = await gcp_provider.get_compute_price("n2-standard-4", "us-central1")
    assert prices[0].price_per_unit > linux_prices[0].price_per_unit

async def test_get_compute_price_gcp_e2_windows_returns_error(gcp_provider):
    prices = await gcp_provider.get_compute_price(
        "e2-standard-4", "us-central1", os="Windows"
    )
    # Should return empty list or raise, not silently return Linux price
```

## Acceptance criteria
- `os="Windows"` returns a higher price than `os="Linux"` for supported families
- E2 instances return a clear error for Windows
- `os="Linux"` (default) behaviour unchanged
- Investigation finding documented in a comment in the code
