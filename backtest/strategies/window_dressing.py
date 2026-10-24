"""Active strategy (issue #72): ride 投信 quarter-end window dressing.

Taiwan's investment trusts report performance at each quarter end (year end
most of all) and tend to push up what they hold before it; an NTU thesis on
2001-2014 found their top net-bought stocks beat the market that quarter. The
rule:

- Once the end of a quarter in `quarters` is `entry_days` calendar days away
  or less (and more than `exit_days`), buy once: the liquid common stocks,
  leaving out the `skip_largest` most traded, with the highest 投信 net NTD
  bought over the last `flow_days` trading days as a share of their traded
  value (positive only), each with 1/`max_positions` of equity.
- Sell everything once the quarter end is `exit_days` calendar days away or
  less, or a holding earlier at `stop_loss` below its cost.

Calendar days, not trading days, so the live paper portfolio can tell where
it is without knowing future holidays. 投信 flows are complete from 2013.
"""

from __future__ import annotations

import sys
from pathlib import Path

import pandas as pd

if __package__ in (None, ""):
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from strategies import swing  # noqa: E402
from strategies.swing import Book, Order  # noqa: E402


def quarter_end(day: pd.Timestamp) -> pd.Timestamp:
    return (day.to_period("Q").end_time).normalize()


def _trust_value(md) -> pd.DataFrame:
    """Date x ticker: 投信 net NTD bought (net shares x close)."""
    f = md.flows
    net = f.pivot_table(index="date", columns="ticker", values="trust_net", aggfunc="last")
    net = net.reindex(index=md.close.index, columns=md.close.columns)
    return net * md.close


class WindowDressing(swing.Swing):
    name = "window_dressing"
    DEFAULTS = {
        "quarters": [3, 6, 9, 12],
        "entry_days": 28,
        "exit_days": 3,
        "flow_days": 20,
        "max_positions": 10,
        "min_adv_ntd": 20_000_000,
        "adv_days": 20,
        "skip_largest": 100,
        "stop_loss": 0.10,
    }

    def __init__(self, params: dict):
        super().__init__(params)
        self._entered: pd.Timestamp | None = None  # quarter end bought for

    def orders(self, view, book: Book) -> list[Order]:
        p, day = self.p, view.day
        qend = quarter_end(day)
        left = (qend - day).days
        active = qend.month in p["quarters"]
        today = view._d.adj_close.loc[day]
        out: list[Order] = []
        for t, h in book.items():
            px = today.get(t)
            if left <= p["exit_days"]:
                out.append(Order(t, "sell", 0.0, "quarter_end"))
            elif px is not None and pd.notna(px) and px * h.units <= h.cost * (1 - p["stop_loss"]):
                out.append(Order(t, "sell", 0.0, "stop"))
        if (active and p["exit_days"] < left <= p["entry_days"] and self._entered != qend):
            self._entered = qend
            free = self.free(book, selling=len(out))
            for t in self.ranked(view).index[:max(free, 0)]:
                if t not in book:
                    out.append(Order(t, "buy", self.slot(book), "entry"))
        return out

    def ranked(self, view) -> pd.Series:
        """投信 intensity of the eligible stocks, highest first (positive only)."""
        p, md = self.p, view._d
        cands = self.liquid(view)
        if not cands:
            return pd.Series(dtype=float)
        n = int(p["adv_days"])
        adv = swing.panel(md, f"adv{n}", lambda m: m.turnover.fillna(0.0).rolling(n, min_periods=n).mean())
        big = set(adv.loc[view.day, cands].sort_values(ascending=False).index[: int(p["skip_largest"])])
        cands = [t for t in cands if t not in big]
        k = int(p["flow_days"])
        trust = swing.panel(md, "trust_value", _trust_value)
        i = md.calendar.get_loc(view.day)
        rows = md.calendar[max(0, i - k + 1): i + 1]
        bought = trust.loc[rows, cands].sum(min_count=1)
        traded = md.turnover.loc[rows, cands].sum()
        score = (bought / traded.where(traded > 0)).dropna()
        score = score[score > 0]
        return score.sort_values(ascending=False, kind="stable")


def make(params: dict, settings) -> WindowDressing:
    return WindowDressing(params)


if __name__ == "__main__":
    swing.main(make, WindowDressing.name)
