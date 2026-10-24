"""Alpha lab (issue #76): test every signal in signals.py for short-term edge.

    python -m radar_backtest.alpha [--config ../radar.toml] [--only a,b] [--if-older-than 6]

For each signal and each holding horizon h in [alpha] horizons (trading days):

- Every day, the universe is common stocks listed that day with at least
  `min_adv_ntd` average traded value over 20 days and a close of at least
  `min_price`.
- A stock's forward return buys at the next day's open and sells at the open
  h trading days later (adjusted prices, so dividends count). A buy whose open
  is locked at limit-up can't fill, so that observation is dropped. A stock
  that stops trading is valued at its last close.
- Excess return is that minus the universe's equal-weighted average.
- Score signals: daily rank IC (Spearman correlation of signal and excess
  return) and the top decile's average excess return. Event signals: the
  average excess return of the stocks with the event.
- "Net" subtracts one round trip of [costs.tw] (commission x discount twice,
  plus sell tax). "Late" is the same edge acted on one day later.

t-stats divide by sqrt(h) because h-day returns starting on consecutive days
overlap. With ~120 signal/horizon pairs tested, about 6 will clear t = 2 in a
period by luck alone, so a "passes" verdict needs the design-period edge to
clear a Bonferroni hurdle (about t = 3.3) and the test period, which nothing
was tuned on, to confirm it with t >= 2. Results go to the alpha_results and
alpha_picks tables; the dashboard's Alpha page shows them.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import sqlite3
import sys
import time
import tomllib
from pathlib import Path
from statistics import NormalDist

import numpy as np
import pandas as pd

from . import config, data, signals
from .runner import BACKTEST_DIR, code_version

PERIODS = ("design", "test", "recent")
METRICS = ("days", "n", "ic", "ic_t", "edge", "edge_t", "net", "hit")
GOOD = ("passes", "promising", "recent only")

SCHEMA = """
CREATE TABLE IF NOT EXISTS alpha_results (
	run_at   TEXT NOT NULL,
	snapshot TEXT NOT NULL DEFAULT '',
	code     TEXT NOT NULL DEFAULT '',
	signal   TEXT NOT NULL,
	family   TEXT NOT NULL,
	kind     TEXT NOT NULL,
	label    TEXT NOT NULL,
	horizon  INTEGER NOT NULL,
	verdict  TEXT NOT NULL,
	hurdle   REAL,
	cost     REAL,
	late_net REAL,
	data_end TEXT,
	{metrics},
	PRIMARY KEY (run_at, signal, horizon)
);
CREATE TABLE IF NOT EXISTS alpha_picks (
	run_at  TEXT NOT NULL,
	date    TEXT NOT NULL,
	signal  TEXT NOT NULL,
	horizon INTEGER NOT NULL,
	rank    INTEGER NOT NULL,
	ticker  TEXT NOT NULL,
	name    TEXT NOT NULL DEFAULT '',
	value   REAL,
	PRIMARY KEY (run_at, signal, rank)
);
""".replace("{metrics}", ",\n\t".join(
    f"{p}_{m} {'INTEGER' if m in ('days', 'n') else 'REAL'}" for p in PERIODS for m in METRICS))


def lab_config(path: str | Path) -> dict:
    with Path(path).open("rb") as f:
        raw = tomllib.load(f)
    a = raw.get("alpha", {})
    return {
        "horizons": [int(h) for h in a.get("horizons", [1, 5, 10, 20])],
        "min_adv_ntd": float(a.get("min_adv_ntd", 20_000_000)),
        "min_price": float(a.get("min_price", 10)),
        "min_stocks": int(a.get("min_stocks", 30)),
        "recent_days": int(a.get("recent_days", 250)),
        "family_alpha": float(a.get("family_alpha", 0.05)),
        "picks": int(a.get("picks", 10)),
        "min_base_ntd": float(raw.get("revenue", {}).get("low_base", {}).get("min_base_ntd", 10_000_000)),
    }


def round_trip(costs: config.Costs) -> float:
    """One buy and one sell of a stock, ignoring the minimum fee."""
    return 2 * costs.commission_rate * costs.broker_discount + costs.sell_tax


def load_dividends(db_path: str | Path) -> pd.DataFrame:
    con = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        d = pd.read_sql_query("SELECT ticker, date FROM tw_adj_events WHERE kind = 'dividend'", con)
    except (sqlite3.OperationalError, pd.errors.DatabaseError):
        d = pd.DataFrame(columns=["ticker", "date"])
    finally:
        con.close()
    d["date"] = pd.to_datetime(d["date"], format="%Y-%m-%d", errors="coerce")
    return d.dropna()


def limit_series(cal: pd.DatetimeIndex, limits: config.Limits | None) -> pd.Series:
    if limits is None:
        return pd.Series(np.inf, index=cal)
    since = pd.Timestamp(limits.since)
    return pd.Series(np.where(cal >= since, limits.pct, limits.old_pct), index=cal)


def universe(md: data.MarketData, cfg: dict) -> pd.DataFrame:
    """Liquid common stocks listed each day (no ETFs, warrants or preferreds)."""
    cal, cols = md.calendar, md.close.columns
    c = md.companies.drop_duplicates("ticker").set_index("ticker").reindex(cols)
    common = cols.str.fullmatch(r"[1-9]\d{3}")
    days = cal.values[:, None]
    listed = c["listed_on"].isna().values | (c["listed_on"].values <= days)
    alive = c["delisted_on"].isna().values | (c["delisted_on"].values > days)
    known = c["name"].notna().values
    adv = md.turnover.rolling(20, min_periods=15).mean()
    ok = (listed & alive & known & common) & (adv >= cfg["min_adv_ntd"]).values \
        & (md.close >= cfg["min_price"]).values
    return pd.DataFrame(ok, index=cal, columns=cols)


def excess_returns(x: signals.Inputs, univ: pd.DataFrame, h: int, lag: int,
                   tolerance: float) -> pd.DataFrame:
    """Return from the open `1 + lag` days after each date to the open h days
    after that, minus the universe average. NaN where the buy can't fill."""
    o, c = x.open, x.close
    entry = o.shift(-(1 + lag))
    prev = c.shift(-lag)
    lim = x.limit_pct.reindex(c.index).astype(np.float32)
    blocked = (entry / prev - 1).ge(lim - tolerance, axis=0)
    exit_ = o.shift(-(1 + lag + h))
    exit_ = exit_.fillna(c.ffill().shift(-(lag + h)))
    ret = (exit_ / entry - 1).where(~blocked).clip(-0.95, 5.0)
    ret = ret.where(univ)
    return ret.sub(ret.mean(axis=1), axis=0)


