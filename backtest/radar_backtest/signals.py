"""Candidate short-term signals for the alpha lab (issue #76).

Each signal scores every stock every trading day from what was public by that
day's close, as a date x ticker frame. Higher means "expected to beat the
market". A "score" signal ranks the whole universe; an "event" signal is 1.0 on
the days something happens and NaN otherwise.

The direction of every signal is fixed here, before looking at results. A
signal that turns out to work backwards shows up with a negative IC; it is not
flipped automatically, since flipping after the fact is how data mining finds
patterns that are not there.

To add a signal: write a function of `Inputs` and add it to SIGNALS.
"""

from __future__ import annotations

from dataclasses import dataclass
from functools import cached_property
from typing import Callable

import numpy as np
import pandas as pd

from .data import MarketData

F = np.float32


@dataclass(frozen=True)
class Signal:
    name: str
    family: str        # price, flows, revenue, calendar
    kind: str          # "score" or "event"
    label: str         # what it means, in plain words
    fn: Callable[["Inputs"], pd.DataFrame]


class Inputs:
    """MarketData plus the derived frames several signals share, built once."""

    def __init__(self, data: MarketData, dividends: pd.DataFrame | None = None,
                 limit_pct: pd.Series | None = None, min_base_ntd: float = 10_000_000,
                 limit_tolerance: float = 0.005):
        self.d = data
        self.dividends = dividends if dividends is not None else pd.DataFrame(columns=["ticker", "date"])
        self.min_base_ntd = min_base_ntd
        self.limit_tolerance = limit_tolerance
        cal = data.calendar
        self.limit_pct = limit_pct if limit_pct is not None else pd.Series(0.10, index=cal)

    # Prices -----------------------------------------------------------------

    @cached_property
    def close(self) -> pd.DataFrame:
        return self.d.adj_close.astype(F)

    @cached_property
    def open(self) -> pd.DataFrame:
        return self.d.adj_open.astype(F)

    @cached_property
    def ret1(self) -> pd.DataFrame:
        return self.close / self.close.shift(1) - 1

    @cached_property
    def volume(self) -> pd.DataFrame:
        return self.d.volume.astype(F)

    @cached_property
    def avg_volume(self) -> pd.DataFrame:
        """20-day average volume up to yesterday, so today's spike isn't in it."""
        return self.volume.shift(1).rolling(20, min_periods=10).mean()

    @cached_property
    def at_limit_up(self) -> pd.DataFrame:
        lim = self.limit_pct.reindex(self.close.index).astype(F)
        return self.ret1.ge(lim - self.limit_tolerance, axis=0)

    @cached_property
    def at_limit_down(self) -> pd.DataFrame:
        lim = self.limit_pct.reindex(self.close.index).astype(F)
        return self.ret1.le(-(lim - self.limit_tolerance), axis=0)

    # Flows ------------------------------------------------------------------

    def _flow(self, col: str) -> pd.DataFrame:
        f = self.d.flows
        w = f.pivot_table(index="date", columns="ticker", values=col, aggfunc="sum")
        w = w.reindex(index=self.close.index, columns=self.close.columns).astype(F)
        # Before flows were collected for a stock, NaN (unknown), not zero.
        return w

    @cached_property
    def foreign(self) -> pd.DataFrame:
        return self._flow("foreign_net")

    @cached_property
    def trust(self) -> pd.DataFrame:
        return self._flow("trust_net")

    @cached_property
    def dealer(self) -> pd.DataFrame:
        return self._flow("dealer_net")

    def flow_ratio(self, flow: pd.DataFrame, days: int) -> pd.DataFrame:
        """Net shares bought over `days`, as days of average volume."""
        total = flow.rolling(days, min_periods=days).sum()
        return total / self.avg_volume.where(self.avg_volume > 0)

    # Revenue ----------------------------------------------------------------

    @cached_property
    def revenue(self) -> pd.DataFrame:
        """Long revenue rows with the trading day each became usable (`usable`:
        the first trading day after announced_on) and derived fields."""
        r = self.d.revenue.sort_values(["ticker", "month"]).copy()
        cal = self.close.index
        pos = cal.searchsorted(r["announced_on"].values, side="right")
        r = r[pos < len(cal)].copy()
        r["usable"] = cal[pos[pos < len(cal)]]
        base = r["revenue"] / (1 + r["yoy_pct"] / 100)
        r["low_base"] = ~(base >= self.min_base_ntd)
        g = r.groupby("ticker", sort=False)
        prior_max = g["revenue"].transform(lambda s: s.shift(1).rolling(35, min_periods=24).max())
        r["record"] = r["revenue"] > prior_max
        r["accel"] = r["yoy_pct"] - g["yoy_pct"].transform(lambda s: s.shift(1).rolling(3, min_periods=3).mean())
        return r

    def revenue_wide(self, col: str, fresh_days: int | None = None) -> pd.DataFrame:
        """The newest usable month's `col` each day (NaN on a low base), for at
        most 45 trading days, or only `fresh_days` after it became usable."""
        r = self.revenue
        r = r[~r["low_base"]]
        last = r.sort_values("month").groupby(["usable", "ticker"]).tail(1)
        w = last.pivot(index="usable", columns="ticker", values=col).astype(F)
        w = w.reindex(index=self.close.index, columns=self.close.columns)
        return w.ffill(limit=fresh_days if fresh_days is not None else 45)


