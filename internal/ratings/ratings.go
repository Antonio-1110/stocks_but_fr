// Package ratings gives single stocks a daily 1-5 trend rating for the short
// and the long term, logs every rating with the day it was made, and scores
// each one against what the stock did once its horizon has passed (issue #78).
//
// Ratings rank stocks within their market on the day (quintiles of a score),
// so a 5 says "likely to do better than most", not "will rise". The page
// reports both: whether up and down calls got the direction right, and
// whether they beat the market's benchmark.
package ratings

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

// The tables live in the state database (#46): a rating logged on a day must
// never be rebuilt later with hindsight, and must survive a market-data rollback.
const schema = `
CREATE TABLE IF NOT EXISTS state.ratings (
	market       TEXT NOT NULL,      -- TW or US
	ticker       TEXT NOT NULL,
	made_on      TEXT NOT NULL,      -- the trading day whose close it uses
	backfill     INTEGER NOT NULL,   -- 1: rated after the fact on the first run (a backtest)
	short_rating INTEGER NOT NULL,   -- 1..5
	short_score  REAL,
	long_rating  INTEGER NOT NULL,
	long_score   REAL,
	high         REAL,               -- close vs 52-week high, -0.1 = 10% below
	month_ret    REAL,
	week_ret     REAL,
	year_ret     REAL,               -- 12 months, skipping the latest one
	ma200        REAL,               -- close vs 200-day average
	rev_yoy      REAL,               -- %, TW only
	PRIMARY KEY (market, ticker, made_on)
);
CREATE TABLE IF NOT EXISTS state.rating_outcomes (
	market   TEXT NOT NULL,
	ticker   TEXT NOT NULL,
	made_on  TEXT NOT NULL,
	horizon  INTEGER NOT NULL,       -- trading days
	start_on TEXT NOT NULL,          -- the next trading day: returns run from its close...
	end_on   TEXT NOT NULL,          -- ...to this day's close
	ret      REAL,                   -- the stock's return, dividends included; NULL if unknown
	bench    REAL,                   -- the benchmark's (0050, SPY) over the same days
	PRIMARY KEY (market, ticker, made_on, horizon)
);
CREATE TABLE IF NOT EXISTS state.rating_runs (
	market     TEXT PRIMARY KEY,
	data_end   TEXT NOT NULL,        -- newest stored day and its row count at the last run:
	data_rows  INTEGER NOT NULL,     -- nothing to do while they don't change
	backfilled INTEGER NOT NULL DEFAULT 0
);
`

// Run is the `radar collect` step: rate the newest fully collected day, on
// the first run also rate past days, then score every rating whose horizon
// has passed.
func Run(_ context.Context, cfg config.Config, st *store.Store) error {
	if _, err := st.DB.Exec(schema); err != nil {
		return err
	}
	for _, m := range []string{"TW", "US"} {
		if err := runMarket(st.DB, cfg, m); err != nil {
			return fmt.Errorf("%s: %w", m, err)
		}
	}
	return nil
}

func horizons(cfg config.Ratings) []int {
	return append(append([]int{}, cfg.ShortHorizons...), cfg.LongHorizons...)
}

func runMarket(db *sql.DB, cfg config.Config, market string) error {
	end, rows, err := newestDate(db, market)
	if err != nil || end == "" {
		return err
	}
	var lastEnd string
	var lastRows, backfilled int
	db.QueryRow(`SELECT data_end, data_rows, backfilled FROM state.rating_runs WHERE market = ?`, market).
		Scan(&lastEnd, &lastRows, &backfilled)
	if lastEnd == end && lastRows == rows && backfilled == 1 {
		log.Printf("ratings %s: nothing new since %s", market, end)
		return nil
	}

	rc := cfg.Ratings
	hs := horizons(rc)
	longest := 0
	for _, h := range hs {
		longest = max(longest, h)
	}
	endT, err := time.Parse("2006-01-02", end)
	if err != nil {
		return err
	}
	// A year of history for the newest rating; more for a backfill; and back
	// to the oldest rating still waiting for an outcome.
	from := endT.AddDate(0, 0, -400)
	if backfilled == 0 {
		from = from.AddDate(0, 0, -rc.BackfillDays*7/5-20)
	}
	pending, err := pendingDates(db, market, len(hs))
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		if t, err := time.Parse("2006-01-02", pending[0]); err == nil && t.Before(from) {
			from = t.AddDate(0, 0, -10)
		}
	}

	var p *panel
	var rev map[string]map[string]float64
	if market == "TW" {
		p, err = loadTW(db, cfg, from.Format("2006-01-02"))
		if err == nil {
			rev, err = revenueYoY(db, from.Format("2006-01-02"))
		}
	} else {
		p, err = loadUS(db, cfg, from.Format("2006-01-02"))
	}
	if err != nil {
		return err
	}
	if len(p.dates) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Today's ratings.
	last := len(p.dates) - 1
	var exists int
	tx.QueryRow(`SELECT COUNT(*) FROM state.ratings WHERE market = ? AND made_on = ?`, market, p.dates[last]).Scan(&exists)
	newRows := 0
	if exists == 0 {
		rs := rate(p, rc, last, rev, false)
		if err := insertRatings(tx, rs); err != nil {
			return err
		}
		newRows += len(rs)
		log.Printf("ratings %s: rated %d stocks on %s", market, len(rs), p.dates[last])
	}

	// Past days, once: every BackfillStep-th day before the first live rating.
	if backfilled == 0 {
		var first sql.NullString
		tx.QueryRow(`SELECT MIN(made_on) FROM state.ratings WHERE market = ? AND backfill = 0`, market).Scan(&first)
		top := last
		if i, ok := p.idx[first.String]; ok {
			top = i
		}
		step := max(rc.BackfillStep, 1)
		n := 0
		for t := top - step; t >= 0 && t > top-rc.BackfillDays; t -= step {
			rs := rate(p, rc, t, rev, true)
			if len(rs) == 0 {
				continue
			}
			if err := insertRatings(tx, rs); err != nil {
				return err
			}
			n += len(rs)
		}
		log.Printf("ratings %s: backfilled %d ratings", market, n)
	}

	// Outcomes for every rating whose horizon has now passed.
	pending, err = pendingDatesTx(tx, market, len(hs))
	if err != nil {
		return err
	}
	scored := 0
	for _, d := range pending {
		n, err := score(tx, p, d, hs)
		if err != nil {
			return err
		}
		scored += n
	}
	if _, err := tx.Exec(`INSERT INTO state.rating_runs (market, data_end, data_rows, backfilled) VALUES (?, ?, ?, 1)
		ON CONFLICT (market) DO UPDATE SET data_end = excluded.data_end, data_rows = excluded.data_rows, backfilled = 1`,
		market, end, rows); err != nil {
		return err
	}
	log.Printf("ratings %s: scored %d outcomes", market, scored)
	return tx.Commit()
}

