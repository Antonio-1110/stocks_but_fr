"""Reads the SQLite file the Go collectors write (schema: internal/store).

Strategies never touch the database or the full tables. Each day they get a
PointInTime view that only holds what was public by that day's close.
"""

from __future__ import annotations

import sqlite3
from dataclasses import dataclass
from pathlib import Path

import pandas as pd

MARKET = "TW"


@dataclass
class MarketData:
    """Wide price frames (date x ticker) plus the long fundamental tables."""

    open: pd.DataFrame        # raw open
    close: pd.DataFrame       # raw close
    adj_open: pd.DataFrame    # open on the dividend/split-adjusted scale
    adj_close: pd.DataFrame
    turnover: pd.DataFrame    # traded value, NTD
    volume: pd.DataFrame      # shares
    companies: pd.DataFrame   # ticker, name, industry, exchange, listed_on, delisted_on
    revenue: pd.DataFrame     # ticker, month, revenue, yoy_pct, mom_pct, announced_on
    flows: pd.DataFrame       # ticker, date, foreign_net, trust_net, dealer_net

    @property
    def calendar(self) -> pd.DatetimeIndex:
        return self.close.index

    def last_traded(self) -> pd.Series:
        """Last date each ticker has a price; later than that it is delisted."""
        return self.close.apply(lambda s: s.last_valid_index())

    def view(self, day: pd.Timestamp) -> "PointInTime":
        return PointInTime(self, day)


class PointInTime:
    """What a strategy may know at the close of `day`.

    Prices up to and including `day`; monthly revenue only from the trading day
    after its announced_on date (the Go collector stores the legal filing
    deadline there); institutional flows up to `day` (published after close).
    """

    def __init__(self, data: MarketData, day: pd.Timestamp):
        self._d = data
        self.day = day

    def _upto(self, frame: pd.DataFrame) -> pd.DataFrame:
        return frame.loc[: self.day]

    @property
    def close(self) -> pd.DataFrame:
        return self._upto(self._d.close)

    @property
    def adj_close(self) -> pd.DataFrame:
        return self._upto(self._d.adj_close)

    @property
    def open(self) -> pd.DataFrame:
        return self._upto(self._d.open)

    @property
    def turnover(self) -> pd.DataFrame:
        return self._upto(self._d.turnover)

    @property
    def volume(self) -> pd.DataFrame:
        return self._upto(self._d.volume)

    def revenue(self) -> pd.DataFrame:
        r = self._d.revenue
        return r[r["announced_on"] < self.day]

    def flows(self) -> pd.DataFrame:
        f = self._d.flows
        return f[f["date"] <= self.day]

    def companies(self) -> pd.DataFrame:
        """Companies listed on `day` (delisted ones included before delisting)."""
        c = self._d.companies
        listed = c["listed_on"].isna() | (c["listed_on"] <= self.day)
        alive = c["delisted_on"].isna() | (c["delisted_on"] > self.day)
        return c[listed & alive]

    def traded_today(self) -> pd.Index:
        row = self._d.close.loc[self.day]
        return row.index[row.notna()]


def _date(col: pd.Series) -> pd.Series:
    """'' means unknown in the Go schema."""
    return pd.to_datetime(col.replace("", None), format="%Y-%m-%d")


def load(db_path: str | Path, tickers: list[str] | None = None) -> MarketData:
    """Load Taiwan data. `tickers` limits the price load (tests, quick runs)."""
    db_path = Path(db_path)
    if not db_path.exists():
        raise FileNotFoundError(f"{db_path} not found: run `radar collect` first")
    con = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        where, args = "market = ?", [MARKET]
        if tickers:
            where += f" AND ticker IN ({','.join('?' * len(tickers))})"
            args += tickers
        prices = pd.read_sql_query(
            f"SELECT ticker, date, open, close, adj_close, volume, turnover FROM prices WHERE {where}",
            con, params=args)
        companies = pd.read_sql_query(
            "SELECT ticker, name, industry, exchange, listed_on, delisted_on FROM companies WHERE market = ?",
            con, params=[MARKET])
        revenue = pd.read_sql_query(
            "SELECT ticker, month, revenue, yoy_pct, mom_pct, announced_on FROM monthly_revenue WHERE market = ?",
            con, params=[MARKET])
        flows = pd.read_sql_query(
            "SELECT ticker, date, foreign_net, trust_net, dealer_net FROM institutional_flows WHERE market = ?",
            con, params=[MARKET])
    finally:
        con.close()

    if prices.empty:
        raise ValueError(f"{db_path} has no Taiwan prices")
    prices["date"] = _date(prices["date"])
    # Suspended or broken rows: no usable close means no trade that day.
    prices = prices[prices["close"] > 0]
    wide = {c: prices.pivot(index="date", columns="ticker", values=c).sort_index()
            for c in ("open", "close", "adj_close", "volume", "turnover")}
    adj_close = wide["adj_close"].where(wide["adj_close"] > 0, wide["close"])
    factor = adj_close / wide["close"]
    open_ = wide["open"].where(wide["open"] > 0, wide["close"])

    for col in ("listed_on", "delisted_on"):
        companies[col] = _date(companies[col])
    revenue["month"] = _date(revenue["month"])
    revenue["announced_on"] = _date(revenue["announced_on"])
    # Rows with no announcement date can't be placed in time: never use them.
    revenue = revenue[revenue["announced_on"].notna()].reset_index(drop=True)
    flows["date"] = _date(flows["date"])

    return MarketData(
        open=open_, close=wide["close"], adj_open=open_ * factor, adj_close=adj_close,
        turnover=wide["turnover"], volume=wide["volume"],
        companies=companies, revenue=revenue, flows=flows,
    )

