"""Shared pieces for the active strategies (issues #70-#73): record-revenue
trades, breakouts, 投信 window dressing and pullbacks.

They all hold a few stocks for days or weeks and decide per stock ("sell this
one, buy that one with an eighth of equity"), which target weights can't say
without trimming every other holding. So they place orders and run on
strategies.dca's OrderEngine: same fills, costs, liquidity cap, price-limit
and halt rules as every other backtest. SwingEngine adds the account's equity
to the book the strategy sees, so each buy can be a fixed share of it.

Indicators are computed once over the whole price history with trailing
windows (rolling, shifted where the rule says "before today"), then read at
the view's day, so a strategy never sees a later price. Strategies take them
from the view's MarketData through `panel`, which caches per dataset.

Run one from backtest/:  python3 -m strategies.<name> [--no-save]
"""

from __future__ import annotations

import argparse
import dataclasses
import html
import re
import sys
from pathlib import Path
from typing import Callable
from unittest import mock

import pandas as pd

BACKTEST_DIR = Path(__file__).resolve().parents[1]
if str(BACKTEST_DIR) not in sys.path:
    sys.path.insert(0, str(BACKTEST_DIR))

from radar_backtest import config, data, runner  # noqa: E402
from radar_backtest.engine import RunResult  # noqa: E402
from strategies.dca import Holding, Order, OrderEngine, Stats  # noqa: E402

COMMON = re.compile(r"^[1-9]\d{3}$")  # four-digit common stocks; ETFs start with 00

__all__ = ["Book", "Holding", "Order", "SwingEngine", "Swing", "panel", "main"]


class Book(dict):
    """ticker -> Holding, plus the account at the signal day's close."""

    equity: float = 0.0
    cash: float = 0.0


class SwingEngine(OrderEngine):
    def run(self, strategy, start, end=None) -> RunResult:
        self._equity = self.s.initial_capital
        return super().run(strategy, start, end)

    def _mark(self, day) -> float:
        value = super()._mark(day)
        self._equity = self._cash + value
        return value

    def _book(self) -> Book:
        b = Book(super()._book())
        b.equity, b.cash = self._equity, self._cash
        return b


_PANELS: dict[tuple[int, str], pd.DataFrame | pd.Series] = {}


def panel(md: data.MarketData, key: str, build: Callable[[data.MarketData], object]):
    """build(md), computed once per dataset and key."""
    k = (id(md), key)
    if k not in _PANELS:
        _PANELS[k] = build(md)
    return _PANELS[k]


class Swing:
    """Base for the active strategies: params check, sizing and helpers."""

    name = "swing"
    DEFAULTS: dict = {}

    def __init__(self, params: dict):
        unknown = set(params) - set(self.DEFAULTS)
        if unknown:
            raise ValueError(f"unknown [strategy.{self.name}] keys: {sorted(unknown)}")
        self.params = {**self.DEFAULTS, **params}
        self.p = self.params

    def targets(self, view):
        raise RuntimeError(f"{self.name} places orders, not target weights: "
                           f"run `python3 -m strategies.{self.name}` from backtest/")

    def orders(self, view, book: Book) -> list[Order]:
        raise NotImplementedError

    # --- helpers ------------------------------------------------------------

    def slot(self, book: Book) -> float:
        """NTD for one new position: an equal share of equity."""
        return book.equity / int(self.p["max_positions"])

    def free(self, book: Book, selling: int = 0) -> int:
        return int(self.p["max_positions"]) - len(book) + selling

    @staticmethod
    def held_days(view, since: pd.Timestamp) -> int:
        """Trading days from `since` (a fill day) to the view's day."""
        cal = view._d.calendar
        return int(cal.searchsorted(view.day, "right") - cal.searchsorted(since, "left")) - 1

    @staticmethod
    def peak(view, ticker: str, since: pd.Timestamp) -> float:
        """Highest adjusted close from `since` to the view's day."""
        return float(view._d.adj_close.loc[since:view.day, ticker].max())

    def liquid(self, view) -> list[str]:
        """Common stocks listed today, traded today, with average traded value
        of at least min_adv_ntd over adv_days."""
        md = view._d
        n = int(self.p["adv_days"])
        adv = panel(md, f"adv{n}", lambda m: m.turnover.fillna(0.0).rolling(n, min_periods=n).mean())
        row = adv.loc[view.day]
        close = md.close.loc[view.day]
        listed = set(view.companies()["ticker"])
        ok = row[(row >= self.p["min_adv_ntd"]) & close.reindex(row.index).notna()]
        return [t for t in ok.index if t in listed and COMMON.match(t)]


# --- reporting ---------------------------------------------------------------

