"""Active strategy (issue #73): buy short, sharp pullbacks in strong stocks.

Connors & Alvarez (Short Term Trading Strategies That Work, 2008): in a stock
that is trending up, a few days of hard selling tend to snap back. The rule,
checked on every close:

- Eligible: liquid common stocks above their `trend_days` average whose
  `strength_days` return is in the top `strength_pct` of liquid stocks.
- Buy those whose `rsi_days`-day RSI is below `entry_rsi`, most oversold
  first, into free slots, each with 1/`max_positions` of equity.
- Sell when the close is back above its `exit_ma_days` average, after
  `max_hold_days` trading days, or at `stop_loss` below cost.

Signals are taken at the close and filled at the next open, like every other
strategy here (Connors buys at the close). A round trip costs about 0.6%, so
the bounce has to be bigger than that to pay.
"""

from __future__ import annotations

import sys
from pathlib import Path

import pandas as pd

if __package__ in (None, ""):
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from strategies import swing  # noqa: E402
from strategies.swing import Book, Order  # noqa: E402


def rsi(adj: pd.DataFrame, n: int) -> pd.DataFrame:
    """Wilder's RSI on adjusted closes; days without a trade are skipped."""
    out = {}
    for t in adj.columns:
        s = adj[t].dropna()
        d = s.diff()
        up = d.clip(lower=0).ewm(alpha=1 / n, adjust=False, min_periods=n).mean()
        down = (-d.clip(upper=0)).ewm(alpha=1 / n, adjust=False, min_periods=n).mean()
        r = 100 - 100 / (1 + up / down)
        r[(down == 0) & (up > 0)] = 100.0
        r[(down == 0) & (up == 0)] = 50.0
        out[t] = r
    return pd.DataFrame(out).reindex(index=adj.index, columns=adj.columns)


def _frames(md, p) -> dict:
    adj = md.adj_close
    tr, st, ex = int(p["trend_days"]), int(p["strength_days"]), int(p["exit_ma_days"])
    filled = adj.ffill()
    return {
        "trend": adj.rolling(tr, min_periods=int(0.9 * tr)).mean(),
        "strength": filled / filled.shift(st) - 1,
        "exit_ma": adj.rolling(ex, min_periods=ex).mean(),
        "rsi": rsi(adj, int(p["rsi_days"])),
    }


class Pullback(swing.Swing):
    name = "pullback"
    DEFAULTS = {
        "max_positions": 10,
        "min_adv_ntd": 30_000_000,
        "adv_days": 20,
        "trend_days": 200,
        "strength_days": 120,
        "strength_pct": 0.2,
        "rsi_days": 2,
        "entry_rsi": 10.0,
        "exit_ma_days": 5,
        "max_hold_days": 10,
        "stop_loss": 0.10,
    }

    def _f(self, md) -> dict:
        p = self.p
        key = f"pullback:{p['trend_days']}:{p['strength_days']}:{p['exit_ma_days']}:{p['rsi_days']}"
        return swing.panel(md, key, lambda m: _frames(m, p))

    def orders(self, view, book: Book) -> list[Order]:
        p, md, day = self.p, view._d, view.day
        f = self._f(md)
        today = md.adj_close.loc[day]
        out: list[Order] = []
        for t, h in book.items():
            px = today.get(t)
            if px is None or pd.isna(px):
                continue
            ma = f["exit_ma"].at[day, t]
            if px * h.units <= h.cost * (1 - p["stop_loss"]):
                out.append(Order(t, "sell", 0.0, "stop"))
            elif pd.notna(ma) and px > ma:
                out.append(Order(t, "sell", 0.0, "bounce"))
            elif self.held_days(view, h.opened) >= p["max_hold_days"]:
                out.append(Order(t, "sell", 0.0, "time"))

        free = self.free(book, selling=len(out))
        if free <= 0:
            return out
        liquid = self.liquid(view)
        if not liquid:
            return out
        strength = f["strength"].loc[day, liquid].dropna()
        if strength.empty:
            return out
        strong = strength[strength.rank(pct=True) >= 1 - p["strength_pct"]].index
        cands = [t for t in strong if t not in book]
        trend = f["trend"].loc[day, cands]
        r = f["rsi"].loc[day, cands]
        ok = (today[cands] > trend) & (r < p["entry_rsi"])
        for t in r[ok].sort_values(kind="stable").index[:free]:
            out.append(Order(t, "buy", self.slot(book), "entry"))
        return out


def make(params: dict, settings) -> Pullback:
    return Pullback(params)


if __name__ == "__main__":
    swing.main(make, Pullback.name)
