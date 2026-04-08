"""Tests for the SQLite cache manager."""
from __future__ import annotations

import asyncio
from pathlib import Path

import pytest

from opencloudcosts.cache import CacheManager


@pytest.fixture
async def cache(tmp_path: Path):
    cm = CacheManager(tmp_path / "test_cache")
    await cm.initialize()
    yield cm
    await cm.close()


async def test_prices_round_trip(cache: CacheManager):
    data = [{"sku": "ABC", "price": "0.192", "unit": "per_hour"}]
    await cache.set_prices("aws", "compute", "us-east-1", {"instance": "m5.xlarge"}, data, ttl_hours=1)
    result = await cache.get_prices("aws", "compute", "us-east-1", {"instance": "m5.xlarge"})
    assert result == data


async def test_prices_cache_miss(cache: CacheManager):
    result = await cache.get_prices("aws", "compute", "us-west-2", {"instance": "c5.large"})
    assert result is None


async def test_metadata_round_trip(cache: CacheManager):
    await cache.set_metadata("test:regions", ["us-east-1", "us-west-2"], ttl_hours=1)
    result = await cache.get_metadata("test:regions")
    assert result == ["us-east-1", "us-west-2"]


async def test_metadata_cache_miss(cache: CacheManager):
    result = await cache.get_metadata("nonexistent:key")
    assert result is None


async def test_stats(cache: CacheManager):
    stats = await cache.stats()
    assert "price_entries" in stats
    assert "db_size_mb" in stats


async def test_clear_provider(cache: CacheManager):
    data = [{"sku": "X"}]
    await cache.set_prices("aws", "compute", "us-east-1", {}, data, ttl_hours=1)
    await cache.set_prices("gcp", "compute", "us-east1", {}, data, ttl_hours=1)
    await cache.clear_provider("aws")
    assert await cache.get_prices("aws", "compute", "us-east-1", {}) is None
    assert await cache.get_prices("gcp", "compute", "us-east1", {}) is not None
