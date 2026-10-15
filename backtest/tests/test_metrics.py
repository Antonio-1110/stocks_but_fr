"""Metrics on hand-made equity curves whose answers can be worked out on paper
(issue #54)."""

import math
import sqlite3

import numpy as np
import pandas as pd
import pytest

from radar_backtest import metrics, report, results
from radar_backtest.engine import RunResult, Trade

DAYS = pd.bdate_range("2020-01-01", periods=10)


def _run(values, invested=None, trades=(), open_trades=(), days=DAYS, **extra):
    eq = pd.Series(values, index=days[: len(values)], dtype=float)
    inv = pd.Series(invested if invested is not None else values, index=eq.index, dtype=float)
    res = RunResult("t", {}, eq.index[0], eq.index[-1], 100.0, eq, inv, list(trades),
                    list(open_trades))
    for k, v in extra.items():
        setattr(res, k, v)
    return res


def _trade(pnl):
    return Trade("X", DAYS[0], DAYS[1], invested=100.0, received=100.0 + pnl)


def test_existing_keys_are_kept():
    s = metrics.summarize(_run([100, 101, 102]))
    for k in ("start", "end", "cagr", "max_drawdown", "sharpe", "trades", "win_rate",
              "worst_trade_pct", "peak_deployed", "longest_underwater_days",
              "annual_turnover", "total_costs", "skipped_orders"):
        assert k in s


def test_volatility_and_sortino():
    # Daily returns alternate +2% and -1%.
    v = [100.0]
    for i in range(9):
        v.append(v[-1] * (1.02 if i % 2 == 0 else 0.99))
    s = metrics.summarize(_run(v))
    r = pd.Series(v).pct_change().dropna()
    assert s["volatility"] == pytest.approx(r.std() * math.sqrt(252))
    # Downside deviation over all 9 days: 4 days of -1%.
    down = math.sqrt(4 * 0.01 ** 2 / 9)
    assert s["sortino"] == pytest.approx(r.mean() / down * math.sqrt(252))


def test_sortino_is_none_without_down_days():
    assert metrics.summarize(_run([100, 101, 102, 103]))["sortino"] is None


def test_longest_drawdown_peak_to_recovery():
    # Peak on day 1, under water days 2-4, back to 110 on day 5 (4 trading
    # days). A shorter dip later (day 7 to day 8) doesn't count.
    s = metrics.summarize(_run([100, 110, 105, 95, 108, 110, 112, 111, 113, 114]))
    assert s["longest_drawdown_days"] == 4
    assert s["longest_drawdown_dates"] == f"{DAYS[1].date()} to {DAYS[5].date()}"
    assert s["max_drawdown"] == pytest.approx(95 / 110 - 1)


def test_longest_drawdown_not_recovered():
    s = metrics.summarize(_run([100, 120, 110, 100, 105, 101]))
    assert s["longest_drawdown_days"] == 4
    assert s["longest_drawdown_dates"].endswith("(not recovered)")


def test_no_drawdown():
    s = metrics.summarize(_run([100, 100, 101, 102]))
    assert s["longest_drawdown_days"] == 0 and s["longest_drawdown_dates"] is None


def test_average_exposure():
    s = metrics.summarize(_run([100, 100, 100, 100], invested=[0, 50, 100, 50]))
    assert s["avg_exposure"] == pytest.approx(0.5)


def test_top5_share():
    trades = [_trade(p) for p in (50, 30, 10, 10, 5, 5, -10)]
    s = metrics.summarize(_run([100, 101], trades=trades[:4], open_trades=trades[4:]))
    # Total 100; the five best are 50+30+10+10+5.
    assert s["top5_pnl_share"] == pytest.approx(1.05)
    assert metrics.top_share([_trade(-5), _trade(2)], 5) is None


def test_relative_to_benchmark():
    # The strategy earns the benchmark's return plus 0.1% every day: the
    # active return is constant, so tracking error is ~0 and only excess CAGR
    # is meaningful. Then a noisy case checked against the definition.
    days = pd.bdate_range("2020-01-01", periods=253)
    rng = np.random.default_rng(1)
    b = 100 * np.cumprod(np.r_[1, 1 + rng.normal(0.0005, 0.01, 252)])
    a = 100 * np.cumprod(np.r_[1, 1 + rng.normal(0.001, 0.015, 252)])
    res, bres = _run(a, days=days), _run(b, days=days)
    s = metrics.summarize(res, bres)
    active = pd.Series(a).pct_change() - pd.Series(b).pct_change()
    te = active.std() * math.sqrt(252)
    assert s["excess_cagr"] == pytest.approx(s["cagr"] - metrics.summarize(bres)["cagr"])
    assert s["tracking_error"] == pytest.approx(te)
    assert s["information_ratio"] == pytest.approx(active.mean() * 252 / te)
    # Without a benchmark these keys are absent.
    assert "excess_cagr" not in metrics.summarize(res)


def test_limit_blocked_read_when_present():
    assert metrics.summarize(_run([100, 101]))["limit_blocked"] == 0
    assert metrics.summarize(_run([100, 101], limit_blocked=3))["limit_blocked"] == 3


def test_calendar_years():
    days = pd.DatetimeIndex(["2019-12-30", "2019-12-31", "2020-06-30", "2020-12-31", "2021-03-01"])
    eq = pd.Series([95, 110, 100, 121, 133.1], index=days)
    y = metrics.calendar_years(eq, 100.0)
    assert list(y.index) == [2019, 2020, 2021]
    assert y.to_list() == pytest.approx([0.10, 0.10, 0.10])


def _period(values, bench):
    days = pd.bdate_range("2019-12-02", periods=len(values), freq="7B")
    res, bres = _run(values, days=days), _run(bench, days=days)
    return {"period": "design", "benchmark": "0050", "summary": metrics.summarize(res, bres),
            "bench_summary": metrics.summarize(bres), "bench_price_return": 0.1,
            "equity": res.equity, "bench_equity": bres.equity, "log": []}


def test_report_shows_new_metrics_and_years():
    p = _period([100, 105, 102, 120, 130, 125], [100, 101, 99, 104, 108, 110])
    md = report.markdown("s", {}, "now", "db", [p])
    for label in ("Sortino", "Volatility", "Longest drawdown", "Average exposure",
                  "Excess CAGR", "Tracking error", "Information ratio", "top 5 trades",
                  "price limits", "## Calendar-year returns"):
        assert label in md
    assert "| 2019 (partial) | design |" in md
    assert "Calendar-year returns" in report.html_page("s", {}, "now", "db", [p])


def test_results_add_columns_to_an_old_table(tmp_path):
    db = tmp_path / "radar.db"
    con = sqlite3.connect(db)
    con.executescript(results.SCHEMA)  # the table as it first shipped
    con.close()
    p = _period([100, 105, 102, 120], [100, 101, 99, 104])
    results.save(db, "now", "s", {}, [p], "r.md")
    con = sqlite3.connect(db)
    con.row_factory = sqlite3.Row
    row = con.execute("SELECT * FROM backtest_runs").fetchone()
    con.close()
    for c in ("volatility", "sortino", "longest_drawdown_days", "avg_exposure", "excess_cagr",
              "tracking_error", "information_ratio", "top5_pnl_share", "limit_blocked",
              "bench_volatility", "bench_sortino", "bench_longest_drawdown_days"):
        assert c in row.keys()
    assert row["excess_cagr"] == pytest.approx(p["summary"]["excess_cagr"])
    assert row["limit_blocked"] == 0
