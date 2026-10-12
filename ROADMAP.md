# Roadmap

Updated 2026-10-13, at the end of Sprint 1. Earlier: 2026-10-11, after the repository audit
(`/mnt/project-files/audit/repo-audit-2026-10.md` in the project). Update this
file at the end of every sprint. The owner's full handoff is not in the repo;
the section numbers below refer to it.

Status words: **done**, **planned** (with the release it belongs to),
**deferred** (with the trigger that brings it back), **cut** (with the reason).

## Now

Release 1 (trustworthy foundation) is half done. The data is complete and
versioned, and revenue momentum has a pinned baseline
([backtest/baselines/revenue_momentum.md](backtest/baselines/revenue_momentum.md)).
Sprint 2 is proposed and waits for approval.

**Sprint 0 (done 2026-10-11): fix what was broken or could lose data**

- #37 Paper portfolios and alerts no longer act on a partly collected revenue month.
- #38 The release keeps the previous database and 7 daily snapshots (rollback in README).
- #39 TPEx make-up trading Saturdays are fetched (15 days were missing).
- #40 Fills no longer use an open price outside the day's low-high range.
- #41 This file, PRODUCT.md, short CLAUDE.md.

**Sprint 1 (done 2026-10-13): make results reproducible**

- #42 The revenue backfill had stalled on delisted tickers with no data. It's fixed,
  and now only 8 listed stocks lack revenue (from 813).
- #43 Tests (Go, gofmt, backtester) run before every publish and every collect.
- #44 `radar validate` blocks a publish that loses rows, moves a date backwards or
  has impossible prices. Each snapshot records counts, coverage and commit.
  Backtest reports name their snapshot and code.
- #45 Data-health page.
- #46 Paper and alert state live in `state.db`, apart from market data.
- #47 Telegram alerts are queued and sent only after state is saved.
- #48 Paper trades are also written to an append-only ledger, reconciled each run.
- #49, #51 The daily save retries GitHub errors, resumes a half-finished save, and
  never starts from an empty database.
- #50 Revenue momentum baseline on snapshot 2026-10-12T22:51:31Z, kept on the
  release as `baseline-2026-10-12.db.gz`. Design CAGR is 30.9% (0050 11.9%), test
  CAGR 45.3% (0050 29.4%), and test max drawdown -34.6% (0050 -34.0%). A
  regression test pins it.

**Sprint 2 (proposed): finish Release 1**

1. Security master: listing dates (empty for all 3,048 companies) and industry
   history, so the universe on each past date is right.
2. Fills a real account couldn't get: no buy at limit-up, no sell at limit-down.
3. The metrics the audit asked for: volatility, Sortino, drawdown length, exposure,
   return relative to 0050, and trade concentration. Add them to reports and the
   baseline.
4. Survivorship check for cycle bottom, then a cycle bottom baseline.
5. Find the source of the 60,495 TWSE rows whose open is outside the day's range,
   and fix them at collection if FinMind's open is wrong.

