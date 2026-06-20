"""Tests for GCP auth resolver (gcp_auth.py).

Covers all credential resolution paths, token-expiry detection, refresh logic,
concurrent-refresh locking, and error handling for malformed credentials.

google-auth is an optional dependency; all google.auth/google.oauth2 imports
are injected as mock modules into sys.modules so tests run without GCP credentials.
"""

from __future__ import annotations

import asyncio
import base64
import json
import sys
import time
import types
from datetime import UTC, datetime, timedelta
from unittest.mock import MagicMock, patch

import pytest

from opencloudcosts.config import Settings
from opencloudcosts.providers.base import NotConfiguredError
from opencloudcosts.providers.gcp_auth import (
    GcpAuthProvider,
    _decode_json_b64,
    _parse_json,
)

# ---------------------------------------------------------------------------
# Helpers / shared fixtures
# ---------------------------------------------------------------------------

_FAKE_SA_INFO = {
    "type": "service_account",
    "project_id": "my-project",
    "private_key_id": "key-id",
    "private_key": "fake-key",
    "client_email": "sa@my-project.iam.gserviceaccount.com",
    "client_id": "12345",
    "auth_uri": "https://accounts.google.com/o/oauth2/auth",
    "token_uri": "https://oauth2.googleapis.com/token",
}

_FAKE_SA_JSON = json.dumps(_FAKE_SA_INFO)
_FAKE_SA_B64 = base64.b64encode(_FAKE_SA_JSON.encode()).decode()

_FAKE_EXTERNAL_INFO = {
    "type": "external_account",
    "audience": "//iam.googleapis.com/projects/123/pools/pool/providers/prov",
    "subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
    "token_url": "https://sts.googleapis.com/v1/token",
    "credential_source": {"url": "http://metadata.google.internal/token"},
}

_FAKE_EXTERNAL_JSON = json.dumps(_FAKE_EXTERNAL_INFO)
_FAKE_EXTERNAL_B64 = base64.b64encode(_FAKE_EXTERNAL_JSON.encode()).decode()


def _make_valid_creds(token: str = "mock-token") -> MagicMock:
    """Return a mock google-auth Credentials object that is already valid."""
    creds = MagicMock()
    creds.valid = True
    creds.token = token
    return creds


def _make_invalid_creds(token: str = "refreshed-token") -> MagicMock:
    """Return a mock google-auth Credentials object that needs refreshing."""
    creds = MagicMock()
    creds.valid = False
    creds.token = token

    def _do_refresh(request):
        creds.valid = True

    creds.refresh.side_effect = _do_refresh
    return creds


def _settings(**kwargs) -> Settings:
    """Build a Settings object with all GCP credential fields defaulting to None."""
    defaults = dict(
        gcp_access_token=None,
        gcp_access_token_expires_at=None,
        gcp_service_account_json_b64=None,
        gcp_service_account_json=None,
        gcp_external_account_json_b64=None,
        gcp_external_account_json=None,
    )
    defaults.update(kwargs)
    return Settings(**defaults)


class FakeDefaultCredentialsError(Exception):
    """Stand-in for google.auth.exceptions.DefaultCredentialsError."""


