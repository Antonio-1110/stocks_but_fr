"""Strategy A (issue #18): revenue momentum + 52-week high, optional 投信 filter.

Once a month, on the first trading day after the monthly revenue deadline
(the 10th, moved to the next trading day when the 10th is a holiday), rank
liquid common stocks that pass all of:

- revenue YoY growth of at least `min_yoy_pct` in each of the last
  `yoy_months` months, the newest being last month (persistence);
- an adjusted close within `max_below_high` of its `high_days`-day high;
- `min_adv_ntd` average traded value over `adv_days`;
- with `trust_filter` on: 投信 net buying over the last `trust_days` days.

The score is the average percentile rank of the weakest of those YoY months
(how persistent the growth is) and of close / 52-week high, weighted by
`high_weight`. Hold the top `top_n` at equal weight. A holding is kept while
it still passes the filters and ranks within `exit_rank`; free slots go to
the best-ranked new names. Between rebalances a holding is sold when its
adjusted close falls `stop_loss` below the close it was bought on (and, with
`stop_ma_days` > 0, when it closes below that moving average). Stopped-out
money stays in cash until the next rebalance.

All parameters live in radar.toml under [strategy.revenue_momentum].

Run both variants (without and with the 投信 filter) from backtest/:

    python3 -m strategies.revenue_momentum [--no-save]
"""

from __future__ import annotations

import re

import pandas as pd

from radar_backtest.data import PointInTime

DEFAULTS = {
    "top_n": 12,
    "exit_rank": 30,
    "yoy_months": 3,
    "min_yoy_pct": 10.0,
    "high_days": 252,
    "max_below_high": 0.10,
    "high_weight": 0.5,
    "min_adv_ntd": 10_000_000,
    "adv_days": 20,
    "revenue_deadline_day": 10,
    "trust_filter": False,
    "trust_days": 10,
    "stop_loss": 0.15,
    "stop_ma_days": 0,
}

# Taiwan common stocks have four-digit codes; ETFs start with 00, and
# warrants, preferreds and TDRs use longer codes or letters.
COMMON = re.compile(r"^[1-9]\d{3}$")


class RevenueMomentum:
    def __init__(self, params: dict):
        unknown = set(params) - set(DEFAULTS)
        if unknown:
            raise ValueError(f"unknown [strategy.revenue_momentum] keys: {sorted(unknown)}")
        self.params = {**DEFAULTS, **params}
        p = self.params
        if p["exit_rank"] < p["top_n"]:
            raise ValueError("exit_rank must be at least top_n")
        self.name = "revenue_momentum_trust" if p["trust_filter"] else "revenue_momentum"
        self._done_month: pd.Period | None = None
        # Our own book, so a stop-out can leave the other weights as they drifted:
        # ticker -> (units per unit of equity at the rebalance, entry adj close)
        self._units: dict[str, float] = {}
        self._entry: dict[str, float] = {}
        self._cash = 1.0
        self.history: list[tuple[pd.Timestamp, str, list[str]]] = []  # day, why, holdings

    # --- engine hook ------------------------------------------------------

    def targets(self, view: PointInTime) -> dict[str, float] | None:
        adj = view.adj_close
        if self._rebalance_day(adj.index):
            self._done_month = view.day.to_period("M")
            return self._rebalance(view, adj)
        return self._stops(view, adj)

    # --- schedule ---------------------------------------------------------

    def _rebalance_day(self, days: pd.DatetimeIndex) -> bool:
        """True on the first trading day after this month's revenue deadline."""
        day = days[-1]
        if self._done_month == day.to_period("M"):
            return False
        this_month = days[days >= day.replace(day=1)]
        on_or_after = this_month[this_month.day >= self.params["revenue_deadline_day"]]
        # on_or_after[0] is the deadline (the 10th or the next trading day).
        return len(on_or_after) >= 2

    # --- monthly rebalance --------------------------------------------------

    def ranked(self, view: PointInTime, adj: pd.DataFrame | None = None) -> pd.DataFrame:
        """Every stock passing the filters on `view.day`, best first, with its
        score inputs. Public so the dashboard or a notebook can show the list."""
        p = self.params
        adj = view.adj_close if adj is None else adj
        day = view.day
        last = adj.iloc[-1]
        window = adj.iloc[-p["high_days"]:]
        if len(window) < p["high_days"]:
            return _empty()
        listed = set(view.companies()["ticker"])
        cands = [t for t in last.index[last.notna()] if t in listed and COMMON.match(t)]
        if not cands:
            return _empty()

        # A full year of prices, so the 52-week high means what it says.
        enough = window[cands].notna().sum() >= int(0.9 * p["high_days"])
        cands = list(enough.index[enough])
        high = window[cands].max()
        near = last[cands] / high
        adv = view.turnover.iloc[-p["adv_days"]:][cands].fillna(0.0).mean()
        ok = (near >= 1 - p["max_below_high"]) & (adv >= p["min_adv_ntd"])
        cands = list(ok.index[ok])
        if not cands:
            return _empty()

        growth = _persistent_yoy(view.revenue(), cands, day, p["yoy_months"])
        growth = growth[growth >= p["min_yoy_pct"]]
        cands = list(growth.index)
        if p["trust_filter"] and cands:
            trust = _trust_net(view, cands, p["trust_days"])
            cands = list(trust.index[trust > 0])
        if not cands:
            return _empty()

        out = pd.DataFrame({"min_yoy_pct": growth[cands], "near_high": near[cands],
                            "adv_ntd": adv[cands]})
        out["score"] = ((1 - p["high_weight"]) * out["min_yoy_pct"].rank(pct=True)
                        + p["high_weight"] * out["near_high"].rank(pct=True))
        # Ties broken by ticker so a run is repeatable.
        out = out.rename_axis("ticker").reset_index()
        out = out.sort_values(["score", "ticker"], ascending=[False, True]).reset_index(drop=True)
        out.index += 1
        return out

    def _rebalance(self, view: PointInTime, adj: pd.DataFrame) -> dict[str, float]:
        p = self.params
        ranks = self.ranked(view, adj)
        order = list(ranks["ticker"])
        keep_rank = set(order[: p["exit_rank"]])
        keep = [t for t in order if t in self._units and t in keep_rank][: p["top_n"]]
        new = [t for t in order if t not in keep][: p["top_n"] - len(keep)]
        hold = keep + new
        last = adj.iloc[-1]
        w = 1.0 / p["top_n"]
        self._units = {t: w / last[t] for t in hold}
        self._entry = {t: self._entry[t] if t in keep else float(last[t]) for t in hold}
        self._cash = 1.0 - w * len(hold)
        self.history.append((view.day, "rebalance", sorted(hold)))
        return {t: w for t in hold}

    # --- stops between rebalances -------------------------------------------

    def _stops(self, view: PointInTime, adj: pd.DataFrame) -> dict[str, float] | None:
        p = self.params
        if not self._units:
            return None
        held = list(self._units)
        recent = adj[held].iloc[-max(p["stop_ma_days"], 1):]
        price = recent.ffill().iloc[-1]
        hit = [t for t in held
               if pd.notna(price[t]) and price[t] <= self._entry[t] * (1 - p["stop_loss"])]
        if p["stop_ma_days"] > 0 and len(recent) >= p["stop_ma_days"]:
            ma = recent.mean()
            hit += [t for t in held if t not in hit and pd.notna(adj[t].iloc[-1])
                    and adj[t].iloc[-1] < ma[t]]
        if not hit:
            return None
        # Weights as the book has drifted since the rebalance, minus the stopped names.
        value = {t: u * (price[t] if pd.notna(price[t]) else 0.0) for t, u in self._units.items()}
        equity = self._cash + sum(value.values())
        for t in hit:
            self._cash += value.pop(t)
            del self._units[t], self._entry[t]
        self.history.append((view.day, "stop", sorted(hit)))
        return {t: v / equity for t, v in value.items()}


