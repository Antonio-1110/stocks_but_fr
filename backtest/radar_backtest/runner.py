"""Runs a strategy over the design and test periods against the benchmark."""

from __future__ import annotations

import datetime as dt
import importlib
import sys
from pathlib import Path
from typing import Callable

from . import metrics, report, results
from .baseline import BuyAndHold
from .config import Settings
from .data import MarketData
from .engine import Engine, Strategy

BACKTEST_DIR = Path(__file__).resolve().parent.parent

Factory = Callable[[], Strategy]


def strategy_factory(name: str, settings: Settings) -> Factory:
    """`buy_and_hold` or `buy_and_hold:<ticker>` is built in. Anything else is
    backtest/strategies/<name>.py, which must define
    `make(params: dict, settings: Settings) -> Strategy`, with params taken
    from radar.toml's [strategy.<name>] table."""
    if name == "buy_and_hold" or name.startswith("buy_and_hold:"):
        ticker = name.partition(":")[2] or settings.benchmark
        return lambda: BuyAndHold(ticker)
    if str(BACKTEST_DIR) not in sys.path:
        sys.path.insert(0, str(BACKTEST_DIR))
    module = importlib.import_module(f"strategies.{name}")
    params = settings.strategies.get(name, {})
    return lambda: module.make(params, settings)


def periods(settings: Settings) -> list[tuple[str, str, str | None]]:
    return [("design", settings.design_start, settings.design_end),
            ("test", settings.test_start, None)]


def run(data: MarketData, settings: Settings, factory: Factory,
        report_dir: Path | None = BACKTEST_DIR / "reports", save: bool = True,
        spans: list[tuple[str, str, str | None]] | None = None,
        now: dt.datetime | None = None) -> dict:
    engine = Engine(data, settings)
    bench = settings.benchmark
    out = []
    name, params = None, None
    for period, start, end in spans or periods(settings):
        strat = factory()
        name, params = strat.name, strat.params
        try:
            res = engine.run(strat, start, end)
        except ValueError as err:  # no data for this period yet
            print(f"skipping {period} period: {err}", file=sys.stderr)
            continue
        bres = engine.run(BuyAndHold(bench), start, end)
        out.append({
            "period": period,
            "benchmark": bench,
            "summary": metrics.summarize(res),
            "bench_summary": metrics.summarize(bres),
            "bench_price_return": metrics.price_return(data.adj_close, bench, res.start, res.end),
            "equity": res.equity,
            "bench_equity": bres.equity,
            "log": res.log,
        })

    if not out:
        raise ValueError("no period has price data")
    run_at = (now or dt.datetime.now()).strftime("%Y-%m-%d %H:%M:%S")
    source = str(settings.db_path)
    paths = []
    if report_dir is not None:
        report_dir.mkdir(parents=True, exist_ok=True)
        stem = report_dir / f"{name}-{run_at.replace(' ', '_').replace(':', '')}"
        stem.with_suffix(".md").write_text(report.markdown(name, params, run_at, source, out), encoding="utf-8")
        stem.with_suffix(".html").write_text(report.html_page(name, params, run_at, source, out), encoding="utf-8")
        paths = [stem.with_suffix(".md"), stem.with_suffix(".html")]
    if save:
        results.save(settings.db_path, run_at, name, params, out,
                     str(paths[0]) if paths else "")
    return {"strategy": name, "params": params, "periods": out, "reports": paths}
