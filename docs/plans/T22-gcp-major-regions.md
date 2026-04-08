# T22 · Add GCP major-regions default list

**Status:** pending  
**Branch:** task/T22-gcp-major-regions

## Problem
`find_cheapest_region` and `find_available_regions` default to 12 major regions for AWS but query ALL 30+ GCP regions when no `regions` param is provided. This causes:
- Cold-cache timeout risk for GCP on first run
- Inconsistent UX between providers
- Not documented anywhere

## Files to change
- `src/opencloudcosts/tools/availability.py` — add `_GCP_MAJOR_REGIONS`, apply scoped logic
- `docs/tools.md` — document the default behaviour for both providers

## Implementation

### 1. Add `_GCP_MAJOR_REGIONS` constant (availability.py, after `_AWS_MAJOR_REGIONS`)
```python
_GCP_MAJOR_REGIONS = [
    "us-central1",          # Iowa
    "us-east1",             # South Carolina
    "us-west1",             # Oregon
    "us-west2",             # Los Angeles
    "europe-west1",         # Belgium
    "europe-west2",         # London
    "europe-west3",         # Frankfurt
    "europe-west4",         # Netherlands
    "asia-east1",           # Taiwan
    "asia-northeast1",      # Tokyo
    "asia-southeast1",      # Singapore
    "australia-southeast1", # Sydney
]
```

### 2. Apply scoped logic in `find_cheapest_region`
The existing block:
```python
if provider == "aws" and not all_regions_requested:
    regions = _AWS_MAJOR_REGIONS
    scoped_search = True
```
Extend to:
```python
if provider == "aws" and not all_regions_requested:
    regions = _AWS_MAJOR_REGIONS
    scoped_search = True
elif provider == "gcp" and not all_regions_requested:
    regions = _GCP_MAJOR_REGIONS
    scoped_search = True
```

### 3. Same change in `find_available_regions`
Mirror the same logic in the `find_available_regions` scoped-search block.

### 4. Update `note` message
Change the note to be provider-aware:
```python
result["note"] = (
    f"Searched {len(regions)} major {provider.upper()} regions. "
    "Pass regions=['all'] to search all available regions (slower on first run)."
)
```

### 5. docs/tools.md
Under `find_cheapest_region` and `find_available_regions`, update the `regions` parameter description to explicitly state the major-regions default applies to both AWS and GCP.

## Tests to add
```python
async def test_find_cheapest_region_gcp_defaults_to_major_regions():
    # Mock pvdr.get_compute_price and pvdr.list_regions
    # Call with provider="gcp", no regions param
    # Assert only _GCP_MAJOR_REGIONS were queried (not all 30+)
    # Assert "note" field present in result
```

## Acceptance criteria
- GCP `find_cheapest_region` without `regions` param queries 12 major regions
- `regions=["all"]` still queries all GCP regions
- AWS behaviour unchanged
- `note` field present when scoped search used for either provider
