# stocks_but_fr

A personal stock research terminal, Taiwan first: it collects daily prices,
monthly revenue and institutional flows, ranks stocks worth a closer look on a
static dashboard, backtests strategies honestly, and paper-trades the ones that
hold up. Goals and non-goals: [PRODUCT.md](PRODUCT.md). Plans:
[ROADMAP.md](ROADMAP.md).

The to-do list is the repo's GitHub Issues.

Contributors (human or agent): read [AGENTS.md](AGENTS.md) first.

## Backtesting

`backtest/` is a Python backtester that reads `data/radar.db` and
`radar.toml`; see [backtest/README.md](backtest/README.md).

## Daily run

`.github/workflows/daily.yml` runs `radar collect` and `radar render` every
hour (so evening runs pick up the day's close) (and on demand from the Actions tab), then publishes
`public/` to GitHub Pages. The database is kept between runs as the
`radar.db.gz` asset on the `data` release of a private repo,
`Antonio-1110/stocks_but_fr-data` (#62). `.github/workflows/publish.yml`
re-renders from that database and republishes on every push to `main`, so
site changes go live in minutes. Download the database to backtest locally:

    gh release download data -R Antonio-1110/stocks_but_fr-data --pattern radar.db.gz --dir data && gunzip data/radar.db.gz

The release also keeps `radar.prev.db.gz` (the copy before the latest run) and
`radar-YYYY-MM-DD.db.gz` (the first run of each of the last 7 Taipei days). To
roll back after a bad run, cancel any queued `daily` run, rename the
`radar.db.gz` asset (e.g. to `radar.bad.db.gz`), rename the copy you want to
`radar.db.gz`, then delete the bad one; the next run starts from it. A run
that finds no `radar.db.gz` on the release stops rather than starting empty.

Between collect and saving, `radar validate` runs hard checks (sane prices, no
rows lost, no date going backwards since the last snapshot). If one fails, the
run stops and nothing is uploaded. On a pass it records the snapshot (row
counts, latest dates, revenue coverage, commit, soft-check results) in the
database's `snapshots` table and as `manifest.json` on the release. Backtest
reports name the snapshot and commit they ran on.

Paper portfolios and alert state are kept apart in `data/state.db` (the
`state.db.gz` asset, with `state.prev.db.gz` one run back), so rolling back
market data never rewinds them. `radar` attaches it automatically. If
`state.db.gz` goes missing the run stops rather than restarting the
portfolios; rename `state.prev.db.gz` to restore it.

## Calendar

`public/calendar.html` (linked from the dashboard) marks 法說會, ex-dividend
and ex-rights days, shareholder meetings and the revenue/financial report
deadlines. It reads MOPS and the TWSE/TPEx open data directly (no FinMind
requests), each source at most every 6 hours.

## US large caps

`public/us.html` tracks the tickers in `[us]` of `radar.toml` (about the 50
largest US companies, plus SPY and QQQ): price and 1D/1W/1M/YTD/1Y change,
distance from the 52-week high and dollar volume. Daily bars come from Yahoo
Finance's chart data (no key), once per ticker after each US close, into the
`us_companies` and `us_prices` tables; Taiwan tables, paper and the backtester
don't read them. Add or remove a ticker in `radar.toml`; a new one gets its
history from `history_start` on the next run.

## Paper portfolios

`public/paper.html` runs each strategy in `radar.toml` `[paper]` forward on
live data with NT$5M of pretend money: it decides on a trading day's close,
fills at the next open with the `[costs.tw]` commission and sell tax, and is
compared with the same money held in 0050. State is in the `paper_*` tables,
so picks once made never change. No real orders are placed.

Each portfolio shows its last 30 days against 0050, flagged "behind" when it
trails. Once a month the daily run opens a "Paper review YYYY-MM" issue
(label `review`) from `radar review`: every portfolio against 0050 over the
month just ended. Strategies behind get improved or proposed for retirement.

## Environment variables

Every new one gets listed here.

- `FINMIND_TOKEN`: FinMind API token (Actions secret). Optional; without it
  FinMind allows fewer requests per hour, so backfills take more runs.
- `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID`: where alerts go (Actions secrets,
  or `.env` on the Pi). Optional; without them alerts are only logged. Rules
  are set in `radar.toml` under `[alerts]`. `radar collect` only queues
  alerts; `radar notify` sends them, after the state is saved.
- `DATA_TOKEN`: Actions secret, a fine-grained token with Contents read and
  write on the private data repo. Required: without it the daily run stops.
- `DATA_REPO`: Actions variable naming the data repo. Optional; the default is
  this repo's name plus `-data`.

Not financial advice.
