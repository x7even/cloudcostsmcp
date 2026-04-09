"""Tests for cache_stats and refresh_cache tools (T24)."""
from __future__ import annotations

from pathlib import Path
from typing import Any
from unittest.mock import MagicMock

import pytest

from opencloudcosts.cache import CacheManager
from opencloudcosts.tools.availability import register_availability_tools
from opencloudcosts.tools.lookup import register_lookup_tools


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

class _ToolCapture:
    """Minimal mock MCP object that captures registered tool functions by name."""

    def __init__(self) -> None:
        self._tools: dict[str, Any] = {}

    def tool(self):
        def decorator(fn):
            self._tools[fn.__name__] = fn
            return fn
        return decorator

    def __getitem__(self, name: str):
        return self._tools[name]


def _make_ctx(providers: dict[str, Any], cache: CacheManager) -> MagicMock:
    ctx = MagicMock()
    ctx.request_context.lifespan_context = {
        "providers": providers,
        "cache": cache,
    }
    return ctx


@pytest.fixture
async def cache_manager(tmp_path: Path):
    cm = CacheManager(tmp_path / "test_cache_tools")
    await cm.initialize()
    yield cm
    await cm.close()


@pytest.fixture
def availability_tools():
    capture = _ToolCapture()
    register_availability_tools(capture)
    return capture


@pytest.fixture
def lookup_tools():
    capture = _ToolCapture()
    register_lookup_tools(capture)
    return capture


# ---------------------------------------------------------------------------
# test_cache_stats_returns_fields
# ---------------------------------------------------------------------------

async def test_cache_stats_returns_fields(availability_tools, cache_manager: CacheManager):
    """cache_stats tool should return price_entries and db_size_mb fields."""
    ctx = _make_ctx({}, cache_manager)
    result = await availability_tools["cache_stats"](ctx)

    assert "price_entries" in result
    assert "db_size_mb" in result


async def test_cache_stats_reflects_inserted_prices(availability_tools, cache_manager: CacheManager):
    """cache_stats price_entries count should reflect inserted price rows."""
    # Insert a price entry
    await cache_manager.set_prices(
        "aws", "compute", "us-east-1",
        {"instance": "m5.xlarge"},
        [{"price": "0.192"}],
        ttl_hours=1,
    )

    ctx = _make_ctx({}, cache_manager)
    result = await availability_tools["cache_stats"](ctx)

    assert result["price_entries"] >= 1


# ---------------------------------------------------------------------------
# test_refresh_cache_clears_prices
# ---------------------------------------------------------------------------

async def test_refresh_cache_clears_prices(lookup_tools, cache_manager: CacheManager):
    """refresh_cache(provider='aws') should remove aws price entries and report prices_deleted > 0."""
    # Store a price so there's something to delete
    await cache_manager.set_prices(
        "aws", "compute", "us-east-1",
        {"instance": "t3.micro"},
        [{"price": "0.0104"}],
        ttl_hours=1,
    )

    ctx = _make_ctx({}, cache_manager)
    result = await lookup_tools["refresh_cache"](ctx, provider="aws")

    assert "prices_deleted" in result
    assert result["prices_deleted"] > 0


async def test_refresh_cache_does_not_delete_other_provider(lookup_tools, cache_manager: CacheManager):
    """refresh_cache(provider='aws') must leave gcp entries untouched."""
    await cache_manager.set_prices(
        "aws", "compute", "us-east-1", {}, [{"p": "1"}], ttl_hours=1
    )
    await cache_manager.set_prices(
        "gcp", "compute", "us-central1", {}, [{"p": "2"}], ttl_hours=1
    )

    ctx = _make_ctx({}, cache_manager)
    await lookup_tools["refresh_cache"](ctx, provider="aws")

    # GCP price entry must survive
    gcp_entry = await cache_manager.get_prices("gcp", "compute", "us-central1", {})
    assert gcp_entry is not None
