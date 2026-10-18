"""Follow foreign investors (外資), one day late (issue #66).

三大法人 flows are published after the close, so the earliest a strategy can
act on day t's foreign buying is day t+1's open. That is exactly what the
engine does (signal at the close, fill at the next open), so nothing here
peeks ahead.

The measure is foreign "intensity": NTD net bought by foreign investors over
the last `flow_days` trading days divided by the stock's total NTD traded
over the same days. A stock foreigners bought on most days (`min_buy_share`
of the window) and hard (high intensity) is what they are accumulating; a
one-day spike is mostly priced in by the close (see
docs/foreign-flow-results.md).

Two strategies share the revenue momentum schedule, book and stop-loss
(rebalance on the first trading day after the 10th, top_n equal weight, keep
while within exit_rank, sell between rebalances at stop_loss):

- `foreign_flow`: rank liquid common stocks by intensity alone.
- `revenue_momentum_foreign`: revenue momentum's filters and score, plus a
  foreign gate (`min_intensity`) and/or foreign rank in the score
  (`foreign_weight`).

Parameters live in radar.toml under [strategy.foreign_flow]; each variant is
a sub-table. Run every variant from backtest/:

    python3 -m strategies.foreign_flow [--no-save]
"""

from __future__ import annotations

import pandas as pd

from radar_backtest.data import PointInTime
from strategies.revenue_momentum import COMMON, DEFAULTS as RM_DEFAULTS, RevenueMomentum

FLOW_DEFAULTS = {
    "variant": "flow",          # "flow" or "revenue"
    "flow_days": 60,            # window for intensity and buy-day share
    "min_buy_share": 0.0,       # share of window days with foreign net buying
    "min_intensity": None,      # foreign net buy / traded value over the window
    "foreign_weight": 0.0,      # revenue variant: weight of foreign rank in the score
    "ma_days": 0,               # flow variant: close must be above this moving average
}


def intensity(view: PointInTime, tickers: list[str], days: int) -> pd.DataFrame:
    """Per ticker over the last `days` trading days: foreign net NTD bought /
    NTD traded, and the share of those days with foreign net buying."""
    since = view.close.index[-days]
    f = view.flows()
    f = f[(f["date"] >= since) & f["ticker"].isin(tickers)]
    net = f.pivot(index="date", columns="ticker", values="foreign_net")
    close = view.close.loc[since:, :].reindex(columns=net.columns)
    net = net.reindex(index=close.index)
    traded = view.turnover.loc[since:, net.columns].sum()
    value = (net * close).sum()
    out = pd.DataFrame({"intensity": value / traded.where(traded > 0),
                        "buy_share": (net > 0).sum() / days})
    return out.reindex(tickers)


class _Base(RevenueMomentum):
    """RevenueMomentum's schedule, book and stops with our own params."""

    def __init__(self, params: dict, name: str):
        unknown = set(params) - set(RM_DEFAULTS) - set(FLOW_DEFAULTS)
        if unknown:
            raise ValueError(f"unknown [strategy.foreign_flow] keys: {sorted(unknown)}")
        rm = {k: v for k, v in params.items() if k in RM_DEFAULTS}
        super().__init__(rm)
        self.params = {**RM_DEFAULTS, **FLOW_DEFAULTS, **params}
        self.name = name

    def _foreign_ok(self, flows: pd.DataFrame) -> pd.Series:
        """No flow data (before 2013 for many stocks) fails only an active gate."""
        p = self.params
        ok = pd.Series(True, index=flows.index)
        if p["min_buy_share"] > 0:
            ok &= flows["buy_share"] >= p["min_buy_share"]
        if p["min_intensity"] is not None:
            ok &= flows["intensity"] > p["min_intensity"]
        return ok


