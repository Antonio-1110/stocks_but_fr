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
