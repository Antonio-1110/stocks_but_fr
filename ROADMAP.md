# Roadmap

Updated 2026-10-11, after the repository audit
(`/mnt/project-files/audit/repo-audit-2026-10.md` in the project). Update this
file at the end of every sprint. The owner's full handoff is not in the repo;
the section numbers below refer to it.

Status words: **done**, **planned** (with the release it belongs to),
**deferred** (with the trigger that brings it back), **cut** (with the reason).

## Now

Revenue backfill finishing (about 60% of listed stocks on 2026-10-11). Until it
is complete, revenue-based results are provisional, and paper portfolios and
alerts wait (#37).

**Sprint 0 (done 2026-10-11): fix what was broken or could lose data**

- #37 Paper portfolios and alerts no longer act on a partly collected revenue month.
- #38 The release keeps the previous database and 7 daily snapshots (rollback in README).
- #39 TPEx make-up trading Saturdays are fetched (15 days were missing).
- #40 Fills no longer use an open price outside the day's low-high range.
- #41 This file, PRODUCT.md, short CLAUDE.md.

**Sprint 1 (proposed, waiting for approval): make results reproducible**

1. Tests on every push and before each data publish (CI gate).
2. A manifest per snapshot (row counts, latest dates, coverage, code version,
   checksum); backtest reports record which snapshot and code they ran on.
3. `radar validate`: hard checks block a publish, soft checks go to a
   data-health page.
4. Paper and alert state in their own file, as an append-only ledger; Telegram
   sends only after state is saved.
5. Once the backfill is complete: rerun revenue momentum on one named snapshot
   and make that the baseline.

Release 1 is done when that baseline exists, with its assumptions written down
and a regression test pinning it.

## The handoff, section by section

| § | Section | Status |
|---|---|---|
| 1 | Product charter | done: condensed into PRODUCT.md. **Cut:** Singapore. A third market's calendars, costs and data roughly doubles upkeep for one person; nothing in the strategy research needs it. Bring back if you hold SGX stocks. |
| 2 | Principles | done: in PRODUCT.md |
| 3.1 | Discretionary research | partly done (dashboard, calendar, links out). Rest: see §10–12 |
| 3.2 | Systematic strategies | done for Taiwan (backtester, two strategies, paper) |
| 4.1 | Daily bars, swing horizon | done |
| 4.2 | NT$5M capital, sizing, lots, liquidity, costs | done except: lot sizes (fractional units; fine after 2020-10 odd-lot trading) and slippage (**planned R3**: one cost-stress setting) |
| 4.3 | Benchmarks | done for Taiwan (0050 total return, verified). Other markets **deferred** with them |
| 4.4 | Autonomy progression | done as a rule: research → backtest → paper; live **deferred** (§15) |
| 5 | Reported state | verified in the audit; differences listed there |
| 6 | Six-layer architecture | **planned** as boundaries inside the current monolith (state file separate from market data in Sprint 1). No new services |
| 7.1 | Curated archive | done for Taiwan prices, events, revenue, flows. **Planned R1:** listing dates, industry history. FX, exchange calendars **deferred** to a second market |
| 7.2 | On-demand data with provenance | **planned R1:** source and retrieval time on stored rows. Filings, transcripts, news **cut**: link out instead |
| 7.3 | Storage technology | SQLite stays. Parquet/DuckDB **deferred** until a backtest load takes over 5 minutes or a second market is added (measured: Parquet loads prices in 3 s instead of 47 s) |
| 7.4 | Storage benchmark | done in the audit |
| 8.1 | Staged publication | **trimmed:** snapshots + manifest + validation before publish (Sprint 1). A separate candidate store and atomic manifest switching are **cut**: one writer, a daily cadence and kept snapshots cover the same risk |
| 8.2 | Validation checks | **planned Sprint 1** (`radar validate`) |
| 8.3 | Point-in-time | done for revenue (deadline rule), prices, flows. **Planned R1:** keep revenue revisions; real filing dates **deferred** until the deadline rule is shown to cost return |
| 8.4 | Reproducibility | **planned Sprint 1** |
| 8.5 | Data-health dashboard | **planned Sprint 1** as one static page |
| 9 | Security master | **planned R1:** listing dates, industry history, provider ids for Taiwan. Internal ids, multi-currency, settlement and short rules **deferred** to a second market |
| 10.1 | Market overview | **planned R2:** breadth, new highs and lows, sector strength, from data already stored |
| 10.2 | Sector analysis | **planned R2:** sector revenue and price trends on the dashboard |
| 10.3 | Cross-market comparison | **cut:** needs two markets and normalisation work for little decision value |
| 11.1 | Screening engine | partly done (two rankings). Configurable filters **planned R2** |
| 11.2 | Explainable signals | partly done (alerts say why). **Planned R2** on the dashboard |
| 11.3 | Emerging-trend detection | **planned R3** as hypotheses run through the backtester, not a separate engine |
| 11.4 | Signal history, no duplicate alerts | **planned Sprint 1** (with the state file) |
| 12 | Company research pages, thesis records | **cut** as built pages: Goodinfo, Yahoo and MOPS links already cover it. Thesis notes **deferred** until you keep them: a markdown file in the repo is enough to start |
| 13.1 | Versioned strategy definitions | partly done (rules in code, settings in `radar.toml`). **Planned R3:** a version number recorded per run |
| 13.2 | Backtest correctness audit | done (audit). Fixes: #40 done; survivorship in cycle bottom **planned R1** |
| 13.3 | Execution assumptions | done: next open, costs, ADV cap. Locked limit-up fills **planned R1**. Partial fills and spread models **cut**: at NT$5M with a 5% ADV cap they change little |
| 13.4 | Robustness (walk-forward, sensitivity, subperiods) | **planned R3** as small scripts |
| 13.5 | Experiment registry | **planned R3**, light: one row and one report per run, with dataset version and a conclusion field. No separate app |
| 13.6 | Metrics | partly done. **Planned R1:** volatility, Sortino, drawdown length, exposure, benchmark-relative, trade concentration |
| 14.1–14.3 | Paper portfolios | done for two strategies. **Planned Sprint 1:** append-only ledger in its own file |
| 14.4 | Portfolio analysis | partly done (equity vs 0050). Attribution **planned R3** |
| 15 | Broker integration, live execution | **deferred**: trigger is a strategy that has paper-traded for 6+ months within its backtest range, and your explicit go-ahead |
| 16 | AI integration | **cut** as a product feature: Claude in this project already summarises and drafts. Numbers stay in code |
| 17.1 | Keep Actions and Pages | done |
| 17.2 | Agent workflow (branches, PRs) | open question to the owner; until then main-only per AGENTS.md |
| 17.3 | CI/CD checks | **planned Sprint 1**: tests on push and before publish |
| 17.4 | Cost management | done as a rule (PRODUCT.md non-goals). Licensing of the public data release: **open question** |
| 18 R1 | Trustworthy foundation | in progress (Sprints 0–1) |
| 18 R2 | Market and company intelligence | **planned**, trimmed as above (§10–12) |
| 18 R3 | Research laboratory | **planned**, light (§13) |
| 18 R4 | Multi-market | **deferred**: start a US prices-only universe (≤ 500 names) once R1–R3 work for Taiwan and you want a US sleeve. Singapore cut |
| 18 R5 | Portfolio and execution | ledger in Sprint 1; broker work **deferred** (§15) |
| 19 | Explicitly deferred list | agreed, unchanged |
| 20.1 | Data pipeline tests | partly done (parsers, backfill resume). **Planned Sprint 1:** validation and rollback tests |
| 20.2 | Backtest tests | done for engine and strategies on fixtures. **Planned R1:** a golden regression run |
| 20.3 | Paper trading tests | partly done (33% coverage). **Planned Sprint 1** with the ledger |
| 20.4 | Regression testing | **planned R1** |
| 21 | Documentation | PRODUCT.md, ROADMAP.md, README, AGENTS.md, backtest/README.md. ARCHITECTURE, DATA and RESEARCH files **cut** for now: README and backtest/README cover them at this size. Revisit when either passes about 200 lines |
| 22–23 | Audit | done |
| 24 | Guiding principle | adopted |
