from __future__ import annotations

import hashlib
import json
import logging
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import aiosqlite

logger = logging.getLogger(__name__)

# Bump this integer whenever the DB schema or serialised data format changes
# in a way that requires old cached rows to be discarded.
_SCHEMA_VERSION = 1

_CREATE_SCHEMA = """
CREATE TABLE IF NOT EXISTS prices (
    cache_key   TEXT PRIMARY KEY,
    provider    TEXT NOT NULL,
    service     TEXT NOT NULL,
    region      TEXT NOT NULL,
    data        TEXT NOT NULL,   -- JSON-serialised payload
    fetched_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_prices_lookup  ON prices(provider, service, region);
CREATE INDEX IF NOT EXISTS idx_prices_expiry  ON prices(expires_at);

CREATE TABLE IF NOT EXISTS metadata (
    cache_key   TEXT PRIMARY KEY,
    data        TEXT NOT NULL,
    fetched_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_metadata_expiry ON metadata(expires_at);
"""


def _make_key(*parts: Any) -> str:
    raw = json.dumps(parts, sort_keys=True, default=str)
    return hashlib.sha256(raw.encode()).hexdigest()


def _now() -> str:
    return datetime.now(UTC).isoformat()


def _expires(hours: float) -> str:
    from datetime import timedelta
    return (datetime.now(UTC) + timedelta(hours=hours)).isoformat()


