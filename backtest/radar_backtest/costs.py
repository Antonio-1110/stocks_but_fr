"""Taiwan trading costs, rounded the way brokers and the tax office do."""

from __future__ import annotations

import math

from .config import Costs


def commission(costs: Costs, value: float) -> float:
    """Broker fee for one order. Mirrors TWCosts.Commission in internal/config."""
    if value <= 0:
        return 0.0
    fee = math.floor(value * costs.commission_rate * costs.broker_discount)
    return float(max(fee, costs.min_commission))


def sell_tax(costs: Costs, value: float, etf: bool) -> float:
    """Securities transaction tax on a sell, floored to whole NTD."""
    if value <= 0:
        return 0.0
    return float(math.floor(value * (costs.etf_sell_tax if etf else costs.sell_tax)))


def is_etf(ticker: str, industry: str = "") -> bool:
    """Taiwan ETF codes start with "00" (0050, 00878); FinMind also tags them."""
    return ticker.startswith("00") or "ETF" in industry.upper()