Release 1 is done when both strategies have baselines on data that passes these checks.

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
| 6 | Six-layer architecture | partly done as boundaries inside the current monolith: state file separate from market data (#46). No new services |
| 7.1 | Curated archive | done for Taiwan prices, events, revenue, flows. **Planned R1:** listing dates, industry history. FX, exchange calendars **deferred** to a second market |
| 7.2 | On-demand data with provenance | **planned R1:** source and retrieval time on stored rows. Filings, transcripts, news **cut**: link out instead |
| 7.3 | Storage technology | SQLite stays. Parquet/DuckDB **deferred** until a backtest load takes over 5 minutes or a second market is added (measured: Parquet loads prices in 3 s instead of 47 s) |
| 7.4 | Storage benchmark | done in the audit |
| 8.1 | Staged publication | **trimmed**, done: snapshots + manifest + validation before publish (#38, #44). A separate candidate store and atomic manifest switching are **cut**: one writer, a daily cadence and kept snapshots cover the same risk |
| 8.2 | Validation checks | done (`radar validate`, #44); more checks come with Sprint 2 |
| 8.3 | Point-in-time | done for revenue (deadline rule), prices, flows. **Planned R1:** keep revenue revisions; real filing dates **deferred** until the deadline rule is shown to cost return |
| 8.4 | Reproducibility | done: snapshot id and code version on every backtest run, pinned baseline (#44, #50) |
| 8.5 | Data-health dashboard | done: one static page (#45) |
| 9 | Security master | **planned R1:** listing dates, industry history, provider ids for Taiwan. Internal ids, multi-currency, settlement and short rules **deferred** to a second market |
| 10.1 | Market overview | **planned R2:** breadth, new highs and lows, sector strength, from data already stored |
| 10.2 | Sector analysis | **planned R2:** sector revenue and price trends on the dashboard |
| 10.3 | Cross-market comparison | **cut:** needs two markets and normalisation work for little decision value |
| 11.1 | Screening engine | partly done (two rankings). Configurable filters **planned R2** |
| 11.2 | Explainable signals | partly done (alerts say why). **Planned R2** on the dashboard |
| 11.3 | Emerging-trend detection | **planned R3** as hypotheses run through the backtester, not a separate engine |
| 11.4 | Signal history, no duplicate alerts | done: alert outbox is the history, sent only after saving (#47) |
| 12 | Company research pages, thesis records | **cut** as built pages: Goodinfo, Yahoo and MOPS links already cover it. Thesis notes **deferred** until you keep them: a markdown file in the repo is enough to start |
| 13.1 | Versioned strategy definitions | partly done (rules in code, settings in `radar.toml`). **Planned R3:** a version number recorded per run |
| 13.2 | Backtest correctness audit | done (audit). Fixes: #40 done; survivorship in cycle bottom **planned R1** |
| 13.3 | Execution assumptions | done: next open, costs, ADV cap. Locked limit-up fills **planned R1**. Partial fills and spread models **cut**: at NT$5M with a 5% ADV cap they change little |
| 13.4 | Robustness (walk-forward, sensitivity, subperiods) | **planned R3** as small scripts |
| 13.5 | Experiment registry | **planned R3**, light: one row and one report per run, with dataset version and a conclusion field. No separate app |
| 13.6 | Metrics | partly done. **Planned R1:** volatility, Sortino, drawdown length, exposure, benchmark-relative, trade concentration |
| 14.1–14.3 | Paper portfolios | done for two strategies, with an append-only ledger in its own file (#46, #48) |
| 14.4 | Portfolio analysis | partly done (equity vs 0050). Attribution **planned R3** |
| 15 | Broker integration, live execution | **deferred**: trigger is a strategy that has paper-traded for 6+ months within its backtest range, and your explicit go-ahead |
| 16 | AI integration | **cut** as a product feature: Claude in this project already summarises and drafts. Numbers stay in code |
| 17.1 | Keep Actions and Pages | done |
| 17.2 | Agent workflow (branches, PRs) | open question to the owner; until then main-only per AGENTS.md |
| 17.3 | CI/CD checks | done: tests before every publish and collect (#43) |
| 17.4 | Cost management | done as a rule (PRODUCT.md non-goals). Licensing of the public data release: **open question** |
| 18 R1 | Trustworthy foundation | in progress: Sprints 0–1 done, Sprint 2 proposed |
| 18 R2 | Market and company intelligence | **planned**, trimmed as above (§10–12) |
| 18 R3 | Research laboratory | **planned**, light (§13) |
| 18 R4 | Multi-market | **deferred**: start a US prices-only universe (≤ 500 names) once R1–R3 work for Taiwan and you want a US sleeve. Singapore cut |
| 18 R5 | Portfolio and execution | ledger done (#48); broker work **deferred** (§15) |
| 19 | Explicitly deferred list | agreed, unchanged |
| 20.1 | Data pipeline tests | done: parsers, backfill resume, validation (#44); rollback steps checked against a stubbed release (#46, #49) |
| 20.2 | Backtest tests | done for engine and strategies on fixtures, plus the baseline regression run (#50) |
| 20.3 | Paper trading tests | done: every paper test reconciles the ledger (#48) |
| 20.4 | Regression testing | done for revenue momentum (#50); cycle bottom in Sprint 2 |
| 21 | Documentation | PRODUCT.md, ROADMAP.md, README, AGENTS.md, backtest/README.md. ARCHITECTURE, DATA and RESEARCH files **cut** for now: README and backtest/README cover them at this size. Revisit when either passes about 200 lines |
| 22–23 | Audit | done |
| 24 | Guiding principle | adopted |
