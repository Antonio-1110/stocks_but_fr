"""Markdown and HTML reports in backtest/reports/."""

from __future__ import annotations

import html
import json

import pandas as pd

from .metrics import calendar_years

# (key, label, format)
ROWS = [
    ("start", "Start", "s"), ("end", "End", "s"),
    ("final_equity", "Final equity (NTD)", "ntd"),
    ("total_return", "Total return", "pct"),
    ("cagr", "CAGR", "pct"),
    ("max_drawdown", "Max drawdown", "pct"),
    ("sharpe", "Sharpe (rf = 0)", "f2"),
    ("sortino", "Sortino (rf = 0)", "f2"),
    ("volatility", "Volatility (annualised)", "pct"),
    ("longest_drawdown_days", "Longest drawdown, peak to recovery (trading days)", "s"),
    ("longest_drawdown_dates", "Longest drawdown: dates", "s"),
    ("avg_exposure", "Average exposure (invested / equity)", "pct"),
    ("excess_cagr", "Excess CAGR over the benchmark", "pct"),
    ("tracking_error", "Tracking error (annualised)", "pct"),
    ("information_ratio", "Information ratio", "f2"),
    ("trades", "Closed trades", "s"),
    ("open_trades", "Still open at end", "s"),
    ("win_rate", "Win rate (closed trades)", "pct"),
    ("top5_pnl_share", "Share of total P&L from the top 5 trades", "pct"),
    ("worst_trade_pct", "Worst single trade", "pct"),
    ("worst_trade_ntd", "Worst single trade (NTD)", "ntd"),
    ("worst_trade_ticker", "Worst trade: ticker", "s"),
    ("worst_trade_dates", "Worst trade: held", "s"),
    ("peak_deployed", "Peak capital deployed (NTD)", "ntd"),
    ("peak_deployed_pct", "Peak capital deployed (% of equity)", "pct"),
    ("longest_underwater_days", "Longest a position was underwater (trading days)", "s"),
    ("longest_underwater_ticker", "Longest underwater: ticker", "s"),
    ("longest_underwater_dates", "Longest underwater: dates", "s"),
    ("annual_turnover", "Turnover per year (x equity)", "f2"),
    ("commission_paid", "Commission paid (NTD)", "ntd"),
    ("tax_paid", "Sell tax paid (NTD)", "ntd"),
    ("total_costs", "Total costs paid (NTD)", "ntd"),
    ("skipped_orders", "Orders skipped (no price on fill day)", "s"),
    ("limit_blocked", "Fills blocked by price limits (buys at limit-up, sell-days at limit-down)", "s"),
]


def _fmt(v, kind: str) -> str:
    if v is None or (isinstance(v, float) and pd.isna(v)):
        return "–"
    if kind == "pct":
        return f"{v * 100:.2f}%"
    if kind == "ntd":
        return f"{v:,.0f}"
    if kind == "f2":
        return f"{v:.2f}"
    return str(v)


def _table(periods: list[dict], strategy: str) -> tuple[list[str], list[list[str]]]:
    head = ["Metric"]
    for p in periods:
        head += [f"{strategy} ({p['period']})", f"{p['benchmark']} ({p['period']})"]
    body = []
    for key, label, kind in ROWS:
        row = [label]
        for p in periods:
            row += [_fmt(p["summary"].get(key), kind), _fmt(p["bench_summary"].get(key), kind)]
        body.append(row)
    row = [f"{periods[0]['benchmark']} adjusted-close return, no costs"]
    for p in periods:
        row += ["", _fmt(p["bench_price_return"], "pct")]
    body.append(row)
    return head, body


def _years(periods: list[dict], strategy: str) -> tuple[list[str], list[list[str]]]:
    """Calendar-year returns, strategy against benchmark, every period in one
    table (a year split between periods appears once per period)."""
    bench = periods[0]["benchmark"]
    head = ["Year", "Period", strategy, bench, "Difference"]
    body = []
    for p in periods:
        cap = p["summary"]["initial_capital"]
        a = calendar_years(p["equity"], cap)
        b = calendar_years(p["bench_equity"], p["bench_summary"]["initial_capital"])
        last = pd.Timestamp(p["summary"]["end"])
        first = pd.Timestamp(p["summary"]["start"])
        for y in a.index:
            partial = (y == first.year and (first.month, first.day) > (1, 5)) or \
                      (y == last.year and (last.month, last.day) < (12, 24))
            diff = a[y] - b[y] if y in b.index else None
            body.append([f"{y}" + (" (partial)" if partial else ""), p["period"],
                         _fmt(a[y], "pct"), _fmt(b.get(y), "pct"), _fmt(diff, "pct")])
    return head, body


