# Baseline: cycle bottom

The reference result for `cycle_bottom` (issues #55, #61). Later changes are judged
against it, and `tests/test_baseline.py` fails if a rerun drifts from it.

- **Data:** snapshot `2026-10-14T19:10:02Z`, collected by commit `4398a29`. The file is
  `radar-2026-10-15.db.gz` on the `data` release (gz sha256 `5854cbf9017f39f5…`). The daily
  cleanup keeps any snapshot a baseline names. Revenue covers 99.4% of listed
  stocks for 2026-09 and 99.8% for 2026-08. The file has 7,364,869 price rows and
  343,093 revenue rows.
- **Code:** backtester at commit `a8476e4`. Parameters come from `radar.toml`
  `[strategy.cycle_bottom]` and are recorded in `cycle_bottom.json`.
- **Periods:** design is 2012-01-02 to 2019-12-31 (the rules were tuned on it); test is 2020-01-02 to 2026-10-13.
  Each period starts from NT$5,000,000 in cash.

| | Design | 0050 | Test | 0050 |
|---|---|---|---|---|
| CAGR | 14.6% | 11.9% | 27.1% | 29.0% |
| Max drawdown | -38.7% | -21.0% | -46.9% | -34.0% |
| Sharpe (rf = 0) | 0.76 | | 0.93 | |
| Closed trades | 185 | | 152 | |
| Win rate | 47.6% | | 48.7% | |
| Turnover per year | 2.3x | | 2.0x | |
| Costs paid (NTD) | 709,760 | | 484,362 | |

Calendar-year returns (2026 is partial):

| Year | Strategy | 0050 |
|---|---|---|
| 2012 | +27.9% | +12.5% |
| 2013 | +27.3% | +11.7% |
| 2014 | -3.9% | +16.7% |
| 2015 | -20.1% | -6.3% |
| 2016 | +17.6% | +19.7% |
| 2017 | +50.7% | +18.1% |
| 2018 | -10.9% | -4.9% |
| 2019 | +51.0% | +33.5% |
| 2020 | +16.4% | +29.2% |
| 2021 | +35.4% | +21.9% |
| 2022 | -38.8% | -21.4% |
| 2023 | +47.1% | +27.5% |
| 2024 | +4.1% | +48.6% |
| 2025 | +56.0% | +36.9% |
| 2026 | +120.2% | +74.8% |

## Assumptions

- Orders are decided at a close and filled at the next trading day's open, on dividend- and
  split-adjusted prices, so returns include dividends.
- A month's revenue is used only on trading days after the 10th of the following month (the
  filing deadline). Real filing dates aren't collected.
- Costs come from `radar.toml`: commission 0.1425% × 0.6 broker discount with a NT$20 minimum,
  and a 0.3% sell tax (0.1% on ETFs). No slippage is modelled beyond that.
- A buy may not take a position above 5% of its 20-day average traded value. Shares are
  fractional (odd lots).
- No fill at a locked open ([limits.tw], #53): a buy opening at limit-up is skipped, and a sell
  opening at limit-down waits for the next open.
- The universe includes delisted stocks, from their listing date. An open outside the day's
  low–high range is replaced by the close. A newest day only partly collected is left out (#60).
- The benchmark is 0050, bought once and held, through the same engine and costs.

## What it doesn't tell you

The design period was used to choose the rules, so only the test period is out of sample, and
even that was seen before this baseline was set. Since 2020 the strategy returned about 2 points a year less than 0050,
with a worst drop of -46.9% against -34.0% for 0050, and turnover of about
2x a year. Its worst calendar year was 2022 (-38.8%).

## Reproduce

    gh release download data --pattern radar-2026-10-15.db.gz && gunzip radar-2026-10-15.db.gz
    cd backtest && RADAR_BASELINE_DB=$PWD/../radar-2026-10-15.db python -m pytest tests/test_baseline.py
