"""Turns a RunResult into the numbers every report shows."""

from __future__ import annotations

import math

import pandas as pd

from .engine import RunResult

TRADING_DAYS = 252


def summarize(res: RunResult) -> dict:
    eq = res.equity
    years = max((res.end - res.start).days / 365.25, 1 / 365.25)
    final = float(eq.iloc[-1])
    daily = eq.pct_change().dropna()
    sd = daily.std()
    everything = res.trades + res.open_trades
    worst = min(everything, key=lambda t: t.ret, default=None)
    under = max(everything, key=lambda t: t.longest_underwater, default=None)
    wins = sum(t.pnl > 0 for t in res.trades)
    return {
        "start": res.start.date().isoformat(),
        "end": res.end.date().isoformat(),
        "initial_capital": res.initial_capital,
        "final_equity": final,
        "total_return": final / res.initial_capital - 1,
        "cagr": (final / res.initial_capital) ** (1 / years) - 1,
        "max_drawdown": float((eq / eq.cummax() - 1).min()),
        # Risk-free rate taken as 0; annualised from daily returns.
        "sharpe": float(daily.mean() / sd * math.sqrt(TRADING_DAYS)) if sd > 0 else None,
        "trades": len(res.trades),
        "open_trades": len(res.open_trades),
        "win_rate": wins / len(res.trades) if res.trades else None,
        "worst_trade_pct": worst.ret if worst else None,
        "worst_trade_ntd": worst.pnl if worst else None,
        "worst_trade_ticker": worst.ticker if worst else None,
        "worst_trade_dates": _span(worst.opened, worst.closed or res.end) if worst else None,
        "peak_deployed": float(res.invested.max()),
        "peak_deployed_pct": float((res.invested / eq).max()),
        "longest_underwater_days": under.longest_underwater if under else 0,
        "longest_underwater_ticker": under.ticker if under and under.longest_underwater else None,
        "longest_underwater_dates": (_span(under.underwater_from, under.underwater_to)
                                     if under and under.longest_underwater else None),
        # Average of buys and sells, per year, as a fraction of average equity.
        "annual_turnover": res.traded_value / 2 / float(eq.mean()) / years,
        "commission_paid": res.commission_paid,
        "tax_paid": res.tax_paid,
        "total_costs": res.commission_paid + res.tax_paid,
        "skipped_orders": res.skipped_orders,
    }


def price_return(adj_close: pd.DataFrame, ticker: str, start, end) -> float | None:
    """Plain total return of one ticker's adjusted close, no costs: the
    "actual" return to compare a buy-and-hold run with."""
    s = adj_close[ticker].loc[start:end].dropna() if ticker in adj_close else pd.Series()
    return float(s.iloc[-1] / s.iloc[0] - 1) if len(s) >= 2 else None


def _span(a, b) -> str:
    return f"{a.date()} to {b.date()}"
