"""Shared retry helpers for upstream cloud-pricing HTTP calls."""
from __future__ import annotations

import httpx
from tenacity import (
    AsyncRetrying,
    RetryError,
    Retrying,
    retry_if_exception,
    stop_after_attempt,
    wait_exponential,
)

_TRANSIENT_CODES = frozenset({429, 500, 502, 503, 504})


def _is_transient(exc: BaseException) -> bool:
    if isinstance(exc, httpx.HTTPStatusError):
        return exc.response.status_code in _TRANSIENT_CODES
    return isinstance(exc, (httpx.TimeoutException, httpx.ConnectError, httpx.RemoteProtocolError))


_RETRY_KWARGS = dict(
    retry=retry_if_exception(_is_transient),
    wait=wait_exponential(multiplier=1, min=1, max=30),
    stop=stop_after_attempt(3),
    reraise=True,
)


def sync_retry() -> Retrying:
    """Return a Retrying context manager for synchronous HTTP calls."""
    return Retrying(**_RETRY_KWARGS)  # type: ignore[arg-type]


def async_retry() -> AsyncRetrying:
    """Return an AsyncRetrying context manager for async HTTP calls."""
    return AsyncRetrying(**_RETRY_KWARGS)  # type: ignore[arg-type]
