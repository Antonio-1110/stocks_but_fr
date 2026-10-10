"""Strategy B (issue #19): buy cyclical stocks as their revenue cycle turns up.

Cyclical industries (memory and other semiconductors, panels, shipping, steel,
plastics by default) go through long revenue downturns. The idea is to buy
when revenue growth has been negative for a while and starts improving, and
to sell when growth peaks and rolls over.

Revenue growth is YoY on a trailing `yoy_window`-month sum (3 by default),
which smooths out Lunar New Year shifting sales between January and February.
Only revenue already announced (data.PointInTime) is used.

Entry, checked every day on the latest announced month:
- at least `min_negative_months` of the `trough_months` months before the
  turn had negative YoY,
- YoY rose `improve_months` months in a row up to the latest month,
- the latest YoY is at most `max_entry_yoy` percent (not chasing a recovery
  that is already well under way),
- optionally, the close is above its `price_ma_days`-day average (0 = off),
- the stock traded at least `min_avg_turnover` NTD a day on average over the
  last `turnover_days` days.
When more stocks qualify than free slots, the biggest YoY improvement over
the turn goes first.

Exit:
- YoY fell `rollover_months` months in a row (the cycle peaked), or
- the adjusted close is `stop_loss` below the close on the entry signal day.
A stock stopped out can't come back until a newer revenue month is out.

Each position gets 1/`max_positions` of equity when bought, and is then left
to run: the strategy keeps a shadow of the portfolio so that adding a new
stock doesn't trim the winners back to equal weight.
"""

from __future__ import annotations

import numpy as np
import pandas as pd

DEFAULTS = {
    "industries": ["半導體業", "光電業", "航運業", "鋼鐵工業", "塑膠工業"],
    "tickers": [],              # extra tickers to include whatever their industry
    "yoy_window": 3,
    "trough_months": 12,
    "min_negative_months": 6,
    "improve_months": 2,
    "max_entry_yoy": 10.0,
    "rollover_months": 2,
    "stop_loss": 0.25,
    "price_ma_days": 60,
    "max_positions": 10,
    "min_avg_turnover": 20_000_000,
    "turnover_days": 20,
}


def make(params: dict, settings) -> "CycleBottom":
    unknown = set(params) - set(DEFAULTS)
    if unknown:
        raise ValueError(f"[strategy.cycle_bottom]: unknown keys {sorted(unknown)}")
    return CycleBottom({**DEFAULTS, **params})


def growth(revenue: pd.DataFrame, window: int) -> pd.DataFrame:
    """YoY percent of the trailing `window`-month revenue sum, month x ticker.
    A month with any missing revenue in either window is NaN."""
    wide = revenue.pivot_table(index="month", columns="ticker", values="revenue", aggfunc="last")
    months = pd.date_range(wide.index.min(), wide.index.max(), freq="MS")
    wide = wide.reindex(months)
    rolled = wide.rolling(window, min_periods=window).sum()
    base = rolled.shift(12)
    return (rolled / base.where(base > 0) - 1) * 100


def rising(y: np.ndarray, n: int) -> bool:
    """The last n steps of y all went up (needs n+1 values, none NaN)."""
    tail = y[-(n + 1):]
    return len(tail) == n + 1 and not np.isnan(tail).any() and bool((np.diff(tail) > 0).all())


def falling(y: np.ndarray, n: int) -> bool:
    tail = y[-(n + 1):]
    return len(tail) == n + 1 and not np.isnan(tail).any() and bool((np.diff(tail) < 0).all())


