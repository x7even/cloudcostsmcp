"""GCP OAuth2 auth resolver for the Cloud Billing Pricing API v1beta.

Resolution order (first match wins):
  1. OCC_GCP_ACCESS_TOKEN          — raw Bearer; debug/escape hatch only, ~1 h lifetime
  2. OCC_GCP_SERVICE_ACCOUNT_JSON_B64 / OCC_GCP_SERVICE_ACCOUNT_JSON
                                   — service-account key, auto-refresh via google-auth
  3. OCC_GCP_EXTERNAL_ACCOUNT_JSON_B64 / OCC_GCP_EXTERNAL_ACCOUNT_JSON
                                   — Workload Identity Federation config, auto-refresh
  4. GOOGLE_APPLICATION_CREDENTIALS / GCP metadata server / local ADC
                                   — auto-refresh via google-auth

google-auth is an optional dependency (pip install opencloudcosts[gcp]).
If not installed, only the raw-token path works.
"""
from __future__ import annotations

import asyncio
import base64
import binascii
import json
import logging
from datetime import UTC, datetime

from opencloudcosts.config import Settings
from opencloudcosts.providers.base import NotConfiguredError

logger = logging.getLogger(__name__)

_BILLING_READONLY_SCOPE = "https://www.googleapis.com/auth/cloud-billing.readonly"
_MAX_JSON_BYTES = 65_536  # 64 KiB — guard against absurdly large env var values

_RAW_TOKEN_WARNING = (
    "OCC_GCP_ACCESS_TOKEN is set. Raw Bearer tokens expire after ~1 hour and are "
    "NOT suitable for long-running MCP servers. Use OCC_GCP_SERVICE_ACCOUNT_JSON_B64, "
    "GOOGLE_APPLICATION_CREDENTIALS, or a GCP metadata-server credential instead."
)


