"""Daily long-only simulation shared by every strategy.

Rules, so results stay comparable and honest:

- A strategy sees the market at the close of day t (data.PointInTime) and may
  return target weights. Orders fill at the open of the next trading day.
- Positions are held on the dividend/split-adjusted price scale, so returns
  include dividends (reinvested in the same stock) and survive splits. Units
  are fractional: Taiwan has odd-lot trading, so lot size is ignored.
- Costs: commission x broker discount with a minimum fee on every order, and
  sell tax (lower for ETFs), all from radar.toml.
- Liquidity: a buy may not take a position above max_adv_fraction of its
  average traded value over the adv_days before the signal. The rest stays cash.
- A held stock that stops trading (delisted) is sold at its last traded price.
- A stock with no price on the fill day (suspended) is skipped; the order is
  counted in skipped_orders and the next signal can try again.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Protocol

import pandas as pd

from . import costs as fees
from .config import Settings
from .data import MarketData, PointInTime


class Strategy(Protocol):
    name: str
    params: dict

    def targets(self, view: PointInTime) -> dict[str, float] | None:
        """Weights (fractions of equity, summing to at most 1) to hold from the
        next open, or None to leave the portfolio alone."""


@dataclass
class Trade:
    """One round trip: from a position opening until it is back to zero."""

    ticker: str
    opened: pd.Timestamp
    closed: pd.Timestamp | None = None
    invested: float = 0.0       # buy value plus buy commission, all fills
    received: float = 0.0       # sell value minus commission and tax, all fills
    mark: float = 0.0           # value still held at the end of the run
    delisted: bool = False
    longest_underwater: int = 0  # trading days in a row worth less than its cost
    underwater_from: pd.Timestamp | None = None
    underwater_to: pd.Timestamp | None = None

    @property
    def pnl(self) -> float:
        return self.received + self.mark - self.invested

    @property
    def ret(self) -> float:
        return self.pnl / self.invested if self.invested else 0.0


@dataclass
class Fill:
    day: pd.Timestamp
    ticker: str
    side: str          # "buy" or "sell"
    value: float       # NTD traded, before costs
    price: float       # adjusted price
    commission: float
    tax: float


@dataclass
class _Position:
    units: float
    basis: float      # cost of the units still held
    trade: Trade
    streak: int = 0
    streak_from: pd.Timestamp | None = None


@dataclass
class RunResult:
    strategy: str
    params: dict
    start: pd.Timestamp
    end: pd.Timestamp
    initial_capital: float
    equity: pd.Series            # NTD at each close
    invested: pd.Series          # NTD in positions at each close
    trades: list[Trade]          # closed round trips
    open_trades: list[Trade] = field(default_factory=list)  # marked at the last close
    commission_paid: float = 0.0
    tax_paid: float = 0.0
    traded_value: float = 0.0    # buys plus sells, NTD
    skipped_orders: int = 0
    fills: list[Fill] = field(default_factory=list)
    log: list[str] = field(default_factory=list)


class Engine:
    def __init__(self, data: MarketData, settings: Settings):
        self.data = data
        self.s = settings
        self.marks = data.adj_close.ffill()
        self.last_traded = data.last_traded()
        self.adv = (data.turnover.fillna(0.0)
                    .rolling(settings.adv_days, min_periods=settings.adv_days).mean())
        etf = {r.ticker: fees.is_etf(r.ticker, r.industry or "")
               for r in data.companies.itertuples()}
        self.etf = lambda t: etf.get(t, fees.is_etf(t))

    def run(self, strategy: Strategy, start: str | pd.Timestamp,
            end: str | pd.Timestamp | None = None) -> RunResult:
        cal = self.data.calendar
        days = cal[cal >= pd.Timestamp(start)]
        if end is not None:
            days = days[days <= pd.Timestamp(end)]
        if len(days) < 2:
            raise ValueError(f"no trading days between {start} and {end}")
        res = RunResult(strategy.name, dict(strategy.params), days[0], days[-1],
                        self.s.initial_capital, pd.Series(dtype=float), pd.Series(dtype=float), [])
        self._cash = self.s.initial_capital
        self._pos: dict[str, _Position] = {}
        self._res = res
        pending: tuple[pd.Timestamp, dict[str, float]] | None = None
        equity, invested = [], []

        for day in days:
            self._delist(day)
            if pending is not None:
                self._rebalance(day, *pending)
                pending = None
            value = self._mark(day)
            equity.append(self._cash + value)
            invested.append(value)
            want = strategy.targets(self.data.view(day))
            if want is not None:
                pending = (day, _clean(want))

        res.equity = pd.Series(equity, index=days)
        res.invested = pd.Series(invested, index=days)
        for t, p in self._pos.items():
            p.trade.mark = p.units * self.marks.at[days[-1], t]
            res.open_trades.append(p.trade)
        return res

    # --- order handling -------------------------------------------------

    def _sell(self, day, ticker: str, value: float, price: float, everything: bool) -> None:
        p = self._pos[ticker]
        frac = 1.0 if everything else min(1.0, value / (p.units * price))
        commission = fees.commission(self.s.costs, value)
        tax = fees.sell_tax(self.s.costs, value, self.etf(ticker))
        self._cash += value - commission - tax
        self._res.commission_paid += commission
        self._res.tax_paid += tax
        self._res.traded_value += value
        p.trade.received += value - commission - tax
        self._res.fills.append(Fill(day, ticker, "sell", value, price, commission, tax))
        if frac >= 1.0:
            p.trade.closed = day
            self._res.trades.append(p.trade)
            del self._pos[ticker]
        else:
            p.units *= 1 - frac
            p.basis *= 1 - frac

    def _buy(self, day, ticker: str, value: float, price: float) -> None:
        commission = fees.commission(self.s.costs, value)
        self._cash -= value + commission
        self._res.commission_paid += commission
        self._res.traded_value += value
        p = self._pos.get(ticker)
        if p is None:
            p = self._pos[ticker] = _Position(0.0, 0.0, Trade(ticker, day))
        p.units += value / price
        p.basis += value + commission
        p.trade.invested += value + commission
        self._res.fills.append(Fill(day, ticker, "buy", value, price, commission, 0.0))

    def _delist(self, day) -> None:
        for t in [t for t in self._pos if self.last_traded[t] < day]:
            last = self.last_traded[t]
            price = self.data.adj_close.at[last, t]
            self._pos[t].trade.delisted = True
            self._res.log.append(f"{day.date()} {t}: no trades since {last.date()}, sold at last close")
            self._sell(day, t, self._pos[t].units * price, price, everything=True)

    def _rebalance(self, day, signal_day, weights: dict[str, float]) -> None:
        opens = self.data.adj_open.loc[day]
        tradable = {t for t in set(weights) | set(self._pos)
                    if t in opens.index and pd.notna(opens[t])}
        px = {t: opens[t] for t in tradable}
        held = {t: p.units * (px[t] if t in px else self.marks.at[day, t])
                for t, p in self._pos.items()}
        equity = self._cash + sum(held.values())
        min_trade = self.s.min_trade_value

        buys: list[tuple[str, float]] = []
        for t in sorted(set(weights) | set(self._pos)):
            now = held.get(t, 0.0)
            target = weights.get(t, 0.0) * equity
            if target > now:
                cap = self.s.max_adv_fraction * self.adv.at[signal_day, t] if t in self.adv else 0.0
                target = max(now, min(target, cap if pd.notna(cap) else 0.0))
            delta = target - now
            if abs(delta) < 1e-6 or (target > 0 and abs(delta) < min_trade):
                continue
            if t not in tradable:
                self._res.skipped_orders += 1
                continue
            if delta < 0:
                self._sell(day, t, now if target == 0 else -delta, px[t], everything=target == 0)
            else:
                buys.append((t, delta))

        # Scale buys down so their value plus commissions fits in cash.
        scale = 1.0
        for _ in range(20):
            need = sum(v * scale + fees.commission(self.s.costs, v * scale) for _, v in buys)
            if need <= self._cash + 1e-6:
                break
            spare = self._cash - sum(fees.commission(self.s.costs, v * scale) for _, v in buys)
            scale = max(0.0, min(scale * 0.9999, spare / sum(v for _, v in buys)))
        for t, v in buys:
            if v * scale >= min(min_trade, v) and v * scale > 0:
                self._buy(day, t, v * scale, px[t])

    def _mark(self, day) -> float:
        total = 0.0
        for t, p in self._pos.items():
            value = p.units * self.marks.at[day, t]
            total += value
            if value < p.basis - 1e-9:
                if p.streak == 0:
                    p.streak_from = day
                p.streak += 1
                if p.streak > p.trade.longest_underwater:
                    p.trade.longest_underwater = p.streak
                    p.trade.underwater_from, p.trade.underwater_to = p.streak_from, day
            else:
                p.streak = 0
        return total


def _clean(weights: dict[str, float]) -> dict[str, float]:
    """Long only; weights above 100% in total are scaled down to 100%."""
    w = {str(t): float(v) for t, v in weights.items() if v and v > 0}
    total = sum(w.values())
    if total > 1.0:
        w = {t: v / total for t, v in w.items()}
    return w
