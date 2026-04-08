from __future__ import annotations

from pathlib import Path

from pydantic import field_validator
from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(
        env_prefix="CLOUDCOSTMCP_",
        env_file=".env",
        env_file_encoding="utf-8",
        extra="ignore",
    )

    # General
    cache_dir: Path = Path.home() / ".cache" / "cloudcostmcp"
    cache_ttl_hours: int = 24
    metadata_ttl_days: int = 7
    effective_price_ttl_hours: int = 1
    default_currency: str = "USD"
    default_regions: list[str] = ["us-east-1", "us-west-2"]
    max_results: int = 20

    # AWS
    aws_profile: str | None = None
    aws_region: str = "us-east-1"
    # Cost Explorer costs $0.01/call — opt-in only
    aws_enable_cost_explorer: bool = False

    # GCP (Phase 3)
    gcp_project_id: str | None = None
    gcp_billing_dataset: str | None = None
    gcp_api_key: str | None = None

    @field_validator("cache_dir", mode="before")
    @classmethod
    def expand_path(cls, v: str | Path) -> Path:
        return Path(v).expanduser()
