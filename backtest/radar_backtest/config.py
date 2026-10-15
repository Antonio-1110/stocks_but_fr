"""Loads radar.toml. Every cost and parameter comes from here, never from code."""

from __future__ import annotations

import tomllib
from dataclasses import dataclass, field
from pathlib import Path


@dataclass(frozen=True)
class Costs:
    commission_rate: float
    broker_discount: float
    min_commission: float
    sell_tax: float
    etf_sell_tax: float


@dataclass(frozen=True)
class Limits:
    """[limits.tw]: Taiwan's daily price limit (issue #53)."""

    pct: float          # limit from `since` on...
    old_pct: float      # ...and before it
    since: str
    tolerance: float    # a move this close to the limit counts as locked

    def at(self, day) -> float:
        return self.pct if str(day)[:10] >= self.since else self.old_pct


@dataclass(frozen=True)
class Settings:
    db_path: Path
    costs: Costs
    initial_capital: float
    benchmark: str
    design_start: str
    design_end: str
    test_start: str
    max_adv_fraction: float
    adv_days: int
    min_trade_value: float
    min_coverage: float = 0.9      # [paper] min_coverage: newest day counts once this share is collected
    limits: Limits | None = None   # None: fill at any open (no price-limit check)
    strategies: dict = field(default_factory=dict)  # raw [strategy.<name>] tables
    root: Path = Path(".")


def load(path: str | Path) -> Settings:
    """Read radar.toml. Relative paths inside it are relative to its folder."""
    path = Path(path)
    with path.open("rb") as f:
        raw = tomllib.load(f)
    root = path.resolve().parent
    tw = raw["costs"]["tw"]
    bt = raw["backtest"]
    return Settings(
        db_path=root / raw["run"]["db_path"],
        costs=Costs(
            commission_rate=tw["commission_rate"],
            broker_discount=tw["broker_discount"],
            min_commission=tw["min_commission"],
            sell_tax=tw["sell_tax"],
            etf_sell_tax=tw["etf_sell_tax"],
        ),
        initial_capital=float(bt["initial_capital"]),
        benchmark=bt["benchmark"],
        design_start=bt["design_start"],
        design_end=bt["design_end"],
        test_start=bt["test_start"],
        max_adv_fraction=bt["max_adv_fraction"],
        adv_days=bt["adv_days"],
        min_trade_value=float(bt["min_trade_value"]),
        min_coverage=float(raw.get("paper", {}).get("min_coverage", 0.9)),
        limits=_limits(raw.get("limits", {}).get("tw")),
        strategies=raw.get("strategy", {}),
        root=root,
    )


def _limits(raw: dict | None) -> Limits | None:
    if not raw:
        return None
    return Limits(pct=float(raw["pct"]), old_pct=float(raw["old_pct"]),
                  since=str(raw["since"]), tolerance=float(raw["tolerance"]))
