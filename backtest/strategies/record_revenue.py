"""Active strategy (issue #70): trade record monthly revenue announcements.

Taiwan event studies find abnormal returns after a company reports its
highest-ever monthly sales. The rule:

- On the first trading day after this month's revenue deadline (the 10th, or
  the next trading day), look at last month's revenue. A liquid common stock
  qualifies when that month is above every one of the `record_months` - 1
  months before it, its YoY is at least `min_yoy_pct`, and last year's same
  month was at least `min_base_ntd` (YoY off a tiny base is noise).
- Buy the qualifiers with the highest YoY into free slots, each with
  1/`max_positions` of equity.
- A holding that qualifies again keeps running and its clock restarts.
  Otherwise it is sold after `hold_days` trading days, or earlier when the
  close falls `trail_stop` below its highest close since it was bought.

The stored announced_on is the legal deadline, not the real filing day, so
this acts after the deadline, as revenue momentum does. Companies that file
early get no head start here.
"""

from __future__ import annotations

import sys
from pathlib import Path

import pandas as pd

if __package__ in (None, ""):
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from strategies import swing  # noqa: E402
from strategies.swing import Book, Order  # noqa: E402


class RecordRevenue(swing.Swing):
    name = "record_revenue"
    DEFAULTS = {
        "max_positions": 8,
        "record_months": 36,         # newest month above all of the previous 35
        "min_yoy_pct": 20.0,
        "min_base_ntd": 10_000_000,  # last year's same month at least this
        "min_adv_ntd": 20_000_000,
        "adv_days": 20,
        "hold_days": 20,             # trading days
        "trail_stop": 0.10,          # sell this far below the highest close since buying
        "revenue_deadline_day": 10,
    }

    def __init__(self, params: dict):
        super().__init__(params)
        self._done: pd.Period | None = None
        self._renewed: dict[str, pd.Timestamp] = {}

    def orders(self, view, book: Book) -> list[Order]:
        out: list[Order] = []
        records: pd.DataFrame | None = None
        if self._rebalance_day(view):
            self._done = view.day.to_period("M")
            records = self.records(view)
            for t in book:
                if t in records.index:
                    self._renewed[t] = view.day
        self._renewed = {t: d for t, d in self._renewed.items() if t in book}

        today = view._d.adj_close.loc[view.day]
        for t, h in book.items():
            px = today.get(t)
            if px is None or pd.isna(px):
                continue
            start = max(h.opened, self._renewed.get(t, h.opened))
            if px <= self.peak(view, t, h.opened) * (1 - self.p["trail_stop"]):
                out.append(Order(t, "sell", 0.0, "trail_stop"))
            elif self.held_days(view, start) >= self.p["hold_days"]:
                out.append(Order(t, "sell", 0.0, "time"))

        if records is not None:
            free = self.free(book, selling=len(out))
            for t in [t for t in records.index if t not in book][:max(free, 0)]:
                out.append(Order(t, "buy", self.slot(book), "entry"))
        return out

    def _rebalance_day(self, view) -> bool:
        """The first trading day after this month's revenue deadline."""
        if self._done == view.day.to_period("M"):
            return False
        cal = view._d.calendar
        day = view.day
        month = cal[(cal >= day.replace(day=1)) & (cal <= day)]
        return len(month[month.day >= self.p["revenue_deadline_day"]]) >= 2

    def records(self, view) -> pd.DataFrame:
        """Last month's record-revenue qualifiers, best YoY first."""
        p = self.p
        md = view._d
        wide = swing.panel(md, "revenue_wide", lambda m: m.revenue.pivot_table(
            index="month", columns="ticker", values="revenue", aggfunc="last").sort_index())
        newest = (view.day.to_period("M") - 1).to_timestamp()
        if newest not in wide.index:
            return pd.DataFrame(columns=["yoy_pct"])
        rev = view.revenue()
        last = rev[rev["month"] == newest].set_index("ticker")
        cands = [t for t in self.liquid(view) if t in last.index]
        if not cands:
            return pd.DataFrame(columns=["yoy_pct"])
        n = int(p["record_months"])
        hist = wide.loc[:newest].iloc[-n:][cands]
        cur = hist.iloc[-1]
        before = hist.iloc[:-1]
        full = before.notna().sum() >= n - 1
        record = full & (cur > before.max())
        yoy = last.loc[cands, "yoy_pct"]
        base = last.loc[cands, "revenue"] / (1 + yoy / 100)
        ok = record & (yoy >= p["min_yoy_pct"]) & (base >= p["min_base_ntd"])
        out = pd.DataFrame({"yoy_pct": yoy[ok]}).rename_axis("ticker").reset_index()
        out = out.sort_values(["yoy_pct", "ticker"], ascending=[False, True])
        return out.set_index("ticker")


def make(params: dict, settings) -> RecordRevenue:
    return RecordRevenue(params)


if __name__ == "__main__":
    swing.main(make, RecordRevenue.name)