class GcpAuthProvider:
    """Returns Authorization headers for the GCP Cloud Billing v1beta API.

    Instantiate once per GCPProvider; call get_headers() before each request.
    Token refresh is handled transparently for all credential sources except
    the raw access-token path.
    """

    def __init__(self, settings: Settings) -> None:
        self._settings = settings
        self._credentials: object | None = None  # google-auth Credentials object
        self._warned_raw_token = False
        self._refresh_lock: asyncio.Lock | None = None

    def _get_lock(self) -> asyncio.Lock:
        # Lazily create the lock inside the running event loop.
        if self._refresh_lock is None:
            self._refresh_lock = asyncio.Lock()
        return self._refresh_lock

    # ------------------------------------------------------------------
    # Public interface
    # ------------------------------------------------------------------

    async def get_headers(self) -> dict[str, str]:
        """Return {'Authorization': 'Bearer <token>'} ready to attach to a request."""
        token = await self._resolve_token()
        return {"Authorization": f"Bearer {token}"}

    # ------------------------------------------------------------------
    # Internal resolution
    # ------------------------------------------------------------------

    async def _resolve_token(self) -> str:
        s = self._settings

        # 1. Raw access token — escape hatch, no refresh
        if s.gcp_access_token:
            if not self._warned_raw_token:
                logger.warning(_RAW_TOKEN_WARNING)
                self._warned_raw_token = True
            self._check_raw_token_expiry(s.gcp_access_token_expires_at)
            return s.gcp_access_token

        # 2–4. google-auth paths (require optional [gcp] extra)
        try:
            import google.auth  # noqa: F401 — presence check
        except ImportError:
            raise NotConfiguredError(
                "GCP effective pricing requires google-auth.\n"
                "Install it: pip install opencloudcosts[gcp]\n\n"
                "Alternatively, set OCC_GCP_ACCESS_TOKEN for a short-lived token "
                "(debug/testing only — expires in ~1 hour)."
            )

        creds = await self._get_or_refresh_credentials()
        return creds.token  # type: ignore[attr-defined]

    async def _get_or_refresh_credentials(self) -> object:
        """Return a valid (refreshed-if-needed) google-auth Credentials object."""
        if self._credentials is None:
            self._credentials = self._build_credentials()

        creds = self._credentials
        if not getattr(creds, "valid", False):
            async with self._get_lock():
                # Re-check under the lock — another coroutine may have refreshed already.
                if not getattr(creds, "valid", False):
                    import google.auth.transport.requests
                    request = google.auth.transport.requests.Request()
                    # creds.refresh() is a blocking network call; run off the event loop.
                    await asyncio.to_thread(creds.refresh, request)  # type: ignore[union-attr]

        return creds

    def _build_credentials(self) -> object:
        """Construct google-auth Credentials from the first matching source."""
        import google.auth
        import google.oauth2.service_account

        s = self._settings

        # 2a. Service account JSON — B64 variant
        if s.gcp_service_account_json_b64:
            info = _decode_json_b64(s.gcp_service_account_json_b64, "OCC_GCP_SERVICE_ACCOUNT_JSON_B64")
            return google.oauth2.service_account.Credentials.from_service_account_info(
                info, scopes=[_BILLING_READONLY_SCOPE]
            )

        # 2b. Service account JSON — raw
        if s.gcp_service_account_json:
            info = _parse_json(s.gcp_service_account_json, "OCC_GCP_SERVICE_ACCOUNT_JSON")
            return google.oauth2.service_account.Credentials.from_service_account_info(
                info, scopes=[_BILLING_READONLY_SCOPE]
            )

        # 3a. External account / WIF — B64 variant
        if s.gcp_external_account_json_b64:
            info = _decode_json_b64(s.gcp_external_account_json_b64, "OCC_GCP_EXTERNAL_ACCOUNT_JSON_B64")
            return _external_account_creds(info)

        # 3b. External account / WIF — raw
        if s.gcp_external_account_json:
            info = _parse_json(s.gcp_external_account_json, "OCC_GCP_EXTERNAL_ACCOUNT_JSON")
            return _external_account_creds(info)

        # 4. ADC: GOOGLE_APPLICATION_CREDENTIALS, metadata server, local gcloud ADC
        try:
            creds, _ = google.auth.default(scopes=[_BILLING_READONLY_SCOPE])
            return creds
        except google.auth.exceptions.DefaultCredentialsError as exc:
            raise NotConfiguredError(
                "GCP effective pricing: no credentials found.\n\n"
                "Supported credential sources (in priority order):\n"
                "  1. OCC_GCP_SERVICE_ACCOUNT_JSON_B64 — base64-encoded SA key (Docker/K8s)\n"
                "  2. OCC_GCP_SERVICE_ACCOUNT_JSON    — raw SA key JSON\n"
                "  3. OCC_GCP_EXTERNAL_ACCOUNT_JSON_B64 — Workload Identity Federation\n"
                "  4. GOOGLE_APPLICATION_CREDENTIALS  — path to a key or ADC config file\n"
                "  5. GCP metadata server             — Cloud Run, GKE, GCE attached SA\n"
                "  6. OCC_GCP_ACCESS_TOKEN            — raw token (debug only, ~1 h)\n\n"
                "Run 'gcloud auth application-default login' or set one of the env vars above."
            ) from exc

    @staticmethod
    def _check_raw_token_expiry(expires_at: str | None) -> None:
        if not expires_at:
            return
        try:
            expiry = datetime.fromisoformat(expires_at.replace("Z", "+00:00"))
            if datetime.now(UTC) >= expiry:
                raise NotConfiguredError(
                    f"OCC_GCP_ACCESS_TOKEN expired at {expires_at}. "
                    "Provide a fresh token or switch to a service-account credential."
                )
        except ValueError:
            # Unparseable timestamp: log and continue rather than hard-failing.
            logger.warning(
                "OCC_GCP_ACCESS_TOKEN_EXPIRES_AT %r is not a valid ISO-8601 datetime "
                "— expiry check skipped. Verify the value format.",
                expires_at,
            )


# ------------------------------------------------------------------
# Helpers
# ------------------------------------------------------------------

def _decode_json_b64(value: str, var_name: str) -> dict:
    stripped = value.strip()
    if len(stripped) > _MAX_JSON_BYTES:
        raise NotConfiguredError(
            f"{var_name} exceeds maximum allowed size ({_MAX_JSON_BYTES} bytes). "
            "Verify the value is a base64-encoded service-account key."
        )
    try:
        decoded = base64.b64decode(stripped)
        return json.loads(decoded)
    except (ValueError, binascii.Error, json.JSONDecodeError) as exc:
        raise NotConfiguredError(
            f"{var_name} is not valid base64-encoded JSON. Check the encoding."
        ) from exc


def _parse_json(value: str, var_name: str) -> dict:
    if len(value) > _MAX_JSON_BYTES:
        raise NotConfiguredError(
            f"{var_name} exceeds maximum allowed size ({_MAX_JSON_BYTES} bytes)."
        )
    try:
        return json.loads(value)
    except json.JSONDecodeError as exc:
        raise NotConfiguredError(
            f"{var_name} is not valid JSON. Check the value."
        ) from exc


def _external_account_creds(info: dict) -> object:
    """Build credentials for Workload Identity Federation external account configs.

    Uses google.auth.load_credentials_from_dict which dispatches to the correct
    concrete subclass based on the 'type' field in the config JSON.
    """
    import google.auth
    creds, _ = google.auth.load_credentials_from_dict(
        info, scopes=[_BILLING_READONLY_SCOPE]
    )
    return creds
