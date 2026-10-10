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
`radar.db.gz` asset on the `data` release; download it to backtest locally:

    gh release download data --pattern radar.db.gz --dir data && gunzip data/radar.db.gz

## Environment variables

Every new one gets listed here.

- `FINMIND_TOKEN`: FinMind API token (Actions secret). Optional; without it
  FinMind allows fewer requests per hour, so backfills take more runs.

Not financial advice.
