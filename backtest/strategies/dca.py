"""Strategy C (issue #20): average down as a stock falls, sell on a small bounce.

The owner's idea, tested as described and in a capped form:

- Start a cycle in a liquid stock that has dipped: the close is `entry_drop`
  below its `entry_high_days`-day high. Only the `universe_size` most traded
  common stocks listed that day qualify (ETFs excluded), each averaging at
  least `min_avg_turnover` NTD a day over `universe_days` days. Stocks that
  were later delisted are in the universe while they traded. When more stocks
  dip than there are free slots (`max_cycles`), the most traded go first.
- The first buy is `base_order` NTD. Every time the close falls `add_step`
  below the last buy price, buy again: add k is base_order x multiplier^k
  (multiplier 2 doubles every add, the classic martingale; 1 is plain DCA).
- Sell everything when the close is `take_profit` above the average cost
  (cost includes buy commissions; the sell tax is paid on top).
- `max_adds` = 0 means no limit: adds continue until cash runs out. A
  `stop_loss` above 0 sells everything when the close is that far below the
  average cost.

Variants are tables under [strategy.dca] in radar.toml that override these
keys: `martingale` (as described: no cap, no stop) and `capped`.

Every signal is taken at a close and filled at the next open, like the shared
engine. The shared engine takes target weights, which can't say "buy 200,000
NTD more of this one and leave the rest alone", so this file runs the same
engine with order-based execution (OrderEngine below): same fills, costs,
liquidity cap and delisting rules, plus a record of each cycle and of every add
the account could not pay for.

Run both variants from backtest/:  python3 -m strategies.dca
"""

from __future__ import annotations

import argparse
import dataclasses
import html
import sys
from dataclasses import dataclass, field
from pathlib import Path
from unittest import mock

import pandas as pd

BACKTEST_DIR = Path(__file__).resolve().parents[1]
if str(BACKTEST_DIR) not in sys.path:
    sys.path.insert(0, str(BACKTEST_DIR))

from radar_backtest import config, costs as fees, data, runner  # noqa: E402
from radar_backtest.engine import Engine, RunResult  # noqa: E402

DEFAULTS = {
    "universe_size": 150,
    "universe_days": 60,
    "min_avg_turnover": 50_000_000,
    "entry_drop": 0.10,
    "entry_high_days": 60,
    "max_cycles": 5,
    "base_order": 50_000,
    "add_step": 0.05,
    "multiplier": 2.0,
    "take_profit": 0.01,
    "max_adds": 0,
    "stop_loss": 0.0,
}
OTHER_KEYS = {"variant", "stress_windows"}


def variants(params: dict) -> list[str]:
    return [k for k, v in params.items() if isinstance(v, dict)]


def make(params: dict, settings, variant: str | None = None) -> "DCA":
    """`variant` names a table under [strategy.dca]; default: params["variant"]."""
    variant = variant or params.get("variant", "martingale")
    tables = {k: v for k, v in params.items() if isinstance(v, dict)}
    if variant not in tables:
        raise ValueError(f"[strategy.dca]: no [strategy.dca.{variant}] table")
    base = {k: v for k, v in params.items() if not isinstance(v, dict)}
    unknown = (set(base) - set(DEFAULTS) - OTHER_KEYS) | (set(tables[variant]) - set(DEFAULTS))
    if unknown:
        raise ValueError(f"[strategy.dca]: unknown keys {sorted(unknown)}")
    p = {**DEFAULTS, **{k: v for k, v in base.items() if k in DEFAULTS}, **tables[variant]}
    return DCA(variant, p, settings)


# --- orders and the book the strategy sees ---------------------------------

@dataclass(frozen=True)
class Order:
    ticker: str
    side: str        # "buy" or "sell" (sell = the whole position)
    value: float     # NTD to buy; ignored for sells
    reason: str      # "entry", "add", "take_profit", "stop"


