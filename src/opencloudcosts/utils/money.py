from __future__ import annotations

from decimal import Decimal
from typing import Any


def _price(amount: Decimal, unit: str) -> dict[str, Any]:
    """Structured price dict with embedded display string."""
    amt = float(amount)
    if 0 < amount < Decimal("0.0000005"):
        display = f"${amt:.2e}/{unit}"
    else:
        display = f"${amount:.6f}/{unit}"
    return {"amount": amt, "unit": unit, "currency": "USD", "display": display}


def _money(amount: Decimal, label: str = "") -> dict[str, Any]:
    """Structured aggregate money value (monthly costs, totals) with display string."""
    return {"amount": float(amount), "currency": "USD", "display": f"${amount:.2f}{label}"}
