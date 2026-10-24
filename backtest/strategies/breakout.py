"""Active strategy (issue #71): breakout swing trading.

Trend following on single stocks (Donchian / Turtle style; Wilcox &
Crittenden, "Does Trend Following Work on Stocks?", 2005): many small losses,
a few big winners. The rule, checked on every close:

- No new buys while `regime_ticker` closes below its `regime_days` average.
- A liquid common stock breaks out when its close is above the highest close
  of the previous `breakout_days` trading days and its traded value today is
  at least `volume_mult` x its average over the previous `volume_days`.
- Breakouts with the biggest volume ratio go first into free slots, each
  with 1/`max_positions` of equity.
- A holding is sold when its close falls below the highest close since it
  was bought minus `atr_mult` x its average absolute daily move over
  `atr_days` (an ATR on closes: the database has no adjusted highs and lows).
"""

from __future__ import annotations

import sys
from pathlib import Path

import pandas as pd

if __package__ in (None, ""):
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from strategies import swing  # noqa: E402
from strategies.swing import Book, Order  # noqa: E402


def _frames(md, p) -> dict:
    adj = md.adj_close
    n, v, a = int(p["breakout_days"]), int(p["volume_days"]), int(p["atr_days"])
    return {
        "prior_high": adj.rolling(n, min_periods=int(0.8 * n)).max().shift(1),
        "avg_value": md.turnover.rolling(v, min_periods=int(0.8 * v)).mean().shift(1),
        "atr": adj.ffill().diff().abs().rolling(a, min_periods=int(0.8 * a)).mean(),
    }


class Breakout(swing.Swing):
    name = "breakout"
    DEFAULTS = {
        "max_positions": 8,
        "breakout_days": 55,
        "volume_days": 50,
        "volume_mult": 2.0,
        "min_adv_ntd": 30_000_000,
        "adv_days": 20,
        "regime_ticker": "0050",
        "regime_days": 200,
        "atr_days": 20,
        "atr_mult": 3.0,
    }

    def _f(self, md) -> dict:
        p = self.p
        key = f"breakout:{p['breakout_days']}:{p['volume_days']}:{p['atr_days']}"
        return swing.panel(md, key, lambda m: _frames(m, p))

    def orders(self, view, book: Book) -> list[Order]:
        md, day = view._d, view.day
        f = self._f(md)
        today = md.adj_close.loc[day]
        atr = f["atr"].loc[day]
        out: list[Order] = []
        for t, h in book.items():
            px = today.get(t)
            if px is None or pd.isna(px) or pd.isna(atr.get(t)):
                continue
            stop = self.peak(view, t, h.opened) - self.p["atr_mult"] * atr[t]
            if px < stop:
                out.append(Order(t, "sell", 0.0, "trail_stop"))

        free = self.free(book, selling=len(out))
        if free <= 0 or not self.uptrend(view):
            return out
        cands = [t for t in self.liquid(view) if t not in book]
        if not cands:
            return out
        high = f["prior_high"].loc[day, cands]
        avg = f["avg_value"].loc[day, cands]
        ratio = md.turnover.loc[day, cands] / avg.where(avg > 0)
        hit = (today[cands] > high) & (ratio >= self.p["volume_mult"])
        picks = ratio[hit].sort_values(ascending=False, kind="stable")
        for t in picks.index[:free]:
            out.append(Order(t, "buy", self.slot(book), "entry"))
        return out

    def uptrend(self, view) -> bool:
        n, t = int(self.p["regime_days"]), self.p["regime_ticker"]
        if n <= 0:
            return True
        md = view._d
        if t not in md.adj_close.columns:
            return False
        ma = swing.panel(md, f"ma:{t}:{n}", lambda m: m.adj_close[t].rolling(n, min_periods=n).mean())
        px, avg = md.adj_close.at[view.day, t], ma.loc[view.day]
        return bool(pd.notna(px) and pd.notna(avg) and px > avg)


def make(params: dict, settings) -> Breakout:
    return Breakout(params)


if __name__ == "__main__":
    swing.main(make, Breakout.name)
