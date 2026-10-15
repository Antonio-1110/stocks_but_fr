"""Turns a RunResult into the numbers every report shows."""

from __future__ import annotations

import math

import pandas as pd

from .engine import RunResult

TRADING_DAYS = 252


def summarize(res: RunResult, bench: RunResult | None = None) -> dict:
    """Headline numbers for one run. With `bench` (the benchmark run over the
    same days), also the benchmark-relative ones."""
    eq = res.equity
    years = _years(res)
    final = float(eq.iloc[-1])
    daily = eq.pct_change().dropna()
    sd = daily.std()
    everything = res.trades + res.open_trades
    worst = min(everything, key=lambda t: t.ret, default=None)
    under = max(everything, key=lambda t: t.longest_underwater, default=None)
    wins = sum(t.pnl > 0 for t in res.trades)
    out = {
        "start": res.start.date().isoformat(),
        "end": res.end.date().isoformat(),
        "initial_capital": res.initial_capital,
        "final_equity": final,
        "total_return": final / res.initial_capital - 1,
        "cagr": _cagr(res),
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
        # Added in #54. Older keys above are unchanged.
        "volatility": float(sd * math.sqrt(TRADING_DAYS)) if len(daily) > 1 else None,
        "sortino": sortino(daily),
        **_longest_drawdown(eq),
        "avg_exposure": float((res.invested / eq).mean()),
        "top5_pnl_share": top_share(everything, 5),
        # Orders a price limit stopped (#53); 0 on engines without the rule.
        "limit_blocked": int(getattr(res, "limit_blocked", 0) or 0),
    }
    if bench is not None:
        out.update(relative(eq, bench.equity, out["cagr"], _cagr(bench)))
    return out


def _years(res: RunResult) -> float:
    return max((res.end - res.start).days / 365.25, 1 / 365.25)


def _cagr(res: RunResult) -> float:
    return (float(res.equity.iloc[-1]) / res.initial_capital) ** (1 / _years(res)) - 1


def sortino(daily: pd.Series) -> float | None:
    """Annualised mean daily return over annualised downside deviation, both
    against 0 (rf = 0). Downside deviation counts every day, up days as 0."""
    if len(daily) < 2:
        return None
    down = math.sqrt(float((daily.clip(upper=0) ** 2).mean()))
    return float(daily.mean() / down * math.sqrt(TRADING_DAYS)) if down > 0 else None


def _longest_drawdown(eq: pd.Series) -> dict:
    """Longest spell below a previous peak, from the peak's close to the first
    close back at or above it, in trading days. A spell still open at the end
    runs to the last day and is marked not recovered."""
    values = eq.to_numpy()
    best = (0, 0, 0, True)          # length, peak index, end index, recovered
    peak_i = 0
    for i in range(1, len(values)):
        if values[i] >= values[peak_i]:
            if i - peak_i >= 2 and i - peak_i > best[0]:
                best = (i - peak_i, peak_i, i, True)
            peak_i = i
    last = len(values) - 1
    if last > peak_i and last - peak_i > best[0]:
        best = (last - peak_i, peak_i, last, False)
    days, a, b, recovered = best
    if not days:
        return {"longest_drawdown_days": 0, "longest_drawdown_dates": None}
    span = _span(eq.index[a], eq.index[b])
    return {"longest_drawdown_days": days,
            "longest_drawdown_dates": span if recovered else span + " (not recovered)"}


def top_share(trades: list, n: int) -> float | None:
    """Share of the total P&L that the n most profitable trades made. None
    when the total isn't positive (the share has no meaning then)."""
    total = sum(t.pnl for t in trades)
    if not trades or total <= 0:
        return None
    return sum(sorted((t.pnl for t in trades), reverse=True)[:n]) / total


def relative(eq: pd.Series, bench_eq: pd.Series, cagr: float, bench_cagr: float) -> dict:
    """Strategy against the benchmark over the same days. Tracking error is the
    annualised standard deviation of daily active returns (strategy minus
    benchmark); the information ratio is their annualised mean over it."""
    active = (eq.pct_change() - bench_eq.reindex(eq.index).ffill().pct_change()).dropna()
    te = float(active.std() * math.sqrt(TRADING_DAYS)) if len(active) > 1 else None
    return {
        "excess_cagr": cagr - bench_cagr,
        "tracking_error": te,
        "information_ratio": float(active.mean() * TRADING_DAYS / te) if te else None,
    }


def calendar_years(eq: pd.Series, initial_capital: float) -> pd.Series:
    """Return per calendar year, from the previous year's last close (the
    initial capital for the first year) to this year's last close. The first
    and last years are partial when the run starts or ends mid-year."""
    if eq.empty:
        return pd.Series(dtype=float)
    ends = eq.groupby(eq.index.year).last()
    starts = ends.shift(1)
    starts.iloc[0] = initial_capital
    return ends / starts - 1


def price_return(adj_close: pd.DataFrame, ticker: str, start, end) -> float | None:
    """Plain total return of one ticker's adjusted close, no costs: the
    "actual" return to compare a buy-and-hold run with."""
    s = adj_close[ticker].loc[start:end].dropna() if ticker in adj_close else pd.Series()
    return float(s.iloc[-1] / s.iloc[0] - 1) if len(s) >= 2 else None


def _span(a, b) -> str:
    return f"{a.date()} to {b.date()}"
