"""
OpenCloudCosts MCP Server entry point.

Run via:
    uv run cloudcostmcp          # stdio (default, for MCP clients)
    uv run cloudcostmcp --help
"""
from __future__ import annotations

import logging
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


def create_server() -> FastMCP:
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
    mcp = create_server()
    mcp.run()


if __name__ == "__main__":
    main()