def _inject_google_mocks() -> tuple[dict, MagicMock, MagicMock, MagicMock]:
    """Inject minimal fake google.* modules into sys.modules.

    Returns (saved_modules, mock_google_auth, mock_sa_credentials, mock_transport_request)
    so callers can configure them before use.
    """
    # Save any existing google modules to restore later
    saved = {k: sys.modules[k] for k in list(sys.modules) if k.startswith("google")}

    # Build mock hierarchy
    mock_google = types.ModuleType("google")
    mock_google_auth = types.ModuleType("google.auth")
    mock_google_auth_exceptions = types.ModuleType("google.auth.exceptions")
    mock_google_auth_transport = types.ModuleType("google.auth.transport")
    mock_google_auth_transport_requests = types.ModuleType("google.auth.transport.requests")
    mock_google_oauth2 = types.ModuleType("google.oauth2")
    mock_google_oauth2_sa = types.ModuleType("google.oauth2.service_account")

    # DefaultCredentialsError on the exceptions module
    mock_google_auth_exceptions.DefaultCredentialsError = FakeDefaultCredentialsError

    # Request class (returned from transport.requests.Request())
    mock_request_instance = MagicMock()
    mock_request_cls = MagicMock(return_value=mock_request_instance)
    mock_google_auth_transport_requests.Request = mock_request_cls

    # SA credentials factory
    mock_sa_creds = _make_valid_creds("sa-token")
    mock_sa_cls = MagicMock()
    mock_sa_cls.from_service_account_info = MagicMock(return_value=mock_sa_creds)
    mock_google_oauth2_sa.Credentials = mock_sa_cls

    # Stitch together
    mock_google_auth.exceptions = mock_google_auth_exceptions
    mock_google_auth.transport = mock_google_auth_transport
    mock_google_auth_transport.requests = mock_google_auth_transport_requests

    mock_google.auth = mock_google_auth
    mock_google.oauth2 = mock_google_oauth2
    mock_google_oauth2.service_account = mock_google_oauth2_sa

    # Install into sys.modules
    sys.modules["google"] = mock_google
    sys.modules["google.auth"] = mock_google_auth
    sys.modules["google.auth.exceptions"] = mock_google_auth_exceptions
    sys.modules["google.auth.transport"] = mock_google_auth_transport
    sys.modules["google.auth.transport.requests"] = mock_google_auth_transport_requests
    sys.modules["google.oauth2"] = mock_google_oauth2
    sys.modules["google.oauth2.service_account"] = mock_google_oauth2_sa

    return saved, mock_google_auth, mock_sa_cls, mock_request_cls


def _restore_google_mocks(saved: dict) -> None:
    """Remove injected fake google modules and restore saved state."""
    for k in list(sys.modules):
        if k.startswith("google"):
            del sys.modules[k]
    sys.modules.update(saved)


@pytest.fixture
def google_mocks():
    """Fixture that injects fake google.* modules for the duration of one test."""
    saved, mock_auth, mock_sa_cls, mock_request_cls = _inject_google_mocks()

    # Expose the top-level mock objects so tests can configure them
    yield {
        "auth": mock_auth,
        "sa_cls": mock_sa_cls,
        "request_cls": mock_request_cls,
    }

    _restore_google_mocks(saved)


# ---------------------------------------------------------------------------
# Path 1: raw access token — no google-auth needed
# ---------------------------------------------------------------------------


class TestRawAccessToken:
    async def test_returns_bearer_header(self):
        s = _settings(gcp_access_token="raw-tok")
        provider = GcpAuthProvider(s)
        headers = await provider.get_headers()
        assert headers == {"Authorization": "Bearer raw-tok"}

    async def test_warns_once(self, caplog):
        s = _settings(gcp_access_token="raw-tok")
        provider = GcpAuthProvider(s)
        import logging

        with caplog.at_level(logging.WARNING, logger="opencloudcosts.providers.gcp_auth"):
            await provider.get_headers()
            await provider.get_headers()  # second call should NOT warn again

        raw_warnings = [r for r in caplog.records if "NOT suitable for long-running" in r.message]
        assert len(raw_warnings) == 1

    async def test_not_expired_token_succeeds(self):
        future = (datetime.now(UTC) + timedelta(hours=1)).isoformat()
        s = _settings(gcp_access_token="raw-tok", gcp_access_token_expires_at=future)
        provider = GcpAuthProvider(s)
        headers = await provider.get_headers()
        assert headers["Authorization"] == "Bearer raw-tok"

    async def test_expired_token_raises(self):
        past = "2020-01-01T00:00:00Z"
        s = _settings(gcp_access_token="raw-tok", gcp_access_token_expires_at=past)
        provider = GcpAuthProvider(s)
        with pytest.raises(NotConfiguredError, match="expired"):
            await provider.get_headers()

    async def test_unparseable_expiry_logs_warning_does_not_raise(self, caplog):
        s = _settings(gcp_access_token="raw-tok", gcp_access_token_expires_at="not-a-date")
        provider = GcpAuthProvider(s)
        import logging

        with caplog.at_level(logging.WARNING, logger="opencloudcosts.providers.gcp_auth"):
            headers = await provider.get_headers()

        assert headers["Authorization"] == "Bearer raw-tok"
        assert any("not a valid ISO-8601" in r.message for r in caplog.records)

    async def test_raw_token_beats_sa_json(self, google_mocks):
        """Raw token is checked before any google-auth credential is built."""
        s = _settings(
            gcp_access_token="raw-wins",
            gcp_service_account_json_b64=_FAKE_SA_B64,
        )
        provider = GcpAuthProvider(s)
        headers = await provider.get_headers()
        assert headers == {"Authorization": "Bearer raw-wins"}
        # SA class should never have been touched
        google_mocks["sa_cls"].from_service_account_info.assert_not_called()


