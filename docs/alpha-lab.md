# Alpha lab: first results (issue #76)

*2026-10-25 run on snapshot 2026-10-24T20:46:21Z (prices to 2026-10-22), code 88e1e7b.
The full leaderboard is on the dashboard's Alpha page and in each lab run's
GitHub Actions summary; it refreshes weekly.*

The owner asked for a tool to find short-term edge (days to a few weeks). The lab
tests 30 signals × 4 holding periods (1, 5, 10, 20 trading days): 120 tries. Each
day it buys the top 10% of liquid common stocks by the signal (or every stock with
the event) at the next open, sells at the open after the holding period, and
compares that with the average liquid stock. "Net" takes off one round trip of
fees and tax (0.47%). With 120 tries, a "passes" verdict needs t ≥ 3.53 in
2012–2019 and t ≥ 2 with a positive net from 2020, which nothing was tuned on.
How it works: `backtest/radar_backtest/alpha.py`; signals: `signals.py`.

## What survived

| Signal | Hold | 2012–19 net (t) | 2020+ net (t) | A day late | Last 12 months |
|---|---|---|---|---|---|
| Closest to the 52-week high | 20d | +0.61% (3.8) | **+0.71%** (3.1) | +0.69% | +1.45% |
| Closest to the 52-week high | 10d | +0.24% (5.2) | +0.23% (3.6) | +0.22% | +0.43% |
| 投信 starts buying after 20 quiet days | 1d | +0.17% (10.1) | +0.04% (5.5) | −0.25% | −0.21% |

- **Near the 52-week high** is the one robust short-term edge: it worked in both
  periods, doesn't need speed (acting a day late costs almost nothing) and has been
  strongest in the last year. Revenue momentum already uses it as a filter; the
  lab says it carries its weight alone over 2–4 weeks.
- **投信 first buy** passes the statistics but is too thin to trade: 0.04% a trade
  after costs since 2020, negative a day late and over the last year.

## Works since 2020 only (could be a new regime, or luck)

| Signal | Hold | 2012–19 net (t) | 2020+ net (t) | A day late | Last 12 months |
|---|---|---|---|---|---|
| Record monthly revenue (3 years), first 5 days | 20d | +0.40% (1.9) | **+1.18%** (2.7) | +1.11% | +1.93% |
| Record monthly revenue, first 5 days | 10d | −0.05% (1.9) | +0.59% (3.2) | +0.71% | +1.28% |
| Revenue YoY, first 5 days after the deadline | 20d | +0.05% (1.3) | +0.91% (2.3) | +0.76% | +0.52% |
| Last month's winners | 20d | −0.44% (0.1) | +0.62% (2.2) | +0.60% | +2.09% |
| Latest revenue YoY | 20d | −0.20% (1.4) | +0.31% (2.7) | +0.27% | +0.46% |

Revenue news has paid since 2020 but not before. This matches the record-revenue
paper portfolio's backtest (5.5% a year in 2012–2019, 29.6% since 2020).

## Faded: strong in 2012–2019, gone since 2020

Every 投信 buying signal: today's 投信 buying held 5 days made +0.48% a trade
(t 9.9) before costs in 2012–2019, but only +0.17% since 2020, which costs eat. Same
for 外資 and 投信 buying together and for 投信 buying streaks. Funds' buying is
now priced in within a day.

## Costs eat it

One-day signals are real but tiny next to the 0.47% round trip: 投信 buying today
(+0.13% a day since 2020, t 9.0), last week's losers bouncing (+0.06%), 外資 buying
(+0.03%). They would only pay with much lower costs (day-trade tax, a bigger
broker discount).

## Doesn't work

Volume surges, gap-ups, 20-day breakouts (slightly negative), limit-up closes,
buying calm stocks, 6-month momentum, 外資 flows at any horizon, and trading
around ex-dividend dates (before or after) show no net edge in either period.

## Caveats

- Flows start in 2013, so flow signals have a 7-year design period.
- Revenue is placed at its legal deadline (the 10th), not the real filing day, so
  early filers' moves are missed.
- Event signals with few big days (limit-down closes) show a large average but a
  t near 0: a few crash-rebound days carry them.
- Top-10% averages are equal-weighted across 130 or so stocks a day; a real
  portfolio of 8–10 names will be noisier.

## Next

- A paper portfolio on "near the 52-week high", held 20 days, if the owner wants it.
- Margin and short data (融資融券) is the biggest gap: it is often the strongest
  short-term signal in Taiwan and we don't collect it yet.