class CycleBottom:
    name = "cycle_bottom"

    def __init__(self, params: dict):
        self.params = params
        self.p = params
        self._rev_rows = -1
        self._yoy: pd.DataFrame | None = None
        self._entry_price: dict[str, float] = {}
        self._stopped: dict[str, pd.Timestamp] = {}   # ticker -> latest month when stopped
        # Shadow portfolio on the adjusted close, starting from equity 1.0.
        self._cash = 1.0
        self._units: dict[str, float] = {}

    # --- signals --------------------------------------------------------

    def _growth(self, view) -> pd.DataFrame | None:
        rev = view.revenue()
        if len(rev) != self._rev_rows:  # new months announced: recompute
            self._rev_rows = len(rev)
            self._yoy = growth(rev, int(self.p["yoy_window"])) if len(rev) else None
        return self._yoy

    def _universe(self, view) -> set[str]:
        c = view.companies()
        inds = set(self.p["industries"])
        pick = c[c["industry"].isin(inds) | c["ticker"].isin(self.p["tickers"])]
        return {t for t in pick["ticker"] if not t.startswith("00")}

    def _entry_ok(self, y: np.ndarray) -> float | None:
        """YoY improvement over the turn if the series qualifies, else None."""
        k, n = int(self.p["improve_months"]), int(self.p["trough_months"])
        if not rising(y, k) or y[-1] > self.p["max_entry_yoy"]:
            return None
        # The n months up to and including the one the rise started from.
        start = len(y) - 1 - k
        trough = y[max(0, start - n + 1): start + 1]
        if (trough < 0).sum() < self.p["min_negative_months"]:
            return None
        return float(y[-1] - y[-(k + 1)])

    # --- daily decision -------------------------------------------------

    def targets(self, view) -> dict[str, float] | None:
        yoy = self._growth(view)
        if yoy is None or yoy.empty:
            return None
        latest = yoy.index[-1]
        px = view.adj_close
        today = px.iloc[-1]
        changed = False

        # Exits.
        for t in list(self._units):
            price = today.get(t, np.nan)
            y = yoy[t].to_numpy() if t in yoy else np.array([])
            stop = (pd.notna(price)
                    and price < self._entry_price[t] * (1 - self.p["stop_loss"]))
            if stop:
                self._stopped[t] = latest
            if stop or falling(y, int(self.p["rollover_months"])):
                self._sell(t, price if pd.notna(price) else self._last(px, t))
                changed = True

        # Entries.
        slots = int(self.p["max_positions"]) - len(self._units)
        if slots > 0:
            picks = self._candidates(view, yoy, latest, px, today)
            if picks:
                equity = self._equity(px, today)
                for t in picks[:slots]:
                    value = min(equity / self.p["max_positions"], self._cash)
                    if value <= 0:
                        break
                    self._buy(t, value, today[t])
                    changed = True

        if not changed:
            return None
        equity = self._equity(px, today)
        return {t: u * self._price(px, today, t) / equity for t, u in self._units.items()}

    def _candidates(self, view, yoy, latest, px, today) -> list[str]:
        universe = self._universe(view) & set(yoy.columns) & set(view.traded_today())
        universe -= set(self._units)
        universe -= {t for t, m in self._stopped.items() if m >= latest}
        if not universe:
            return []
        cols = sorted(universe)
        turnover = view.turnover[cols].iloc[-int(self.p["turnover_days"]):]
        if len(turnover) < int(self.p["turnover_days"]):
            return []
        liquid = turnover.fillna(0).mean() >= self.p["min_avg_turnover"]
        ma_days = int(self.p["price_ma_days"])
        ma = px[cols].iloc[-ma_days:].mean() if ma_days else None
        scored = []
        for t in cols:
            if not liquid[t]:
                continue
            if ma_days and (len(px) < ma_days or not today[t] > ma[t]):
                continue
            score = self._entry_ok(yoy[t].to_numpy())
            if score is not None:
                scored.append((-score, t))
        return [t for _, t in sorted(scored)]

    # --- shadow portfolio -----------------------------------------------

    def _price(self, px, today, t) -> float:
        p = today.get(t, np.nan)
        return p if pd.notna(p) else self._last(px, t)

    @staticmethod
    def _last(px, t) -> float:
        s = px[t].dropna() if t in px else pd.Series(dtype=float)
        return float(s.iloc[-1]) if len(s) else 0.0

    def _equity(self, px, today) -> float:
        return self._cash + sum(u * self._price(px, today, t) for t, u in self._units.items())

    def _buy(self, t, value, price) -> None:
        self._units[t] = value / price
        self._cash -= value
        self._entry_price[t] = price

    def _sell(self, t, price) -> None:
        self._cash += self._units.pop(t) * price
        self._entry_price.pop(t, None)
