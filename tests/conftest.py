"""Shared pytest fixtures."""
from __future__ import annotations

from collections.abc import AsyncGenerator
from pathlib import Path

import pytest

from opencloudcosts.cache import CacheManager
from opencloudcosts.config import Settings


@pytest.fixture
def settings(tmp_path: Path) -> Settings:
    return Settings(
        cache_dir=tmp_path / "cache",
        cache_ttl_hours=1,
        aws_enable_cost_explorer=False,
    )


@pytest.fixture
async def cache(settings: Settings) -> AsyncGenerator[CacheManager, None]:
    cm = CacheManager(settings.cache_dir)
    await cm.initialize()
    yield cm
    await cm.close()