def _rowcorr(a: pd.DataFrame, b: pd.DataFrame) -> pd.Series:
    a = a.sub(a.mean(axis=1), axis=0)
    b = b.sub(b.mean(axis=1), axis=0)
    num = (a * b).sum(axis=1)
    den = np.sqrt((a * a).sum(axis=1) * (b * b).sum(axis=1))
    return num / den.where(den > 0)


def daily_stats(sig: pd.DataFrame, kind: str, exc: pd.DataFrame, univ: pd.DataFrame,
                min_stocks: int) -> pd.DataFrame:
    """Per date: ic, edge (top decile / event mean excess), n, wins."""
    if kind == "event":
        m = sig.eq(1) & univ & exc.notna()
        ev = exc.where(m)
        return pd.DataFrame({"ic": np.nan, "edge": ev.mean(axis=1), "n": m.sum(axis=1),
                             "wins": (ev > 0).sum(axis=1)})
    m = sig.notna() & univ & exc.notna()
    cnt = m.sum(axis=1)
    m = m & (cnt >= min_stocks).values[:, None]
    s, f = sig.where(m), exc.where(m)
    rs = s.rank(axis=1, pct=True)
    top = f.where(rs > 0.9)
    return pd.DataFrame({"ic": _rowcorr(rs, f.rank(axis=1)), "edge": top.mean(axis=1),
                         "n": top.notna().sum(axis=1), "wins": (top > 0).sum(axis=1)})


def _t(series: pd.Series, h: int) -> float:
    s = series.dropna()
    if len(s) < 20 or s.std() == 0:
        return float("nan")
    return float(s.mean() / s.std() * np.sqrt(len(s)) / np.sqrt(h))


def summarize(stats: pd.DataFrame, kind: str, h: int, cost: float) -> dict:
    active = stats[stats["n"] > 0]
    n = int(active["n"].sum())
    if n == 0:
        return {"days": 0, "n": 0}
    if kind == "event":
        edge = float((active["edge"] * active["n"]).sum() / n)  # average trade
    else:
        edge = float(active["edge"].mean())
    return {
        "days": int(len(active)), "n": n,
        "ic": float(active["ic"].mean()) if kind == "score" else None,
        "ic_t": _t(active["ic"], h) if kind == "score" else None,
        "edge": edge, "edge_t": _t(active["edge"], h), "net": edge - cost,
        "hit": float(active["wins"].sum() / n),
    }


