# stocks_but_fr

A personal theme radar for US and Taiwan stocks. It watches what's being
talked about (Reddit, StockTwits, PTT, news), spots themes whose chatter is
accelerating, maps each theme to the listed companies exposed to it, and
publishes a static site with a card per company, so the names become
familiar before the move is over.

Status: just started. The to-do list is the repo's GitHub Issues.

Contributors (human or agent): read [AGENTS.md](AGENTS.md) first.

## Backtesting

`backtest/` is a Python backtester that reads `data/radar.db` and
`radar.toml`; see [backtest/README.md](backtest/README.md).

## Daily run

`.github/workflows/daily.yml` runs `radar collect` and `radar render` every
hour (so evening runs pick up the day's close) (and on demand from the Actions tab), then publishes
`public/` to GitHub Pages. The database is kept between runs as the
`radar.db.gz` asset on the `data` release. `.github/workflows/publish.yml`
re-renders from that database and republishes on every push to `main`, so
site changes go live in minutes. Download the database to backtest locally:

    gh release download data --pattern radar.db.gz --dir data && gunzip data/radar.db.gz

## Calendar

`public/calendar.html` (linked from the dashboard) marks 法說會, ex-dividend
and ex-rights days, shareholder meetings and the revenue/financial report
deadlines. It reads MOPS and the TWSE/TPEx open data directly (no FinMind
requests), each source at most every 6 hours.

## Paper portfolios

`public/paper.html` runs each strategy in `radar.toml` `[paper]` forward on
live data with NT$5M of pretend money: it decides on a trading day's close,
fills at the next open with the `[costs.tw]` commission and sell tax, and is
compared with the same money held in 0050. State is in the `paper_*` tables,
so picks once made never change. No real orders are placed.

## Environment variables

Every new one gets listed here.

- `FINMIND_TOKEN`: FinMind API token (Actions secret). Optional; without it
  FinMind allows fewer requests per hour, so backfills take more runs.
- `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID`: where alerts go (Actions secrets,
  or `.env` on the Pi). Optional; without them alerts are only logged. Rules
  are set in `radar.toml` under `[alerts]`.

Not financial advice.
