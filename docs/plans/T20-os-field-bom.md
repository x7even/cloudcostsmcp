# T20 · Add `os` field to estimate_bom / estimate_unit_economics

**Status:** pending  
**Branch:** task/T20-os-field-bom

## Problem
`estimate_bom` and `estimate_unit_economics` both hardcode `"Linux"` for all compute pricing lookups (`bom.py:69` and `bom.py:162`). Windows instances cost 30–40% more, so TCO estimates for Windows workloads are silently wrong.

## Files to change
- `src/opencloudcosts/tools/bom.py` — read `os` from item dict, pass to `get_compute_price()`
- `docs/tools.md` — add `os` field to BoM line item fields table

## Implementation steps

### 1. bom.py — read `os` field
In `estimate_bom`, at the item-parsing block (around line 58), add:
```python
os_type = item.get("os", "Linux")
```
Then change both compute calls from:
```python
prices = await pvdr.get_compute_price(resource_type, region, "Linux", pricing_term)
```
to:
```python
prices = await pvdr.get_compute_price(resource_type, region, os_type, pricing_term)
```
Do the same in `estimate_unit_economics` (~line 162).

### 2. Update docstrings
In both tool docstrings, add `os` to the "Each item should be a dict with:" list:
```
- os: "Linux" (default) or "Windows"
```

### 3. docs/tools.md
In the `estimate_bom` BoM line item fields table, add a row:
| `os` | `"Linux"` | Operating system — `"Linux"` (default) or `"Windows"` |

## Tests to add
File: `tests/test_tools/test_bom.py` (create if not exists)

```python
async def test_estimate_bom_windows_os(bom_context):
    """Windows BoM item should pass os="Windows" to get_compute_price."""
    # Mock get_compute_price to capture the os argument
    # Assert it was called with "Windows", not "Linux"

async def test_estimate_bom_os_default_linux(bom_context):
    """BoM item without os field should default to Linux."""
```

## Acceptance criteria
- `estimate_bom(items=[{..., "os": "Windows", "type": "m5.xlarge", ...}])` fetches Windows pricing
- `estimate_bom` without `os` field still returns Linux pricing (no regression)
- All existing tests pass