@dataclass(frozen=True)
class Holding:
    units: float
    cost: float          # NTD paid for the units held, buy commissions included
    adds: int            # buys after the first in this cycle
    last_buy: float      # adjusted fill price of the latest buy
    opened: pd.Timestamp

    @property
    def avg_cost(self) -> float:
        return self.cost / self.units


@dataclass
class Cycle:
    ticker: str
    opened: pd.Timestamp
    trade: object                    # engine.Trade for this round trip
    adds: int = 0
    last_buy: float = 0.0
    peak_cost: float = 0.0           # most NTD tied up in this cycle at once
    days: int = 0                    # trading days held
    closed: pd.Timestamp | None = None
    reason: str = "open"             # take_profit, stop, delisted, open


@dataclass
class Stats:
    cycles: list[Cycle] = field(default_factory=list)
    # (ticker, cycle opened, add number) -> (first day, NTD wanted, NTD bought)
    short: dict = field(default_factory=dict)
    short_days: set = field(default_factory=set)
    adv_capped: int = 0


class OrderEngine(Engine):
    """Engine that also runs order-based strategies (those with `orders`).

    Each day, after the close is marked, `strategy.orders(view, book)` returns
    orders to fill at the next open. Sells go first, then buys in the order
    given. A buy is cut to what the cash pays for (counted in stats.short) and
    to the engine's liquidity cap (stats.adv_capped). Weight-based strategies,
    like the benchmark, run exactly as in Engine.
    """

    def run(self, strategy, start, end=None) -> RunResult:
        if not hasattr(strategy, "orders"):
            return super().run(strategy, start, end)
        cal = self.data.calendar
        days = cal[cal >= pd.Timestamp(start)]
        if end is not None:
            days = days[days <= pd.Timestamp(end)]
        if len(days) < 2:
            raise ValueError(f"no trading days between {start} and {end}")
        res = RunResult(strategy.name, dict(strategy.params), days[0], days[-1],
                        self.s.initial_capital, pd.Series(dtype=float), pd.Series(dtype=float), [])
        self._cash = self.s.initial_capital
        self._pos = {}
        self._res = res
        self.stats = Stats()
        self._cycles: dict[str, Cycle] = {}
        self._reason = "delisted"
        pending = None
        equity, invested = [], []

        for day in days:
            self._reason = "delisted"
            self._delist(day)
            if pending is not None:
                self._execute(day, *pending)
                pending = None
            value = self._mark(day)
            for c in self._cycles.values():
                c.days += 1
            equity.append(self._cash + value)
            invested.append(value)
            orders = strategy.orders(self.data.view(day), self._book())
            if orders:
                pending = (day, list(orders))

        res.equity = pd.Series(equity, index=days)
        res.invested = pd.Series(invested, index=days)
        for t, p in self._pos.items():
            p.trade.mark = p.units * self.marks.at[days[-1], t]
            res.open_trades.append(p.trade)
        self.stats.cycles += self._cycles.values()
        strategy.result, strategy.stats = res, self.stats
        return res

    def _book(self) -> dict[str, Holding]:
        return {t: Holding(p.units, p.basis, self._cycles[t].adds, self._cycles[t].last_buy,
                           self._cycles[t].opened) for t, p in self._pos.items()}

    def _affordable(self) -> float:
        """Most NTD one buy can be for, commission included, from cash."""
        c = self.s.costs
        v = self._cash / (1 + c.commission_rate * c.broker_discount)
        while v > 0 and v + fees.commission(c, v) > self._cash:
            v = min(v - 1, self._cash - fees.commission(c, v))
        return max(v, 0.0)

    def _execute(self, day, signal_day, orders: list[Order]) -> None:
        opens = self.data.adj_open.loc[day]
        for o in [o for o in orders if o.side == "sell"]:
            px = opens.get(o.ticker)
            if o.ticker not in self._pos:
                continue
            if px is None or pd.isna(px):
                self._res.skipped_orders += 1
                continue
            self._reason = o.reason
            self._sell(day, o.ticker, self._pos[o.ticker].units * px, px, everything=True)
        for o in [o for o in orders if o.side == "buy"]:
            px = opens.get(o.ticker)
            if px is None or pd.isna(px):
                self._res.skipped_orders += 1
                continue
            held = self._pos[o.ticker].units * px if o.ticker in self._pos else 0.0
            adv = self.adv.at[signal_day, o.ticker] if o.ticker in self.adv else float("nan")
            room = self.s.max_adv_fraction * adv - held if pd.notna(adv) else 0.0
            value = min(o.value, max(room, 0.0))
            if value < o.value:
                self.stats.adv_capped += 1
            cash = self._affordable()
            if value > cash:
                c = self._cycles.get(o.ticker)
                key = (o.ticker, c.opened if c else day, c.adds + 1 if c else 0)
                self.stats.short.setdefault(key, (day, o.value, cash if cash >= self.s.min_trade_value else 0.0))
                self.stats.short_days.add(day)
                value = cash
            if value < min(self.s.min_trade_value, o.value) or value <= 0:
                continue
            self._buy(day, o.ticker, value, px)
            c = self._cycles.get(o.ticker)
            if c is None:
                c = self._cycles[o.ticker] = Cycle(o.ticker, day, self._pos[o.ticker].trade)
            elif o.reason == "add":
                c.adds += 1
            c.last_buy = px
            c.peak_cost = max(c.peak_cost, self._pos[o.ticker].basis)

    def _sell(self, day, ticker, value, price, everything) -> None:
        super()._sell(day, ticker, value, price, everything)
        if ticker not in self._pos and ticker in self._cycles:
            c = self._cycles.pop(ticker)
            c.closed, c.reason = day, self._reason
            self.stats.cycles.append(c)


