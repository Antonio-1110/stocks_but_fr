"""foreign_flow (issue #66) on a made-up market: the intensity measure, the
gates, and the ranking. Synthetic numbers; nothing here says anything about
real returns."""

import sqlite3

import pandas as pd
import pytest

from radar_backtest import data
from strategies.foreign_flow import ForeignFlow, RevenueMomentumForeign, intensity

from . import fixture

DAYS = pd.bdate_range("2019-01-01", "2019-06-28")
PRICE, TURNOVER = 50.0, 1e8   # NTD traded a day

# ticker: foreign net shares a day (None = no flow rows at all)
STOCKS = {
    "1101": 200_000,    # foreigners buy 10% of the traded value every day
    "1102": 50_000,     # buy 2.5% on average, on half the days
    "1103": -200_000,   # sell 10%
    "1104": None,       # no flow data (as for many stocks before 2013)
}


@pytest.fixture
def view(tmp_path):
    db = tmp_path / "radar.db"
    con = sqlite3.connect(db)
    con.executescript(fixture.SCHEMA)
    for t, net in STOCKS.items():
        con.executemany("INSERT INTO prices VALUES (?,?,?,?,?,?,?,?,?,?)", [
            ("TW", t, d.strftime("%Y-%m-%d"), PRICE, PRICE, PRICE, PRICE, PRICE, 2_000_000, TURNOVER)
            for d in DAYS])
        con.execute("INSERT INTO companies VALUES (?,?,?,?,?,?,?)",
                    ("TW", t, t, "Electronics", "TWSE", "2000-01-01", ""))
        if net is not None:
            # 1102 buys only on every other day, so its buy-day share is 50%.
            con.executemany("INSERT INTO institutional_flows VALUES (?,?,?,?,?,?)", [
                ("TW", t, d.strftime("%Y-%m-%d"), (2 * net if i % 2 else 0) if t == "1102" else net, 0, 0)
                for i, d in enumerate(DAYS)])
    con.commit()
    con.close()
    return data.load(db).view(DAYS[-1])


def test_intensity(view):
    fl = intensity(view, list(STOCKS), 20)
    assert fl.loc["1101", "intensity"] == pytest.approx(0.10)
    assert fl.loc["1102", "intensity"] == pytest.approx(0.025)
    assert fl.loc["1102", "buy_share"] == pytest.approx(0.5)
    assert fl.loc["1103", "intensity"] == pytest.approx(-0.10)
    assert fl.loc["1101", "buy_share"] == 1.0
    assert pd.isna(fl.loc["1104", "intensity"])


def test_flow_ranks_buyers_and_drops_missing(view):
    ranked = ForeignFlow({"flow_days": 20}, "t").ranked(view)
    assert list(ranked["ticker"]) == ["1101", "1102", "1103"]
    steady = ForeignFlow({"flow_days": 20, "min_buy_share": 0.6}, "t").ranked(view)
    assert list(steady["ticker"]) == ["1101"]   # 1102 buys on half the days, 1103 never


def test_gate_only_applies_when_set(view):
    fl = intensity(view, list(STOCKS), 20)
    plain = RevenueMomentumForeign({}, "t")._foreign_ok(fl)
    assert plain.all()   # no gate: missing flow data is no reason to drop a stock
    gated = RevenueMomentumForeign({"min_intensity": 0.0}, "t")._foreign_ok(fl)
    assert list(gated.index[gated]) == ["1101", "1102"]
