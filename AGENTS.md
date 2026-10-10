# Rules for agents working in this repo

Read this whole file before editing any code. These rules come from the repo
owner and override any default habit of yours (including "make a branch and
open a PR").

## 1. One branch: `main`

This project is pre-product. There are no feature branches and no pull
requests until the owner says otherwise.

- Work directly on `main`. Do not create branches. Do not open PRs.
- Commit small and often. Push after every commit.
- Sync loop, every time:
  ```sh
  git pull --rebase origin main   # before you start, and before every commit
  go vet ./... && go test ./...   # must pass before you push
  git push origin main
  ```
- If the push is rejected, `git pull --rebase origin main`, re-run the build
  and tests, and push again.
- On a rebase conflict, keep both sides' intent. If you can't tell what the
  other side meant, stop and ask the owner. Never `--force` push, never
  rewrite history, never revert someone else's commit to make yours fit.
- Never leave `main` broken. If you can't finish, push only code that builds
  (stub it, or leave it unwired), and say what's left on the issue.

## 2. The to-do list is GitHub Issues

Every piece of work is an issue. Before you write code:

1. Find the issue for your task. If none exists, create one first (with a
   **Files** section, see below).
2. Check it isn't already labelled `in-progress`. If it is, someone else owns
   it: don't touch it. Pick something else or tell the owner.
3. Claim it: add the `in-progress` label and comment
   `Claimed by <short description of your session/thread>`.
4. Check its **Blocked by** list. Don't start if a blocker is still open.

When you finish: the last commit message ends with `Closes #N`, and remove
the `in-progress` label. If you find extra work along the way, open a new
issue for it instead of growing your current one.

## 3. Stay inside your files

Each issue has a **Files** section listing the directories it owns. Only
edit those. This is what keeps parallel agents from stepping on each other.

Shared files, which anyone may touch but only with small, additive edits
(add a line or a field; don't reorder, reformat, or rename):

- `go.mod`, `go.sum` (run `go mod tidy` only for packages you added)
- `internal/model/` (shared types)
- `cmd/radar/main.go` (wiring only: add your step, don't restructure)
- `AGENTS.md`, `README.md`
- `radar.toml` (add your own section; don't change other sections' values)

If you need a bigger change to a shared file or to another issue's files,
open an issue describing it and leave it for the owner to schedule.

## 4. Stack and layout

- **Language:** Go (version pinned in `go.mod`). Standard library first; add a dependency only when
  it saves real work. Pure-Go deps only (no cgo) so it cross-compiles to the
  Raspberry Pi (`GOARCH=arm64`).
- **Storage:** SQLite via `modernc.org/sqlite` (pure Go), file at `data/radar.db`.
- **Output:** static site generated with `html/template` into `public/`,
  deployed to GitHub Pages. No live backend.
- **Run:** GitHub Actions cron first; Docker (`Dockerfile`, `compose.yaml`) so
  the same thing runs on the owner's Pi.

Current focus: **Taiwan only, backtest first.** Issues labelled `later`
are parked; don't start them unless the owner asks.

```
radar.toml            THE single config file: trading costs (commission,
                      broker discount, min fee, sell tax), strategy params.
                      Read by Go (internal/config) and Python (tomllib).
                      Never hard-code a cost or a strategy parameter.
cmd/radar/            entrypoint: radar collect | radar render
internal/config/      loads radar.toml
internal/model/       shared types (Company, Price, MonthlyRevenue, ...)
internal/store/       SQLite open/migrate/read/write
internal/market/tw/   Taiwan universe (incl. delisted) + daily prices
  revenue/            monthly revenue (月營收)
  flows/              institutional flows (三大法人)
internal/site/        static dashboard generator
internal/calendar/    company event calendar (法說會, 除權息, 股東會): collect + page
web/                  dashboard templates and static assets
.github/workflows/    scheduled run + Pages deploy
backtest/             Python backtester; reads data/radar.db + radar.toml
  strategies/         one file per strategy
docs/                 research notes
(later) internal/market/us/, internal/companies/, internal/sources/,
        internal/themes/, internal/exposure/
```

Go is the primary language. Python is used only in `backtest/`, with its
own `pyproject.toml`. The two never import each other: the SQLite file is
the only contract between them.

## 5. Other rules

- Secrets (API keys) come from environment variables, never committed. List
  any new variable in `README.md`.
- Every collector must be polite: identify itself with a User-Agent, respect
  rate limits, cache responses, and fail soft (log and skip, don't crash the run).
- Write a test for parsing code using a saved sample response in
  `testdata/`, so tests never hit the network.
- No code that places broker orders unless the owner asks for it explicitly.