# --- the strategy -----------------------------------------------------------

class DCA:
    def __init__(self, variant: str, params: dict, settings):
        self.variant = variant
        self.name = f"dca_{variant}"
        self.params = params
        self.p = params

    def targets(self, view):
        raise RuntimeError("dca places orders, not target weights: run `python3 -m strategies.dca` from backtest/")

    def orders(self, view, book: dict[str, Holding]) -> list[Order]:
        p = self.p
        px = view.adj_close
        today = px.iloc[-1]
        out: list[Order] = []
        for t, h in book.items():
            price = today.get(t)
            if price is None or pd.isna(price):
                continue  # not traded today; a delisting is handled by the engine
            if price >= h.avg_cost * (1 + p["take_profit"]):
                out.append(Order(t, "sell", 0.0, "take_profit"))
            elif p["stop_loss"] > 0 and price <= h.avg_cost * (1 - p["stop_loss"]):
                out.append(Order(t, "sell", 0.0, "stop"))
            elif ((p["max_adds"] <= 0 or h.adds < p["max_adds"])
                  and price <= h.last_buy * (1 - p["add_step"])):
                out.append(Order(t, "buy", p["base_order"] * p["multiplier"] ** (h.adds + 1), "add"))

        free = int(p["max_cycles"]) - len(book)
        if free > 0:
            for t in self._dips(view, px, today, set(book))[:free]:
                out.append(Order(t, "buy", float(p["base_order"]), "entry"))
        return out

    def _dips(self, view, px, today, held: set[str]) -> list[str]:
        p = self.p
        n = int(p["universe_days"])
        turnover = view.turnover.iloc[-n:]
        if len(turnover) < n:
            return []
        c = view.companies()
        common = [t for t, ind in zip(c["ticker"], c["industry"].fillna(""))
                  if not fees.is_etf(t, ind) and t in turnover.columns]
        avg = turnover[common].fillna(0.0).mean()
        avg = avg[avg >= p["min_avg_turnover"]].sort_values(ascending=False, kind="stable")
        universe = list(avg.index[: int(p["universe_size"])])
        k = int(p["entry_high_days"])
        if len(px) < k:
            return []
        high = px[universe].iloc[-k:].max()
        return [t for t in universe
                if t not in held and pd.notna(today.get(t))
                and today[t] <= high[t] * (1 - p["entry_drop"])]