# ---------------------------------------------------------------------------
# Static helper: _check_raw_token_expiry
# ---------------------------------------------------------------------------


class TestCheckRawTokenExpiry:
    def test_none_is_noop(self):
        GcpAuthProvider._check_raw_token_expiry(None)

    def test_future_expiry_passes(self):
        future = (datetime.now(UTC) + timedelta(hours=2)).isoformat()
        GcpAuthProvider._check_raw_token_expiry(future)

    def test_past_expiry_raises(self):
        past = "2010-06-01T00:00:00+00:00"
        with pytest.raises(NotConfiguredError, match="expired"):
            GcpAuthProvider._check_raw_token_expiry(past)

    def test_z_suffix_accepted(self):
        future = "2099-01-01T00:00:00Z"
        GcpAuthProvider._check_raw_token_expiry(future)

    def test_invalid_format_does_not_raise(self, caplog):
        import logging

        with caplog.at_level(logging.WARNING):
            GcpAuthProvider._check_raw_token_expiry("tomorrow")
        assert any("not a valid ISO-8601" in r.message for r in caplog.records)


# ---------------------------------------------------------------------------
# Path 2a: Service account JSON — base64
# ---------------------------------------------------------------------------


class TestServiceAccountB64:
    async def test_returns_sa_token(self, google_mocks):
        mock_sa_creds = _make_valid_creds("sa-b64-token")
        google_mocks["sa_cls"].from_service_account_info.return_value = mock_sa_creds

        s = _settings(gcp_service_account_json_b64=_FAKE_SA_B64)
        provider = GcpAuthProvider(s)
        headers = await provider.get_headers()

        assert headers == {"Authorization": "Bearer sa-b64-token"}

    async def test_b64_decoded_correctly(self, google_mocks):
        mock_sa_creds = _make_valid_creds()
        google_mocks["sa_cls"].from_service_account_info.return_value = mock_sa_creds

        s = _settings(gcp_service_account_json_b64=_FAKE_SA_B64)
        provider = GcpAuthProvider(s)
        await provider.get_headers()

        call_args = google_mocks["sa_cls"].from_service_account_info.call_args
        passed_info = call_args[0][0]
        assert passed_info == _FAKE_SA_INFO

    async def test_billing_scope_requested(self, google_mocks):
        mock_sa_creds = _make_valid_creds()
        google_mocks["sa_cls"].from_service_account_info.return_value = mock_sa_creds

        s = _settings(gcp_service_account_json_b64=_FAKE_SA_B64)
        provider = GcpAuthProvider(s)
        await provider.get_headers()

        call_kwargs = google_mocks["sa_cls"].from_service_account_info.call_args[1]
        scopes = call_kwargs.get("scopes", [])
        assert any("cloud-billing" in sc for sc in scopes)


# ---------------------------------------------------------------------------
# Path 2b: Service account JSON — raw
# ---------------------------------------------------------------------------


