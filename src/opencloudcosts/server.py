"""
OpenCloudCosts MCP Server entry point.

Run via:
    uv run opencloudcosts                          # stdio (default, for MCP clients)
    uv run opencloudcosts --transport http         # HTTP/streamable-http on port 8080
    uv run opencloudcosts --transport http --host 0.0.0.0 --port 9000
    uv run opencloudcosts --help
"""
from __future__ import annotations

import argparse
import logging
import os
from contextlib import asynccontextmanager
from typing import Any, AsyncIterator

from mcp.server.fastmcp import FastMCP

from opencloudcosts.cache import CacheManager
from opencloudcosts.config import Settings

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)


@asynccontextmanager
async def _lifespan(server: FastMCP) -> AsyncIterator[dict[str, Any]]:
    """Initialise the cache and provider clients, then yield the shared context."""
    settings = Settings()
    settings.cache_dir.mkdir(parents=True, exist_ok=True)

    cache = CacheManager(settings.cache_dir)
    await cache.initialize()

    providers: dict[str, Any] = {}

    # AWS provider — always available (public pricing requires no credentials)
    try:
        from opencloudcosts.providers.aws import AWSProvider
        providers["aws"] = AWSProvider(settings, cache)
        logger.info("AWS provider initialised (Cost Explorer: %s)", settings.aws_enable_cost_explorer)
    except Exception as e:
        logger.warning("Could not initialise AWS provider: %s", e)

    # GCP provider — enabled with an API key OR when google-auth ADC is available
    gcp_provider = None
    try:
        from opencloudcosts.providers.gcp import GCPProvider
        gcp_provider = GCPProvider(settings, cache)
        providers["gcp"] = gcp_provider
        auth_method = "API key" if settings.gcp_api_key else "ADC"
        logger.info("GCP provider initialised (auth: %s)", auth_method)
    except Exception as e:
        logger.warning("Could not initialise GCP provider: %s", e)

    logger.info("Configured providers: %s", list(providers))

    yield {"settings": settings, "cache": cache, "providers": providers}

    # Clean up async HTTP clients
    if gcp_provider is not None:
        await gcp_provider.close()

    await cache.close()
    logger.info("CloudCost MCP server shut down")


def create_server(host: str = "127.0.0.1", port: int = 8080) -> FastMCP:
    mcp = FastMCP(
        name="OpenCloudCosts MCP",
        instructions=(
            "OpenCloudCosts MCP provides accurate public and effective cloud pricing data. "
            "Use it to look up compute, storage, and database pricing on AWS and GCP; "
            "compare prices across regions; estimate TCO from a Bill of Materials; "
            "and calculate unit economics. For effective/bespoke pricing (post-discount), "
            "ensure provider credentials are configured."
        ),
        lifespan=_lifespan,
        host=host,
        port=port,
    )

    # Register tool groups
    from opencloudcosts.tools.availability import register_availability_tools
    from opencloudcosts.tools.bom import register_bom_tools
    from opencloudcosts.tools.lookup import register_lookup_tools

    register_lookup_tools(mcp)
    register_availability_tools(mcp)
    register_bom_tools(mcp)

    return mcp


def main() -> None:
    parser = argparse.ArgumentParser(description="OpenCloudCosts MCP server")
    parser.add_argument(
        "--transport",
        choices=["stdio", "http"],
        default="stdio",
        help="Transport type (default: stdio)",
    )
    parser.add_argument(
        "--host",
        default=os.getenv("OCC_HTTP_HOST", "127.0.0.1"),
        help="HTTP bind address (default: 127.0.0.1, env: OCC_HTTP_HOST)",
    )
    parser.add_argument(
        "--port",
        type=int,
        default=int(os.getenv("OCC_HTTP_PORT", "8080")),
        help="HTTP port (default: 8080, env: OCC_HTTP_PORT)",
    )
    args = parser.parse_args()

    if args.transport == "http":
        # FastMCP uses streamable-http transport (MCP spec 2025-03-26).
        # host and port are passed at construction time via FastMCP settings.
        mcp = create_server(host=args.host, port=args.port)
        logger.info("Starting HTTP server on %s:%s", args.host, args.port)
        mcp.run(transport="streamable-http")
    else:
        # stdio — default, unchanged behaviour for MCP clients (Claude Code, etc.)
        mcp = create_server()
        mcp.run()


if __name__ == "__main__":
    main()
