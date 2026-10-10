"""Strategy A on a made-up market: which names it picks, when, and when it sells.
Prices and revenue are synthetic; nothing here says anything about real returns."""

import sqlite3

import numpy as np
import pandas as pd
import pytest

from radar_backtest import data, runner
from radar_backtest.engine import Engine

from . import fixture

DAYS = pd.bdate_range("2018-01-01", "2019-12-31")
CRASH = pd.Timestamp("2019-06-20")   # 1106 drops 25% here
BAD_MONTH = pd.Timestamp("2019-02-01")  # 1107's one weak month, public after 2019-03-10

# ticker: (price path, revenue YoY %, NTD traded a day, 投信 net shares a day)
STOCKS = {
    "1101": ("up", 30.0, 5e7, 100),     # the textbook pick
    "1102": ("down", 30.0, 5e7, 100),   # growing revenue, price far below its high
    "1103": ("up", -5.0, 5e7, 100),     # near its high, shrinking revenue
    "1104": ("up", 30.0, 1e6, 100),     # too illiquid
    "1105": ("up", 25.0, 5e7, -100),    # good, but 投信 are selling
    "1106": ("crash", 40.0, 5e7, 100),  # best, until it falls 25%
    "1107": ("up", 30.0, 5e7, 100),     # one bad month breaks its streak
}


def _path(kind: str) -> np.ndarray:
    i = np.arange(len(DAYS))
    if kind == "down":
        return 100 * 0.999 ** i
    p = 100 * 1.001 ** i
    if kind == "crash":
        p = np.where(DAYS >= CRASH, p * 0.75, p)
    return p


@pytest.fixture
def md(tmp_path):
    db = tmp_path / "radar.db"
    con = sqlite3.connect(db)
    con.executescript(fixture.SCHEMA)
    months = pd.date_range("2017-01-01", "2019-11-01", freq="MS")
    for t, (kind, yoy, turnover, trust) in STOCKS.items():
        c = _path(kind)
        con.executemany("INSERT INTO prices VALUES (?,?,?,?,?,?,?,?,?,?)", [
            ("TW", t, d.strftime("%Y-%m-%d"), x, x, x, x, x, 1000, turnover) for d, x in zip(DAYS, c)])
        con.execute("INSERT INTO companies VALUES (?,?,?,?,?,?,?)",
                    ("TW", t, t, "Electronics", "TWSE", "2000-01-01", ""))
        con.executemany("INSERT INTO monthly_revenue VALUES (?,?,?,?,?,?,?)", [
            ("TW", t, m.strftime("%Y-%m-%d"), 100.0,
             -50.0 if (t == "1107" and m == BAD_MONTH) else yoy, 0.0,
             (m + pd.DateOffset(months=1, days=9)).strftime("%Y-%m-%d")) for m in months])
        con.executemany("INSERT INTO institutional_flows VALUES (?,?,?,?,?,?)", [
            ("TW", t, d.strftime("%Y-%m-%d"), 0, trust, 0) for d in DAYS])
    # The benchmark, so the runner has something to compare with.
    c = _path("up")
    con.executemany("INSERT INTO prices VALUES (?,?,?,?,?,?,?,?,?,?)", [
        ("TW", "0050", d.strftime("%Y-%m-%d"), x, x, x, x, x, 1000, 2e9) for d, x in zip(DAYS, c)])
    con.execute("INSERT INTO companies VALUES (?,?,?,?,?,?,?)",
                ("TW", "0050", "元大台灣50", "ETF", "TWSE", "2003-06-30", ""))
    con.commit()
    con.close()
    return data.load(db)


@pytest.fixture
def settings(tmp_path):
    return fixture.settings(tmp_path / "radar.db")


def make(settings, **params):
    base = settings.strategies["revenue_momentum"]
    return runner.strategy_factory("revenue_momentum", settings)().__class__({**base, **params})


def walk(md, strat, start="2019-01-01", end="2019-12-31"):
    """Feed the strategy every close; return {day: targets} for days it traded."""
    out = {}
    for day in md.calendar[(md.calendar >= start) & (md.calendar <= end)]:
        w = strat.targets(md.view(day))
        if w is not None:
            out[day] = w
    return out


