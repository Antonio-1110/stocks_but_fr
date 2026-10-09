"""python -m radar_backtest [--config ../radar.toml] <strategy>"""

from __future__ import annotations

import argparse
from pathlib import Path

from . import config, data, runner


def main() -> None:
    ap = argparse.ArgumentParser(prog="radar_backtest", description=__doc__)
    ap.add_argument("strategy", help="buy_and_hold[:TICKER] or a file name in backtest/strategies/")
    ap.add_argument("--config", default=str(runner.BACKTEST_DIR.parent / "radar.toml"))
    ap.add_argument("--no-save", action="store_true", help="don't write to backtest_runs")
    args = ap.parse_args()

    settings = config.load(args.config)
    md = data.load(settings.db_path)
    out = runner.run(md, settings, runner.strategy_factory(args.strategy, settings),
                     save=not args.no_save)
    for p in out["periods"]:
        s, b = p["summary"], p["bench_summary"]
        print(f"{p['period']:>6} {s['start']}..{s['end']}  CAGR {s['cagr']:.2%}  "
              f"maxDD {s['max_drawdown']:.2%}  | {p['benchmark']} CAGR {b['cagr']:.2%}")
    for path in out["reports"]:
        print("report:", Path(path))


if __name__ == "__main__":
    main()