class ForeignFlow(_Base):
    def ranked(self, view: PointInTime, adj: pd.DataFrame | None = None) -> pd.DataFrame:
        p = self.params
        adj = view.adj_close if adj is None else adj
        if len(adj) < max(p["flow_days"], p["adv_days"], p["ma_days"]):
            return _empty()
        last = adj.iloc[-1]
        listed = set(view.companies()["ticker"])
        cands = [t for t in last.index[last.notna()] if t in listed and COMMON.match(t)]
        adv = view.turnover.iloc[-p["adv_days"]:][cands].fillna(0.0).mean()
        cands = list(adv.index[adv >= p["min_adv_ntd"]])
        if p["ma_days"] > 0 and cands:
            ma = adj[cands].iloc[-p["ma_days"]:].mean()
            above = last[cands] > ma
            cands = list(above.index[above])
        if not cands:
            return _empty()
        fl = intensity(view, cands, p["flow_days"])
        fl = fl[self._foreign_ok(fl) & fl["intensity"].notna()]
        out = fl.assign(adv_ntd=adv[fl.index], score=fl["intensity"])
        out = out.rename_axis("ticker").reset_index()
        out = out.sort_values(["score", "ticker"], ascending=[False, True]).reset_index(drop=True)
        out.index += 1
        return out


class RevenueMomentumForeign(_Base):
    def ranked(self, view: PointInTime, adj: pd.DataFrame | None = None) -> pd.DataFrame:
        p = self.params
        out = super().ranked(view, adj)
        if out.empty:
            return out
        fl = intensity(view, list(out["ticker"]), p["flow_days"])
        fl.index.name = "ticker"
        out = out.merge(fl, left_on="ticker", right_index=True)
        out = out[self._foreign_ok(out.set_index("ticker")).to_numpy()]
        if out.empty:
            return out
        w = p["foreign_weight"]
        if w:
            out["score"] = (1 - w) * out["score"].rank(pct=True) + w * out["intensity"].rank(pct=True).fillna(0.5)
        out = out.sort_values(["score", "ticker"], ascending=[False, True]).reset_index(drop=True)
        out.index += 1
        return out


def _empty() -> pd.DataFrame:
    return pd.DataFrame(columns=["ticker", "intensity", "buy_share", "adv_ntd", "score"])


def variants(settings) -> dict[str, dict]:
    """Every [strategy.foreign_flow.<name>] table, each over the shared keys."""
    raw = settings.strategies.get("foreign_flow", {})
    base = {k: v for k, v in raw.items() if not isinstance(v, dict)}
    return {name: {**base, **v} for name, v in raw.items() if isinstance(v, dict)}


def build(name: str, params: dict) -> RevenueMomentum:
    variant = params.get("variant", FLOW_DEFAULTS["variant"])
    if variant == "flow":
        return ForeignFlow(params, f"foreign_flow_{name}")
    if variant == "revenue":
        return RevenueMomentumForeign(params, f"revenue_momentum_foreign_{name}")
    raise ValueError(f"unknown foreign_flow variant {variant!r}")


def make(params: dict, settings) -> RevenueMomentum:
    """The base table as one strategy (`python -m radar_backtest foreign_flow`)."""
    flat = {k: v for k, v in params.items() if not isinstance(v, dict)}
    return build("base", flat)


def main() -> None:
    import argparse

    from radar_backtest import config, data, runner

    ap = argparse.ArgumentParser(description="Run every [strategy.foreign_flow] variant")
    ap.add_argument("--config", default=str(runner.BACKTEST_DIR.parent / "radar.toml"))
    ap.add_argument("--no-save", action="store_true", help="don't write to backtest_runs")
    ap.add_argument("--only", nargs="*", help="variant names to run")
    args = ap.parse_args()

    settings = config.load(args.config)
    md = data.load(settings.db_path, min_coverage=settings.min_coverage)
    for name, params in variants(settings).items():
        if args.only and name not in args.only:
            continue
        out = runner.run(md, settings, lambda: build(name, params), save=not args.no_save)
        for p in out["periods"]:
            s, b = p["summary"], p["bench_summary"]
            print(f"{out['strategy']:>40} {p['period']:>6} {s['start']}..{s['end']}  "
                  f"CAGR {s['cagr']:.2%}  maxDD {s['max_drawdown']:.2%}  Sharpe {s['sharpe'] or 0:.2f}"
                  f"  | {p['benchmark']} CAGR {b['cagr']:.2%}  maxDD {b['max_drawdown']:.2%}", flush=True)
        for path in out["reports"]:
            print("report:", path)


if __name__ == "__main__":
    main()