def rebalances(strat):
    return {d: h for d, why, h in strat.history if why == "rebalance"}


def test_params_come_from_radar_toml(settings):
    s = runner.strategy_factory("revenue_momentum", settings)()
    assert s.params["top_n"] == 12 and s.params["exit_rank"] == 30
    assert s.name == "revenue_momentum"
    assert make(settings, trust_filter=True).name == "revenue_momentum_trust"
    with pytest.raises(ValueError):
        make(settings, typo=1)


def test_rebalances_once_a_month_after_the_revenue_deadline(md, settings):
    strat = make(settings)
    walk(md, strat)
    days = sorted(rebalances(strat))
    assert len(days) == 12
    assert pd.Timestamp("2019-06-11") in days   # the 10th was a Monday
    assert pd.Timestamp("2019-08-13") in days   # the 10th was a Saturday: due Monday the 12th
    assert pd.Timestamp("2019-08-12") not in days


def test_picks_persistent_growth_near_the_high(md, settings):
    strat = make(settings)
    walk(md, strat, end="2019-02-28")
    assert rebalances(strat)[pd.Timestamp("2019-02-12")] == ["1101", "1105", "1106", "1107"]


def test_trust_filter_drops_names_trusts_are_selling(md, settings):
    strat = make(settings, trust_filter=True)
    walk(md, strat, end="2019-02-28")
    assert rebalances(strat)[pd.Timestamp("2019-02-12")] == ["1101", "1106", "1107"]


def test_uses_last_month_as_soon_as_it_is_due(md, settings):
    # 1107's February revenue (YoY -50%) is due 2019-03-10 (a Sunday, so Monday the 11th), so the 2019-03-12
    # rebalance must already drop it, and keep doing so while February is in
    # the three-month window.
    strat = make(settings)
    walk(md, strat, end="2019-06-30")
    reb = rebalances(strat)
    assert "1107" in reb[pd.Timestamp("2019-02-12")]
    for day in ("2019-03-12", "2019-04-11", "2019-05-13"):
        assert "1107" not in reb[pd.Timestamp(day)], day
    assert "1107" in reb[pd.Timestamp("2019-06-11")]


def test_stop_loss_sells_between_rebalances(md, settings):
    strat = make(settings)
    out = walk(md, strat, start="2019-06-01", end="2019-07-31")
    stops = [(d, h) for d, why, h in strat.history if why == "stop"]
    assert stops == [(CRASH, ["1106"])]
    w = out[CRASH]
    assert "1106" not in w and set(w) == {"1101", "1105", "1107"}
    # The rest keep their drifted weights instead of being re-levelled.
    assert all(v == pytest.approx(w["1101"]) for v in w.values())
    assert w["1101"] > 1 / 12
    # A month later it is far below its high and stays out.
    assert "1106" not in rebalances(strat)[pd.Timestamp("2019-07-11")]


def test_holdings_stay_until_they_drop_out_of_exit_rank(md, settings):
    # With one slot, 1106 (YoY 40%) outranks 1101 (YoY 30%). Already holding
    # 1101, the strategy keeps it while it ranks within exit_rank.
    day = pd.Timestamp("2019-02-12")
    for exit_rank, want in ((1, "1106"), (3, "1101")):
        strat = make(settings, top_n=1, exit_rank=exit_rank)
        strat._units, strat._entry = {"1101": 1.0}, {"1101": 100.0}
        strat._done_month = None
        assert list(strat.targets(md.view(day))) == [want], exit_rank


def test_runs_through_the_engine(md, settings):
    res = Engine(md, settings).run(make(settings), "2019-01-02", "2019-12-31")
    assert res.skipped_orders == 0
    held = {t.ticker for t in res.trades + res.open_trades}
    assert held <= {"1101", "1105", "1106", "1107"}
    # 1106 is bought, stopped out after the crash, and never bought again.
    [f106] = [t for t in res.trades if t.ticker == "1106"]
    assert f106.closed == CRASH + pd.offsets.BDay(1)
