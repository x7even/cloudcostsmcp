# T18 · AWS spot pricing via EC2 SpotPrice API

**Status:** pending  
**Branch:** task/T18-aws-spot-pricing

## Problem
`get_compute_price(term="spot")` is documented and the `PricingTerm.SPOT` enum value exists, but the AWS provider has no implementation. The bulk pricing API doesn't include spot prices — they're time-varying and require the EC2 `DescribeSpotPriceHistory` API.

## Files to change
- `src/opencloudcosts/providers/aws.py` — add `_get_spot_price()`, route SPOT term
- `tests/test_providers/test_aws.py` — add spot pricing tests

## Design decisions

### Credentials required
Spot pricing requires AWS credentials (no public bulk endpoint). When credentials are absent, return:
```json
{"error": "Spot pricing requires AWS credentials. Set AWS_PROFILE or AWS_ACCESS_KEY_ID. Public pricing is not available for spot instances."}
```
Do NOT return an empty list silently.

### AZ handling
`DescribeSpotPriceHistory` returns prices per Availability Zone (e.g. `us-east-1a`, `us-east-1b`). Strategy: return the cheapest AZ price as the representative price, and include the AZ in the `attributes` dict.

### Caching
Spot prices change frequently. Cache with a short TTL (5 minutes) vs the 24-hour TTL used for on-demand. Add a `spot_cache_ttl_minutes` setting (default 5) to `config.py`.

## Implementation

### 1. `_get_spot_price()` method (aws.py)
```python
async def _get_spot_price(
    self, instance_type: str, region: str, os: str = "Linux"
) -> list[NormalizedPrice]:
    product_desc = "Linux/UNIX" if os == "Linux" else "Windows"
    try:
        ec2 = await asyncio.to_thread(
            boto3.client, "ec2", region_name=region
        )
        resp = await asyncio.to_thread(
            ec2.describe_spot_price_history,
            InstanceTypes=[instance_type],
            ProductDescriptions=[product_desc],
            MaxResults=10,
        )
    except (NoCredentialsError, PartialCredentialsError):
        raise ValueError(
            "Spot pricing requires AWS credentials. "
            "Set AWS_PROFILE or AWS_ACCESS_KEY_ID."
        )
    
    items = resp.get("SpotPriceHistory", [])
    if not items:
        return []
    
    # Pick most recent price per AZ, then take cheapest AZ
    by_az: dict[str, Decimal] = {}
    for item in items:
        az = item["AvailabilityZone"]
        price = Decimal(item["SpotPrice"])
        if az not in by_az or price < by_az[az]:
            by_az[az] = price
    
    cheapest_az = min(by_az, key=lambda az: by_az[az])
    price = by_az[cheapest_az]
    
    return [NormalizedPrice(
        provider=CloudProvider.AWS,
        service="compute",
        region=region,
        pricing_term=PricingTerm.SPOT,
        price_per_unit=price,
        unit=PriceUnit.PER_HOUR,
        description=f"{instance_type} spot ({cheapest_az})",
        attributes={
            "instanceType": instance_type,
            "availabilityZone": cheapest_az,
            "allAZPrices": {az: str(p) for az, p in by_az.items()},
            "operatingSystem": os,
        },
    )]
```

### 2. Route SPOT in `get_compute_price()` (aws.py)
In `get_compute_price()`, before the existing term routing:
```python
if pricing_term == PricingTerm.SPOT:
    return await self._get_spot_price(instance_type, region, os)
```

### 3. Cache key
Use a short TTL for spot prices. Add to `config.py`:
```python
spot_cache_ttl_minutes: int = Field(default=5)
```
In `_get_spot_price()`, check/set cache with TTL = `settings.spot_cache_ttl_minutes * 60`.

## Tests to add
```python
_SPOT_PRICE_HISTORY_RESPONSE = {
    "SpotPriceHistory": [
        {"AvailabilityZone": "us-east-1a", "SpotPrice": "0.0420", "InstanceType": "m5.xlarge"},
        {"AvailabilityZone": "us-east-1b", "SpotPrice": "0.0380", "InstanceType": "m5.xlarge"},
    ]
}

async def test_get_spot_price(aws_provider):
    with patch("boto3.client") as mock_client:
        mock_client.return_value.describe_spot_price_history.return_value = _SPOT_PRICE_HISTORY_RESPONSE
        prices = await aws_provider.get_compute_price("m5.xlarge", "us-east-1", term=PricingTerm.SPOT)
    assert len(prices) == 1
    assert prices[0].price_per_unit == Decimal("0.0380")  # cheapest AZ
    assert prices[0].attributes["availabilityZone"] == "us-east-1b"

async def test_get_spot_price_no_credentials(aws_provider):
    import botocore.exceptions
    with patch("boto3.client", side_effect=botocore.exceptions.NoCredentialsError()):
        with pytest.raises(ValueError, match="Spot pricing requires AWS credentials"):
            await aws_provider.get_compute_price("m5.xlarge", "us-east-1", term=PricingTerm.SPOT)
```

## Acceptance criteria
- `get_compute_price(term="spot")` returns current spot price for the cheapest AZ
- No credentials → clear error (not empty list, not silent failure)
- `allAZPrices` in attributes shows all AZ prices
- Spot prices cached for 5 minutes (configurable)
- `get_prices_batch(term="spot")` works (passes through automatically)