class TestServiceAccountRaw:
    async def test_returns_sa_token(self, google_mocks):
        mock_sa_creds = _make_valid_creds("sa-raw-token")
        google_mocks["sa_cls"].from_service_account_info.return_value = mock_sa_creds

        s = _settings(gcp_service_account_json=_FAKE_SA_JSON)
        provider = GcpAuthProvider(s)
        headers = await provider.get_headers()

        assert headers == {"Authorization": "Bearer sa-raw-token"}
        passed_info = google_mocks["sa_cls"].from_service_account_info.call_args[0][0]
        assert passed_info["client_email"] == _FAKE_SA_INFO["client_email"]

    async def test_b64_beats_raw_sa(self, google_mocks):
        """SA b64 should win when both b64 and raw SA JSON are set."""
        call_count_before = google_mocks["sa_cls"].from_service_account_info.call_count
        mock_sa_creds = _make_valid_creds("b64-wins")
        google_mocks["sa_cls"].from_service_account_info.return_value = mock_sa_creds

        s = _settings(
            gcp_service_account_json_b64=_FAKE_SA_B64,
            gcp_service_account_json=_FAKE_SA_JSON,
        )
        provider = GcpAuthProvider(s)
        await provider.get_headers()

        # called exactly once (the b64 branch)
        assert google_mocks["sa_cls"].from_service_account_info.call_count == call_count_before + 1
        passed_info = google_mocks["sa_cls"].from_service_account_info.call_args[0][0]
        assert passed_info == _FAKE_SA_INFO  # decoded from b64


# ---------------------------------------------------------------------------
# Path 3a/3b: External account / Workload Identity Federation
# ---------------------------------------------------------------------------


class TestExternalAccount:
    async def test_external_b64_resolves(self, google_mocks):
        mock_ext_creds = _make_valid_creds("ext-b64-token")
        google_mocks["auth"].load_credentials_from_dict = MagicMock(
            return_value=(mock_ext_creds, "project-id")
        )

        s = _settings(gcp_external_account_json_b64=_FAKE_EXTERNAL_B64)
        provider = GcpAuthProvider(s)
        headers = await provider.get_headers()

        assert headers == {"Authorization": "Bearer ext-b64-token"}
        google_mocks["auth"].load_credentials_from_dict.assert_called_once()

    async def test_external_raw_resolves(self, google_mocks):
        mock_ext_creds = _make_valid_creds("ext-raw-token")
        google_mocks["auth"].load_credentials_from_dict = MagicMock(
            return_value=(mock_ext_creds, "project-id")
        )

        s = _settings(gcp_external_account_json=_FAKE_EXTERNAL_JSON)
        provider = GcpAuthProvider(s)
        headers = await provider.get_headers()

        assert headers == {"Authorization": "Bearer ext-raw-token"}

    async def test_sa_beats_external(self, google_mocks):
        """SA credential should win over external account when both are set."""
        mock_sa_creds = _make_valid_creds("sa-wins")
        google_mocks["sa_cls"].from_service_account_info.return_value = mock_sa_creds
        google_mocks["auth"].load_credentials_from_dict = MagicMock()

        s = _settings(
            gcp_service_account_json=_FAKE_SA_JSON,
            gcp_external_account_json=_FAKE_EXTERNAL_JSON,
        )
        provider = GcpAuthProvider(s)
        headers = await provider.get_headers()

        assert headers == {"Authorization": "Bearer sa-wins"}
        google_mocks["auth"].load_credentials_from_dict.assert_not_called()


# ---------------------------------------------------------------------------
# Path 4: Application Default Credentials (ADC)
# ---------------------------------------------------------------------------


class TestApplicationDefaultCredentials:
    async def test_adc_resolves(self, google_mocks):
        mock_adc_creds = _make_valid_creds("adc-token")
        google_mocks["auth"].default = MagicMock(return_value=(mock_adc_creds, "project-id"))

        s = _settings()
        provider = GcpAuthProvider(s)
        headers = await provider.get_headers()

        assert headers == {"Authorization": "Bearer adc-token"}
        google_mocks["auth"].default.assert_called_once()

    async def test_adc_failure_raises_not_configured_error(self, google_mocks):
        google_mocks["auth"].default = MagicMock(
            side_effect=FakeDefaultCredentialsError("no creds found")
        )
        # Make exceptions.DefaultCredentialsError match
        google_mocks["auth"].exceptions.DefaultCredentialsError = FakeDefaultCredentialsError

        s = _settings()
        provider = GcpAuthProvider(s)

        with pytest.raises(NotConfiguredError, match="no credentials found"):
            await provider.get_headers()

    async def test_adc_error_message_mentions_sources(self, google_mocks):
        google_mocks["auth"].default = MagicMock(
            side_effect=FakeDefaultCredentialsError("no creds")
        )
        google_mocks["auth"].exceptions.DefaultCredentialsError = FakeDefaultCredentialsError

        s = _settings()
        provider = GcpAuthProvider(s)

        with pytest.raises(NotConfiguredError) as exc_info:
            await provider.get_headers()
        msg = str(exc_info.value)
        assert "OCC_GCP_SERVICE_ACCOUNT_JSON_B64" in msg
        assert "gcloud auth application-default login" in msg

    async def test_external_beats_adc(self, google_mocks):
        """External account credential should win over ADC."""
        mock_ext_creds = _make_valid_creds("ext-wins")
        google_mocks["auth"].load_credentials_from_dict = MagicMock(
            return_value=(mock_ext_creds, "proj")
        )
        google_mocks["auth"].default = MagicMock()

        s = _settings(gcp_external_account_json=_FAKE_EXTERNAL_JSON)
        provider = GcpAuthProvider(s)
        headers = await provider.get_headers()

        assert headers == {"Authorization": "Bearer ext-wins"}
        google_mocks["auth"].default.assert_not_called()


