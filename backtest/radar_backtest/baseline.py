"""Buy and hold one ticker: the benchmark run, and the engine's sanity check."""

from __future__ import annotations

from .data import PointInTime


class BuyAndHold:
    """Asks for 100% every day. Once invested the engine sees nothing worth
    trading (the gap is under min_trade_value), but if the first buy could not
    happen (no liquidity history yet, or no price) it is retried next day."""

    def __init__(self, ticker: str):
        self.ticker = ticker
        self.name = f"buy_and_hold_{ticker}"
        self.params = {"ticker": ticker}

    def targets(self, view: PointInTime) -> dict[str, float] | None:
        return {self.ticker: 1.0}
