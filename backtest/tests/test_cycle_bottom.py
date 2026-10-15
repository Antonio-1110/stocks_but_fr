"""cycle_bottom on a made-up market: one cyclical stock whose revenue YoY sits
at -20% for a year, turns up, then peaks and rolls over. Synthetic numbers."""

import dataclasses
import sqlite3

import numpy as np
import pandas as pd
import pytest

from radar_backtest import data, runner
from radar_backtest.engine import Engine

from . import fixture

DAYS = pd.bdate_range("2019-01-01", "2020-06-30")

# YoY (%) per month for 2019 and 2020; 2017 revenue is 100 a month, 2018 is 80.
YOY_2019 = [-20, -20, -20, -20, -20, -20, -15, -10, -5, 0, 5, 10]
YOY_2020 = [15, 20, 18, 16, 20, 20]


def _revenue(ticker):
    rows, level = [], {}
    for y in (2017, 2018):
        for m in range(1, 13):
            level[(y, m)] = 100.0 if y == 2017 else 80.0
    for y, yoys in ((2019, YOY_2019), (2020, YOY_2020)):
        for m, g in enumerate(yoys, 1):
            level[(y, m)] = level[(y - 1, m)] * (1 + g / 100)
    for (y, m), r in sorted(level.items()):
        month = pd.Timestamp(y, m, 1)
        announced = (month + pd.DateOffset(months=1, days=9)).strftime("%Y-%m-%d")
        rows.append(("TW", ticker, month.strftime("%Y-%m-%d"), r, None, None, announced))
    return rows


def _prices(ticker, closes):
    return [("TW", ticker, d.strftime("%Y-%m-%d"), c, c, c, c, c, 1000, 1e9)
            for d, c in zip(DAYS, closes)]


def build(path, crash=False):
    con = sqlite3.connect(path)
    con.executescript(fixture.SCHEMA)
    i = np.arange(len(DAYS))
    up = 50 * 1.0005 ** i
    cyc = up.copy()
    if crash:  # falls 40% in early November 2019
        cyc = np.where(DAYS >= pd.Timestamp("2019-11-01"), up * 0.6, up)
    rows = _prices("0050", up) + _prices("2408", cyc) + _prices("1201", up)
    con.executemany("INSERT INTO prices VALUES (?,?,?,?,?,?,?,?,?,?)", rows)
    con.executemany("INSERT INTO companies VALUES (?,?,?,?,?,?,?)", [
        ("TW", "0050", "元大台灣50", "ETF", "TWSE", "2003-06-30", ""),
        ("TW", "2408", "Memory Co", "半導體業", "TWSE", "2000-01-01", ""),
        ("TW", "1201", "Food Co", "食品工業", "TWSE", "2000-01-01", ""),
    ])
    con.executemany("INSERT INTO monthly_revenue VALUES (?,?,?,?,?,?,?)",
                    _revenue("2408") + _revenue("1201"))
    con.commit()
    con.close()
    return path


def run(tmp_path, crash=False, **params):
    db = build(tmp_path / "radar.db", crash)
    s = fixture.settings(db)
    s = dataclasses.replace(s, strategies={"cycle_bottom": {"yoy_window": 1, **params}})
    strat = runner.strategy_factory("cycle_bottom", s)()
    return Engine(data.load(db), s).run(strat, DAYS[0], DAYS[-1])


def test_growth_uses_trailing_window():
    from strategies.cycle_bottom import growth
    months = pd.date_range("2018-01-01", "2019-03-01", freq="MS")
    rev = pd.DataFrame({"ticker": "2408", "month": months,
                        "revenue": [10.0] * 12 + [11.0, 12.0, 13.0]})
    g = growth(rev, 3)["2408"]
    assert np.isnan(g.iloc[11])
    assert g.loc["2019-03-01"] == pytest.approx((11 + 12 + 13) / 30 * 100 - 100)
    assert growth(rev, 1)["2408"].loc["2019-01-01"] == pytest.approx(10.0)


def test_buys_the_turn_and_sells_the_rollover(tmp_path):
    res = run(tmp_path)
    buys = [f for f in res.fills if f.side == "buy"]
    sells = [f for f in res.fills if f.side == "sell"]
    # Only the cyclical stock is bought, the day after August's revenue is out
    # (the second month of rising YoY), and filled at the next open.
    assert {f.ticker for f in res.fills} == {"2408"}
    assert buys[0].day == pd.Timestamp("2019-09-12")
    # 1/max_positions of equity, not everything.
    assert buys[0].value == pytest.approx(res.initial_capital / 10, rel=0.01)
    # YoY 20 -> 18 -> 16 (Feb..Apr 2020): sold after April's revenue is out.
    assert [f.day for f in sells] == [pd.Timestamp("2020-05-12")]
    assert len(res.trades) == 1 and not res.open_trades