def trade_stats(res: RunResult, stats: Stats) -> dict:
    """How the strategy trades: count, hit rate, holding time, exits, costs."""
    cycles = stats.cycles
    closed = [c for c in cycles if c.closed is not None]
    years = max((res.end - res.start).days / 365.25, 1e-9)
    avg_eq = float(res.equity.mean()) if len(res.equity) else res.initial_capital
    rets = pd.Series([c.trade.ret for c in closed], dtype=float)
    reasons = pd.Series([c.reason for c in cycles], dtype=object).value_counts()
    return {
        "trades": len(cycles),
        "trades_per_year": len(cycles) / years,
        "win_rate": float((rets > 0).mean()) if len(rets) else None,
        "avg_trade": float(rets.mean()) if len(rets) else None,
        "median_trade": float(rets.median()) if len(rets) else None,
        "avg_days": (sum(c.days for c in closed) / len(closed)) if closed else None,
        "exits": ", ".join(f"{k}: {v}" for k, v in reasons.items()) or None,
        "turnover_per_year": res.traded_value / 2 / avg_eq / years,
        "costs_per_year": (res.commission_paid + res.tax_paid) / avg_eq / years,
        "exposure": float((res.invested / res.equity).mean()) if len(res.equity) else None,
        "adv_capped": stats.adv_capped,
        "limit_blocked_buys": res.limit_blocked_buys,
    }


ROWS = [("trades", "Trades", "n"), ("trades_per_year", "Trades a year", "f1"),
        ("win_rate", "Winning trades", "pct"), ("avg_trade", "Average trade, after costs", "pct"),
        ("median_trade", "Median trade, after costs", "pct"), ("avg_days", "Average days held", "f1"),
        ("exits", "Exits by reason", "s"), ("turnover_per_year", "Turnover a year (x equity)", "f1"),
        ("costs_per_year", "Fees and tax a year (% of equity)", "pct"),
        ("exposure", "Average share invested", "pct"),
        ("adv_capped", "Buys cut by the liquidity cap", "n"),
        ("limit_blocked_buys", "Buys skipped at limit-up", "n")]


def _fmt(v, kind) -> str:
    if v is None:
        return "–"
    if kind == "pct":
        return f"{v * 100:.2f}%"
    if kind == "f1":
        return f"{v:.1f}"
    return str(v)


def _append(paths, periods: list[tuple[str, dict]]) -> None:
    head = ["Metric"] + [p for p, _ in periods]
    rows = [[label] + [_fmt(s.get(k), kind) for _, s in periods] for k, label, kind in ROWS]
    md = ["", "## Trades", "", "| " + " | ".join(head) + " |", "|" + "---|" * len(head)]
    md += ["| " + " | ".join(r) + " |" for r in rows]
    ht = ("<h2>Trades</h2><div class=\"wrap\"><table><thead><tr>"
          + "".join(f"<th>{html.escape(h)}</th>" for h in head) + "</tr></thead><tbody>"
          + "".join("<tr>" + "".join(f"<td>{html.escape(c)}</td>" for c in r) + "</tr>" for r in rows)
          + "</tbody></table></div>")
    for path in paths:
        text = Path(path).read_text(encoding="utf-8")
        if str(path).endswith(".md"):
            text = text.rstrip("\n") + "\n" + "\n".join(md) + "\n"
        else:
            text = text.replace("</body>", ht + "</body>")
        Path(path).write_text(text, encoding="utf-8")


def run(md: data.MarketData, settings, make: Callable, name: str, save: bool = True,
        report_dir: Path | None = runner.BACKTEST_DIR / "reports", spans=None) -> dict:
    params = settings.strategies.get(name, {})
    made = []

    def factory():
        made.append(make(params, settings))
        return made[-1]

    with mock.patch.object(runner, "Engine", SwingEngine):
        out = runner.run(md, settings, factory, report_dir=report_dir, save=save, spans=spans)
    ran = [s for s in made if hasattr(s, "result")]
    periods = []
    for p, s in zip(out["periods"], ran):
        p["trades"] = trade_stats(s.result, s.stats)
        periods.append((p["period"], p["trades"]))
    if out["reports"]:
        _append(out["reports"], periods)
    return out


def main(make: Callable, name: str, argv=None) -> None:
    ap = argparse.ArgumentParser(prog=f"strategies.{name}", description=f"Backtest {name}.")
    ap.add_argument("--config", default=str(BACKTEST_DIR.parent / "radar.toml"))
    ap.add_argument("--db", help="database to read instead of radar.toml's db_path")
    ap.add_argument("--no-save", action="store_true", help="don't write to backtest_runs")
    args = ap.parse_args(argv)
    settings = config.load(args.config)
    if args.db:
        settings = dataclasses.replace(settings, db_path=Path(args.db))
    md = data.load(settings.db_path, min_coverage=settings.min_coverage)
    out = run(md, settings, make, name, save=not args.no_save)
    for p in out["periods"]:
        s, t, b = p["summary"], p["trades"], p["bench_summary"]
        print(f"{name:>16} {p['period']:>6} {s['start']}..{s['end']}  CAGR {s['cagr']:.2%}  "
              f"maxDD {s['max_drawdown']:.2%}  trades {t['trades']}  win {_fmt(t['win_rate'], 'pct')}  "
              f"avg {_fmt(t['avg_trade'], 'pct')}  days {_fmt(t['avg_days'], 'f1')}  "
              f"costs/yr {_fmt(t['costs_per_year'], 'pct')}  | {p['benchmark']} CAGR {b['cagr']:.2%}")
    for path in out["reports"]:
        print("report:", path)
