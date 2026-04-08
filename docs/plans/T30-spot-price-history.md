# T30 · AWS spot price history tool

**Status:** pending  
**Depends on:** T18 (basic spot pricing)  
**Branch:** task/T30-spot-history

## Overview
T18 adds the current spot price. T30 adds a `get_spot_history` tool that provides historical context — price over the last N hours, min/max/avg, and a stability score. This helps LLMs answer "is this instance type suitable for spot?" questions.

## Files to change
- `src/opencloudcosts/providers/aws.py` — `_get_spot_history()` method
- `src/opencloudcosts/tools/lookup.py` — `get_spot_history` tool
- `docs/tools.md` — add tool under Pricing Lookup

## Tool signature
```python
async def get_spot_history(
    ctx: Context,
    provider: str,              # "aws"
    instance_type: str,         # "m5.xlarge"
    region: str,                # "us-east-1"
    availability_zone: str = "", # filter to specific AZ; empty = all AZs
    os: str = "Linux",
    hours: int = 24,            # lookback window (max 720 = 30 days)
) -> dict[str, Any]:
```

## AWS implementation

### `_get_spot_history()` (aws.py)
```python
async def _get_spot_history(
    self, instance_type: str, region: str, os: str = "Linux",
    availability_zone: str = "", hours: int = 24
) -> dict:
    from datetime import datetime, timezone, timedelta
    
    start_time = datetime.now(timezone.utc) - timedelta(hours=hours)
    product_desc = "Linux/UNIX" if os == "Linux" else "Windows"
    
    kwargs = {
        "InstanceTypes": [instance_type],
        "ProductDescriptions": [product_desc],
        "StartTime": start_time,
        "MaxResults": 1000,
    }
    if availability_zone:
        kwargs["AvailabilityZone"] = availability_zone
    
    ec2 = boto3.client("ec2", region_name=region)
    items = []
    paginator = ec2.get_paginator("describe_spot_price_history")
    async for page in asyncio.to_thread(lambda: list(paginator.paginate(**kwargs))):
        items.extend(page["SpotPriceHistory"])
    
    return _compute_spot_stats(items, instance_type, region, hours)
```

### `_compute_spot_stats()` helper
```python
def _compute_spot_stats(items, instance_type, region, hours):
    if not items:
        return {"error": f"No spot price history for {instance_type} in {region}"}
    
    # Group by AZ
    by_az: dict[str, list[Decimal]] = defaultdict(list)
    for item in items:
        az = item["AvailabilityZone"]
        by_az[az].append(Decimal(item["SpotPrice"]))
    
    az_stats = {}
    all_prices = []
    for az, prices in by_az.items():
        all_prices.extend(prices)
        avg = sum(prices) / len(prices)
        stddev = (sum((p - avg) ** 2 for p in prices) / len(prices)) ** Decimal("0.5")
        az_stats[az] = {
            "current": str(prices[0]),   # most recent (API returns newest first)
            "min": str(min(prices)),
            "max": str(max(prices)),
            "avg": f"{avg:.6f}",
            "stddev": f"{stddev:.6f}",
            "sample_count": len(prices),
        }
    
    # Overall stats
    overall_avg = sum(all_prices) / len(all_prices)
    overall_std = (sum((p - overall_avg) ** 2 for p in all_prices) / len(all_prices)) ** Decimal("0.5")
    volatility_ratio = overall_std / overall_avg if overall_avg > 0 else Decimal("0")
    
    if volatility_ratio < Decimal("0.05"):
        stability = "stable"
    elif volatility_ratio < Decimal("0.15"):
        stability = "moderate"
    else:
        stability = "volatile"
    
    return {
        "instance_type": instance_type,
        "region": region,
        "os": os,
        "lookback_hours": hours,
        "stability": stability,
        "volatility_ratio": f"{volatility_ratio:.4f}",
        "overall": {
            "min": f"${min(all_prices):.6f}",
            "max": f"${max(all_prices):.6f}",
            "avg": f"${overall_avg:.6f}",
        },
        "by_availability_zone": az_stats,
        "recommendation": _spot_recommendation(stability, overall_avg),
    }

def _spot_recommendation(stability: str, avg_price: Decimal) -> str:
    if stability == "stable":
        return "Low interruption risk. Good candidate for fault-tolerant batch workloads."
    elif stability == "moderate":
        return "Moderate price variation. Use with checkpointing for long-running jobs."
    else:
        return "High volatility. Consider on-demand or reserved instances for reliable workloads."
```

## Response example
```json
{
  "instance_type": "m5.xlarge",
  "region": "us-east-1",
  "os": "Linux",
  "lookback_hours": 24,
  "stability": "stable",
  "volatility_ratio": "0.0123",
  "overall": {
    "min": "$0.038000",
    "max": "$0.042000",
    "avg": "$0.039500"
  },
  "by_availability_zone": {
    "us-east-1a": {
      "current": "$0.039000",
      "min": "$0.038000",
      "max": "$0.041000",
      "avg": "$0.039200",
      "stddev": "0.000800",
      "sample_count": 12
    },
    "us-east-1b": { ... }
  },
  "recommendation": "Low interruption risk. Good candidate for fault-tolerant batch workloads."
}
```

## docs/tools.md entry
Add under **Pricing Lookup**:

### `get_spot_history`
Get spot price history and stability analysis for an AWS instance type.  
**Requires:** AWS credentials.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"` |
| `instance_type` | string | ✓ | e.g. `"m5.xlarge"` |
| `region` | string | ✓ | Region code |
| `availability_zone` | string | | Filter to specific AZ (e.g. `"us-east-1a"`). Empty = all AZs. |
| `os` | string | | `"Linux"` (default) or `"Windows"` |
| `hours` | int | | Lookback window in hours (default `24`, max `720`) |

## Tests
```python
_SPOT_HISTORY = {
    "SpotPriceHistory": [
        {"AvailabilityZone": "us-east-1a", "SpotPrice": "0.039", "Timestamp": "..."},
        {"AvailabilityZone": "us-east-1a", "SpotPrice": "0.040", "Timestamp": "..."},
        {"AvailabilityZone": "us-east-1b", "SpotPrice": "0.038", "Timestamp": "..."},
    ]
}

async def test_get_spot_history_returns_stats(aws_provider):
    with patch("boto3.client") as mock_ec2:
        mock_ec2.return_value.get_paginator.return_value.paginate.return_value = [_SPOT_HISTORY]
        result = await aws_provider._get_spot_history("m5.xlarge", "us-east-1")
    
    assert "stability" in result
    assert "by_availability_zone" in result
    assert "us-east-1a" in result["by_availability_zone"]
    assert result["overall"]["min"] == "$0.038000"

async def test_get_spot_history_stable_label(aws_provider):
    # Feed prices with <5% variation → stability="stable"

async def test_get_spot_history_volatile_label(aws_provider):
    # Feed prices with >15% variation → stability="volatile"
```

## Acceptance criteria
- Returns per-AZ stats: current, min, max, avg, stddev, sample count
- Returns overall stats and volatility_ratio
- `stability` label: `stable` / `moderate` / `volatile`
- `recommendation` field with actionable guidance
- `hours` param controls lookback window
- No credentials → clear error (not empty dict)
