"""The active strategies (issues #70-#73) on made-up prices. Numbers are
synthetic, not real. Opens equal closes, so a signal at one close fills at
the next day's close price."""

import dataclasses
import sqlite3

import pandas as pd

from radar_backtest import data
from strategies import breakout, pullback, record_revenue, swing, window_dressing

from . import fixture

DAYS = pd.bdate_range("2019-01-01", "2019-12-31")


def build(path, closes: dict, turnover: dict | None = None, revenue=(), flows=()):
    """closes/turnover: ticker -> list from the first day (padded with the last value)."""
    con = sqlite3.connect(path)
    con.executescript(fixture.SCHEMA)
    rows, companies = [], []
    for t, cs in closes.items():
        cs = list(cs) + [cs[-1]] * (len(DAYS) - len(cs))
        tv = list((turnover or {}).get(t, [1e9]))
        tv = tv + [tv[-1]] * (len(DAYS) - len(tv))
        rows += [("TW", t, d.strftime("%Y-%m-%d"), c, c, c, c, c, 1000, v)
                 for d, c, v in zip(DAYS, cs, tv)]
        companies.append(("TW", t, t, "ETF" if t.startswith("00") else "Electronics", "TWSE",
                          "2000-01-01", ""))
    con.executemany("INSERT INTO prices VALUES (?,?,?,?,?,?,?,?,?,?)", rows)
    con.executemany("INSERT INTO companies VALUES (?,?,?,?,?,?,?)", companies)
    con.executemany("INSERT INTO monthly_revenue VALUES ('TW',?,?,?,?,NULL,?)", list(revenue))
    con.executemany("INSERT INTO institutional_flows VALUES ('TW',?,?,0,?,0)", list(flows))
    con.commit()
    con.close()
    return path


def run(db, make, params, end=None):
    s = dataclasses.replace(fixture.settings(db), adv_days=5)
    strat = make(params, s)
    res = swing.SwingEngine(data.load(db), s).run(strat, DAYS[0], end or DAYS[-1])
    return strat, res


def cycles(strat):
    return [(c.ticker, c.opened.date().isoformat(), c.reason) for c in strat.stats.cycles]


def months(first, last, value, record=None):
    """Revenue rows for each month; `record` overrides the last month."""
    out = []
    for m in pd.date_range(first, last, freq="MS"):
        v = record if (record is not None and m == pd.Timestamp(last)) else value
        announced = (m + pd.offsets.MonthBegin(1)).replace(day=10)
        out.append((m.strftime("%Y-%m-%d"), v, announced.strftime("%Y-%m-%d")))
    return out


def test_record_revenue_buys_records_and_sells_on_time(tmp_path):
    rev = [("2001", m, v, 50.0, a) for m, v, a in months("2016-01-01", "2018-12-01", 1e8, record=3e8)]
    rev += [("2002", m, v, 50.0, a) for m, v, a in months("2016-01-01", "2018-12-01", 1e8)]  # no record
    db = build(tmp_path / "radar.db", {"2001": [100.0], "2002": [100.0], "0050": [50.0]}, revenue=rev)
    params = {"min_adv_ntd": 1e6, "adv_days": 5, "hold_days": 20}
    strat, res = run(db, record_revenue.make, params, end=DAYS[60])
    # Revenue deadline Thu 2019-01-10; the next trading day signals, Mon the 14th fills.
    assert cycles(strat)[0] == ("2001", "2019-01-14", "time")
    assert all(t != "2002" for t, *_ in cycles(strat))
    c = strat.stats.cycles[0]
    assert c.closed == pd.Timestamp("2019-02-12")  # 20 trading days after the fill, sold at the next open