def test_entry_needs_a_trough(tmp_path):
    # Demanding 13 negative months out of 12 can never be met.
    res = run(tmp_path, min_negative_months=13)
    assert not res.fills


def test_late_recovery_is_not_chased(tmp_path):
    # With max_entry_yoy below -10 the August signal (-10%) is too late.
    res = run(tmp_path, max_entry_yoy=-12)
    assert not res.fills


def test_hard_stop_and_no_reentry_until_new_revenue(tmp_path):
    res = run(tmp_path, crash=True, price_ma_days=0)
    sells = [f for f in res.fills if f.side == "sell"]
    buys = [f for f in res.fills if f.side == "buy"]
    # Stopped the day after the crash close, filled the next open.
    assert sells[0].day == pd.Timestamp("2019-11-04")
    # September revenue (out 2019-10-10) was the latest month at the stop;
    # October's (deadline 2019-11-10) is still rising, so it re-enters then.
    assert buys[1].day == pd.Timestamp("2019-11-12")


def test_unknown_param_is_rejected(tmp_path):
    from strategies import cycle_bottom
    with pytest.raises(ValueError):
        cycle_bottom.make({"stoploss": 0.1}, None)


# --- survivorship (issue #55) ----------------------------------------------

LAST_TRADE = pd.Timestamp("2019-10-31")   # 2344's last trading day
DELISTED = "2019-11-15"                    # its delisting date


def build_delisted(path, listed_on=""):
    """The base market plus 2344, a memory maker with 2408's revenue and
    prices that stops trading on LAST_TRADE and is delisted on DELISTED.
    Ties in the entry score go to the lower ticker, so with one slot 2344 is
    picked first."""
    build(path)
    con = sqlite3.connect(path)
    i = np.arange(len(DAYS))
    live = int((DAYS <= LAST_TRADE).sum())
    con.executemany("INSERT INTO prices VALUES (?,?,?,?,?,?,?,?,?,?)",
                    _prices("2344", 50 * 1.0005 ** i)[:live])
    con.execute("INSERT INTO companies VALUES (?,?,?,?,?,?,?)",
                ("TW", "2344", "Gone Memory Co", "半導體業", "TWSE", listed_on, DELISTED))
    # Revenue stops after September 2019, like a company on its way out.
    rev = [r for r in _revenue("2344") if r[2] <= "2019-09-01"]
    con.executemany("INSERT INTO monthly_revenue VALUES (?,?,?,?,?,?,?)", rev)
    con.commit()
    con.close()
    return path


def run_delisted(tmp_path, listed_on="", **params):
    db = build_delisted(tmp_path / "radar.db", listed_on)
    s = fixture.settings(db)
    s = dataclasses.replace(s, strategies={"cycle_bottom": {"yoy_window": 1, "max_positions": 1, **params}})
    strat = runner.strategy_factory("cycle_bottom", s)()
    return Engine(data.load(db), s).run(strat, DAYS[0], DAYS[-1])


def test_a_later_delisted_stock_is_picked_and_held(tmp_path):
    res = run_delisted(tmp_path)
    first = [f for f in res.fills if f.side == "buy"][0]
    assert (first.ticker, first.day) == ("2344", pd.Timestamp("2019-09-12"))


def test_delisted_holding_exits_at_last_close_and_frees_its_slot(tmp_path):
    res = run_delisted(tmp_path)
    sells = [f for f in res.fills if f.ticker == "2344" and f.side == "sell"]
    # The engine sells it the day after its last trade, at that last close.
    assert [f.day for f in sells] == [pd.Timestamp("2019-11-01")]
    last = int((DAYS <= LAST_TRADE).sum()) - 1
    assert sells[0].price == pytest.approx(50 * 1.0005 ** last)
    assert [t for t in res.trades if t.ticker == "2344"][0].delisted
    # Until the delisting date it may only be suspended, so the slot stays
    # taken; then the strategy drops it and buys 2408 at the next open.
    later = [f for f in res.fills if f.ticker == "2408" and f.side == "buy"]
    assert later and later[0].day == pd.Timestamp("2019-11-18")


def test_not_bought_before_listing(tmp_path):
    # A listing date after the signal keeps 2344 out of the universe even
    # though it has prices, so 2408 takes the only slot instead.
    res = run_delisted(tmp_path, listed_on="2019-10-01")
    assert [f for f in res.fills if f.side == "buy"][0].ticker == "2408"
    assert "2344" not in {f.ticker for f in res.fills}