def verdict(row: dict, hurdle: float) -> str:
    def get(p, m):
        v = row.get(f"{p}_{m}")
        return v if v is not None and not np.isnan(v) else float("nan")
    dt_, tt = get("design", "edge_t"), get("test", "edge_t")
    dn, tn = get("design", "net"), get("test", "net")
    d_ok, t_ok = dt_ >= 2 and dn > 0, tt >= 2 and tn > 0
    if dt_ >= hurdle and dn > 0 and t_ok:
        return "passes"
    if d_ok and t_ok:
        return "promising"
    if t_ok:
        return "recent only"
    if d_ok:
        return "faded"
    if dt_ >= 2 and tt >= 2:
        return "costs eat it"
    if dt_ <= -2 and tt <= -2:
        return "works backwards"
    return "no edge"


def spans(cal: pd.DatetimeIndex, settings: config.Settings, recent_days: int) -> dict:
    return {
        "design": (pd.Timestamp(settings.design_start), pd.Timestamp(settings.design_end)),
        "test": (pd.Timestamp(settings.test_start), cal[-1]),
        "recent": (cal[max(0, len(cal) - recent_days)], cal[-1]),
    }


def run(md: data.MarketData, settings: config.Settings, cfg: dict,
        dividends: pd.DataFrame | None = None, only: list[str] | None = None,
        log=print) -> tuple[pd.DataFrame, pd.DataFrame]:
    """Evaluate the signals; returns (results, picks)."""
    lim = limit_series(md.calendar, settings.limits)
    tol = settings.limits.tolerance if settings.limits else 0.0
    x = signals.Inputs(md, dividends, lim, cfg["min_base_ntd"], tol)
    univ = universe(md, cfg)
    cost = round_trip(settings.costs)
    sigs = [s for s in signals.SIGNALS if not only or s.name in only]
    hurdle = NormalDist().inv_cdf(1 - cfg["family_alpha"] / (2 * len(signals.SIGNALS) * len(cfg["horizons"])))
    periods = spans(md.calendar, settings, cfg["recent_days"])
    exc = {(h, lag): excess_returns(x, univ, h, lag, tol)
           for h in cfg["horizons"] for lag in (0, 1)}
    rows, picks = [], []
    last = md.calendar[-1]
    names = md.companies.drop_duplicates("ticker").set_index("ticker")["name"]
    for s in sigs:
        t0 = time.time()
        frame = s.fn(x).astype(np.float32)
        best = None
        for h in cfg["horizons"]:
            st = daily_stats(frame, s.kind, exc[(h, 0)], univ, cfg["min_stocks"])
            row = {"signal": s.name, "family": s.family, "kind": s.kind, "label": s.label,
                   "horizon": h, "hurdle": hurdle, "cost": cost, "data_end": last.strftime("%Y-%m-%d")}
            for p, (a, b) in periods.items():
                for k, v in summarize(st.loc[a:b], s.kind, h, cost).items():
                    row[f"{p}_{k}"] = v
            a, b = periods["test"]
            late = daily_stats(frame.loc[a:b], s.kind, exc[(h, 1)].loc[a:b], univ.loc[a:b], cfg["min_stocks"])
            row["late_net"] = summarize(late, s.kind, h, cost).get("net")
            row["verdict"] = verdict(row, hurdle)
            rows.append(row)
            if row["verdict"] in GOOD and (best is None or (row.get("test_edge_t") or 0) > (best.get("test_edge_t") or 0)):
                best = row
        if best is not None:
            picks += _picks(s, frame, univ, last, best["horizon"], cfg["picks"], md, names)
        log(f"{s.name:<20} {time.time() - t0:5.1f}s  " + "  ".join(
            f"{r['horizon']}d {r['verdict']}" for r in rows if r["signal"] == s.name))
    return pd.DataFrame(rows), pd.DataFrame(picks)


def _picks(s, frame, univ, day, horizon, n, md, names) -> list[dict]:
    """Today's stocks for a signal that works: its top n (scores) or the
    events today, most traded first."""
    v = frame.loc[day].where(univ.loc[day]).dropna()
    if s.kind == "event":
        v = md.turnover.loc[day].reindex(v.index).sort_values(ascending=False)
    else:
        v = v.sort_values(ascending=False)
    return [{"date": day.strftime("%Y-%m-%d"), "signal": s.name, "horizon": horizon, "rank": i + 1,
             "ticker": t, "name": str(names.get(t, "")), "value": float(val)}
            for i, (t, val) in enumerate(v.head(n).items())]


