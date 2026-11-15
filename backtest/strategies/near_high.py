"""Active strategy (issue #92): hold the stocks closest to their 52-week high.

The alpha lab's one signal that held in both periods (#76, #82): liquid
stocks trading at or near their 52-week closing high beat the market over
the next 2-4 weeks, and acting a day late costs almost nothing. The rule:

- Every `rebalance_days` trading days, rank liquid common stocks (at least
  `min_adv_ntd` average traded value, a close of at least `min_price`) by
  a score: the weighted average of the percentile ranks of closeness to the
  `high_days` closing high and of the latest monthly revenue YoY (weight
  `revenue_weight`; a month counts from the trading day after its deadline
  for 45 trading days, and a YoY off a base under `min_base_ntd` doesn't
  count). With revenue_weight > 0 a stock needs a usable YoY to rank. The
  lab's best version (#82) is half each.
- Hold the top `max_positions`, each bought with 1/`max_positions` of
  equity. A holding is kept while it still ranks in the top `exit_rank`.
- Between rebalances, sell a holding whose close falls `stop_loss` below its
  highest close since it was bought.

internal/paper/active.go (nearHigh) runs the same rule on the paper portfolio.
"""

from __future__ import annotations

import sys
from pathlib import Path

import pandas as pd

if __package__ in (None, ""):
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from radar_backtest import signals  # noqa: E402
from strategies import swing  # noqa: E402
from strategies.swing import Book, Order  # noqa: E402


class NearHigh(swing.Swing):
    name = "near_high"
    DEFAULTS = {
        "max_positions": 10,
        "exit_rank": 30,
        "rebalance_days": 5,       # trading days between rankings
        "high_days": 250,
        "revenue_weight": 0.5,     # weight of the latest monthly revenue YoY rank (0 = off)
        "min_adv_ntd": 30_000_000,
        "adv_days": 20,
        "min_price": 10.0,
        "min_base_ntd": 10_000_000,  # revenue YoY off a smaller base than this is ignored
        "stop_loss": 0.10,         # trailing, from the highest close since buying
    }

    def __init__(self, params: dict):
        super().__init__(params)
        self._since_rebalance: int | None = None

    def _frames(self, md) -> dict:
        p = self.p
        n = int(p["high_days"])

        def build(m):
            px = m.adj_close.ffill()
            out = {"close_to_high": m.adj_close / px.rolling(n, min_periods=int(0.8 * n)).max()}
            if p["revenue_weight"] > 0:
                out["revenue_yoy"] = signals.Inputs(m, min_base_ntd=p["min_base_ntd"]).revenue_wide("yoy_pct")
            return out
        return swing.panel(md, f"near_high:{n}:{p['revenue_weight'] > 0}:{p['min_base_ntd']}", build)

    def ranking(self, view) -> pd.Series:
        """Score of each eligible stock today, best first."""
        p, md, day = self.p, view._d, view.day
        liquid = self.liquid(view)
        close = md.close.loc[day, liquid]
        liquid = list(close[close >= p["min_price"]].index)
        f = self._frames(md)
        near = f["close_to_high"].loc[day, liquid].dropna()
        if near.empty:
            return near
        rw = float(p["revenue_weight"])
        if rw > 0:  # no usable revenue (or a low base): not ranked
            near = near[f["revenue_yoy"].loc[day, near.index].notna()]
        score = (1 - rw) * near.rank(pct=True)
        if rw > 0:
            score += rw * f["revenue_yoy"].loc[day, near.index].rank(pct=True)
        return score.sort_values(ascending=False, kind="stable")

    def orders(self, view, book: Book) -> list[Order]:
        p, md, day = self.p, view._d, view.day
        out: list[Order] = []
        selling: set[str] = set()
        today = md.adj_close.loc[day]
        for t, h in book.items():
            px = today.get(t)
            if px is None or pd.isna(px):
                continue
            if px <= self.peak(view, t, h.opened) * (1 - p["stop_loss"]):
                out.append(Order(t, "sell", 0.0, "trail_stop"))
                selling.add(t)

        self._since_rebalance = 0 if self._since_rebalance is None else self._since_rebalance + 1
        if self._since_rebalance % int(p["rebalance_days"]):
            return out
        ranked = list(self.ranking(view).index)
        keep = set(ranked[:int(p["exit_rank"])])
        for t in book:
            if t not in selling and t not in keep:
                out.append(Order(t, "sell", 0.0, "rank"))
                selling.add(t)
        free = self.free(book, selling=len(selling))
        if free <= 0:
            return out
        for t in [t for t in ranked[:int(p["max_positions"])] if t not in book][:free]:
            out.append(Order(t, "buy", self.slot(book), "entry"))
        return out


def make(params: dict, settings) -> NearHigh:
    return NearHigh(params)


if __name__ == "__main__":
    swing.main(make, NearHigh.name)