type querier interface {
	Query(string, ...any) (*sql.Rows, error)
}

func pendingDates(db *sql.DB, market string, nh int) ([]string, error) {
	return pendingDatesTx(db, market, nh)
}

// pendingDatesTx lists, oldest first, rating days that still miss an outcome.
func pendingDatesTx(q querier, market string, nh int) ([]string, error) {
	rows, err := q.Query(`SELECT r.made_on, r.n, COALESCE(o.n, 0) FROM
		(SELECT made_on, COUNT(*) AS n FROM state.ratings WHERE market = ? GROUP BY made_on) r
		LEFT JOIN (SELECT made_on, COUNT(*) AS n FROM state.rating_outcomes WHERE market = ? GROUP BY made_on) o
		ON o.made_on = r.made_on ORDER BY r.made_on`, market, market)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		var nr, no int
		if err := rows.Scan(&d, &nr, &no); err != nil {
			return nil, err
		}
		if no < nr*nh {
			out = append(out, d)
		}
	}
	return out, rows.Err()
}

func null(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return f
}

func insertRatings(tx *sql.Tx, rs []Rating) error {
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO state.ratings (market, ticker, made_on, backfill,
		short_rating, short_score, long_rating, long_score, high, month_ret, week_ret, year_ret, ma200, rev_yoy)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rs {
		bf := 0
		if r.Backfill {
			bf = 1
		}
		if _, err := stmt.Exec(r.Market, r.Ticker, r.Date, bf, r.Short, null(r.ShortScore), r.Long, null(r.LongScore),
			null(r.High), null(r.Month), null(r.Week), null(r.Year), null(r.MA), null(r.RevYoY)); err != nil {
			return err
		}
	}
	return nil
}

// score fills the outcomes of the ratings made on d for every horizon that
// has passed. A return runs from the close of the next trading day (time to
// act on the rating) to the close `horizon` trading days after it.
func score(tx *sql.Tx, p *panel, d string, hs []int) (int, error) {
	i, ok := p.idx[d]
	if !ok {
		return 0, nil
	}
	have := map[string]bool{}
	var tickers []string
	rows, err := tx.Query(`SELECT r.ticker, o.horizon FROM state.ratings r
		LEFT JOIN state.rating_outcomes o ON o.market = r.market AND o.ticker = r.ticker AND o.made_on = r.made_on
		WHERE r.market = ? AND r.made_on = ?`, p.market, d)
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	for rows.Next() {
		var t string
		var h sql.NullInt64
		if err := rows.Scan(&t, &h); err != nil {
			rows.Close()
			return 0, err
		}
		if !seen[t] {
			seen[t] = true
			tickers = append(tickers, t)
		}
		if h.Valid {
			have[fmt.Sprintf("%s/%d", t, h.Int64)] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO state.rating_outcomes
		(market, ticker, made_on, horizon, start_on, end_on, ret, bench) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	n := 0
	bi, hasBench := p.row[p.bench]
	for _, h := range hs {
		start, end := i+1, i+1+h
		if end >= len(p.dates) {
			continue
		}
		bench := math.NaN()
		if hasBench {
			bench = change(p, bi, start, end)
		}
		for _, t := range tickers {
			if have[fmt.Sprintf("%s/%d", t, h)] {
				continue
			}
			ret := math.NaN()
			if k, ok := p.row[t]; ok {
				ret = change(p, k, start, end)
			}
			if _, err := stmt.Exec(p.market, t, d, h, p.dates[start], p.dates[end], null(ret), null(bench)); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// change is a ticker's adjusted return from the close of day a to the close
// of day b. A stock that stopped trading in between (a halt, a delisting)
// keeps its last close.
func change(p *panel, i, a, b int) float64 {
	x := p.at(i, a, staleDays)
	y := p.at(i, b, b-a+staleDays)
	if math.IsNaN(x) || math.IsNaN(y) || x <= 0 {
		return math.NaN()
	}
	return y/x - 1
}
