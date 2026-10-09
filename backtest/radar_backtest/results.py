"""The backtest_runs table: one row per strategy per period.

This engine owns its schema. internal/site shows the newest row as-is, so
new columns can be added here without touching Go.
"""

from __future__ import annotations

import json
import sqlite3
from pathlib import Path

SCHEMA = """
CREATE TABLE IF NOT EXISTS backtest_runs (
	id                        INTEGER PRIMARY KEY AUTOINCREMENT,
	run_at                    TEXT NOT NULL,
	strategy                  TEXT NOT NULL,
	period                    TEXT NOT NULL,
	start_date                TEXT NOT NULL,
	end_date                  TEXT NOT NULL,
	params                    TEXT NOT NULL DEFAULT '{}',
	initial_capital           REAL,
	final_equity              REAL,
	total_return              REAL,
	cagr                      REAL,
	max_drawdown              REAL,
	sharpe                    REAL,
	trades                    INTEGER,
	win_rate                  REAL,
	worst_trade_pct           REAL,
	worst_trade_ticker        TEXT,
	peak_deployed             REAL,
	longest_underwater_days   INTEGER,
	longest_underwater_ticker TEXT,
	annual_turnover           REAL,
	total_costs               REAL,
	benchmark                 TEXT,
	bench_total_return        REAL,
	bench_cagr                REAL,
	bench_max_drawdown        REAL,
	bench_sharpe              REAL,
	report                    TEXT
);
"""

_COLUMNS = ["total_return", "cagr", "max_drawdown", "sharpe", "trades", "win_rate",
            "worst_trade_pct", "worst_trade_ticker", "peak_deployed",
            "longest_underwater_days", "longest_underwater_ticker",
            "annual_turnover", "total_costs", "initial_capital", "final_equity"]
_BENCH = ["total_return", "cagr", "max_drawdown", "sharpe"]


def save(db_path: str | Path, run_at: str, strategy: str, params: dict,
         periods: list[dict], report: str) -> None:
    """periods: dicts with keys period, summary, bench_summary, benchmark."""
    con = sqlite3.connect(db_path)
    try:
        con.executescript(SCHEMA)
        for p in periods:
            s, b = p["summary"], p["bench_summary"]
            row = {"run_at": run_at, "strategy": strategy, "period": p["period"],
                   "start_date": s["start"], "end_date": s["end"],
                   "params": json.dumps(params, sort_keys=True, ensure_ascii=False),
                   "benchmark": p["benchmark"], "report": report}
            row.update({c: s[c] for c in _COLUMNS})
            row.update({f"bench_{c}": b[c] for c in _BENCH})
            cols = ", ".join(row)
            con.execute(f"INSERT INTO backtest_runs ({cols}) VALUES ({', '.join('?' * len(row))})",
                        list(row.values()))
        con.commit()
    finally:
        con.close()
