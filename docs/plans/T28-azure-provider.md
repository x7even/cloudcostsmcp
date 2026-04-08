# T28 · Azure provider (compute + storage public pricing)

**Status:** pending  
**Branch:** task/T28-azure-provider

## Overview
Add Azure as a third provider. The Azure Retail Prices API is fully public (no credentials) — similar to AWS bulk pricing. This makes it the easiest provider to add.

## API reference
```
GET https://prices.azure.com/api/retail/prices
    ?api-version=2023-01-01-preview
    &$filter=armRegionName eq 'eastus' and armSkuName eq 'Standard_D4s_v3' and priceType eq 'Consumption'
    &$top=100
```

Response:
```json
{
  "Items": [
    {
      "retailPrice": 0.192,
      "unitPrice": 0.192,
      "armRegionName": "eastus",
      "location": "US East",
      "effectiveStartDate": "2021-09-01T00:00:00Z",
      "meterId": "...",
      "meterName": "D4s v3",
      "productName": "Virtual Machines DSv3 Series",
      "skuName": "D4s v3",
      "serviceName": "Virtual Machines",
      "serviceFamily": "Compute",
      "unitOfMeasure": "1 Hour",
      "type": "Consumption",
      "isPrimaryMeterRegion": true,
      "armSkuName": "Standard_D4s_v3",
      "currencyCode": "USD"
    }
  ],
  "NextPageLink": "..."
}
```

## Instance type format
Azure uses ARM SKU names: `Standard_D4s_v3`, `Standard_E8s_v3`, `Standard_B2ms`, `Standard_NC6s_v3` (GPU).

## Region format
ARM region names (lowercase, no spaces): `eastus`, `westus2`, `westeurope`, `southeastasia`, `australiaeast`.

## Files to create/change
- `src/opencloudcosts/providers/azure.py` — new provider
- `src/opencloudcosts/server.py` — register AzureProvider
- `src/opencloudcosts/config.py` — no new config needed (public API)
- `src/opencloudcosts/utils/regions.py` — add `AZURE_REGION_DISPLAY` dict
- `docs/tools.md` — add Azure to provider notes
- `docs/finops-guide.md` — add 3-way comparison section
- `README.md` — add Azure to provider list
- `tests/test_providers/test_azure.py` — new test file

## CloudProvider interface methods to implement

### `get_compute_price(instance_type, region, os, term)`
Filter: `armSkuName eq '{instance_type}' and armRegionName eq '{region}'`
- On-demand: `priceType eq 'Consumption'`
- Reserved 1yr: `priceType eq 'Reservation' and reservationTerm eq '1 Year'`
- Reserved 3yr: `priceType eq 'Reservation' and reservationTerm eq '3 Years'`
- Spot: `priceType eq 'Consumption'` + `skuName contains 'Spot'`
- Windows: add `and productName contains 'Windows'`

### `get_storage_price(storage_type, region, size_gb)`
Map storage types:
| Input | Azure productName filter |
|-------|--------------------------|
| `premium-ssd` | `"Premium SSD Managed Disks"` |
| `standard-ssd` | `"Standard SSD Managed Disks"` |
| `standard-hdd` | `"Standard HDD Managed Disks"` |
| `ultra-ssd` | `"Ultra Disks"` |
| `blob` | `"Blob Storage"` |

### `list_regions(service)`
Fetch all `armRegionName` values from a sample query, deduplicate.
Or use a hardcoded list from Azure docs (more reliable). Cache result.

### `list_instance_types(region, family, min_vcpus, min_memory_gb, gpu)`
Filter by `armRegionName eq '{region}' and priceType eq 'Consumption' and serviceName eq 'Virtual Machines'`.
Extract vCPU/memory from instance type metadata (requires a separate API call or local mapping file).
For MVP: return instance names without vCPU/memory if the metadata API is complex.

### `check_availability(service, sku_or_type, region)`
Attempt price lookup; return `True` if results found, `False` otherwise.

### `search_pricing(query, region, service_code, max_results)`
Use `meterName contains '{query}' or productName contains '{query}'`.

## Key mapping needed
Azure instance metadata (vCPU/memory) is NOT in the Retail Prices API. Options:
1. Call `https://prices.azure.com/api/retail/prices?$filter=armSkuName eq '...'` and derive from skuName pattern (unreliable)
2. Hardcode a common instance family table for popular types
3. Call the Azure Compute List VM Sizes API (requires subscription ID — skip for Phase 5)

For MVP: return instance type name without vCPU/memory from `list_instance_types`. Document the limitation.

## `AZURE_REGION_DISPLAY` dict (utils/regions.py)
```python
AZURE_REGION_DISPLAY = {
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
    "brazilsouth": "Brazil South",
    "northeurope": "North Europe",
    "westeurope": "West Europe",
    "uksouth": "UK South",
    "ukwest": "UK West",
    "francecentral": "France Central",
    "germanywestcentral": "Germany West Central",
    "norwayeast": "Norway East",
    "switzerlandnorth": "Switzerland North",
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
    "southafricanorth": "South Africa North",
    "uaenorth": "UAE North",
}
```

## docs/tools.md additions
Under **Provider Notes**, add an Azure section:
- Instance types: `Standard_D4s_v3`, `Standard_E8s_v3`, etc.
- Regions: `eastus`, `westeurope`, `southeastasia`, etc.
- Public pricing — no credentials needed.
- Storage types: `premium-ssd`, `standard-ssd`, `standard-hdd`, `ultra-ssd`, `blob`.

## docs/finops-guide.md addition
New section: **AWS vs GCP vs Azure 3-way comparison**
```
get_compute_price(provider="aws", instance_type="m5.xlarge", region="us-east-1")
get_compute_price(provider="gcp", instance_type="n2-standard-4", region="us-central1")
get_compute_price(provider="azure", instance_type="Standard_D4s_v3", region="eastus")
```

## Tests
```python
_AZURE_COMPUTE_RESPONSE = {
    "Items": [{
        "retailPrice": 0.192,
        "unitPrice": 0.192,
        "armRegionName": "eastus",
        "armSkuName": "Standard_D4s_v3",
        "productName": "Virtual Machines DSv3 Series",
        "skuName": "D4s v3",
        "unitOfMeasure": "1 Hour",
        "type": "Consumption",
        "currencyCode": "USD",
    }]
}

async def test_azure_get_compute_price():
    with patch("httpx.get", return_value=mock_response(_AZURE_COMPUTE_RESPONSE)):
        prices = await azure_provider.get_compute_price("Standard_D4s_v3", "eastus")
    assert len(prices) == 1
    assert prices[0].price_per_unit == Decimal("0.192")

async def test_azure_get_compute_price_reserved():
    # priceType=Reservation, reservationTerm=1 Year
    ...

async def test_azure_search_pricing():
    ...
```

## Acceptance criteria
- All CloudProvider interface methods implemented
- `get_compute_price` works for on-demand, reserved 1yr, reserved 3yr
- `get_storage_price` works for premium-ssd, standard-ssd, blob
- `list_regions` returns at least 20 Azure regions
- All existing AWS and GCP tests still pass
- Azure provider registered in server.py and accessible via `provider="azure"`