# --- reporting ---------------------------------------------------------------

def summarize(res: RunResult, stats: Stats, bench_equity: pd.Series | None,
              windows: list) -> dict:
    """The numbers this strategy's risk lives in, beyond the shared report."""
    cycles = stats.cycles
    closed = [c for c in cycles if c.closed is not None]
    by = {r: sum(c.reason == r for c in cycles) for r in ("take_profit", "stop", "delisted", "open")}
    worst = min(cycles, key=lambda c: c.trade.pnl, default=None)
    biggest = max(cycles, key=lambda c: c.peak_cost, default=None)
    longest = max(cycles, key=lambda c: c.days, default=None)
    tp = [c.trade.pnl for c in cycles if c.reason == "take_profit"]
    adds = pd.Series([c.adds for c in cycles], dtype=int).value_counts().sort_index()
    short_cycles = {(t, o) for t, o, _ in stats.short}
    days = len(res.equity)
    out = {
        "cycles": len(cycles),
        "closed": len(closed),
        "take_profit": by["take_profit"], "stopped": by["stop"],
        "delisted": by["delisted"], "still_open": by["open"],
        "cycle_win_rate": (sum(c.trade.pnl > 0 for c in closed) / len(closed)) if closed else None,
        "avg_take_profit_ntd": sum(tp) / len(tp) if tp else None,
        "worst_cycle": (f"{worst.ticker} {worst.opened.date()} to "
                        f"{(worst.closed or res.end).date()}: {worst.trade.pnl:,.0f} NTD "
                        f"({worst.trade.ret:.1%}), {worst.adds} adds, {worst.reason}") if worst else None,
        "biggest_cycle": (f"{biggest.ticker} from {biggest.opened.date()}: "
                          f"{biggest.peak_cost:,.0f} NTD at once") if biggest else None,
        "longest_cycle": (f"{longest.ticker} from {longest.opened.date()}: {longest.days} trading days, "
                          f"{longest.reason}") if longest else None,
        "adds_histogram": ", ".join(f"{k} adds: {v}" for k, v in adds.items()) or None,
        "short_adds": len(stats.short),
        "short_cycles": len(short_cycles),
        "short_days": len(stats.short_days),
        "short_days_pct": len(stats.short_days) / days if days else 0.0,
        "first_short": min((d for d, _, _ in stats.short.values()), default=None),
        "adv_capped": stats.adv_capped,
        "windows": [],
    }
    if out["first_short"] is not None:
        out["first_short"] = out["first_short"].date().isoformat()
    for label, a, b in windows:
        eq = res.equity.loc[a:b]
        if len(eq) < 2:
            continue
        dd = float((eq / eq.cummax() - 1).min())
        row = {
            "window": f"{label} ({eq.index[0].date()} to {eq.index[-1].date()})",
            "return": float(eq.iloc[-1] / eq.iloc[0] - 1),
            "max_drawdown": dd,
            "peak_deployed": float(res.invested.loc[a:b].max()),
            "short_adds": sum(pd.Timestamp(a) <= d <= pd.Timestamp(b) for d, _, _ in stats.short.values()),
            "stops_delists": sum(c.closed is not None and c.reason in ("stop", "delisted")
                                 and pd.Timestamp(a) <= c.closed <= pd.Timestamp(b) for c in cycles),
        }
        if bench_equity is not None:
            be = bench_equity.loc[a:b]
            row["bench_return"] = float(be.iloc[-1] / be.iloc[0] - 1) if len(be) >= 2 else None
        out["windows"].append(row)
    return out


def coverage(md: data.MarketData, start, end) -> dict:
    """How much of the market the database holds for a period."""
    c = md.companies
    s, e = pd.Timestamp(start), pd.Timestamp(end)
    listed = c[(c["listed_on"].isna() | (c["listed_on"] <= e))
               & (c["delisted_on"].isna() | (c["delisted_on"] >= s))]
    listed = listed[[not fees.is_etf(t, i or "") for t, i in zip(listed["ticker"], listed["industry"])]]
    px = md.close.loc[s:e]
    priced = set(px.columns[px.notna().any()])
    have = listed["ticker"].isin(priced)
    gone = listed["delisted_on"].notna()
    return {"listed": int(len(listed)), "priced": int(have.sum()),
            "delisted_listed": int(gone.sum()), "delisted_priced": int((have & gone).sum()),
            "share": float(have.mean()) if len(listed) else 0.0}


