"""Regression test for the revenue momentum baseline (issue #50).

baselines/revenue_momentum.json records what the strategy returned on one
pinned data snapshot. This reruns it and fails if the result drifts, so a
change to the engine, costs or strategy that moves the headline numbers is
noticed and recorded as a new baseline rather than slipping in.

It needs the snapshot file (400 MB, not in CI):
    gh release download data --pattern radar-2026-10-15.db.gz && gunzip radar-2026-10-15.db.gz
    RADAR_BASELINE_DB=$PWD/radar-2026-10-15.db python -m pytest tests/test_baseline.py
"""

from __future__ import annotations

import dataclasses
import json
import os
from pathlib import Path

import pytest

from radar_backtest import config, data, runner

BASELINE = runner.BACKTEST_DIR / "baselines" / "revenue_momentum.json"
DB = os.environ.get("RADAR_BASELINE_DB")

pytestmark = pytest.mark.skipif(not DB, reason="set RADAR_BASELINE_DB to the baseline snapshot to run")


def test_revenue_momentum_matches_baseline():
    want = json.loads(BASELINE.read_text())
    settings = dataclasses.replace(config.load(runner.BACKTEST_DIR.parent / "radar.toml"), db_path=Path(DB))
    snap, _ = data.snapshot(settings.db_path)
    assert snap == want["snapshot"]["id"], f"{DB} is snapshot {snap}, the baseline is {want['snapshot']['id']}"

    md = data.load(settings.db_path)
    out = runner.run(md, settings, runner.strategy_factory(want["strategy"], settings), report_dir=None, save=False)
    assert out["params"] == want["params"], "strategy parameters changed: record a new baseline"
    tol = want["tolerance"]
    for p in out["periods"]:
        exp = want["periods"][p["period"]]["summary"]
        got = p["summary"]
        assert (str(got["start"])[:10], str(got["end"])[:10]) == (str(exp["start"])[:10], str(exp["end"])[:10])
        for k in ("cagr", "max_drawdown"):
            assert got[k] == pytest.approx(exp[k], abs=tol[k]), f"{p['period']} {k} drifted"
