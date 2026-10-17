# Averaging down (DCA / martingale): real-data results (#20)

Run 2026-10-17 on the 2026-10-14 database, 5M NTD, with costs, limit-up and
limit-down rules, and halts (a sell waits for the first open it can fill at).
Parameters: `[strategy.dca]` in radar.toml (150 most traded common stocks, start
after a 10% drop from the 60-day high, add every 5% lower, sell at +1% over
average cost, at most 5 stocks at once). Rerun with
`cd backtest && python3 -m strategies.dca`; full reports land in `backtest/reports/`.

The design period (2012–2019) has prices for only 86% of the stocks listed then,
so its numbers are not final. The test period (2020 on) is complete.

| | Martingale design | Martingale test | Capped design | Capped test | 0050 design | 0050 test |
|---|---|---|---|---|---|---|
| CAGR | 8.4% | 13.6% | 0.5% | 4.4% | 12.0% | 29.0% |
| Worst drop | −59.7% | −23.7% | −26.1% | −16.8% | | −34.0% |
| Cycles (win rate) | 817 (94.6%) | 1,048 (92.8%) | 1,112 (92.4%) | 1,002 (91.0%) | | |
| Average winning cycle | 5,534 NTD | 6,605 NTD | 3,718 NTD | 4,373 NTD | | |
| Worst cycle | −38,594 (6488, 2018) | −13,996 (3706, 2025) | −217,763 (2498, 2012) | −248,359 (3706, 2025) | | |
| Most capital in one stock | 6.8M (2498, 2015) | 7.9M (3035, 2022) | 0.75M | 0.75M | | |
| Longest cycle | 325 trading days | 127 | 181 | 272 | | |
| Adds short of cash | 792 | 79 | 0 | 0 | | |
| Trading days short of cash | 25.6% | 4.8% | 0% | 0% | | |

Crashes (return over the window, TAIEX's for comparison): 2015: martingale
−57.6%, capped −17.7%, TAIEX −20.8%. 2020: −14.3%, −8.6%, −28.2%. 2022: −4.4%,
−12.1%, −32.8%.

What this says:

- Neither version beats simply holding 0050, in either period.
- The martingale's high win rate hides the risk: its "worst cycle" looks small
  only because every losing cycle was held, doubling, until it bounced. That
  needed up to 7.9M NTD in one stock, more than the account, so it ran out of
  cash on a quarter of all trading days in 2012–2019 and lost 58% in the 2015
  drop.
- The cap and stop keep it solvent, but the stops (about 20 per period, each 20% or more
  under average cost, up to 33%) eat nearly all the +1% wins. After costs, a 1% bounce keeps under
  60% of its gain.
