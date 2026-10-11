# Product

**stocks_but_fr** is a personal research terminal for one hobbyist investor. It
surfaces stocks worth a closer look, tests investing ideas honestly against
history, and follows the ones that hold up with paper money before any real
money is involved. It is built to be cheap to run and maintainable by one
person with AI agents.

Flow: watch the market → find candidates → look into the company → write the
idea as rules → backtest → paper trade → (much later, with approval) supervised
live orders.

## Goals

1. **Trustworthy data.** Taiwan prices, corporate actions, monthly revenue and
   institutional flows that are complete, point-in-time and recoverable.
2. **Honest backtests.** Realistic timing and costs, a comparable benchmark,
   and results that can be rerun on the same data. A rejected idea is a
   valid result.
3. **Explainable screening.** Every flagged stock says which rule it passed
   and with what numbers.
4. **Forward paper trading** of strategies that pass the backtest, recorded so
   past decisions can't change.

Horizon: multi-day to multi-month swing and position investing on daily bars.
Default capital assumption: NT$5M (configurable, not a trading authorisation).

## Non-goals

- Intraday, high-frequency or tick data; options and derivatives.
- Being a market-data vendor: store what strategies use, link out for the rest
  (Goodinfo, Yahoo, MOPS).
- Building company research pages that duplicate those sites.
- Social-media sentiment, news archives, large alternative datasets.
- AI-generated numbers: prices, accounting, sizing and risk checks are
  deterministic code. AI may summarise and suggest hypotheses only.
- Autonomous live trading. Any live order needs the owner's explicit approval
  and a separate milestone.
- Paid infrastructure or data without a written case and the owner's approval.

## Decisions

| Decision | Why | Revisit when |
|---|---|---|
| Taiwan first; US later, Singapore not planned | Monthly revenue is a Taiwan-specific edge; one market keeps it maintainable | Taiwan research loop works end to end (ROADMAP) |
| Go collects and renders; Python backtests; SQLite file is the only contract | Works today; each side is small | – |
| SQLite on a GitHub release, static site on GitHub Pages, GitHub Actions cron | Free, no servers | Backtest load > 5 min, or a second market |
| Daily bars; signal at a close, fill at the next open | Matches the horizon; no look-ahead | – |
| Benchmark: 0050 total return through the same engine and costs | Like-for-like comparison | A second market |
| All costs and strategy settings in `radar.toml` | One place to check assumptions | – |
| Monthly revenue is used only after the 10th-of-month filing deadline | Never uses a figure before it was public | Real filing dates are collected |
| Paper portfolios and alerts wait until a month's revenue covers 90% of listed stocks | A partial backfill isn't the market | – |
| Agents work on `main`, one GitHub issue per change (AGENTS.md) | One owner, fast iteration | The owner says otherwise |

ROADMAP.md says what is planned, deferred or cut, and is updated after each
sprint.