# Price and volume -----------------------------------------------------------

def _reversal_1d(x: Inputs):
    return -x.ret1


def _reversal_5d(x: Inputs):
    return -(x.close / x.close.shift(5) - 1)


def _momentum_20d(x: Inputs):
    return x.close / x.close.shift(20) - 1


def _momentum_6m_skip(x: Inputs):
    return x.close.shift(20) / x.close.shift(120) - 1


def _near_high(x: Inputs):
    return x.close / x.close.rolling(250, min_periods=200).max()


def _below_ma20(x: Inputs):
    return -(x.close / x.close.rolling(20, min_periods=20).mean() - 1)


def _low_vol(x: Inputs):
    return -x.ret1.rolling(20, min_periods=15).std()


def _lottery(x: Inputs):
    return -x.ret1.rolling(20, min_periods=15).max()


def _volume_surge(x: Inputs):
    return np.log((x.volume / x.avg_volume.where(x.avg_volume > 0)).where(x.volume > 0))


def _event(mask: pd.DataFrame) -> pd.DataFrame:
    return mask.astype(F).where(mask)


def _surge_up(x: Inputs):
    return _event((x.volume > 3 * x.avg_volume) & (x.ret1 > 0.03))


def _limit_up(x: Inputs):
    return _event(x.at_limit_up)


def _limit_down(x: Inputs):
    return _event(x.at_limit_down)


def _gap_hold(x: Inputs):
    gap = x.open / x.close.shift(1) - 1
    return _event((gap > 0.03) & (x.close > x.open))


def _breakout(x: Inputs):
    prior = x.close.shift(1).rolling(20, min_periods=20).max()
    return _event((x.close > prior) & (x.volume > 1.5 * x.avg_volume))


# Institutional flows (三大法人) -----------------------------------------------

def _foreign_1d(x: Inputs):
    return x.flow_ratio(x.foreign, 1)


def _foreign_5d(x: Inputs):
    return x.flow_ratio(x.foreign, 5)


def _foreign_20d(x: Inputs):
    return x.flow_ratio(x.foreign, 20)


def _trust_1d(x: Inputs):
    return x.flow_ratio(x.trust, 1)


def _trust_5d(x: Inputs):
    return x.flow_ratio(x.trust, 5)


def _trust_20d(x: Inputs):
    return x.flow_ratio(x.trust, 20)


def _dealer_5d(x: Inputs):
    return x.flow_ratio(x.dealer, 5)


def _trust_streak(x: Inputs):
    buy = (x.trust > 0).to_numpy(dtype=F)
    # Consecutive buying days: the count resets on every day 投信 doesn't buy.
    streak = np.zeros_like(buy)
    for i in range(len(buy)):
        streak[i] = (streak[i - 1] + 1) * buy[i] if i else buy[i]
    out = pd.DataFrame(streak, index=x.trust.index, columns=x.trust.columns)
    return out.where(x.trust.notna())


def _trust_first_buy(x: Inputs):
    """投信 buys at least 5% of a day's average volume after 20 days of not
    buying: a fund starting a new position."""
    quiet = (x.trust.shift(1).rolling(20, min_periods=20).max() <= 0)
    big = x.trust > 0.05 * x.avg_volume
    return _event(quiet & big)


def _both_buy(x: Inputs):
    f = x.foreign > 0.05 * x.avg_volume
    t = x.trust > 0.02 * x.avg_volume
    return _event(f & t)


# Monthly revenue (月營收) -----------------------------------------------------

def _rev_yoy(x: Inputs):
    return x.revenue_wide("yoy_pct")


def _rev_accel(x: Inputs):
    return x.revenue_wide("accel")


