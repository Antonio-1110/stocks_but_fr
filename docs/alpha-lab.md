# Alpha lab (issues #76, #82)

## Round 2: combinations, market regime, consistency (#82)

*2026-11-15 run on snapshot 2026-11-14T17:11:50Z (prices to 2026-11-14), 42
signals x 4 holds = 168 tries, so "passes" now needs t ≥ 3.62 in 2012–2019.*

Two new columns say how to use a signal, not whether it passes: the 2020-on
net edge on days 0050 closed **above** vs **below** its 200-day average, and
the share of calendar years since 2012 with a positive net edge ("years +").

| Signal | Hold | 2012–19 net (t) | 2020+ net (t) | Up market | Down market | Years + |
|---|---|---|---|---|---|---|
| **Near the 52-week high + revenue YoY** (new) | 20d | +0.60% (4.3) | **+1.20%** (5.0) | +1.49% | +0.27% | **100%** |
| Near the 52-week high + revenue YoY | 10d | +0.21% (5.7) | +0.55% (6.2) | +0.69% | −0.13% | 73% |
| Near the 52-week high + 6-month momentum (new) | 20d | +0.63% (3.9) | +1.05% (3.8) | +2.45% | −0.11% | 87% |
| 投信 buying while near the 52-week high (new) | 20d | +1.02% (3.7) | +0.76% (2.0) | +2.81% | −0.11% | 87% |
| Closest to the 52-week high | 20d | +0.61% (3.8) | +0.73% (3.1) | +1.46% | −0.56% | 93% |

- **Adding revenue growth to the 52-week high is the best signal found so far.**
  It beats closeness alone in both periods, was positive in every year since
  2012, and is the only one still positive when the market is below trend.
- **Every 52-week-high signal is an up-market signal.** Below 0050's 200-day
  average the edge is gone or negative: a strategy built on it should stop
  buying (or buy less) in a downtrend.
- **Record revenue while near the high** (new event) made +1.95% a trade over 20
  days since 2020 and was positive every year, but is "recent only": t 1.7 in
  2012–2019 on few events.
- 60-day return / volatility (20d) is "promising" (+0.92% net since 2020, t 3.1,
  and positive in down markets too), short of the hurdle in 2012–2019.
- No edge: first new high after a long base (too few events), drying-up volume
  near the high (faded), overnight-gap momentum, industry momentum, near the
  high + low volatility.

The full leaderboard is in the daily run's Actions summary on lab weeks and on
the Alpha page. The paper portfolio built on the top signal is #92.

## Round 1 (#76)

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