def save(db_path: str | Path, results: pd.DataFrame, picks: pd.DataFrame, run_at: str,
         snapshot: str, code: str) -> None:
    con = sqlite3.connect(db_path)
    try:
        con.executescript(SCHEMA)
        for table, df in (("alpha_results", results), ("alpha_picks", picks)):
            if df.empty:
                continue
            df = df.assign(run_at=run_at, **({"snapshot": snapshot, "code": code} if table == "alpha_results" else {}))
            df = df.astype(object).where(df.notna(), None)
            cols = list(df.columns)
            con.executemany(f"INSERT INTO {table} ({', '.join(cols)}) VALUES ({', '.join('?' * len(cols))})",
                            df.itertuples(index=False, name=None))
        # Keep the last few runs only; the page shows the newest.
        con.execute("""DELETE FROM alpha_results WHERE run_at NOT IN
            (SELECT DISTINCT run_at FROM alpha_results ORDER BY run_at DESC LIMIT 8)""")
        con.execute("DELETE FROM alpha_picks WHERE run_at NOT IN (SELECT DISTINCT run_at FROM alpha_results)")
        con.commit()
    finally:
        con.close()


def last_run(db_path: str | Path) -> dt.datetime | None:
    con = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        row = con.execute("SELECT MAX(run_at) FROM alpha_results").fetchone()
    except sqlite3.OperationalError:
        row = None
    finally:
        con.close()
    return dt.datetime.fromisoformat(row[0]) if row and row[0] else None


def markdown(results: pd.DataFrame, run_at: str, source: str) -> str:
    """The leaderboard as Markdown, best test-period edge first."""
    def pct(v):
        return "–" if v is None or pd.isna(v) else f"{v * 100:+.2f}%"

    def num(v, f="{:.1f}"):
        return "–" if v is None or pd.isna(v) else f.format(v)

    order = {v: i for i, v in enumerate(["passes", "promising", "recent only", "faded", "costs eat it",
                                         "works backwards", "no edge"])}
    r = results.assign(_o=results["verdict"].map(order)).sort_values(["_o", "test_edge_t"], ascending=[True, False])
    lines = [f"# Alpha lab leaderboard", "", f"*Run {run_at} on {source}.*", "",
             f"Round-trip cost {pct(results['cost'].iloc[0])}; passing hurdle t ≥ {results['hurdle'].iloc[0]:.2f} "
             "in 2012–2019, confirmed with t ≥ 2 from 2020. Edge = top-decile (or event) excess return per "
             "trade vs the average liquid stock; net = after costs; late = net acted on a day later (2020 on).", "",
             "| Verdict | Signal | Days | Design edge (t) | Design net | Test edge (t) | Test net | Late net | "
             "Last 12m net | Test IC | Test hit | Test trades |",
             "|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for _, x in r.iterrows():
        lines.append(
            f"| {x['verdict']} | {x['signal']}: {x['label']} | {x['horizon']} | "
            f"{pct(x.get('design_edge'))} ({num(x.get('design_edge_t'))}) | {pct(x.get('design_net'))} | "
            f"{pct(x.get('test_edge'))} ({num(x.get('test_edge_t'))}) | {pct(x.get('test_net'))} | "
            f"{pct(x.get('late_net'))} | {pct(x.get('recent_net'))} | {num(x.get('test_ic'), '{:.3f}')} | "
            f"{num(x.get('test_hit'), '{:.0%}')} | {num(x.get('test_n'), '{:,.0f}')} |")
    return "\n".join(lines) + "\n"


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="radar_backtest.alpha", description=__doc__.split("\n\n")[0])
    ap.add_argument("--config", default=str(BACKTEST_DIR.parent / "radar.toml"))
    ap.add_argument("--only", default="", help="comma-separated signal names")
    ap.add_argument("--if-older-than", type=float, default=None, metavar="DAYS",
                    help="do nothing if the last saved run is newer than this")
    ap.add_argument("--no-save", action="store_true")
    ap.add_argument("--report", default=None, help="also write the leaderboard as Markdown here")
    args = ap.parse_args(argv)

    settings = config.load(args.config)
    if args.if_older_than is not None:
        prev = last_run(settings.db_path)
        if prev and dt.datetime.now() - prev < dt.timedelta(days=args.if_older_than):
            print(f"alpha lab last ran {prev:%Y-%m-%d %H:%M}; skipping")
            return 0
    cfg = lab_config(args.config)
    md = data.load(settings.db_path, min_coverage=settings.min_coverage)
    only = [s for s in args.only.split(",") if s] or None
    results, picks = run(md, settings, cfg, load_dividends(settings.db_path), only)
    run_at = dt.datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    snap, _ = data.snapshot(settings.db_path)
    code = code_version()
    if not args.no_save:
        save(settings.db_path, results, picks, run_at, snap, code)
    if args.report:
        Path(args.report).write_text(markdown(results, run_at, f"snapshot {snap}, code {code}"), encoding="utf-8")
    counts = results["verdict"].value_counts()
    print(json.dumps(counts.to_dict()))
    return 0


if __name__ == "__main__":
    sys.exit(main())