ROWS = [
    ("cycles", "Cycles started", "s"), ("take_profit", "Closed at take profit", "s"),
    ("stopped", "Closed by the stop", "s"), ("delisted", "Ended by delisting", "s"),
    ("still_open", "Still open at the end", "s"),
    ("cycle_win_rate", "Win rate (closed cycles)", "pct"),
    ("avg_take_profit_ntd", "Average take-profit cycle (NTD)", "ntd"),
    ("worst_cycle", "Worst cycle", "s"), ("biggest_cycle", "Most capital in one cycle", "s"),
    ("longest_cycle", "Longest cycle", "s"), ("adds_histogram", "Cycles by number of adds", "s"),
    ("short_adds", "Adds the cash couldn't pay for in full", "s"),
    ("short_cycles", "Cycles that ran out of cash", "s"),
    ("short_days", "Days an add was short of cash", "s"),
    ("short_days_pct", "Share of trading days short of cash", "pct"),
    ("first_short", "First time out of cash", "s"),
    ("adv_capped", "Buys cut by the liquidity cap", "s"),
]


def _fmt(v, kind):
    if v is None:
        return "–"
    return {"pct": lambda x: f"{x * 100:.2f}%", "ntd": lambda x: f"{x:,.0f}"}.get(kind, str)(v)


def section(periods: list[tuple[str, dict, dict, dict]]) -> list[tuple[str, list[str], list[list[str]]]]:
    """(title, header, rows) tables; periods = (name, dca summary, coverage, span)."""
    tables = []
    head = ["Metric"] + [name for name, *_ in periods]
    tables.append(("Cycles and cash", head,
                   [[label] + [_fmt(s.get(k), kind) for _, s, _, _ in periods] for k, label, kind in ROWS]))
    cov = [["Common stocks listed during the period"] + [str(c["listed"]) for _, _, c, _ in periods],
           ["...with prices in the database"] + [f"{c['priced']} ({c['share']:.0%})" for _, _, c, _ in periods],
           ["Delisted stocks listed during the period"] + [str(c["delisted_listed"]) for _, _, c, _ in periods],
           ["...with prices in the database"] + [str(c["delisted_priced"]) for _, _, c, _ in periods]]
    tables.append(("Data coverage", head, cov))
    wrows = [[w["window"], period, _fmt(w["return"], "pct"), _fmt(w.get("bench_return"), "pct"),
              _fmt(w["max_drawdown"], "pct"), _fmt(w["peak_deployed"], "ntd"),
              str(w["short_adds"]), str(w["stops_delists"])]
             for period, s, _, _ in periods for w in s["windows"]]
    tables.append(("Crashes", ["Window", "Period", "Return", "Benchmark", "Max drawdown",
                               "Peak deployed (NTD)", "Adds short of cash", "Stops and delistings"], wrows))
    return tables


def warning(covs: list[dict]) -> str | None:
    low = min((c["share"] for c in covs), default=0.0)
    if low < 0.95:
        return (f"PARTIAL DATA: the database has prices for as little as {low:.0%} of the common "
                "stocks listed in a period. These numbers are not final.")
    return None


