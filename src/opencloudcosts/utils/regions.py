"""
Region code <-> display name mappings for AWS and GCP.

AWS Pricing API uses display names ("US East (N. Virginia)") rather than region
codes ("us-east-1"), so we need bidirectional mapping.
"""
from __future__ import annotations

# AWS: region code -> human-readable display name used by the Pricing API
AWS_REGION_DISPLAY: dict[str, str] = {
    # North America
    "us-east-1": "US East (N. Virginia)",
    "us-east-2": "US East (Ohio)",
    "us-west-1": "US West (N. California)",
    "us-west-2": "US West (Oregon)",
    "ca-central-1": "Canada (Central)",
    "ca-west-1": "Canada West (Calgary)",
    "mx-central-1": "Mexico (Central)",
    # South America
    "sa-east-1": "South America (Sao Paulo)",
    # Europe
    "eu-west-1": "Europe (Ireland)",
    "eu-west-2": "Europe (London)",
    "eu-west-3": "Europe (Paris)",
    "eu-central-1": "EU (Frankfurt)",
    "eu-central-2": "Europe (Zurich)",
    "eu-north-1": "Europe (Stockholm)",
    "eu-south-1": "Europe (Milan)",
    "eu-south-2": "Europe (Spain)",
    # Asia Pacific
    "ap-east-1": "Asia Pacific (Hong Kong)",
    "ap-south-1": "Asia Pacific (Mumbai)",
    "ap-south-2": "Asia Pacific (Hyderabad)",
    "ap-northeast-1": "Asia Pacific (Tokyo)",
    "ap-northeast-2": "Asia Pacific (Seoul)",
    "ap-northeast-3": "Asia Pacific (Osaka)",
    "ap-southeast-1": "Asia Pacific (Singapore)",
    "ap-southeast-2": "Asia Pacific (Sydney)",
    "ap-southeast-3": "Asia Pacific (Jakarta)",
    "ap-southeast-4": "Asia Pacific (Melbourne)",
    "ap-southeast-5": "Asia Pacific (Malaysia)",
    # Middle East & Africa
    "me-south-1": "Middle East (Bahrain)",
    "me-central-1": "Middle East (UAE)",
    "af-south-1": "Africa (Cape Town)",
    "il-central-1": "Israel (Tel Aviv)",
    # GovCloud
    "us-gov-east-1": "AWS GovCloud (US-East)",
    "us-gov-west-1": "AWS GovCloud (US)",
}

# Reverse: display name -> region code
AWS_DISPLAY_REGION: dict[str, str] = {v: k for k, v in AWS_REGION_DISPLAY.items()}

# GCP: region code -> human-readable name
GCP_REGION_DISPLAY: dict[str, str] = {
    # Americas
    "us-east1": "US East (South Carolina)",
    "us-east4": "US East (Northern Virginia)",
    "us-east5": "US East (Columbus)",
    "us-central1": "US Central (Iowa)",
    "us-west1": "US West (Oregon)",
    "us-west2": "US West (Los Angeles)",
    "us-west3": "US West (Salt Lake City)",
    "us-west4": "US West (Las Vegas)",
    "us-south1": "US South (Dallas)",
    "northamerica-northeast1": "Canada (Montréal)",
    "northamerica-northeast2": "Canada (Toronto)",
    "northamerica-south1": "Mexico (Querétaro)",
    "southamerica-east1": "South America (São Paulo)",
    "southamerica-west1": "South America (Santiago)",
    # Europe
    "europe-west1": "Europe (Belgium)",
    "europe-west2": "Europe (London)",
    "europe-west3": "Europe (Frankfurt)",
    "europe-west4": "Europe (Netherlands)",
    "europe-west6": "Europe (Zürich)",
    "europe-west8": "Europe (Milan)",
    "europe-west9": "Europe (Paris)",
    "europe-west10": "Europe (Berlin)",
    "europe-west12": "Europe (Turin)",
    "europe-north1": "Europe (Finland)",
    "europe-central2": "Europe (Warsaw)",
    "europe-southwest1": "Europe (Madrid)",
    # Asia Pacific
    "asia-east1": "Asia Pacific (Taiwan)",
    "asia-east2": "Asia Pacific (Hong Kong)",
    "asia-northeast1": "Asia Pacific (Tokyo)",
    "asia-northeast2": "Asia Pacific (Osaka)",
    "asia-northeast3": "Asia Pacific (Seoul)",
    "asia-south1": "Asia Pacific (Mumbai)",
    "asia-south2": "Asia Pacific (Delhi)",
    "asia-southeast1": "Asia Pacific (Singapore)",
    "asia-southeast2": "Asia Pacific (Jakarta)",
    "australia-southeast1": "Australia (Sydney)",
    "australia-southeast2": "Australia (Melbourne)",
    # Middle East & Africa
    "me-west1": "Middle East (Tel Aviv)",
    "me-central1": "Middle East (Doha)",
    "me-central2": "Middle East (Dammam)",
    "africa-south1": "Africa (Johannesburg)",
}

