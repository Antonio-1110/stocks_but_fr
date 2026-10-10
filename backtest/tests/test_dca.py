"""strategies/dca.py on made-up prices. Numbers are synthetic, not real.

Opens equal closes, so an order signalled at one close fills at the next
day's close price."""

import dataclasses
import sqlite3

import pandas as pd
import pytest

from radar_backtest import costs, data
from strategies import dca

from . import fixture

DAYS = pd.bdate_range("2019-01-01", "2019-12-31")
FLAT = [100.0] * 10
# A dip to 89 starts a cycle; then every other day the close is ~5% lower.
FALL = FLAT + [89, 89, 84, 84, 79, 79, 75, 75, 71, 71, 67, 67, 63, 63, 60, 60]


def build(path, series: dict, turnover=None, delisted=None, unpriced=()):
    """series: ticker -> closes from the first day (shorter = stops trading)."""
    con = sqlite3.connect(path)
    con.executescript(fixture.SCHEMA)
    rows, companies = [], []
    for t, closes in series.items():
        closes = list(closes) + ([closes[-1]] * (len(DAYS) - len(closes)) if t not in (delisted or {}) else [])
        tv = (turnover or {}).get(t, 1e9)
        rows += [("TW", t, d.strftime("%Y-%m-%d"), c, c, c, c, c, 1000, tv) for d, c in zip(DAYS, closes)]
        industry = "ETF" if t.startswith("00") else "Electronics"
        companies.append(("TW", t, t, industry, "TWSE", "2000-01-01", (delisted or {}).get(t, "")))
    companies += [("TW", t, t, "Electronics", "TWSE", "2000-01-01", "") for t in unpriced]
    con.executemany("INSERT INTO prices VALUES (?,?,?,?,?,?,?,?,?,?)", rows)
    con.executemany("INSERT INTO companies VALUES (?,?,?,?,?,?,?)", companies)
    con.commit()
    con.close()
    return path


PARAMS = {"universe_size": 10, "universe_days": 5, "min_avg_turnover": 1e8,
          "entry_high_days": 5, "max_cycles": 1, "base_order": 50_000,
          "martingale": {}, "capped": {"max_adds": 3, "stop_loss": 0.20}}


def run(db, variant="martingale", capital=5_000_000, **params):
    s = dataclasses.replace(fixture.settings(db), initial_capital=capital, adv_days=5)
    strat = dca.make({**PARAMS, **params}, s, variant)
    res = dca.OrderEngine(data.load(db), s).run(strat, DAYS[0], DAYS[60])
    return s, strat, res


def test_take_profit_on_a_one_percent_bounce(tmp_path):
    path = FLAT + [89, 89, 90]
    db = build(tmp_path / "radar.db", {
        "2001": path,
        "0056": path,                                   # an ETF: never picked
        "3003": path,                                   # too thin to qualify
    }, turnover={"3003": 1e6})
    s, strat, res = run(db)
    c = strat.stats.cycles[0]
    assert (c.ticker, c.reason, c.adds) == ("2001", "take_profit", 0)
    buy, sell = res.fills[:2]
    assert (buy.day, buy.price, buy.value) == (DAYS[11], 89, 50_000)
    assert (sell.day, sell.price) == (DAYS[13], 90)
    units = 50_000 / 89
    got = units * 90 - costs.commission(s.costs, units * 90) - costs.sell_tax(s.costs, units * 90, False)
    assert c.trade.pnl == pytest.approx(got - 50_000 - costs.commission(s.costs, 50_000))
    # Tax and commissions eat over 40% of a 1.1% bounce.
    assert 0 < c.trade.pnl < 50_000 * (90 / 89 - 1) * 0.6


def test_martingale_doubles_until_cash_runs_out(tmp_path):
    db = build(tmp_path / "radar.db", {"2001": FALL})
    s, strat, res = run(db, capital=400_000)
    buys = [f for f in res.fills if f.side == "buy"]
    assert [b.value for b in buys[:3]] == [50_000, 100_000, 200_000]
    assert [b.price for b in buys[:3]] == [89, 84, 79]
    # The 400k add only gets what is left; the 800k add gets nothing.
    assert buys[3].value < 50_000 and len(buys) == 4
    short = sorted(strat.stats.short.values())
    assert [w for _, w, _ in short] == [400_000, 800_000]
    assert short[0][2] == pytest.approx(buys[3].value) and short[1][2] == 0
    [c] = strat.stats.cycles
    assert (c.reason, c.adds) == ("open", 3)
    out = dca.summarize(res, strat.stats, None, [])
    assert out["short_adds"] == 2 and out["short_cycles"] == 1 and out["still_open"] == 1


def test_capped_stops_after_three_adds(tmp_path):
    db = build(tmp_path / "radar.db", {"2001": FALL})
    s, strat, res = run(db, "capped")
    c = strat.stats.cycles[0]
    assert c.adds == 3                       # 71 is 5% under 75, but the cap holds
    assert c.reason == "stop"
    sell = [f for f in res.fills if f.side == "sell"]
    assert sell[0].price == 60 and sell[0].day == c.closed   # stop signalled at the 60 close: avg cost ~78
    assert c.trade.ret < -0.2
    assert strat.stats.short == {}


def test_delisted_stock_ends_the_cycle_at_its_last_close(tmp_path):
    db = build(tmp_path / "radar.db", {"2001": FALL[:18], "0050": [50.0] * len(DAYS)}, delisted={"2001": "2019-01-25"})
    s, strat, res = run(db)
    [c] = strat.stats.cycles
    assert c.reason == "delisted" and c.closed == DAYS[18]
    assert res.fills[-1].side == "sell" and res.fills[-1].price == 75
    assert res.trades[0].delisted


def test_make_rejects_unknown_keys_and_reads_radar_toml(tmp_path):
    s = fixture.settings(tmp_path / "radar.db")
    p = s.strategies["dca"]
    assert dca.variants(p) == ["martingale", "capped"]
    assert dca.make(p, s).p["max_adds"] == 0
    assert dca.make(p, s, "capped").p["max_adds"] == 3
    with pytest.raises(ValueError):
        dca.make({**p, "take_proft": 0.01}, s)
    with pytest.raises(ValueError):
        dca.make(p, s, "nope")


def test_report_has_dca_section_and_partial_data_warning(tmp_path):
    db = build(tmp_path / "radar.db", {"2001": FALL, "0050": [50.0] * len(DAYS)}, unpriced=["9999"])
    s = dataclasses.replace(fixture.settings(db), adv_days=5, design_start="2019-01-01",
                            design_end="2019-06-28", test_start="2019-07-01",
                            strategies={"dca": {**PARAMS, "stress_windows": [
                                ["2008", "2008-05-19", "2008-11-20"], ["dip", "2019-01-15", "2019-02-15"]]}})
    out = dca.run_variant(data.load(db), s, "capped", save=False, report_dir=tmp_path / "reports")
    assert out["missing_windows"] == ["2008"]
    assert out["warning"].startswith("PARTIAL DATA")
    md = out["reports"][0].read_text(encoding="utf-8")
    assert "PARTIAL DATA" in md.splitlines()[2]
    assert "## DCA details" in md and "Cycles that ran out of cash" in md and "No data for the 2008" in md
    assert "dip (2019-01-15 to 2019-02-15)" in md
    page = out["reports"][1].read_text(encoding="utf-8")
    assert "DCA details" in page and page.rstrip().endswith("</html>")
    design = out["periods"][0]
    assert design["dca"]["stopped"] == 1 and design["coverage"]["share"] == pytest.approx(0.5)