# ---------------------------------------------------------------------------
# No credentials / google-auth not installed
# ---------------------------------------------------------------------------


class TestNoGoogleAuth:
    async def test_no_google_auth_raises_not_configured(self):
        """Without google-auth and no raw token, expect NotConfiguredError."""
        s = _settings()
        provider = GcpAuthProvider(s)

        # Remove any google modules from sys.modules to simulate uninstalled package
        saved = {k: sys.modules.pop(k) for k in list(sys.modules) if k.startswith("google")}
        try:
            with pytest.raises(NotConfiguredError, match="google-auth"):
                await provider.get_headers()
        finally:
            sys.modules.update(saved)

    async def test_not_configured_message_mentions_install(self):
        s = _settings()
        provider = GcpAuthProvider(s)

        saved = {k: sys.modules.pop(k) for k in list(sys.modules) if k.startswith("google")}
        try:
            with pytest.raises(NotConfiguredError) as exc_info:
                await provider.get_headers()
            assert "pip install opencloudcosts[gcp]" in str(exc_info.value)
        finally:
            sys.modules.update(saved)


# ---------------------------------------------------------------------------
# Token refresh logic
# ---------------------------------------------------------------------------


class TestTokenRefresh:
    async def test_refresh_called_when_creds_invalid(self, google_mocks):
        mock_creds = _make_invalid_creds("refreshed-token")
        google_mocks["sa_cls"].from_service_account_info.return_value = mock_creds

        s = _settings(gcp_service_account_json_b64=_FAKE_SA_B64)
        provider = GcpAuthProvider(s)
        headers = await provider.get_headers()

        mock_creds.refresh.assert_called_once()
        assert headers == {"Authorization": "Bearer refreshed-token"}

    async def test_no_refresh_when_creds_valid(self, google_mocks):
        mock_creds = _make_valid_creds("valid-token")
        google_mocks["sa_cls"].from_service_account_info.return_value = mock_creds

        s = _settings(gcp_service_account_json_b64=_FAKE_SA_B64)
        provider = GcpAuthProvider(s)

        await provider.get_headers()
        await provider.get_headers()

        mock_creds.refresh.assert_not_called()

    async def test_refresh_runs_in_thread(self, google_mocks):
        """refresh() is blocking — must be dispatched via asyncio.to_thread."""
        mock_creds = _make_invalid_creds("tok")

        s = _settings()
        provider = GcpAuthProvider(s)
        provider._credentials = mock_creds  # bypass _build_credentials

        with patch("asyncio.to_thread") as mock_to_thread:

            async def _run_in_thread(fn, *args):
                fn(*args)

            mock_to_thread.side_effect = _run_in_thread
            await provider.get_headers()

        mock_to_thread.assert_called_once_with(
            mock_creds.refresh, google_mocks["request_cls"].return_value
        )

    async def test_credentials_cached_across_calls(self, google_mocks):
        """_build_credentials must only be called once per provider instance."""
        mock_creds = _make_valid_creds("cached-token")
        google_mocks["sa_cls"].from_service_account_info.return_value = mock_creds

        s = _settings(gcp_service_account_json_b64=_FAKE_SA_B64)
        provider = GcpAuthProvider(s)

        await provider.get_headers()
        await provider.get_headers()
        await provider.get_headers()

        assert google_mocks["sa_cls"].from_service_account_info.call_count == 1


