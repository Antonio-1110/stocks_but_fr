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

## Pullback: cutting the fees (issue #83, 2026-11-15)

Snapshot 2026-11-14T17:11:50Z. Same engine; 0050 made 11.9% (2012–2019) and 26.9% (2020 on).

| Variant | 2012–19 CAGR | Max drop | 2020+ CAGR | Max drop | Trades a year | Fees + tax a year |
|---|---|---|---|---|---|---|
| Before (RSI(2) < 10) | 11.4% | −53.6% | −6.9% | −58.8% | 507 / 530 | 24.2% / 24.9% |
| **RSI(2) < 5 (now live)** | **16.7%** | −24.0% | **−5.3%** | −56.9% | 352 / 401 | 16.9% / 18.7% |
| Within 10% of the 52-week high | 12.6% | −20.2% | −4.9% | −44.7% | 336 / 253 | 15.9% / 11.9% |
| Only while 0050 is above its 200-day average | 5.6% | −53.5% | −4.1% | −38.7% | 410 / 434 | 19.7% / 20.3% |
| RSI < 5 + near high + 10-day exit + trend filter | 5.4% | −16.7% | 3.1% | −16.5% | 116 / 68 | 5.5% / 3.2% |

Only RSI(2) < 5 is better than the old rule in both periods, so it is the one change
made (`entry_rsi = 5`). The new `max_below_high` and `regime_days` settings stay off.
No version comes close to 0050 since 2020: in this market the bounce after a sharp
dip in a leader is smaller than a round trip of fees and tax.

## 52-week high + revenue growth (issue #92, 2026-11-15)

Built on the alpha lab's best signal (#82). Every 5 trading days, rank liquid stocks
by the average of two percentile ranks, closeness to the 52-week closing high and
latest monthly revenue YoY; hold the top 10, keep a holding while it ranks in the
top 30, and sell between rankings 10% below the high since bought. Rules in
`backtest/strategies/near_high.py`; paper portfolio from the 2026-11-16 run.
Snapshot 2026-11-14T17:11:50Z; 0050 made 11.9% (2012–2019) and 26.9% (2020 on).

| Version | 2012–19 CAGR | Max drop | 2020+ CAGR | Max drop | Trades a year | Avg trade | Fees + tax a year |
|---|---|---|---|---|---|---|---|
| **Closeness + revenue YoY (live)** | **17.1%** | −34.6% | **41.7%** | −35.4% | 143 / 185 | +1.3% / +2.3% | 6.7% / 8.5% |
| Closeness only | 10.5% | −25.7% | 29.9% | −55.9% | 284 / 324 | +0.4% / +1.0% | 13.4% / 15.1% |
| Closeness + 6-month momentum | 13.1% | −53.9% | 26.0% | −57.3% | 205 / 246 | +0.8% / +1.4% | 9.6% / 11.3% |
| Live, no buys below 0050's 200-day average | 12.5% | −27.1% | 38.1% | −29.7% | 122 / 161 | +1.1% / +2.4% | 5.7% / 7.8% |
| Live, ranking every 10 days | 15.5% | −36.7% | 40.4% | −37.6% | 110 / 137 | +1.5% / +2.8% | 5.1% / 6.2% |
| Live, 15% trailing stop | 19.8% | −29.7% | 44.5% | −37.6% | 134 / 171 | +1.5% / +2.6% | 6.3% / 7.6% |

The untuned version is the first strategy here to beat 0050 in **both** periods,
by about 5 points a year before 2020 and 15 since, with a similar worst drop. Adding
revenue growth to the 52-week high halves the trading and roughly doubles the
average trade. The variants were run after seeing results, so they are a
sensitivity check, not a reason to change settings: a wider stop helped in both
periods, and is the first thing to revisit after some live months.
