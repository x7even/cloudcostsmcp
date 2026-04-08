# T21 · Fix `refresh_cache` to also clear metadata entries for a provider

**Status:** pending  
**Branch:** task/T21-refresh-cache-metadata

## Problem
`refresh_cache(provider="aws")` only deletes from the `prices` table. Metadata (service lists, instance type indexes, GCP SKU catalogs) persists stale. `list_services()`, `list_instance_types()`, and GCP SKU results will continue returning cached data even after a cache clear.

## Root cause
`cache.py: clear_provider()` only runs:
```sql
DELETE FROM prices WHERE provider = ?
```
The `metadata` table has no `provider` column, so there is no way to selectively clear by provider.

## Files to change
- `src/opencloudcosts/cache.py` — prefix metadata keys with provider, update `clear_provider()`
- `src/opencloudcosts/providers/aws.py` — prefix all `set_metadata` / `get_metadata` calls
- `src/opencloudcosts/providers/gcp.py` — same for GCP metadata keys

## Implementation

### Option chosen: key-prefix approach (no schema migration)
Prefix every metadata key with `{provider}:` at the call sites:
- `aws:services_index` instead of `services_index`
- `gcp:sku_catalog_us-central1` instead of `sku_catalog_us-central1`

### 1. Audit all `set_metadata` / `get_metadata` calls
In `aws.py` and `gcp.py`, find every `cache.set_metadata(key, ...)` and `cache.get_metadata(key)` call. Add the provider prefix to the key string at each call site.

### 2. Update `clear_provider()` in `cache.py`
```python
async def clear_provider(self, provider: str) -> None:
    async with self._conn() as db:
        await db.execute("DELETE FROM prices WHERE provider = ?", (provider,))
        await db.execute("DELETE FROM metadata WHERE key LIKE ?", (f"{provider}:%",))
        await db.commit()
```

### 3. Update `refresh_cache` tool response
Return counts of both prices and metadata entries cleared.

## Tests to add
```python
async def test_refresh_cache_clears_metadata(cache_manager):
    await cache_manager.set_metadata("aws:services_index", [...])
    await cache_manager.set_metadata("gcp:sku_list", [...])
    await cache_manager.clear_provider("aws")
    assert await cache_manager.get_metadata("aws:services_index") is None
    assert await cache_manager.get_metadata("gcp:sku_list") is not None  # unaffected
```

## Acceptance criteria
- `refresh_cache(provider="aws")` clears aws prices AND aws metadata
- GCP metadata is unaffected when clearing AWS
- Existing cache tests still pass
