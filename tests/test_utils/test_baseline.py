"""Tests for opencloudcosts.utils.baseline."""

from __future__ import annotations

import pytest

from opencloudcosts.utils.baseline import apply_baseline_deltas


def _make_results(entries):
    """Build result dicts from (region, price_per_hour, monthly_estimate) tuples."""
    return [
        {
            "region": region,
            "price_per_hour": price_per_hour,
            "monthly_estimate": monthly_estimate,
        }
        for region, price_per_hour, monthly_estimate in entries
    ]


# ---------------------------------------------------------------------------
# Basic delta computation
# ---------------------------------------------------------------------------


def test_baseline_region_shows_zero_delta():
    results = _make_results(
        [
            ("us-east-1", "$0.200000/hr", "$146.00/mo"),
            ("eu-west-1", "$0.230000/hr", "$167.90/mo"),
        ]
    )
    apply_baseline_deltas(results, "us-east-1")
    base = next(r for r in results if r["region"] == "us-east-1")
    assert base["delta_per_hour"] == "$+0.000000"
    assert base["delta_monthly"] == "$+0.00/mo"
    assert base["delta_pct"] == "+0.0%"


def test_delta_increase():
    results = _make_results(
        [
            ("us-east-1", "$0.200000/hr", "$146.00/mo"),
            ("eu-west-1", "$0.260000/hr", "$189.80/mo"),
        ]
    )
    apply_baseline_deltas(results, "us-east-1")
    eu = next(r for r in results if r["region"] == "eu-west-1")
    assert eu["delta_per_hour"] == "$+0.060000"
    assert eu["delta_monthly"] == "$+43.80/mo"
    assert eu["delta_pct"] == "+30.0%"


def test_delta_decrease():
    results = _make_results(
        [
            ("us-east-1", "$0.200000/hr", "$146.00/mo"),
            ("ap-southeast-1", "$0.160000/hr", "$116.80/mo"),
        ]
    )
    apply_baseline_deltas(results, "us-east-1")
    ap = next(r for r in results if r["region"] == "ap-southeast-1")
    assert ap["delta_per_hour"] == "$-0.040000"
    assert ap["delta_monthly"] == "$-29.20/mo"
    assert ap["delta_pct"] == "-20.0%"


def test_delta_unchanged_non_baseline_same_price():
    results = _make_results(
        [
            ("us-east-1", "$0.200000/hr", "$146.00/mo"),
            ("us-west-2", "$0.200000/hr", "$146.00/mo"),
        ]
    )
    apply_baseline_deltas(results, "us-east-1")
    west = next(r for r in results if r["region"] == "us-west-2")
    assert west["delta_per_hour"] == "$+0.000000"
    assert west["delta_pct"] == "+0.0%"


# ---------------------------------------------------------------------------
# Edge cases
# ---------------------------------------------------------------------------


def test_zero_baseline_price_pct_is_zero():
    """When base price is 0, pct must default to 0.0 to avoid division by zero."""
    results = _make_results(
        [
            ("us-east-1", "$0.000000/hr", "$0.00/mo"),
            ("eu-west-1", "$0.100000/hr", "$73.00/mo"),
        ]
    )
    apply_baseline_deltas(results, "us-east-1")
    eu = next(r for r in results if r["region"] == "eu-west-1")
    assert eu["delta_pct"] == "+0.0%"


def test_baseline_region_not_found_raises():
    results = _make_results(
        [
            ("us-east-1", "$0.200000/hr", "$146.00/mo"),
        ]
    )
    with pytest.raises(ValueError, match="not found in results"):
        apply_baseline_deltas(results, "ap-northeast-1")


def test_error_message_includes_available_regions():
    results = _make_results(
        [
            ("us-east-1", "$0.200000/hr", "$146.00/mo"),
            ("eu-west-1", "$0.230000/hr", "$167.90/mo"),
        ]
    )
    with pytest.raises(ValueError, match="us-east-1"):
        apply_baseline_deltas(results, "missing-region")


def test_custom_price_keys():
    """apply_baseline_deltas must honour custom hourly_key / monthly_key."""
    results = [
        {"region": "us-east-1", "cost_hr": "$0.100000/hr", "cost_mo": "$73.00/mo"},
        {"region": "eu-west-1", "cost_hr": "$0.150000/hr", "cost_mo": "$109.50/mo"},
    ]
    apply_baseline_deltas(results, "us-east-1", hourly_key="cost_hr", monthly_key="cost_mo")
    eu = next(r for r in results if r["region"] == "eu-west-1")
    assert eu["delta_per_hour"] == "$+0.050000"
    assert eu["delta_pct"] == "+50.0%"


def test_dict_price_format():
    """Prices given as {'amount': ..., ...} dicts (from _price()) must parse correctly."""
    results = [
        {
            "region": "us-east-1",
            "price_per_hour": {
                "amount": 0.2,
                "unit": "hr",
                "currency": "USD",
                "display": "$0.200000/hr",
            },
            "monthly_estimate": {"amount": 146.0, "currency": "USD", "display": "$146.00/mo"},
        },
        {
            "region": "eu-west-1",
            "price_per_hour": {
                "amount": 0.25,
                "unit": "hr",
                "currency": "USD",
                "display": "$0.250000/hr",
            },
            "monthly_estimate": {"amount": 182.5, "currency": "USD", "display": "$182.50/mo"},
        },
    ]
    apply_baseline_deltas(results, "us-east-1")
    eu = next(r for r in results if r["region"] == "eu-west-1")
    assert eu["delta_per_hour"] == "$+0.050000"
    assert eu["delta_monthly"] == "$+36.50/mo"
    assert eu["delta_pct"] == "+25.0%"


def test_multiple_regions_all_get_delta_fields():
    results = _make_results(
        [
            ("us-east-1", "$0.200000/hr", "$146.00/mo"),
            ("us-west-2", "$0.210000/hr", "$153.30/mo"),
            ("eu-west-1", "$0.230000/hr", "$167.90/mo"),
            ("ap-southeast-1", "$0.180000/hr", "$131.40/mo"),
        ]
    )
    apply_baseline_deltas(results, "us-east-1")
    for r in results:
        assert "delta_per_hour" in r
        assert "delta_monthly" in r
        assert "delta_pct" in r