def test_record_revenue_trailing_stop(tmp_path):
    rev = [("2001", m, v, 50.0, a) for m, v, a in months("2016-01-01", "2018-12-01", 1e8, record=3e8)]
    closes = [100.0] * 9 + [100.0] * 5 + [120.0, 120.0, 107.0]  # 107 is 10.8% below the 120 peak
    db = build(tmp_path / "radar.db", {"2001": closes, "0050": [50.0]}, revenue=rev)
    strat, _ = run(db, record_revenue.make, {"min_adv_ntd": 1e6, "adv_days": 5}, end=DAYS[40])
    assert cycles(strat)[0][2] == "trail_stop"


def test_breakout_buys_a_volume_high_and_trails(tmp_path):
    flat = [100.0] * 70
    closes = flat + [110.0] * 5 + [90.0]
    value = [1e8] * 70 + [5e8] + [1e8] * 10
    db = build(tmp_path / "radar.db", {
        "2001": closes,
        "2002": flat + [110.0] * 6,           # same high, no volume: not bought
        "0050": [50.0 + i * 0.1 for i in range(len(DAYS))],
    }, turnover={"2001": value})
    params = {"min_adv_ntd": 1e6, "adv_days": 5, "regime_days": 5, "breakout_days": 20,
              "volume_days": 20, "atr_days": 10}
    strat, _ = run(db, breakout.make, params, end=DAYS[90])
    assert cycles(strat) == [("2001", DAYS[71].date().isoformat(), "trail_stop")]


def test_breakout_waits_while_the_market_is_below_trend(tmp_path):
    closes = [100.0] * 70 + [110.0] * 5
    value = [1e8] * 70 + [5e8] + [1e8] * 10
    db = build(tmp_path / "radar.db", {"2001": closes, "0050": [80.0 - i * 0.1 for i in range(len(DAYS))]},
               turnover={"2001": value})
    params = {"min_adv_ntd": 1e6, "adv_days": 5, "regime_days": 5, "breakout_days": 20, "volume_days": 20}
    strat, _ = run(db, breakout.make, params, end=DAYS[90])
    assert cycles(strat) == []


def test_window_dressing_buys_trust_favourites_and_exits_before_quarter_end(tmp_path):
    db = build(tmp_path / "radar.db", {"2001": [100.0], "2002": [100.0], "0050": [50.0]},
               flows=[("2001", d.strftime("%Y-%m-%d"), 100_000) for d in DAYS[:60]]
               + [("2002", d.strftime("%Y-%m-%d"), -100_000) for d in DAYS[:60]])
    params = {"min_adv_ntd": 1e6, "adv_days": 5, "skip_largest": 0, "quarters": [3]}
    strat, _ = run(db, window_dressing.make, params, end=DAYS[120])
    # 2019-03-04 is 27 days before Mar 31: signal; fill on the 5th. Mar 28 (3 days) signals the sell.
    assert cycles(strat) == [("2001", "2019-03-05", "quarter_end")]
    assert strat.stats.cycles[0].closed == pd.Timestamp("2019-03-29")


def test_pullback_buys_a_dip_in_a_leader_and_sells_the_bounce(tmp_path):
    up = [100.0 * 1.01 ** i for i in range(215)]
    dip = up + [up[-1] * 0.97, up[-1] * 0.94, up[-1] * 0.91, up[-1] * 1.0]
    db = build(tmp_path / "radar.db", {
        "2001": dip,
        "2002": [100.0 + 0.01 * i for i in range(len(DAYS))],   # weak: not a leader
        "0050": [50.0],
    })
    params = {"min_adv_ntd": 1e6, "adv_days": 5, "strength_pct": 0.5}
    strat, _ = run(db, pullback.make, params, end=DAYS[230])
    assert cycles(strat)[0][0] == "2001"
    assert cycles(strat)[0][2] == "bounce"
    assert all(t != "2002" for t, *_ in cycles(strat))


def test_unknown_keys_are_rejected():
    for make in (record_revenue.make, breakout.make, window_dressing.make, pullback.make):
        try:
            make({"nope": 1}, None)
        except ValueError:
            continue
        raise AssertionError(f"{make} accepted an unknown key")