def _rev_yoy_fresh(x: Inputs):
    return x.revenue_wide("yoy_pct", fresh_days=4)


def _rev_record(x: Inputs):
    rec = x.revenue_wide("record", fresh_days=4)
    return _event(rec == 1)


# Calendar -------------------------------------------------------------------

def _dividend_days(x: Inputs, before: int, after: int) -> pd.DataFrame:
    """1.0 from `before` trading days ahead of an ex-dividend day to `after`
    days past it. Ex-dates are published weeks ahead, so knowing one five
    trading days early is not look-ahead."""
    cal = x.close.index
    out = pd.DataFrame(False, index=cal, columns=x.close.columns)
    d = x.dividends
    d = d[d["ticker"].isin(out.columns)]
    pos = cal.searchsorted(d["date"].values)
    for (t, p) in zip(d["ticker"].values, pos):
        lo, hi = p - before, p + after
        lo, hi = max(lo, 0), min(hi, len(cal) - 1)
        if lo <= hi:
            out.iloc[lo:hi + 1, out.columns.get_loc(t)] = True
    return _event(out)


def _pre_exdiv(x: Inputs):
    return _dividend_days(x, before=5, after=-1)


def _post_exdiv(x: Inputs):
    return _dividend_days(x, before=0, after=4)


SIGNALS: list[Signal] = [
    Signal("reversal_1d", "price", "score", "Yesterday's losers (short-term reversal)", _reversal_1d),
    Signal("reversal_5d", "price", "score", "Last week's losers", _reversal_5d),
    Signal("momentum_20d", "price", "score", "Last month's winners", _momentum_20d),
    Signal("momentum_6m", "price", "score", "6-month winners, skipping the last month", _momentum_6m_skip),
    Signal("near_52w_high", "price", "score", "Closest to the 52-week high", _near_high),
    Signal("below_ma20", "price", "score", "Furthest below the 20-day average", _below_ma20),
    Signal("low_volatility", "price", "score", "Calmest stocks over 20 days", _low_vol),
    Signal("no_lottery", "price", "score", "No big one-day jump in 20 days", _lottery),
    Signal("volume_surge", "price", "score", "Volume vs its 20-day average", _volume_surge),
    Signal("surge_up_day", "price", "event", "Volume 3x average and up 3%+", _surge_up),
    Signal("limit_up_close", "price", "event", "Closed at limit-up (漲停)", _limit_up),
    Signal("limit_down_close", "price", "event", "Closed at limit-down (跌停)", _limit_down),
    Signal("gap_and_hold", "price", "event", "Opened 3%+ higher and closed above the open", _gap_hold),
    Signal("breakout_20d", "price", "event", "New 20-day high on 1.5x volume", _breakout),
    Signal("foreign_1d", "flows", "score", "外資 net buying today (days of volume)", _foreign_1d),
    Signal("foreign_5d", "flows", "score", "外資 net buying, 5 days", _foreign_5d),
    Signal("foreign_20d", "flows", "score", "外資 net buying, 20 days", _foreign_20d),
    Signal("trust_1d", "flows", "score", "投信 net buying today", _trust_1d),
    Signal("trust_5d", "flows", "score", "投信 net buying, 5 days", _trust_5d),
    Signal("trust_20d", "flows", "score", "投信 net buying, 20 days", _trust_20d),
    Signal("dealer_5d", "flows", "score", "自營商 net buying, 5 days", _dealer_5d),
    Signal("trust_streak", "flows", "score", "Days in a row 投信 has bought", _trust_streak),
    Signal("trust_first_buy", "flows", "event", "投信 starts buying after 20 quiet days", _trust_first_buy),
    Signal("foreign_and_trust", "flows", "event", "外資 and 投信 both buy big the same day", _both_buy),
    Signal("revenue_yoy", "revenue", "score", "Latest monthly revenue YoY", _rev_yoy),
    Signal("revenue_accel", "revenue", "score", "YoY above its 3-month average (speeding up)", _rev_accel),
    Signal("revenue_yoy_fresh", "revenue", "score", "Revenue YoY, first 5 days after the deadline", _rev_yoy_fresh),
    Signal("revenue_record", "revenue", "event", "Record monthly revenue (3 years), first 5 days", _rev_record),
    Signal("pre_ex_dividend", "calendar", "event", "The 5 trading days before ex-dividend", _pre_exdiv),
    Signal("post_ex_dividend", "calendar", "event", "Ex-dividend day and the 4 days after (填息)", _post_exdiv),
]

BY_NAME = {s.name: s for s in SIGNALS}
