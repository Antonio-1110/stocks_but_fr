"""Alpha lab (issue #76) on a synthetic market with one planted signal: 外資
buying today predicts tomorrow's return. Numbers are made up."""

from __future__ import annotations

import dataclasses
import sqlite3

import numpy as np
import pandas as pd
import pytest

from radar_backtest import alpha, data, signals

from .fixture import REPO, settings as base_settings

N_STOCKS = 60
DAYS = pd.bdate_range("2018-01-01", "2021-06-30")


def market(plant: float = 0.01, seed: int = 7) -> data.MarketData:
    rng = np.random.default_rng(seed)
    tickers = [str(1101 + i) for i in range(N_STOCKS)]
    n = len(DAYS)
    z = rng.standard_normal((n, N_STOCKS))          # 外資 buying, in days of volume / 10
    r = 0.015 * rng.standard_normal((n, N_STOCKS))
    r[1:] += plant * z[:-1]                          # today's buying -> tomorrow's move
    close = 50 * np.exp(np.cumsum(r, axis=0))
    opens = np.vstack([close[:1], close[:-1]])       # open = yesterday's close
    frame = lambda a: pd.DataFrame(a, index=DAYS, columns=tickers)
    vol = frame(np.full((n, N_STOCKS), 1e6))
    flows = frame(z * 1e5).stack().rename("foreign_net").reset_index()
    flows.columns = ["date", "ticker", "foreign_net"]
    flows["trust_net"], flows["dealer_net"] = 0, 0
    companies = pd.DataFrame({"ticker": tickers, "name": [f"Co {t}" for t in tickers],
                              "industry": "", "exchange": "TWSE",
                              "listed_on": pd.NaT, "delisted_on": pd.NaT})
    revenue = pd.DataFrame({
        "ticker": ["1101"] * 3, "month": pd.to_datetime(["2020-01-01", "2020-02-01", "2020-03-01"]),
        "revenue": [5e8, 6e8, 9e8], "yoy_pct": [10.0, 20.0, 50.0], "mom_pct": [0.0, 20.0, 50.0],
        "announced_on": pd.to_datetime(["2020-02-10", "2020-03-10", "2020-04-10"])})
    c = frame(close)
    return data.MarketData(open=frame(opens), close=c, adj_open=frame(opens), adj_close=c,
                           turnover=c * vol, volume=vol, companies=companies,
                           revenue=revenue, flows=flows)


@pytest.fixture
def settings(tmp_path):
    return dataclasses.replace(base_settings(tmp_path / "radar.db"), design_start="2018-01-01",
                               design_end="2019-12-31", test_start="2020-01-01")


def cfg(**kw):
    c = alpha.lab_config(REPO / "radar.toml")
    c.update(horizons=[1, 5], **kw)
    return c


def test_finds_the_planted_signal(settings):
    res, picks = alpha.run(market(), settings, cfg(), log=lambda *_: None)
    got = res.set_index(["signal", "horizon"])
    f = got.loc[("foreign_1d", 1)]
    assert f["verdict"] == "passes"
    assert f["design_ic"] > 0.3 and f["test_ic"] > 0.3
    # The planted 1% a day per unit of buying, minus round-trip costs.
    assert f["test_net"] == pytest.approx(f["test_edge"] - alpha.round_trip(settings.costs))
    assert f["test_edge"] > 0.01
    # Acting a day late loses it: the effect lasts one day.
    assert f["late_net"] < 0
    # Pure noise signals don't pass.
    noise = got.loc[[("reversal_5d", 1), ("low_volatility", 1), ("trust_5d", 1)]]
    assert not (noise["verdict"] == "passes").any()
    # Today's top 外資 buys are listed for a passing signal.
    fp = picks[picks["signal"] == "foreign_1d"]
    assert len(fp) == 10 and list(fp["rank"]) == list(range(1, 11))


def test_no_signal_no_pass(settings):
    res, _ = alpha.run(market(plant=0.0), settings, cfg(), only=["foreign_1d", "reversal_1d"],
                       log=lambda *_: None)
    assert not (res["verdict"] == "passes").any()


def test_limit_up_entry_is_dropped(settings):
    md = market(plant=0.0)
    day = DAYS[300]
    nxt = DAYS[301]
    md.adj_open.loc[nxt, "1101"] = md.adj_close.loc[day, "1101"] * 1.10
    x = signals.Inputs(md, limit_pct=pd.Series(0.10, index=DAYS))
    univ = alpha.universe(md, cfg())
    exc = alpha.excess_returns(x, univ, 1, 0, 0.005)
    assert np.isnan(exc.loc[day, "1101"])
    assert not np.isnan(exc.loc[day, "1102"])
    # Excess returns are relative to the universe average.
    assert abs(exc.loc[DAYS[100]].mean()) < 1e-6


def test_universe_skips_illiquid_and_etf():
    md = market(plant=0.0)
    md.turnover.loc[:, "1101"] = 1e6
    md.companies.loc[md.companies["ticker"] == "1102", "listed_on"] = DAYS[200]
    u = alpha.universe(md, cfg())
    assert not u["1101"].any()
    assert not u.loc[DAYS[199], "1102"] and u.loc[DAYS[220], "1102"]


def test_every_signal_runs(settings):
    md = market()
    x = signals.Inputs(md, pd.DataFrame({"ticker": ["1101"], "date": [DAYS[400]]}),
                       pd.Series(0.10, index=DAYS))
    for s in signals.SIGNALS:
        out = s.fn(x)
        assert out.shape == md.close.shape, s.name
    pre = signals.BY_NAME["pre_ex_dividend"].fn(x)["1101"]
    assert pre.loc[DAYS[395]:DAYS[399]].eq(1).all() and pd.isna(pre.loc[DAYS[400]])
    yoy = signals.BY_NAME["revenue_yoy"].fn(x)["1101"]
    assert pd.isna(yoy.loc["2020-02-10"]) and yoy.loc["2020-02-11"] == 10.0


def test_save_and_skip(settings, tmp_path):
    db = tmp_path / "radar.db"
    sqlite3.connect(db).close()
    res, picks = alpha.run(market(), settings, cfg(), only=["foreign_1d"], log=lambda *_: None)
    alpha.save(db, res, picks, "2026-10-24 10:00:00", "snap", "abc")
    con = sqlite3.connect(db)
    assert con.execute("SELECT COUNT(*) FROM alpha_results").fetchone()[0] == 2
    assert con.execute("SELECT verdict FROM alpha_results WHERE horizon = 1").fetchone()[0] == "passes"
    assert con.execute("SELECT COUNT(*) FROM alpha_picks").fetchone()[0] == 10
    con.close()
    assert alpha.last_run(db).year == 2026
    md = alpha.markdown(res, "now", "test")
    assert "foreign_1d" in md and "passes" in md
