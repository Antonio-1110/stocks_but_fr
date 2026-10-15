# Backtester

Python engine that every strategy plugs into. It reads only `data/radar.db`
(written by `radar collect`) and `radar.toml`.

```sh
cd backtest
python3 -m pip install -e '.[dev]'
python3 -m pytest                       # fixture data only, no network
python3 -m radar_backtest buy_and_hold  # 0050 buy and hold, the sanity check
python3 -m radar_backtest revenue_momentum   # a file in strategies/
```

Each run covers the design period and the test period from `radar.toml`
(each starting from the initial capital in cash), runs the benchmark through
the same engine, writes a Markdown and an HTML report to `reports/`, and adds
one row per period to the `backtest_runs` table (`--no-save` skips that).

## Writing a strategy

`strategies/<name>.py` defines `make(params, settings)` returning an object with
`name`, `params` and `targets(view)`. `params` is radar.toml's
`[strategy.<name>]` table. The engine calls `targets` after every close with a
point-in-time view (`radar_backtest.data.PointInTime`): prices up to that day,
revenue only after its `announced_on` date, flows up to that day, and the
companies listed that day. Return `{ticker: weight}` (fractions of equity,
summing to at most 1) to rebalance at the next open, or `None` to do nothing.

## What the engine assumes

- Fills at the next trading day's open, on dividend/split-adjusted prices, with
  fractional units (Taiwan allows odd lots).
- Commission x broker discount with a minimum fee per order, sell tax (ETF rate
  for `00xx` codes), all from `[costs.tw]`.
- A buy can't make a position larger than `max_adv_fraction` of its
  `adv_days`-day average traded value; the rest stays cash.
- Rebalancing trades under `min_trade_value` are skipped; full exits always happen.
- A held stock whose prices stop is sold at its last traded close. This also
  happens if the collector simply hasn't updated that stock yet, so refresh
  data before trusting the last weeks of a run.
- A stock with no price on the fill day is skipped and counted in the report.
- An open outside the day's low-high range (FinMind has many on TWSE stocks)
  is treated as missing, so that day fills at the close.
- Price limits (`[limits.tw]`, issue #53): ±10% since 2015-06-01, ±7% before.
  A buy whose fill-day open is at limit-up (against the previous close, both
  adjusted, within `tolerance` for tick rounding) is skipped. A sell whose open
  is at limit-down waits for the next open, every day until it fills, unless a
  new signal replaces it first. `RunResult.limit_blocked` counts both (blocked
  buys plus days a sell waited; `limit_blocked_buys` and `limit_deferred_sells`
  are the parts). The paper portfolios apply the same rule.

## Baseline (issue #50)

`baselines/revenue_momentum.md` is the reference result for revenue momentum,
on a pinned data snapshot. `tests/test_baseline.py` reruns it and fails on drift
(skipped unless `RADAR_BASELINE_DB` points to the snapshot). A change that moves
the numbers on purpose records a new baseline instead.
The daily run never deletes a dated snapshot named by a baseline's
`snapshot.asset`, so the pinned data stays downloadable.

## Checked against real data (issue #23)

On FinMind data collected 2026-10-09, `buy_and_hold` on 0050 matched 0050's
dividend-adjusted return after costs:

| Period | 0050 adjusted close | Backtest | Gap explained by |
|---|---|---|---|
| 2012-01-02 to 2019-12-31 | +148.94% | +146.52% | first-night gap (49.06 close to 49.50 open, +0.90%) and one buy commission (NTD 4,271) |
| 2020-01-02 to 2026-10-08 | +478.88% | +474.56% | first-night gap (97.65 to 98.30, +0.67%) and one buy commission (NTD 4,271) |

The adjusted closes were rebuilt independently from raw closes, FinMind's
dividend table (it matches Yuanta's published 0050 distributions) and the
2025-06-18 1:4 split, and agree to the last digit except on the split day,
where the exchange's rounded reference price moves the 2020+ figure by 0.03
points. No engine change was needed.
