"""A tiny made-up market in a temporary radar.db. Numbers are synthetic, not
real prices: tests check the engine's arithmetic, not any real return."""

from __future__ import annotations

import dataclasses
import sqlite3
from pathlib import Path

import numpy as np
import pandas as pd

from radar_backtest import config

REPO = Path(__file__).resolve().parents[2]

# Copy of the tables in internal/store/store.go that the engine reads.
SCHEMA = """
CREATE TABLE companies (market TEXT, ticker TEXT, name TEXT, industry TEXT DEFAULT '',
  exchange TEXT DEFAULT '', listed_on TEXT DEFAULT '', delisted_on TEXT DEFAULT '',
  PRIMARY KEY (market, ticker));
CREATE TABLE prices (market TEXT, ticker TEXT, date TEXT, open REAL, high REAL, low REAL,
  close REAL, adj_close REAL, volume INTEGER, turnover REAL, PRIMARY KEY (market, ticker, date));
CREATE TABLE monthly_revenue (market TEXT, ticker TEXT, month TEXT, revenue REAL, yoy_pct REAL,
  mom_pct REAL, announced_on TEXT DEFAULT '', PRIMARY KEY (market, ticker, month));
CREATE TABLE institutional_flows (market TEXT, ticker TEXT, date TEXT, foreign_net INTEGER,
  trust_net INTEGER, dealer_net INTEGER, PRIMARY KEY (market, ticker, date));
"""

DAYS = pd.bdate_range("2019-11-01", "2020-03-31")
DIVIDEND_DAY = pd.Timestamp("2020-02-03")  # 0050 goes ex-dividend here
DIVIDEND = 3.0
DELIST_LAST = pd.Timestamp("2020-01-31")   # 1111's last trading day


def settings(db: Path, **overrides) -> config.Settings:
    s = config.load(REPO / "radar.toml")
    return dataclasses.replace(s, db_path=db, **overrides)


def _rows(ticker, closes, opens, turnover, days, adj_factor=None):
    adj_factor = np.ones(len(days)) if adj_factor is None else adj_factor
    return [("TW", ticker, d.strftime("%Y-%m-%d"), o, max(o, c), min(o, c), c, c * f, 1000, t)
            for d, o, c, f, t in zip(days, opens, closes, adj_factor, turnover)]


def build(path: Path) -> Path:
    con = sqlite3.connect(path)
    con.executescript(SCHEMA)
    n = len(DAYS)
    i = np.arange(n)

    # 0050: steady 0.1%/day drift with a wobble, minus a cash dividend.
    base = 100 * 1.001 ** i * (1 + 0.01 * np.sin(i / 3))
    after = DAYS >= DIVIDEND_DAY
    close = np.where(after, base - DIVIDEND, base)
    opens = np.roll(close, 1) * 1.002
    opens[0] = close[0]
    # Back-adjusted like FinMind: older closes scaled down for the later dividend.
    k = int(np.argmax(after))
    factor = np.where(after, 1.0, (close[k - 1] - DIVIDEND) / close[k - 1])
    rows = _rows("0050", close, opens, np.full(n, 2e9), DAYS, factor)

    # 1111: falls, then stops trading after DELIST_LAST.
    live = DAYS <= DELIST_LAST
    d1 = DAYS[live]
    c1 = 50 * 0.995 ** np.arange(len(d1))
    rows += _rows("1111", c1, c1, np.full(len(d1), 5e8), d1)

    # 2222: flat price, only 1M NTD traded a day.
    rows += _rows("2222", np.full(n, 20.0), np.full(n, 20.0), np.full(n, 1e6), DAYS)

    con.executemany("INSERT INTO prices VALUES (?,?,?,?,?,?,?,?,?,?)", rows)
    con.executemany("INSERT INTO companies VALUES (?,?,?,?,?,?,?)", [
        ("TW", "0050", "元大台灣50", "ETF", "TWSE", "2003-06-30", ""),
        ("TW", "1111", "Gone Co", "Electronics", "TWSE", "2000-01-01", "2020-02-03"),
        ("TW", "2222", "Thin Co", "Electronics", "TPEx", "2000-01-01", ""),
    ])
    con.executemany("INSERT INTO monthly_revenue VALUES (?,?,?,?,?,?,?)", [
        ("TW", "2222", "2019-12-01", 100.0, 10.0, 1.0, "2020-01-10"),
        ("TW", "2222", "2020-01-01", 120.0, 20.0, 20.0, "2020-02-10"),
    ])
    con.executemany("INSERT INTO institutional_flows VALUES (?,?,?,?,?,?)", [
        ("TW", "2222", "2020-01-02", 10, 5, 0),
        ("TW", "2222", "2020-01-03", -10, 7, 0),
    ])
    con.commit()
    con.close()
    return path
