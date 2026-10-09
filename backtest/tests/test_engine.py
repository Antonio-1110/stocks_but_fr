import math
import sqlite3

import pandas as pd
import pytest

from radar_backtest import costs, data, metrics, runner
from radar_backtest.baseline import BuyAndHold
from radar_backtest.engine import Engine

from . import fixture


@pytest.fixture
def db(tmp_path):
    return fixture.build(tmp_path / "radar.db")


@pytest.fixture
def md(db):
    return data.load(db)


class Script:
    """Returns given targets on given days."""

    def __init__(self, plan):
        self.plan = {pd.Timestamp(k): v for k, v in plan.items()}
        self.name, self.params = "script", {}
        self.seen = []

    def targets(self, view):
        self.seen.append(view)
        return self.plan.get(view.day)


def test_costs_follow_radar_toml(db):
    c = fixture.settings(db).costs
    assert c.commission_rate == 0.001425 and c.broker_discount == 0.6
    assert costs.commission(c, 10_000) == 20                     # minimum fee
    assert costs.commission(c, 1_000_000) == math.floor(1_000_000 * 0.001425 * 0.6)
    assert costs.sell_tax(c, 1_000_000, etf=False) == 3000
    assert costs.sell_tax(c, 1_000_000, etf=True) == 1000
    assert costs.is_etf("0050") and costs.is_etf("00878") and not costs.is_etf("2330")


def test_buy_and_hold_matches_adjusted_return(db, md):
    s = fixture.settings(db)
    res = Engine(md, s).run(BuyAndHold("0050"), "2020-01-02", "2020-03-31")
    fill = pd.Timestamp("2020-01-03")  # signal at the first close, fill next open
    cap = s.initial_capital
    [buy] = res.fills
    assert (buy.day, buy.side, buy.price) == (fill, "buy", md.adj_open.at[fill, "0050"])
    assert buy.commission == costs.commission(s.costs, buy.value)
    t = res.open_trades[0]
    assert t.opened == fill and t.invested == buy.value + buy.commission
    bought = buy.value
    units = bought / md.adj_open.at[fill, "0050"]
    expect = cap - t.invested + units * md.adj_close.at[pd.Timestamp("2020-03-31"), "0050"]
    assert res.equity.iloc[-1] == pytest.approx(expect, rel=1e-12)
    # Almost all cash goes in, and the dividend is not lost.
    assert cap - t.invested < 10
    gross = md.adj_close.at[pd.Timestamp("2020-03-31"), "0050"] / md.adj_open.at[fill, "0050"] - 1
    raw = md.close.at[pd.Timestamp("2020-03-31"), "0050"] / md.open.at[fill, "0050"] - 1
    got = res.equity.iloc[-1] / cap - 1
    assert got == pytest.approx(gross, abs=0.001)
    assert got > raw + 0.02  # raw prices would miss the 3 NTD dividend


def test_buy_and_hold_reports_against_actual_0050(db, md):
    s = fixture.settings(db)
    out = runner.run(md, s, lambda: BuyAndHold("0050"), report_dir=None, save=False,
                     spans=[("test", "2020-01-02", None)])
    p = out["periods"][0]
    actual = metrics.price_return(md.adj_close, "0050", "2020-01-02", "2020-03-31")
    # The run only differs from the stock by one buy fee and the first night's gap.
    first_gap = md.adj_open.at[pd.Timestamp("2020-01-03"), "0050"] / md.adj_close.at[pd.Timestamp("2020-01-02"), "0050"] - 1
    assert (1 + p["summary"]["total_return"]) == pytest.approx((1 + actual) / (1 + first_gap), rel=0.001)
    assert p["bench_price_return"] == pytest.approx(actual)


def test_orders_fill_at_next_open(db, md):
    s = fixture.settings(db)
    res = Engine(md, s).run(Script({"2020-01-06": {"0050": 0.5}}), "2020-01-02", "2020-01-10")
    t = res.open_trades[0]
    assert t.opened == pd.Timestamp("2020-01-07")
    assert res.invested.loc[:"2020-01-06"].eq(0).all()