def _empty() -> pd.DataFrame:
    return pd.DataFrame(columns=["ticker", "min_yoy_pct", "near_high", "adv_ntd", "score"])


def _persistent_yoy(revenue: pd.DataFrame, tickers: list[str], day: pd.Timestamp,
                    months: int) -> pd.Series:
    """Weakest YoY % over the last `months` months, for tickers whose newest
    published month is last month and who have all `months` of them."""
    newest = (day.to_period("M") - 1).to_timestamp()
    wanted = pd.date_range(end=newest, periods=months, freq="MS")
    r = revenue[revenue["ticker"].isin(tickers) & revenue["month"].isin(wanted)]
    r = r[r["yoy_pct"].notna()]
    g = r.groupby("ticker")["yoy_pct"]
    worst = g.min()
    return worst[g.count() == months]


def _trust_net(view: PointInTime, tickers: list[str], days: int) -> pd.Series:
    """投信 net shares bought over the last `days` trading days (0 if none)."""
    since = view.close.index[-days]
    f = view.flows()
    f = f[(f["date"] >= since) & f["ticker"].isin(tickers)]
    return f.groupby("ticker")["trust_net"].sum().reindex(tickers, fill_value=0)


def make(params: dict, settings) -> RevenueMomentum:
    return RevenueMomentum(params)


# --- both variants, with data coverage -------------------------------------

def coverage(data) -> pd.DataFrame:
    """Per year: stocks with prices, with revenue, with 投信 flows. A thin year
    means the collectors haven't backfilled it yet and results there aren't final."""
    years = data.close.index.year
    priced = data.close.notna().groupby(years).any().sum(axis=1)
    rev = data.revenue.groupby(data.revenue["month"].dt.year)["ticker"].nunique()
    flows = data.flows.groupby(data.flows["date"].dt.year)["ticker"].nunique()
    return pd.DataFrame({"stocks_with_prices": priced, "with_revenue": rev,
                         "with_flows": flows}).fillna(0).astype(int)


def main() -> None:
    import argparse

    from radar_backtest import config, data, runner

    ap = argparse.ArgumentParser(description="Run revenue_momentum without and with the 投信 filter")
    ap.add_argument("--config", default=str(runner.BACKTEST_DIR.parent / "radar.toml"))
    ap.add_argument("--no-save", action="store_true", help="don't write to backtest_runs")
    args = ap.parse_args()

    settings = config.load(args.config)
    md = data.load(settings.db_path)
    print("Data coverage by year:")
    print(coverage(md).to_string())
    base = settings.strategies.get("revenue_momentum", {})
    for trust in (False, True):
        params = {**base, "trust_filter": trust}
        out = runner.run(md, settings, lambda: RevenueMomentum(params), save=not args.no_save)
        for p in out["periods"]:
            s, b = p["summary"], p["bench_summary"]
            print(f"{out['strategy']:>24} {p['period']:>6} {s['start']}..{s['end']}  "
                  f"CAGR {s['cagr']:.2%}  maxDD {s['max_drawdown']:.2%}  Sharpe {s['sharpe'] or 0:.2f}"
                  f"  | {p['benchmark']} CAGR {b['cagr']:.2%}  maxDD {b['max_drawdown']:.2%}")
        for path in out["reports"]:
            print("report:", path)


if __name__ == "__main__":
    main()
