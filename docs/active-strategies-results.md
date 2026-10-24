# Active strategies: first backtest (issues #70–#73)

*2026-10-24, database snapshot of 2026-10-24 (prices to 2026-10-22). The owner asked
for aggressive strategies that trade actively, to follow live as paper portfolios; the
backtest is a first look, not a gate. Rules and sources are in each file's docstring
under `backtest/strategies/`; settings are in `radar.toml`. Nothing was tuned: these
are the published or textbook settings, run once.*

Same engine rules as every other backtest: 5M NTD, fills at the next open, fees and
sell tax from `[costs.tw]`, no buys at a limit-up open, sells wait out limit-down
opens and halts, a 5% of average traded value cap per position.

| | Design 2012–2019 CAGR | Max drop | Test 2020–2026 CAGR | Max drop | Trades a year | Win rate | Avg trade after costs | Days held | Fees + tax a year |
|---|---|---|---|---|---|---|---|---|---|
| Record-revenue trades | 5.5% | −25.7% | **29.6%** | −29.4% | 62 / 77 | 43% | +0.9% / +3.3% | 19 / 16 | 3.7% / 4.4% |
| Breakout swing | 0.6% | −46.0% | 6.5% | −43.8% | 130 / 124 | 35% | +0.2% / +0.9% | 12 / 11 | 7.4% / 6.9% |
| 投信 window dressing | −3.6% | −38.1% | 0.2% | −26.4% | 40 / 40 | 41% / 45% | −0.8% / +0.2% | 16 | 1.9% |
| Pullback (RSI 2) | 11.4% | −53.6% | −5.7% | −58.8% | 507 / 530 | 63% / 59% | +0.3% / 0.0% | 4 | 24.2% / 24.9% |
| 0050 | 12.0% | −21.0% | 27.9% | −34.0% | | | | | |

(Pairs are design / test.)

## What it says

- **Record-revenue trades** is the only one that keeps up with 0050: slightly ahead
  since 2020 with a smaller worst drop, but well behind it in 2012–2019. It is in cash
  about 40% of the time (8 slots, many trades stopped out early), so the per-trade
  edge is real but the portfolio isn't fully used.
- **Breakouts** lose to 0050 in both periods. Two in three trades are small losses at
  the trailing stop; the winners don't pay for them plus 7% a year in costs.
- **投信 window dressing** loses in both periods. Whatever quarter-end push there is
  doesn't survive picking from public flow data and a one-month hold.
- **Pullbacks** win 60% of trades, but a 0.6% round trip on 500+ trades a year costs
  about a quarter of the account every year. Before costs it would look good; after
  costs it is the worst of the four since 2020.

None of this is a reason to stop tracking: the owner wants to see them run live. But
only record-revenue trades has backtest support, and pullbacks will most likely lose
money in paper too, mostly to fees and tax.

Re-run one from `backtest/`: `python3 -m strategies.record_revenue` (or `breakout`,
`window_dressing`, `pullback`).