class CacheManager:
    def __init__(self, cache_dir: Path) -> None:
        self._path = cache_dir / "pricing.db"
        self._db: aiosqlite.Connection | None = None

    async def initialize(self) -> None:
        self._path.parent.mkdir(parents=True, exist_ok=True)
        self._db = await aiosqlite.connect(self._path)
        self._db.row_factory = aiosqlite.Row
        await self._db.executescript(_CREATE_SCHEMA)
        await self._db.commit()
        logger.info("Cache initialised at %s", self._path)

    async def close(self) -> None:
        if self._db:
            await self._db.close()

    @property
    def db(self) -> aiosqlite.Connection:
        if not self._db:
            raise RuntimeError("CacheManager not initialised — call initialize() first")
        return self._db

    # ------------------------------------------------------------------
    # Prices table
    # ------------------------------------------------------------------

    async def get_prices(
        self,
        provider: str,
        service: str,
        region: str,
        key_extras: dict[str, Any],
    ) -> list[dict[str, Any]] | None:
        key = _make_key(provider, service, region, key_extras)
        async with self.db.execute(
            "SELECT data, expires_at FROM prices WHERE cache_key = ?", (key,)
        ) as cur:
            row = await cur.fetchone()
        if row is None:
            return None
        if datetime.fromisoformat(row["expires_at"]) < datetime.now(UTC):
            await self.db.execute("DELETE FROM prices WHERE cache_key = ?", (key,))
            await self.db.commit()
            return None
        return json.loads(row["data"])

    async def get_prices_with_meta(
        self,
        provider: str,
        service: str,
        region: str,
        key_extras: dict[str, Any],
    ) -> tuple[list[dict[str, Any]], datetime] | None:
        """Like get_prices but also returns the fetched_at timestamp for trust metadata."""
        key = _make_key(provider, service, region, key_extras)
        async with self.db.execute(
            "SELECT data, fetched_at, expires_at FROM prices WHERE cache_key = ?", (key,)
        ) as cur:
            row = await cur.fetchone()
        if row is None:
            return None
        if datetime.fromisoformat(row["expires_at"]) < datetime.now(UTC):
            await self.db.execute("DELETE FROM prices WHERE cache_key = ?", (key,))
            await self.db.commit()
            return None
        return json.loads(row["data"]), datetime.fromisoformat(row["fetched_at"])

    async def set_prices(
        self,
        provider: str,
        service: str,
        region: str,
        key_extras: dict[str, Any],
        data: list[dict[str, Any]],
        ttl_hours: float,
    ) -> None:
        key = _make_key(provider, service, region, key_extras)
        await self.db.execute(
            """
            INSERT OR REPLACE INTO prices
                (cache_key, provider, service, region, data, fetched_at, expires_at)
            VALUES (?, ?, ?, ?, ?, ?, ?)
            """,
            (key, provider, service, region, json.dumps(data), _now(), _expires(ttl_hours)),
        )
        await self.db.commit()

    # ------------------------------------------------------------------
    # Metadata table (region lists, attribute values, etc.)
    # ------------------------------------------------------------------

    async def get_metadata(self, key: str) -> Any | None:
        async with self.db.execute(
            "SELECT data, expires_at FROM metadata WHERE cache_key = ?", (key,)
        ) as cur:
            row = await cur.fetchone()
        if row is None:
            return None
        if datetime.fromisoformat(row["expires_at"]) < datetime.now(UTC):
            await self.db.execute("DELETE FROM metadata WHERE cache_key = ?", (key,))
            await self.db.commit()
            return None
        return json.loads(row["data"])

    async def set_metadata(self, key: str, data: Any, ttl_hours: float) -> None:
        await self.db.execute(
            """
            INSERT OR REPLACE INTO metadata (cache_key, data, fetched_at, expires_at)
            VALUES (?, ?, ?, ?)
            """,
            (key, json.dumps(data), _now(), _expires(ttl_hours)),
        )
        await self.db.commit()

    # ------------------------------------------------------------------
    # Cache management
    # ------------------------------------------------------------------

    async def purge_expired(self) -> int:
        now = _now()
        async with self.db.execute(
            "DELETE FROM prices WHERE expires_at < ?", (now,)
        ) as cur:
            prices_deleted = cur.rowcount
        async with self.db.execute(
            "DELETE FROM metadata WHERE expires_at < ?", (now,)
        ) as cur:
            meta_deleted = cur.rowcount
        await self.db.commit()
        return prices_deleted + meta_deleted

    async def ensure_schema_version(self) -> bool:
        """
        Check the stored schema version and purge the cache only when it differs
        from _SCHEMA_VERSION. Returns True if a purge was performed.

        Called on startup instead of clear_all() so that cached pricing data
        survives normal restarts (Docker, Claude Desktop, `uv run`).
        Only bump _SCHEMA_VERSION when the DB layout or serialisation format
        changes in a way that makes old rows incorrect.
        """
        _KEY = "__schema_version__"
        stored = await self.get_metadata(_KEY)
        if stored == _SCHEMA_VERSION:
            await self.purge_expired()
            return False

        # Version mismatch (or first run) — wipe and write new version
        async with self.db.execute("DELETE FROM prices") as cur:
            prices_deleted = cur.rowcount
        # Delete everything from metadata except the version key itself
        async with self.db.execute(
            "DELETE FROM metadata WHERE cache_key != ?", (_KEY,)
        ) as cur:
            meta_deleted = cur.rowcount
        await self.db.commit()
        logger.info(
            "Schema version changed (%s → %d): purged %d price rows, %d metadata rows",
            stored, _SCHEMA_VERSION, prices_deleted, meta_deleted,
        )
        await self.set_metadata(_KEY, _SCHEMA_VERSION, ttl_hours=365 * 24)
        return True

    async def clear_all(self) -> dict[str, int]:
        """Delete all cached prices and metadata."""
        async with self.db.execute("DELETE FROM prices") as cur:
            prices_deleted = cur.rowcount
        async with self.db.execute("DELETE FROM metadata") as cur:
            meta_deleted = cur.rowcount
        await self.db.commit()
        logger.info(
            "Cache cleared: %d price entries, %d metadata entries",
            prices_deleted, meta_deleted,
        )
        return {"prices_deleted": prices_deleted, "metadata_deleted": meta_deleted}

    async def clear_provider(self, provider: str) -> dict[str, int]:
        async with self.db.execute(
            "DELETE FROM prices WHERE provider = ?", (provider,)
        ) as cur:
            prices_deleted = cur.rowcount
        async with self.db.execute(
            "DELETE FROM metadata WHERE cache_key LIKE ?", (f"{provider}:%",)
        ) as cur:
            meta_deleted = cur.rowcount
        await self.db.commit()
        logger.info(
            "Cleared cache for provider %s: %d price entries, %d metadata entries",
            provider, prices_deleted, meta_deleted,
        )
        return {"prices_deleted": prices_deleted, "metadata_deleted": meta_deleted}

    async def stats(self) -> dict[str, Any]:
        async with self.db.execute("SELECT COUNT(*) as n FROM prices") as cur:
            prices_count = (await cur.fetchone())["n"]
        async with self.db.execute("SELECT COUNT(*) as n FROM metadata") as cur:
            meta_count = (await cur.fetchone())["n"]
        size_bytes = self._path.stat().st_size if self._path.exists() else 0
        return {
            "price_entries": prices_count,
            "metadata_entries": meta_count,
            "db_size_mb": round(size_bytes / 1024 / 1024, 2),
            "db_path": str(self._path),
        }
