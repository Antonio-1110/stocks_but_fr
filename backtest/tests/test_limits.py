"""Issue #53: no buy at a limit-up open, sells wait out a limit-down open.

A small synthetic market: every stock trades flat at its price, except on the
days set below. Numbers are made up to hit the limit rules exactly."""

import dataclasses
import sqlite3

import pandas as pd
import pytest

from radar_backtest import config, data
from radar_backtest.engine import Engine, _locks

from . import fixture

DAYS = pd.bdate_range("2020-01-01", "2020-02-28")
D = {i: d for i, d in enumerate(DAYS)}
SIGNAL = 25          # strategies trade on this close (enough ADV history before it)
FILL = SIGNAL + 1    # ...and fill at this open


def _flat(price):
    """{day: [open, close, adj_close]} at a flat price."""
    return {d: [price, price, price] for d in DAYS}


def build(path, bars: dict[str, dict]):
    con = sqlite3.connect(path)
    con.executescript(fixture.SCHEMA)
    rows = []
    for t, by_day in bars.items():
        for d, (o, c, a) in by_day.items():
            rows.append(("TW", t, d.strftime("%Y-%m-%d"), o, max(o, c), min(o, c), c, a, 1000, 1e9))
    con.executemany("INSERT INTO prices VALUES (?,?,?,?,?,?,?,?,?,?)", rows)
    con.executemany("INSERT INTO companies VALUES (?,?,?,?,?,?,?)",
                    [("TW", t, t, "Electronics", "TWSE", "2000-01-01", "") for t in bars])
    con.commit()
    con.close()
    return path


class Script:
    def __init__(self, plan):
        self.plan = {D[k]: v for k, v in plan.items()}
        self.name, self.params = "script", {}

    def targets(self, view):
        return self.plan.get(view.day)


def run(tmp_path, bars, plan, limits=True):
    db = build(tmp_path / "radar.db", bars)
    s = fixture.settings(db)
    assert s.limits is not None
    if not limits:
        s = dataclasses.replace(s, limits=None)
    return Engine(data.load(db), s).run(Script(plan), DAYS[0], DAYS[-1])


def test_limits_come_from_radar_toml():
    s = config.load(fixture.REPO / "radar.toml")
    lim = s.limits
    assert (lim.pct, lim.old_pct, lim.since) == (0.10, 0.07, "2015-06-01")
    assert 0 < lim.tolerance < 0.01
    assert lim.at("2015-05-29") == 0.07 and lim.at("2015-06-01") == 0.10


def test_buy_skipped_at_limit_up_open(tmp_path):
    bars = {"1101": _flat(10.0)}
    for i in range(FILL, len(DAYS)):
        bars["1101"][D[i]] = [11.0, 11.0, 11.0]          # +10% open on the fill day, flat after
    res = run(tmp_path, bars, {SIGNAL: {"1101": 0.5}})
    assert res.fills == [] and res.open_trades == []
    assert (res.limit_blocked, res.limit_blocked_buys, res.limit_deferred_sells) == (1, 1, 0)
    assert res.equity.iloc[-1] == res.initial_capital


def test_buy_fills_without_limits_or_below_the_limit(tmp_path):
    bars = {"1101": _flat(10.0)}
    bars["1101"][D[FILL]] = [10.90, 11.0, 11.0]          # +9%: below 10% - tolerance
    res = run(tmp_path, bars, {SIGNAL: {"1101": 0.5}})
    [buy] = res.fills
    assert buy.day == D[FILL] and buy.price == pytest.approx(10.90)
    assert res.limit_blocked == 0


def test_tick_rounded_limit_counts_as_locked(tmp_path):
    # 9.99 x 1.1 = 10.989, but the limit price rounds down to the 0.05 tick: 10.95 (+9.6%).
    bars = {"1101": _flat(9.99)}
    for i in range(FILL, len(DAYS)):
        bars["1101"][D[i]] = [10.95, 10.95, 10.95]
    res = run(tmp_path, bars, {SIGNAL: {"1101": 0.5}})
    assert res.fills == [] and res.limit_blocked_buys == 1


def test_no_limit_check_when_unconfigured(tmp_path):
    bars = {"1101": _flat(10.0)}
    bars["1101"][D[FILL]] = [11.0, 11.0, 11.0]
    res = run(tmp_path, bars, {SIGNAL: {"1101": 0.5}}, limits=False)
    [buy] = res.fills
    assert buy.price == 11.0 and res.limit_blocked == 0