def append_reports(paths: list[Path], tables, note: str | None, windows_missing: list[str]) -> None:
    notes = [note] if note else []
    notes += [f"No data for the {w} drawdown." for w in windows_missing]
    notes.append("A cycle is one stock from its first buy until it is sold or delisted. "
                 "Worst cycle includes cycles still open at the end, marked at the last close.")
    md = ["", "## DCA details", ""] + [f"> {n}" for n in notes]
    ht = "<h2>DCA details</h2>" + "".join(f"<p><strong>{html.escape(n)}</strong></p>" for n in notes)
    for title, head, rows in tables:
        md += ["", f"### {title}", "", "| " + " | ".join(head) + " |", "|" + "---|" * len(head)]
        md += ["| " + " | ".join(r) + " |" for r in rows]
        ht += (f"<h3>{html.escape(title)}</h3><div class=\"wrap\"><table><thead><tr>"
               + "".join(f"<th>{html.escape(h)}</th>" for h in head) + "</tr></thead><tbody>"
               + "".join("<tr>" + "".join(f"<td>{html.escape(c)}</td>" for c in r) + "</tr>" for r in rows)
               + "</tbody></table></div>")
    for path in paths:
        text = Path(path).read_text(encoding="utf-8")
        if str(path).endswith(".md"):
            text = text.rstrip("\n") + "\n" + "\n".join(md) + "\n"
            if note:
                text = text.replace("\n", f"\n\n> {note}\n", 1)
        else:
            text = text.replace("</body>", ht + "</body>")
            if note:
                text = text.replace("<h1>", f"<p><strong>{html.escape(note)}</strong></p><h1>", 1)
        Path(path).write_text(text, encoding="utf-8")


def run_variant(md: data.MarketData, settings, variant: str, save: bool = True,
                report_dir: Path | None = runner.BACKTEST_DIR / "reports") -> dict:
    params = settings.strategies.get("dca", {})
    made: list[DCA] = []

    def factory():
        made.append(make(params, settings, variant))
        return made[-1]

    with mock.patch.object(runner, "Engine", OrderEngine):
        out = runner.run(md, settings, factory, report_dir=report_dir, save=save)
    ran = [s for s in made if hasattr(s, "result")]
    windows = [tuple(w) for w in params.get("stress_windows", [])]
    periods, covs, seen = [], [], set()
    for p, s in zip(out["periods"], ran):
        dsum = summarize(s.result, s.stats, p["bench_equity"], windows)
        seen |= {w["window"].split(" (")[0] for w in dsum["windows"]}
        cov = coverage(md, s.result.start, s.result.end)
        covs.append(cov)
        periods.append((p["period"], dsum, cov, (s.result.start, s.result.end)))
        p["dca"], p["coverage"] = dsum, cov
    missing = [w[0] for w in windows if w[0] not in seen]
    note = warning(covs)
    if out["reports"]:
        append_reports(out["reports"], section(periods), note, missing)
    out["warning"], out["missing_windows"] = note, missing
    return out


def main(argv=None) -> None:
    ap = argparse.ArgumentParser(prog="strategies.dca", description="Run the DCA variants.")
    ap.add_argument("variants", nargs="*", help="variant tables to run (default: all)")
    ap.add_argument("--config", default=str(BACKTEST_DIR.parent / "radar.toml"))
    ap.add_argument("--db", help="database to read instead of radar.toml's db_path")
    ap.add_argument("--no-save", action="store_true", help="don't write to backtest_runs")
    args = ap.parse_args(argv)
    settings = config.load(args.config)
    if args.db:
        settings = dataclasses.replace(settings, db_path=Path(args.db))
    md = data.load(settings.db_path)
    for v in args.variants or variants(settings.strategies.get("dca", {})):
        out = run_variant(md, settings, v, save=not args.no_save)
        if out["warning"]:
            print(out["warning"])
        for p in out["periods"]:
            s, d = p["summary"], p["dca"]
            print(f"{out['strategy']:>16} {p['period']:>6} {s['start']}..{s['end']}  CAGR {s['cagr']:.2%}  "
                  f"maxDD {s['max_drawdown']:.2%}  cycles {d['cycles']}  win {_fmt(d['cycle_win_rate'], 'pct')}  "
                  f"short of cash {d['short_adds']}  | {p['benchmark']} CAGR {p['bench_summary']['cagr']:.2%}")
        for path in out["reports"]:
            print("report:", path)


if __name__ == "__main__":
    main()