def test_strategy_only_sees_the_past(db, md):
    strat = Script({})
    Engine(md, fixture.settings(db)).run(strat, "2020-02-07", "2020-02-12")
    by_day = {v.day: v for v in strat.seen}
    assert by_day[pd.Timestamp("2020-02-07")].close.index.max() == pd.Timestamp("2020-02-07")
    months = lambda d: list(by_day[pd.Timestamp(d)].revenue()["month"].dt.strftime("%Y-%m"))
    assert months("2020-02-10") == ["2019-12"]            # announced_on day: not yet
    assert months("2020-02-11") == ["2019-12", "2020-01"]
    assert len(by_day[pd.Timestamp("2020-02-07")].flows()) == 2
    assert "1111" not in set(by_day[pd.Timestamp("2020-02-07")].companies()["ticker"])


def test_delisted_position_sold_at_last_price(db, md):
    s = fixture.settings(db)
    res = Engine(md, s).run(Script({"2020-01-02": {"1111": 0.5}}), "2020-01-02", "2020-02-14")
    [t] = res.trades
    assert t.delisted and t.closed == pd.Timestamp("2020-02-03")
    buy, sell = res.fills
    last = md.adj_close.at[fixture.DELIST_LAST, "1111"]
    assert sell.price == last and sell.value == pytest.approx(buy.value / buy.price * last)
    assert sell.tax == costs.sell_tax(s.costs, sell.value, etf=False) > 0
    assert res.invested.loc["2020-02-03":].eq(0).all()
    assert t.pnl < 0 and t.longest_underwater > 10


def test_liquidity_cap(db, md):
    s = fixture.settings(db)
    res = Engine(md, s).run(Script({"2020-01-02": {"2222": 1.0}}), "2020-01-02", "2020-01-31")
    held = res.invested.iloc[-1]
    assert held == pytest.approx(s.max_adv_fraction * 1e6)  # 5% of 1M NTD a day
    assert res.equity.iloc[-1] == pytest.approx(s.initial_capital - costs.commission(s.costs, held))


def test_rebalance_sells_before_buying(db, md):
    s = fixture.settings(db)
    plan = {"2020-01-02": {"0050": 1.0}, "2020-01-10": {"1111": 0.3}}
    res = Engine(md, s).run(Script(plan), "2020-01-02", "2020-01-20")
    assert [t.ticker for t in res.trades] == ["0050"]
    assert res.trades[0].closed == pd.Timestamp("2020-01-13")
    assert res.tax_paid > 0 and res.commission_paid > 0
    assert res.invested.iloc[-1] / res.equity.iloc[-1] == pytest.approx(0.3, abs=0.01)


def test_runner_writes_table_and_reports(db, md, tmp_path):
    s = fixture.settings(db, design_start="2019-12-02", design_end="2019-12-31", test_start="2020-01-02")
    out = runner.run(md, s, runner.strategy_factory("buy_and_hold:0050", s),
                     report_dir=tmp_path / "reports")
    assert [p["period"] for p in out["periods"]] == ["design", "test"]
    md_text = out["reports"][0].read_text()
    assert "Worst single trade" in md_text and "Peak capital deployed" in md_text
    assert out["reports"][1].read_text().startswith("<!doctype html>")
    con = sqlite3.connect(db)
    rows = con.execute("SELECT period, strategy, start_date, benchmark, cagr, bench_cagr FROM backtest_runs").fetchall()
    assert [(r[0], r[1], r[3]) for r in rows] == [("design", "buy_and_hold_0050", "0050"),
                                                  ("test", "buy_and_hold_0050", "0050")]
    assert rows[1][2] == "2020-01-02"
    assert rows[0][4] == pytest.approx(rows[0][5])  # same strategy as the benchmark


def test_metrics_on_known_curve():
    from radar_backtest.engine import RunResult
    idx = pd.bdate_range("2020-01-01", periods=5)
    eq = pd.Series([100.0, 120.0, 90.0, 95.0, 130.0], index=idx)
    r = RunResult("x", {}, idx[0], idx[-1], 100.0, eq, eq * 0.5, [])
    m = metrics.summarize(r)
    assert m["max_drawdown"] == pytest.approx(-0.25)
    assert m["total_return"] == pytest.approx(0.30)
    assert m["peak_deployed"] == 65 and m["peak_deployed_pct"] == 0.5
    assert m["win_rate"] is None and m["worst_trade_pct"] is None


def test_buy_and_hold_waits_for_liquidity_history(db, md):
    # The fixture starts 2019-11-01, so the first 20 days have no average
    # traded value yet and the liquidity rule allows no position.
    res = Engine(md, fixture.settings(db)).run(BuyAndHold("0050"), "2019-11-01", "2019-12-31")
    [buy] = res.fills
    assert buy.day == md.calendar[20]
    assert res.invested.iloc[-1] > 0.99 * res.equity.iloc[-1]