def test_exit_waits_out_limit_down_opens(tmp_path):
    bars = {"2201": _flat(100.0)}
    exit_signal = SIGNAL + 5
    # Locked limit-down two opens in a row after the exit signal, then a normal open.
    price = 100.0
    for k, i in enumerate(range(exit_signal + 1, len(DAYS))):
        if k < 2:
            price = round(price * 0.9, 2)
            bars["2201"][D[i]] = [price, price, price]
        else:
            bars["2201"][D[i]] = [price * 0.98, price, price]
    res = run(tmp_path, bars, {SIGNAL: {"2201": 0.5}, exit_signal: {}})
    buy, sell = res.fills
    assert buy.day == D[FILL]
    assert sell.day == D[exit_signal + 3] and sell.price == pytest.approx(81.0 * 0.98)
    assert (res.limit_blocked, res.limit_deferred_sells, res.limit_blocked_buys) == (2, 2, 0)
    [t] = res.trades
    assert t.closed == D[exit_signal + 3] and res.open_trades == []
    assert res.invested.loc[D[exit_signal + 3]:].eq(0).all()


def test_new_signal_replaces_a_waiting_sell(tmp_path):
    bars = {"2201": _flat(100.0)}
    exit_signal = SIGNAL + 5
    bars["2201"][D[exit_signal + 1]] = [90.0, 90.0, 90.0]   # locked: the exit waits
    for i in range(exit_signal + 2, len(DAYS)):
        bars["2201"][D[i]] = [90.0, 90.0, 90.0]
    # The next close asks to hold it again: the waiting sell is dropped.
    res = run(tmp_path, bars, {SIGNAL: {"2201": 0.5}, exit_signal: {}, exit_signal + 1: {"2201": 0.5}})
    assert "sell" not in [f.side for f in res.fills]
    assert res.limit_deferred_sells == 1 and len(res.open_trades) == 1


def test_partial_sell_waits_too(tmp_path):
    bars = {"2201": _flat(100.0)}
    trim = SIGNAL + 5
    bars["2201"][D[trim + 1]] = [90.0, 90.0, 90.0]
    for i in range(trim + 2, len(DAYS)):
        bars["2201"][D[i]] = [91.0, 90.0, 90.0]
    res = run(tmp_path, bars, {SIGNAL: {"2201": 0.5}, trim: {"2201": 0.25}})
    buy, sell = res.fills
    assert sell.day == D[trim + 2] and sell.price == 91.0
    assert res.limit_deferred_sells == 1
    # It sells the units it meant to sell at the locked open, at the next open's price.
    units = buy.value / buy.price
    cash = res.initial_capital - buy.value - buy.commission
    meant = units - 0.25 * (cash + units * 90.0) / 90.0
    assert sell.value == pytest.approx(meant * 91.0) and len(res.open_trades) == 1


def test_ex_dividend_open_is_not_limit_down(tmp_path):
    # Raw close 100, then a 10 NTD dividend: the raw open of 90 is -10%, but on
    # the adjusted scale (older days scaled by 0.9) it is no move at all.
    ex = SIGNAL + 6
    bars = {"2201": {}}
    for i, d in enumerate(DAYS):
        if i < ex:
            bars["2201"][d] = [100.0, 100.0, 90.0]
        else:
            bars["2201"][d] = [90.0, 90.0, 90.0]
    res = run(tmp_path, bars, {SIGNAL: {"2201": 0.5}, ex - 1: {}})
    buy, sell = res.fills
    assert sell.day == D[ex] and res.limit_blocked == 0


def test_old_seven_percent_limit_before_june_2015():
    # +7% is locked before 2015-06-01, and neither +7% nor -6.5% is after it.
    s = config.load(fixture.REPO / "radar.toml")
    days = pd.DatetimeIndex(["2015-05-27", "2015-05-28", "2015-06-01", "2015-06-02"])
    close = pd.DataFrame({"A": [10.0, 10.7, 10.0, 10.7]}, index=days)
    md = data.MarketData(open=close, close=close, adj_open=close, adj_close=close,
                         turnover=close, volume=close, companies=pd.DataFrame(),
                         revenue=pd.DataFrame(), flows=pd.DataFrame())
    lock = _locks(md, s.limits)["A"].tolist()
    assert lock == [0, 1, 0, 0]