NOTES = [
    "Orders are decided at a close and filled at the next trading day's open.",
    "Prices are dividend- and split-adjusted, so returns include dividends.",
    "Costs: commission x broker discount (minimum fee per order) and sell tax, from radar.toml.",
    "Each period starts from the initial capital in cash; positions do not carry over.",
    "Worst trade and longest underwater include positions still open at the end, marked at the last close.",
    "Monthly revenue is used only after its announced_on date (the filing deadline).",
    "Stocks that stop trading are sold at their last traded price.",
    "Volatility, Sharpe and Sortino are annualised from daily returns (252 a year); Sortino counts only down days as risk.",
    "Longest drawdown runs from a peak close to the first close back at or above it.",
    "Tracking error is the annualised spread of daily returns against the benchmark; information ratio is the annualised average daily difference over it.",
    "Top-5 share: profit of the five best trades (closed or still open) over the total profit of all trades.",
    "Calendar-year returns run from the previous year-end close (or the initial capital) to the year-end close.",
]


def markdown(strategy: str, params: dict, run_at: str, source: str, periods: list[dict]) -> str:
    head, body = _table(periods, strategy)
    out = [f"# Backtest: {strategy}", "",
           f"Run at {run_at} on `{source}`.", "",
           f"Parameters: `{json.dumps(params, ensure_ascii=False, sort_keys=True)}`", "",
           "| " + " | ".join(head) + " |",
           "|" + "---|" * len(head)]
    out += ["| " + " | ".join(r) + " |" for r in body]
    yh, yb = _years(periods, strategy)
    out += ["", "## Calendar-year returns", "", "| " + " | ".join(yh) + " |", "|" + "---|" * len(yh)]
    out += ["| " + " | ".join(r) + " |" for r in yb]
    out += ["", "## How to read this", ""] + [f"- {n}" for n in NOTES]
    for p in periods:
        if p["log"]:
            out += ["", f"## Events ({p['period']})", ""] + [f"- {l}" for l in p["log"][:200]]
    return "\n".join(out) + "\n"


def html_page(strategy: str, params: dict, run_at: str, source: str, periods: list[dict]) -> str:
    head, body = _table(periods, strategy)
    e = html.escape
    rows = "".join("<tr>" + "".join(f"<td>{e(c)}</td>" for c in r) + "</tr>" for r in body)
    yh, yb = _years(periods, strategy)
    years = ("<h2>Calendar-year returns</h2><div class=\"wrap\"><table><thead><tr>"
             + "".join(f"<th>{e(h)}</th>" for h in yh) + "</tr></thead><tbody>"
             + "".join("<tr>" + "".join(f"<td>{e(c)}</td>" for c in r) + "</tr>" for r in yb)
             + "</tbody></table></div>")
    charts = "".join(f"<h2>Equity, {e(p['period'])} period</h2>"
                     + _svg(p["equity"], p["bench_equity"], strategy, p["benchmark"])
                     for p in periods)
    notes = "".join(f"<li>{e(n)}</li>" for n in NOTES)
    events = "".join(
        f"<h2>Events ({e(p['period'])})</h2><ul>" + "".join(f"<li>{e(l)}</li>" for l in p["log"][:200]) + "</ul>"
        for p in periods if p["log"])
    return f"""<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Backtest: {e(strategy)}</title>
<style>
body {{ font: 14px/1.5 system-ui, sans-serif; margin: 2rem auto; max-width: 70rem; padding: 0 1rem; color: #222; background: #fff; }}
table {{ border-collapse: collapse; width: 100%; }}
th, td {{ border-bottom: 1px solid #ddd; padding: .3rem .6rem; text-align: right; }}
th:first-child, td:first-child {{ text-align: left; }}
.wrap {{ overflow-x: auto; }}
code {{ word-break: break-all; }}
svg {{ width: 100%; height: auto; }}
</style></head><body>
<h1>Backtest: {e(strategy)}</h1>
<p>Run at {e(run_at)} on <code>{e(source)}</code>.</p>
<p>Parameters: <code>{e(json.dumps(params, ensure_ascii=False, sort_keys=True))}</code></p>
<div class="wrap"><table><thead><tr>{''.join(f'<th>{e(h)}</th>' for h in head)}</tr></thead>
<tbody>{rows}</tbody></table></div>
{years}
{charts}
<h2>How to read this</h2><ul>{notes}</ul>
{events}
</body></html>
"""


def _svg(a: pd.Series, b: pd.Series, la: str, lb: str, w: int = 800, h: int = 260) -> str:
    """Two equity curves on a log scale, no JavaScript."""
    import math
    both = pd.concat([a, b]).clip(lower=1)
    lo, hi = math.log(both.min()), math.log(both.max())
    span = hi - lo or 1.0
    n = max(len(a) - 1, 1)

    def pts(s: pd.Series) -> str:
        return " ".join(f"{i * w / n:.1f},{h - (math.log(max(v, 1)) - lo) / span * (h - 20) - 10:.1f}"
                        for i, v in enumerate(s.reindex(a.index).ffill()))
    e = html.escape
    return (f'<svg viewBox="0 0 {w} {h + 20}" role="img" aria-label="Equity curves">'
            f'<polyline fill="none" stroke="#999" stroke-width="1.5" points="{pts(b)}"/>'
            f'<polyline fill="none" stroke="#1565c0" stroke-width="1.5" points="{pts(a)}"/>'
            f'<text x="0" y="{h + 16}" fill="#1565c0">{e(la)}</text>'
            f'<text x="{w / 2}" y="{h + 16}" fill="#777">{e(lb)}</text></svg>')
