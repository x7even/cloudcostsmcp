"""Tests for opencloudcosts.utils.http_retry."""

from __future__ import annotations

from unittest.mock import MagicMock

import httpx
import pytest

from opencloudcosts.utils.http_retry import _is_transient, async_retry, sync_retry

# ---------------------------------------------------------------------------
# _is_transient
# ---------------------------------------------------------------------------


def _http_status_exc(status_code: int) -> httpx.HTTPStatusError:
    response = MagicMock(spec=httpx.Response)
    response.status_code = status_code
    return httpx.HTTPStatusError("error", request=MagicMock(), response=response)


@pytest.mark.parametrize("code", [429, 500, 502, 503, 504])
def test_is_transient_retryable_status_codes(code):
    exc = _http_status_exc(code)
    assert _is_transient(exc) is True


@pytest.mark.parametrize("code", [200, 201, 400, 401, 403, 404, 422])
def test_is_transient_non_retryable_status_codes(code):
    exc = _http_status_exc(code)
    assert _is_transient(exc) is False


def test_is_transient_timeout():
    exc = httpx.TimeoutException("timed out")
    assert _is_transient(exc) is True


def test_is_transient_connect_error():
    exc = httpx.ConnectError("connection refused")
    assert _is_transient(exc) is True


def test_is_transient_remote_protocol_error():
    exc = httpx.RemoteProtocolError("peer closed connection")
    assert _is_transient(exc) is True


def test_is_transient_unrelated_exception():
    assert _is_transient(ValueError("oops")) is False
    assert _is_transient(RuntimeError("boom")) is False


# ---------------------------------------------------------------------------
# sync_retry
# ---------------------------------------------------------------------------


def test_sync_retry_succeeds_on_first_attempt():
    call_count = 0

    for attempt in sync_retry():
        with attempt:
            call_count += 1

    assert call_count == 1


def test_sync_retry_retries_on_transient_status_then_succeeds():
    call_count = 0

    for attempt in sync_retry():
        with attempt:
            call_count += 1
            if call_count < 2:
                raise _http_status_exc(500)

    assert call_count == 2


def test_sync_retry_exhausts_attempts_and_reraises():
    """After 3 attempts the original exception must propagate (reraise=True)."""
    call_count = 0

    with pytest.raises(httpx.HTTPStatusError):
        for attempt in sync_retry():
            with attempt:
                call_count += 1
                raise _http_status_exc(503)

    assert call_count == 3


def test_sync_retry_does_not_retry_non_transient():
    """A 404 must not trigger a retry."""
    call_count = 0

    with pytest.raises(httpx.HTTPStatusError):
        for attempt in sync_retry():
            with attempt:
                call_count += 1
                raise _http_status_exc(404)

    assert call_count == 1


# ---------------------------------------------------------------------------
# async_retry
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_async_retry_succeeds_on_first_attempt():
    call_count = 0

    async for attempt in async_retry():
        with attempt:
            call_count += 1

    assert call_count == 1


@pytest.mark.asyncio
async def test_async_retry_retries_on_transient_status_then_succeeds():
    call_count = 0

    async for attempt in async_retry():
        with attempt:
            call_count += 1
            if call_count < 2:
                raise _http_status_exc(429)

    assert call_count == 2


@pytest.mark.asyncio
async def test_async_retry_exhausts_attempts_and_reraises():
    call_count = 0

    with pytest.raises(httpx.HTTPStatusError):
        async for attempt in async_retry():
            with attempt:
                call_count += 1
                raise _http_status_exc(502)

    assert call_count == 3


@pytest.mark.asyncio
async def test_async_retry_does_not_retry_non_transient():
    call_count = 0

    with pytest.raises(httpx.HTTPStatusError):
        async for attempt in async_retry():
            with attempt:
                call_count += 1
                raise _http_status_exc(404)

    assert call_count == 1