# ---------------------------------------------------------------------------
# Concurrent refresh — lock prevents double-refresh
# ---------------------------------------------------------------------------


class TestConcurrentRefreshLock:
    async def test_concurrent_calls_refresh_once(self, google_mocks):
        """Multiple concurrent coroutines must trigger exactly one refresh."""
        mock_creds = MagicMock()
        mock_creds.valid = False
        mock_creds.token = "tok"

        def _refresh(request):
            # Small real sleep so other coroutines reach the lock while this one holds it
            time.sleep(0.02)
            mock_creds.valid = True

        mock_creds.refresh.side_effect = _refresh

        s = _settings()
        provider = GcpAuthProvider(s)
        provider._credentials = mock_creds  # bypass _build_credentials

        results = await asyncio.gather(*[provider.get_headers() for _ in range(5)])

        assert mock_creds.refresh.call_count == 1
        assert all(h == {"Authorization": "Bearer tok"} for h in results)

    async def test_lock_is_asyncio_lock(self, google_mocks):
        """_get_lock() must return an asyncio.Lock for correct event-loop behaviour."""
        s = _settings()
        provider = GcpAuthProvider(s)
        lock = provider._get_lock()
        assert isinstance(lock, asyncio.Lock)

    async def test_same_lock_returned_on_repeated_calls(self, google_mocks):
        s = _settings()
        provider = GcpAuthProvider(s)
        lock1 = provider._get_lock()
        lock2 = provider._get_lock()
        assert lock1 is lock2


# ---------------------------------------------------------------------------
# Error handling — malformed credentials (_decode_json_b64, _parse_json)
# ---------------------------------------------------------------------------


class TestDecodeJsonB64:
    def test_valid_b64_json_decoded(self):
        result = _decode_json_b64(_FAKE_SA_B64, "TEST_VAR")
        assert result == _FAKE_SA_INFO

    def test_invalid_base64_raises(self):
        with pytest.raises(NotConfiguredError, match="not valid base64"):
            _decode_json_b64("not-valid-base64!!!", "TEST_VAR")

    def test_valid_b64_but_not_json_raises(self):
        bad_b64 = base64.b64encode(b"this is not json at all").decode()
        with pytest.raises(NotConfiguredError, match="not valid base64"):
            _decode_json_b64(bad_b64, "TEST_VAR")

    def test_oversized_value_raises(self):
        huge = base64.b64encode(b"x" * 70_000).decode()
        with pytest.raises(NotConfiguredError, match="exceeds maximum"):
            _decode_json_b64(huge, "TEST_VAR")

    def test_error_message_includes_var_name(self):
        with pytest.raises(NotConfiguredError) as exc_info:
            _decode_json_b64("!!!bad!!!", "MY_VAR_NAME")
        assert "MY_VAR_NAME" in str(exc_info.value)

    def test_strips_surrounding_whitespace(self):
        padded = f"  {_FAKE_SA_B64}  "
        result = _decode_json_b64(padded, "TEST_VAR")
        assert result == _FAKE_SA_INFO


class TestParseJson:
    def test_valid_json_parsed(self):
        result = _parse_json(_FAKE_SA_JSON, "TEST_VAR")
        assert result == _FAKE_SA_INFO

    def test_invalid_json_raises(self):
        with pytest.raises(NotConfiguredError, match="not valid JSON"):
            _parse_json("{ this is not json }", "TEST_VAR")

    def test_oversized_value_raises(self):
        huge = '{"k": "' + "x" * 70_000 + '"}'
        with pytest.raises(NotConfiguredError, match="exceeds maximum"):
            _parse_json(huge, "TEST_VAR")

    def test_error_message_includes_var_name(self):
        with pytest.raises(NotConfiguredError) as exc_info:
            _parse_json("bad json", "OCC_GCP_SERVICE_ACCOUNT_JSON")
        assert "OCC_GCP_SERVICE_ACCOUNT_JSON" in str(exc_info.value)

    def test_empty_string_raises(self):
        with pytest.raises(NotConfiguredError, match="not valid JSON"):
            _parse_json("", "TEST_VAR")
