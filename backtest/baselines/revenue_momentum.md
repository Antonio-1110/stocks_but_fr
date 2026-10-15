# Baseline: revenue momentum

The reference result for `revenue_momentum` (issues #50, #52). Later changes are judged
against it, and `tests/test_baseline.py` fails if a rerun drifts from it.

- **Data:** snapshot `2026-10-14T19:10:02Z`, collected by commit `4398a29`. The file is
  `radar-2026-10-15.db.gz` on the `data` release (gz sha256 `5854cbf9017f39f5…`). The daily
  cleanup keeps any snapshot a baseline names. Revenue covers 99.4% of listed
  stocks for 2026-09 and 99.8% for 2026-08. The file has 7,364,869 price rows and
  343,093 revenue rows.
- **Code:** backtester at commit `4398a29`. Parameters come from `radar.toml`
  `[strategy.revenue_momentum]` and are recorded in `revenue_momentum.json`.
- **Periods:** design is 2012-01-02 to 2019-12-31 (the rules were tuned on it); test is 2020-01-02 to 2026-10-14.
  Each period starts from NT$5,000,000 in cash.

| | Design | 0050 | Test | 0050 |
|---|---|---|---|---|
| CAGR | 30.9% | 11.9% | 44.7% | 29.1% |
| Max drawdown | -23.1% | -21.0% | -34.6% | -34.0% |
| Sharpe (rf = 0) | 1.53 | | 1.45 | |
| Closed trades | 565 | | 635 | |
| Win rate | 46.9% | | 46.9% | |
| Turnover per year | 5.6x | | 7.5x | |
| Costs paid (NTD) | 3,348,819 | | 5,485,443 | |

Calendar-year returns (2026 is partial):

| Year | Strategy | 0050 |
|---|---|---|
| 2012 | +7.0% | +12.5% |
| 2013 | +42.5% | +11.7% |
| 2014 | +13.4% | +16.7% |
| 2015 | +39.5% | -6.3% |
| 2016 | +15.4% | +19.7% |
| 2017 | +63.3% | +18.1% |
| 2018 | +25.5% | -4.9% |
| 2019 | +51.2% | +33.5% |
| 2020 | +38.7% | +29.2% |
| 2021 | +109.8% | +21.9% |
| 2022 | -17.1% | -21.4% |
| 2023 | +56.6% | +27.5% |
| 2024 | +65.2% | +48.6% |
| 2025 | +39.1% | +36.9% |
| 2026 | +40.9% | +75.8% |

## Assumptions

- Orders are decided at a close and filled at the next trading day's open, on dividend- and
  split-adjusted prices, so returns include dividends.
- A month's revenue is used only on trading days after the 10th of the following month (the
  filing deadline). Real filing dates aren't collected.
- Costs come from `radar.toml`: commission 0.1425% × 0.6 broker discount with a NT$20 minimum,
  and a 0.3% sell tax (0.1% on ETFs). No slippage is modelled beyond that.
- A buy may not take a position above 5% of its 20-day average traded value. Shares are
  fractional (odd lots).
- The universe includes delisted stocks. An open outside the day's low–high range is replaced
  by the close.
- The benchmark is 0050, bought once and held, through the same engine and costs.

## What it doesn't tell you

The design period was used to choose the rules, so only the test period is out of sample, and
even that was seen before this baseline was set. Since 2020 the strategy beat 0050 by about
16 points a year, with a similar worst drop (-34.6% vs -34.0%) and turnover of about
8x a year. Its worst calendar year was 2022 (-17.1%).

## Reproduce

    gh release download data --pattern radar-2026-10-15.db.gz && gunzip radar-2026-10-15.db.gz
    cd backtest && RADAR_BASELINE_DB=$PWD/../radar-2026-10-15.db python -m pytest tests/test_baseline.py
