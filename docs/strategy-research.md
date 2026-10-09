# Where a 5M NTD account can realistically find alpha

*Research notes, 2026-10-09. Not financial or tax advice. All numbers are from the sources linked at the bottom; past backtests overstate future returns.*

## Bottom line

The strategy with the best evidence for your size is **"fundamental momentum" in Taiwan small and mid caps**:

1. Buy stocks whose **monthly revenue (月營收) growth is strong and persistent** (high YoY growth for several months running, ideally record revenue),
2. that are **trading near their 52-week high**,
3. optionally confirmed by **投信 (investment trust) net buying**.
4. Hold 10–15 names, equal weight, **rebalance monthly** right after the 10th, when revenue reports are due.

A smaller US sleeve can run the same idea (earnings/revenue surprise + price strength), but US alpha is harder to get and the tax setup for a Taiwan resident is worse.

Your edge is not speed or information. It's **size**: at 5M NTD you can hold stocks that trade only 10–50M NTD a day, which funds can't touch. That's exactly where these anomalies live.

## Why this and not something else

| Idea | What the evidence says |
|---|---|
| Classic price momentum (buy past 6–12m winners) in Taiwan | **Doesn't work.** 1987–2017: about −0.1%/month, not significant. |
| 52-week-high momentum in Taiwan | **Works.** Roughly 0.4–0.6%/month spread, about 7.5%/yr, Sharpe ~0.45. Stronger in downturns. |
| Monthly revenue momentum in Taiwan | **Works**, a Taiwan-specific data edge (most markets report quarterly). Stronger when growth is persistent, and **concentrated in hard-to-arbitrage (smaller) stocks**. Record-revenue announcements are studied as their own signal. |
| Following 投信 buying alone | **Weak.** One backtest found about 9.7% CAGR with a −45% drawdown, below 0050's 20.6% over 2015–2026/6. |
| 投信 buying + revenue momentum | Same backtest: **33.9% CAGR, Sharpe 1.11**, fees and tax included. The rules are paywalled and it's one practitioner's backtest, so treat it as an optimistic upper bound to verify, not a promise. |
| US combined price + earnings + revenue momentum | About 1.6%/month historically (1974–2007), strongest in small and value stocks. It's old data, and published anomalies usually shrink after they become known. |
| US post-earnings drift | Mostly **gone outside microcaps** since about 2001. |
| Social-media chatter | No good evidence it gives an edge at entry. More useful as a "this is crowded now" exit warning. |

## Costs: why you must hold weeks, not days

- Taiwan securities transaction tax: **0.3% on every sell** (0.15% for day trades, reduced rate extended to the end of 2027).
- Commission: 0.1425% each side before broker discounts.
- So a round trip costs roughly **0.45–0.6%**. With monthly rebalancing and about half the portfolio turning over, that's a **3–4% per year drag**. Higher-frequency trading in Taiwan has to clear a very high bar, which is why this plan is monthly.

## US side, as a Taiwan resident (check with an accountant)

- **No US tax on capital gains** for non-resident aliens. Dividends are **withheld at 30%**; as of the latest info I found, there's no US–Taiwan treaty in force to reduce it.
- **US estate tax applies above only US$60,000** of US-listed assets (about 1.9M NTD), at rates of 18–40%. With 5M NTD, a big US sleeve exposes your heirs to this.
- Taiwan's AMT: if household overseas income reaches **1M NTD in a year**, all of it counts toward basic income. Tax is 20% above a **7.5M NTD** exemption, so it's unlikely to bite at your size, but realised US gains must be reported.
- Practical upshot: keep the US sleeve modest, favour low-dividend growth names, and keep the estate-tax line in mind.

## Illustrative setup (to be backtested before real money)

- **Universe:** TWSE + TPEx stocks with 20-day average traded value at least 20× your position size (about 10M NTD/day for a 400k position).
- **Rank** monthly by: YoY revenue growth persistence (last 3 months) + distance to 52-week high + 投信 net buy (optional).
- **Hold** the top 10–15, equal weight (about 300–500k NTD each).
- **Exit** when a name drops out of the top ~30, or on a hard stop (e.g. −15% or close below the 50-day average).
- **Regime check** to test: reduce exposure when the TAIEX is below its 200-day average. This is a hypothesis to test in the backtest, not settled evidence.
- **Expectation:** if the edge is real after costs, a few % a year above the market with deeper drawdowns than 0050 at times. Anything much better in a backtest is probably overfit.

## What this changes in the build

The core becomes a **Taiwan fundamental-momentum scanner plus a backtester**, not the chatter radar:

1. Collectors for TW daily prices, **monthly revenue** and **三大法人 flows**, with 10+ years of history (FinMind or TWSE).
2. A **Python backtester early**, to check the 33.9% claim with realistic costs before any money is risked.
3. The site shows the monthly ranked list plus company cards (still the "get familiar with names" goal).
4. Chatter moves to a later "crowdedness" layer.

## Sources

- [Trading on Record-Breaking Monthly Revenue Announcements (SSRN)](https://papers.ssrn.com/sol3/papers.cfm?abstract_id=5345252)
- [Market reaction to monthly revenue momentum (Review of Quantitative Finance and Accounting, 2025)](https://link.springer.com/article/10.1007/s11156-025-01434-0)
- [The Effect of the Movement in 52-Week High on Momentum Profit: Evidence from Taiwan](https://ijbesar.ihu.gr/docs/volume16_issue1/16_01_07.pdf)
- [Price, Earnings, and Revenue Momentum Strategies (Chen et al.)](http://centerforpbbefr.rutgers.edu/TaipeiPBFR&D/990515Papers/6-3.pdf)
- [FinLab: institutional-following strategies, backtested](https://finlab.finance/en/blog/institutional-strategy)
- [UCLA Anderson Review: Is post-earnings announcement drift a thing again?](https://anderson-review.ucla.edu/is-post-earnings-announcement-drift-a-thing-again/)
- [Taiwan extends reduced day-trading tax until 2027](https://regfollower.com/taiwan-extends-reduced-day-trading-tax-rate-until-2027/)
- [Dividend withholding and US estate tax for non-US investors (StashAway)](https://www.stashaway.sg/r/dividend-withholding-tax-estate-tax-us-equities-singapore)
- [Taiwan overseas investment tax guide 2026 (AMT)](https://www.shareuhack.com/en/posts/taiwan-overseas-investment-tax-guide-2026)
- [Microcaps: the small investor's playground (Hedge Fund Alpha)](https://hedgefundalpha.com/strategies/ian-cassel-microcaps-the-small-investors-playground/)
