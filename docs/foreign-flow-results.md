# Following foreign investors (外資), one day late (issue #66)

The question: can we follow 外資 net buying when their numbers are published
only after the close, so the earliest we can trade is the next day's open?

Data: snapshot `2026-10-14T19:10:02Z` (`radar-2026-10-15.db.gz`, the one the
baselines pin). Foreign flows cover few stocks before mid-2012 and are complete
from 2013, so the design period is shown both as 2012–2019 (comparable with the
baselines) and 2013–2019 (complete flow data). Code:
`backtest/strategies/foreign_flow.py`, settings in `radar.toml`
`[strategy.foreign_flow]`. Every backtest pays costs, can't buy at a limit-up
open and fills at the open after the signal.

"Foreign intensity" = NTD foreigners net bought over a window / NTD the stock
traded over the same window.

## 1. The one-day lag kills a one-day signal

Liquid common stocks (≥10M NTD a day), each day's top 10% by that day's foreign
intensity, return versus the average stock:

| | The day foreigners bought (can't get it) | Next day, buying at the open |
|---|---|---|
| 2013–2019 | +0.71% | +0.16% |
| 2020–2026 | +0.84% | +0.08% |

80–90% of the move happens on the day foreigners buy, before the data is out.
What is left is far below a round trip's cost (about 0.47%: commission at the
configured discount both ways plus 0.3% sell tax). Long buying streaks are
late too: after 10+ net-buy days in a row the next 20 days are slightly worse
than average.

## 2. Slow foreign buying keeps a small edge

Excess return over the next 60 trading days (bought at the next open), by
quintile of 60-day foreign intensity, lowest to highest:

| | Q1 (sold most) | Q2 | Q3 | Q4 | Q5 (bought most) |
|---|---|---|---|---|---|
| 2013–2019 | −0.40% | −0.64% | −0.17% | +0.70% | +0.85% |
| 2020–2026 | −1.74% | −0.78% | −0.21% | +1.33% | +1.06% |

Real but small: about 1% per quarter for the top fifth, and lately the heavy
selling side is the stronger signal.

## 3. Backtests

Top 12, equal weight, rebalanced monthly on revenue momentum's schedule (the
trading day after the 10th), 15% stop-loss. Flow-only variants use stocks
trading ≥50M NTD a day.

| Strategy | 2012–19 CAGR | 2013–19 CAGR / worst drop | 2020–26 CAGR / worst drop | Sharpe 13–19 / 20–26 |
|---|---|---|---|---|
| 0050 | 12.0% | 11.5% / −21.0% | 29.0% / −34.0% | |
| Revenue momentum (baseline) | 27.5% | 30.7% / −24.1% | 43.3% / −32.9% | 1.54 / 1.43 |
| Foreign only, 20 days | 5.0% | 5.8% / −29.9% | 5.4% / −31.5% | 0.50 / 0.41 |
| Foreign only, 60 days | 11.4% | 9.5% / −23.1% | 9.6% / −27.1% | 0.77 / 0.66 |
| …bought on ≥60% of days | 10.6% | 9.6% / −21.5% | 14.7% / −27.5% | 0.77 / 0.90 |
| …above 60-day average | 10.6% | 9.1% / −24.1% | 17.4% / −25.3% | 0.71 / 1.05 |
| Rev. momentum, foreign net buyers (20d) only | 23.3% | 27.3% / −25.9% | 51.5% / −32.0% | 1.42 / 1.73 |
| Rev. momentum, foreign net buyers (60d) only | 23.3% | 27.9% / −24.0% | 39.9% / −34.7% | 1.45 / 1.39 |
| Rev. momentum, skip heavy foreign selling | 24.5% | 29.4% / −26.9% | 43.2% / −34.9% | 1.48 / 1.42 |
| Rev. momentum, foreign rank as ⅓ of score | 27.3% | 29.9% / −23.8% | 42.2% / −30.6% | 1.68 / 1.60 |

(The 2012–19 column is lower for the gated versions because stocks with no
2012 flow data fail the gate.)

## What it means

- Following foreigners alone loses to 0050 in both periods, by 2 points a year
  in the design period and 12+ in the test period.
- Requiring foreign buying on top of revenue momentum (the 20-day gate) looks
  great since 2020 (51.5%) but loses 3.4 points a year in the design period.
  That's the pattern the 投信 filter showed; picking it now would be choosing
  on the test period.
- The only version that holds up in both periods is the foreign rank as a third
  of revenue momentum's score: about the same return (−0.8 and −1.1 points),
  higher Sharpe (1.54→1.68, 1.43→1.60) and a smaller worst drop in the test
  period (−32.9%→−30.6%). A smoother ride, not more money.

Run them all: `cd backtest && python3 -m strategies.foreign_flow --no-save`.