GCP_DISPLAY_REGION: dict[str, str] = {v: k for k, v in GCP_REGION_DISPLAY.items()}

# Azure: ARM region name -> human-readable display name
AZURE_REGION_DISPLAY: dict[str, str] = {
    # North America
    "eastus": "East US",
    "eastus2": "East US 2",
    "westus": "West US",
    "westus2": "West US 2",
    "westus3": "West US 3",
    "centralus": "Central US",
    "northcentralus": "North Central US",
    "southcentralus": "South Central US",
    "westcentralus": "West Central US",
    "canadacentral": "Canada Central",
    "canadaeast": "Canada East",
    # South America
    "brazilsouth": "Brazil South",
    # Europe
    "northeurope": "North Europe",
    "westeurope": "West Europe",
    "uksouth": "UK South",
    "ukwest": "UK West",
    "francecentral": "France Central",
    "germanywestcentral": "Germany West Central",
    "norwayeast": "Norway East",
    "switzerlandnorth": "Switzerland North",
    # Asia Pacific
    "eastasia": "East Asia",
    "southeastasia": "Southeast Asia",
    "japaneast": "Japan East",
    "japanwest": "Japan West",
    "australiaeast": "Australia East",
    "australiasoutheast": "Australia Southeast",
    "centralindia": "Central India",
    "southindia": "South India",
    "westindia": "West India",
    "koreacentral": "Korea Central",
    # Middle East & Africa
    "southafricanorth": "South Africa North",
    "uaenorth": "UAE North",
}

AZURE_DISPLAY_REGION: dict[str, str] = {v: k for k, v in AZURE_REGION_DISPLAY.items()}


def aws_region_to_display(region_code: str) -> str:
    """Convert AWS region code to the display name used by the Pricing API."""
    display = AWS_REGION_DISPLAY.get(region_code)
    if display is None:
        raise ValueError(
            f"Unknown AWS region code: {region_code!r}. "
            f"Known regions: {sorted(AWS_REGION_DISPLAY)}"
        )
    return display


def aws_display_to_region(display_name: str) -> str:
    """Convert AWS Pricing API display name back to region code."""
    code = AWS_DISPLAY_REGION.get(display_name)
    if code is None:
        raise ValueError(f"Unknown AWS region display name: {display_name!r}")
    return code


def normalize_region(provider: str, value: str) -> str:
    """
    Accept either a region code or display name and always return the region code.
    Useful when tools receive user input that might be either format.
    """
    if provider == "aws":
        if value in AWS_REGION_DISPLAY:
            return value  # already a code
        if value in AWS_DISPLAY_REGION:
            return AWS_DISPLAY_REGION[value]
        raise ValueError(f"Unknown AWS region: {value!r}")
    if provider == "gcp":
        if value in GCP_REGION_DISPLAY:
            return value
        if value in GCP_DISPLAY_REGION:
            return GCP_DISPLAY_REGION[value]
        raise ValueError(f"Unknown GCP region: {value!r}")
    if provider == "azure":
        if value in AZURE_REGION_DISPLAY:
            return value
        if value in AZURE_DISPLAY_REGION:
            return AZURE_DISPLAY_REGION[value]
        raise ValueError(f"Unknown Azure region: {value!r}")
    return value


def list_aws_regions() -> list[str]:
    return sorted(AWS_REGION_DISPLAY.keys())


def list_gcp_regions() -> list[str]:
    return sorted(GCP_REGION_DISPLAY.keys())


def list_azure_regions() -> list[str]:
    return sorted(AZURE_REGION_DISPLAY.keys())


def region_display_name(provider: str, region_code: str) -> str:
    """Return the friendly display name for a region code, or the code itself if unknown."""
    if provider == "aws":
        return AWS_REGION_DISPLAY.get(region_code, region_code)
    if provider == "gcp":
        return GCP_REGION_DISPLAY.get(region_code, region_code)
    if provider == "azure":
        return AZURE_REGION_DISPLAY.get(region_code, region_code)
    return region_code
