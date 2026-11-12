# Stock ratings (issue #78)

The owner asked (2026-11-12) for short- and long-term ratings on single Taiwan
and US stocks, and for a record of how accurate those predictions are.

## What gets rated

Every daily run, once a market's newest trading day is fully collected
(`min_coverage` of the day before's rows), `radar collect` rates:

- **Taiwan:** the 300 common stocks with the highest 20-day average traded
  value and a close of at least NT$10, plus any ticker in `tw_watch`.
- **US:** every ticker in `[us]`.

A stock needs a year of prices to be rated. The benchmark (0050, SPY) isn't rated.

## How a rating is made

Each part is turned into a percentile rank among the stocks rated that day,
the ranks are averaged with the weights in `[ratings]`, and the average is cut
into fifths: 5 = strongest fifth ("Strong up"), 1 = weakest ("Strong down").

| Term | Scored over | Parts (default weight) |
|---|---|---|
| Short | 5 and 20 trading days | closeness to the 52-week high (½), 1-month return (¼), *smaller* 1-week return (¼, short-term reversal) |
| Long | 60 and 120 trading days | 12-month return skipping the latest month, distance above the 200-day average, closeness to the 52-week high, latest revenue YoY known by its deadline (Taiwan only), ¼ each |

Why these: the alpha lab (#76, [alpha-lab.md](alpha-lab.md)) found closeness to
the 52-week high the one short-term signal that held in both test periods;
1-month winners and 1-week losers have worked since 2020; the long-term parts
are the standard trend and momentum measures plus the revenue growth the
Taiwan strategies are built on. None of the weights is tuned: the accuracy
record is how we'll learn whether to change them.

Because ratings rank stocks against each other, an "Up" means "likely to do
better than most of the list". In a falling market up calls can lose money and
still be right relative to the market, so the page reports both.

## How accuracy is recorded

- Each rating is written once to `state.ratings` (in `state.db`, which never
  rolls back with market data) with the day it was made and its inputs.
- When a horizon has passed, `state.rating_outcomes` gets the stock's return
  and the benchmark's, from the close of the **next** trading day (time to act)
  to the close that many trading days later, dividends included. A stock that
  stops trading keeps its last close.
- The page shows, per market and horizon:
  - **Direction right:** up calls (4–5) that rose plus down calls (1–2) that fell.
  - **Up calls rose / down calls fell,** next to the share of all rated stocks
    that rose or fell (the base rate a coin-flip would get).
  - **Beat / lagged the benchmark:** up calls that beat it plus down calls that lagged it.
  - **Average return vs the benchmark for each rating 1–5**, and **5s minus 1s**:
    if the ratings work, this rises from 1 to 5 and the spread is positive.
  - Each stock's own record: how many of its past up and down calls got the
    20- and 120-day direction right.
- **Live** and **backtest** results are kept apart. The first run rates every
  5th trading day over the previous ~500 trading days with the same rules, so
  there's an accuracy record from day one. Overlapping horizons mean those
  ratings aren't independent: treat small differences as noise. The US
  backtest uses today's large caps, which flatters it.

## Settings

`[ratings]` in `radar.toml`. Changing a weight changes future ratings only;
past ratings and their scores stay as they were made. To re-run the backtest,
delete the market's row in `state.rating_runs` and its `backfill = 1` rows.
