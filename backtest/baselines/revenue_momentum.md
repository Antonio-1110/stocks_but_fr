# Baseline: revenue momentum

The reference result for `revenue_momentum` (issues #50, #52, #61). Later changes are judged
against it, and `tests/test_baseline.py` fails if a rerun drifts from it.

- **Data:** snapshot `2026-10-14T19:10:02Z`, collected by commit `4398a29`. The file is
  `radar-2026-10-15.db.gz` on the `data` release (gz sha256 `5854cbf9017f39f5…`). The daily
  cleanup keeps any snapshot a baseline names. Revenue covers 99.4% of listed
  stocks for 2026-09 and 99.8% for 2026-08. The file has 7,364,869 price rows and
  343,093 revenue rows.
- **Code:** backtester at commit `a8476e4`. Parameters come from `radar.toml`
  `[strategy.revenue_momentum]` and are recorded in `revenue_momentum.json`.
- **Periods:** design is 2012-01-02 to 2019-12-31 (the rules were tuned on it); test is 2020-01-02 to 2026-10-13.
  Each period starts from NT$5,000,000 in cash.

| | Design | 0050 | Test | 0050 |
|---|---|---|---|---|
| CAGR | 27.5% | 11.9% | 43.2% | 29.0% |
| Max drawdown | -24.1% | -21.0% | -32.9% | -34.0% |
| Sharpe (rf = 0) | 1.40 | | 1.43 | |
| Closed trades | 558 | | 624 | |
| Win rate | 46.2% | | 46.3% | |
| Turnover per year | 5.6x | | 7.4x | |
| Costs paid (NTD) | 2,905,834 | | 5,141,743 | |

Calendar-year returns (2026 is partial):

| Year | Strategy | 0050 |
|---|---|---|
| 2012 | +7.6% | +12.5% |
| 2013 | +33.0% | +11.7% |
| 2014 | +8.7% | +16.7% |
| 2015 | +34.3% | -6.3% |
| 2016 | +16.8% | +19.7% |
| 2017 | +64.1% | +18.1% |
| 2018 | +14.0% | -4.9% |
| 2019 | +52.6% | +33.5% |
| 2020 | +37.6% | +29.2% |
| 2021 | +115.5% | +21.9% |
| 2022 | -17.2% | -21.4% |
| 2023 | +59.3% | +27.5% |
| 2024 | +55.5% | +48.6% |
| 2025 | +38.4% | +36.9% |
| 2026 | +35.8% | +74.8% |

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
even that was seen before this baseline was set. Since 2020 the strategy returned about 14 points a year more than 0050,
with a worst drop of -32.9% against -34.0% for 0050, and turnover of about
7x a year. Its worst calendar year was 2022 (-17.2%).

## Reproduce

    gh release download data --pattern radar-2026-10-15.db.gz && gunzip radar-2026-10-15.db.gz
    cd backtest && RADAR_BASELINE_DB=$PWD/../radar-2026-10-15.db python -m pytest tests/test_baseline.py
