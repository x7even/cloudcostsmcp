"""Tests for opencloudcosts.utils.money."""

from __future__ import annotations

from decimal import Decimal

import pytest

from opencloudcosts.utils.money import _money, _price

# ---------------------------------------------------------------------------
# _price
# ---------------------------------------------------------------------------


def test_price_returns_dict_with_required_keys():
    result = _price(Decimal("0.200000"), "hr")
    assert set(result.keys()) == {"amount", "unit", "currency", "display"}


def test_price_currency_is_usd():
    result = _price(Decimal("0.1"), "hr")
    assert result["currency"] == "USD"


def test_price_amount_as_float():
    result = _price(Decimal("0.123456"), "hr")
    assert result["amount"] == pytest.approx(0.123456)


def test_price_unit_preserved():
    result = _price(Decimal("1.5"), "GB")
    assert result["unit"] == "GB"


def test_price_normal_display_format():
    result = _price(Decimal("0.200000"), "hr")
    assert result["display"] == "$0.200000/hr"


def test_price_normal_display_six_decimal_places():
    result = _price(Decimal("0.001234"), "hr")
    assert result["display"] == "$0.001234/hr"


def test_price_tiny_value_uses_scientific_notation():
    """Values below 0.0000005 should render in scientific notation."""
    result = _price(Decimal("0.0000001"), "GB")
    assert "e" in result["display"] or "E" in result["display"]


def test_price_zero_uses_fixed_notation():
    """Zero should use fixed notation (not sci notation, since 0 < amount is False)."""
    result = _price(Decimal("0"), "hr")
    assert "e" not in result["display"].lower()
    assert result["display"] == "$0.000000/hr"


def test_price_boundary_just_above_tiny():
    """0.0000005 is NOT below threshold, should use fixed notation."""
    result = _price(Decimal("0.0000005"), "hr")
    assert "e" not in result["display"].lower()


def test_price_boundary_just_below_tiny():
    """0.0000004 IS below threshold, should use scientific notation."""
    result = _price(Decimal("0.0000004"), "hr")
    assert "e" in result["display"].lower()


def test_price_large_value():
    result = _price(Decimal("1234.567890"), "mo")
    assert result["display"] == "$1234.567890/mo"
    assert result["amount"] == pytest.approx(1234.56789)


# ---------------------------------------------------------------------------
# _money
# ---------------------------------------------------------------------------


def test_money_returns_dict_with_required_keys():
    result = _money(Decimal("146.00"))
    assert set(result.keys()) == {"amount", "currency", "display"}


def test_money_currency_is_usd():
    result = _money(Decimal("50.00"))
    assert result["currency"] == "USD"


def test_money_amount_as_float():
    result = _money(Decimal("99.99"))
    assert result["amount"] == pytest.approx(99.99)


def test_money_display_two_decimal_places():
    result = _money(Decimal("146.00"))
    assert result["display"] == "$146.00"


def test_money_display_with_label():
    result = _money(Decimal("146.00"), "/mo")
    assert result["display"] == "$146.00/mo"


def test_money_zero_amount():
    result = _money(Decimal("0"))
    assert result["display"] == "$0.00"
    assert result["amount"] == 0.0


def test_money_label_empty_string():
    result = _money(Decimal("10.50"), "")
    assert result["display"] == "$10.50"


def test_money_rounds_display_to_two_decimals():
    """display must always show exactly 2 decimal places."""
    result = _money(Decimal("9.9"))
    assert result["display"] == "$9.90"


def test_money_large_value():
    result = _money(Decimal("10000.00"), "/yr")
    assert result["display"] == "$10000.00/yr"
    assert result["amount"] == pytest.approx(10000.0)
