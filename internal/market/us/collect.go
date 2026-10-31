// Package us tracks a short list of large US companies (issue #2): daily
// prices from Yahoo's chart API into their own tables, and the dashboard's
// US page. The Taiwan tables, paper portfolios and backtester don't see them.
package us

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

const schema = `
CREATE TABLE IF NOT EXISTS us_companies (
	ticker     TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	exchange   TEXT NOT NULL DEFAULT '',
	currency   TEXT NOT NULL DEFAULT '',
	kind       TEXT NOT NULL DEFAULT '',  -- Yahoo's instrumentType: EQUITY, ETF
	benchmark  INTEGER NOT NULL DEFAULT 0 -- 1 for [us] benchmarks
);
CREATE TABLE IF NOT EXISTS us_prices (
	ticker    TEXT NOT NULL,
	date      TEXT NOT NULL,              -- YYYY-MM-DD, New York
	open      REAL, high REAL, low REAL,
	close     REAL,                       -- split-adjusted (as Yahoo serves it)
	adj_close REAL,                       -- also dividend-adjusted
	volume    INTEGER,                    -- shares
	PRIMARY KEY (ticker, date)
);
CREATE TABLE IF NOT EXISTS us_fetches (
	ticker     TEXT PRIMARY KEY,
	fetched_at TEXT NOT NULL              -- RFC 3339 UTC
);
`

const (
	// The daily workflow runs hourly. A ticker is fetched only once a session
	// it doesn't have has closed, and at most this often (holidays).
	minRefetch = 6 * time.Hour
	// An incremental fetch starts this many days before the newest stored bar.
	overlapDays = 10
)

// Collect is the `radar collect` step for US prices.
func Collect(ctx context.Context, cfg config.Config, st *store.Store) error {
	return collect(ctx, cfg.US, st.DB, newYahoo(), time.Now())
}

func collect(ctx context.Context, cfg config.US, db *sql.DB, y *yahoo, now time.Time) error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	start, err := time.ParseInLocation("2006-01-02", cfg.HistoryStart, newYork)
	if err != nil {
		return fmt.Errorf("[us] history_start: %w", err)
	}
	session := lastSession(now)
	type item struct {
		ticker    string
		benchmark bool
	}
	var items []item
	seen := map[string]bool{}
	for i, list := range [][]string{cfg.Benchmarks, cfg.Tickers} {
		for _, t := range list {
			t = strings.ToUpper(strings.TrimSpace(t))
			if t == "" || seen[t] {
				continue
			}
			seen[t] = true
			items = append(items, item{t, i == 0})
		}
	}
	fetched, skipped, failed := 0, 0, 0
	for _, it := range items {
		last, lastFetch, err := status(db, it.ticker)
		if err != nil {
			return err
		}
		if last >= session || (!lastFetch.IsZero() && now.Sub(lastFetch) < minRefetch) {
			skipped++
			continue
		}
		err = fetchOne(ctx, db, y, it.ticker, it.benchmark, last, start, session, now)
		if errors.Is(err, errRateLimited) || ctx.Err() != nil {
			log.Printf("us-prices: stopping: %v", err)
			failed += len(items) - fetched - skipped - failed
			break
		}
		if err != nil {
			// Remember the attempt too, so a bad symbol isn't asked for every hour.
			log.Printf("us-prices: %s: %v (skipped)", it.ticker, err)
			if err := markFetched(db, it.ticker, now); err != nil {
				return err
			}
			failed++
			continue
		}
		fetched++
	}
	log.Printf("us-prices: %d tickers fetched, %d already current, %d failed (latest session %s)",
		fetched, skipped, failed, session)
	if failed > 0 && fetched == 0 && skipped == 0 {
		return fmt.Errorf("all %d US tickers failed", failed)
	}
	return nil
}

// fetchOne updates one ticker. A new ticker gets its full history; a known
// one gets the days since its newest bar, unless a dividend or split falls in
// that window, which rescales every older close: then it is refetched in full.
func fetchOne(ctx context.Context, db *sql.DB, y *yahoo, ticker string, benchmark bool, last string, start time.Time, session string, now time.Time) error {
	full := last == ""
	from := start
	if !full {
		d, _ := time.ParseInLocation("2006-01-02", last, newYork)
		from = d.AddDate(0, 0, -overlapDays)
	}
	c, err := y.chart(ctx, ticker, from)
	if err != nil {
		return err
	}
	if !full && c.Events > 0 {
		full = true
		if c, err = y.chart(ctx, ticker, start); err != nil {
			return err
		}
	}
	// A bar after the last closed session is today's, still trading.
	bars := c.Bars[:0]
	for _, b := range c.Bars {
		if b.Date <= session {
			bars = append(bars, b)
		}
	}
	if len(bars) == 0 {
		return errors.New("no bars")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if full {
		if _, err := tx.Exec(`DELETE FROM us_prices WHERE ticker = ?`, ticker); err != nil {
			return err
		}
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO us_prices (ticker, date, open, high, low, close, adj_close, volume)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, b := range bars {
		if _, err := stmt.Exec(ticker, b.Date, b.Open, b.High, b.Low, b.Close, b.AdjClose, b.Volume); err != nil {
			return err
		}
	}
	bm := 0
	if benchmark {
		bm = 1
	}
	if _, err := tx.Exec(`INSERT INTO us_companies (ticker, name, exchange, currency, kind, benchmark)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (ticker) DO UPDATE SET name = excluded.name, exchange = excluded.exchange,
			currency = excluded.currency, kind = excluded.kind, benchmark = excluded.benchmark`,
		ticker, c.Name, c.Exchange, c.Currency, c.Kind, bm); err != nil {
		return err
	}
	if err := markFetched(tx, ticker, now); err != nil {
		return err
	}
	return tx.Commit()
}

func markFetched(db interface {
	Exec(string, ...any) (sql.Result, error)
}, ticker string, now time.Time) error {
	_, err := db.Exec(`INSERT OR REPLACE INTO us_fetches (ticker, fetched_at) VALUES (?, ?)`,
		ticker, now.UTC().Format(time.RFC3339))
	return err
}

// status is a ticker's newest stored date ("" if none) and last fetch time.
func status(db *sql.DB, ticker string) (string, time.Time, error) {
	var last sql.NullString
	if err := db.QueryRow(`SELECT MAX(date) FROM us_prices WHERE ticker = ?`, ticker).Scan(&last); err != nil {
		return "", time.Time{}, err
	}
	var at string
	err := db.QueryRow(`SELECT fetched_at FROM us_fetches WHERE ticker = ?`, ticker).Scan(&at)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, err
	}
	t, _ := time.Parse(time.RFC3339, at)
	return last.String, t, nil
}

// lastSession is the newest weekday whose regular session (to 16:00 New
// York) has ended, with half an hour for the final prints. Holidays aren't
// known; on one, fetches simply find nothing new.
func lastSession(now time.Time) string {
	ny := now.In(newYork)
	d := time.Date(ny.Year(), ny.Month(), ny.Day(), 0, 0, 0, 0, newYork)
	if ny.Before(d.Add(16*time.Hour + 30*time.Minute)) {
		d = d.AddDate(0, 0, -1)
	}
	for d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
		d = d.AddDate(0, 0, -1)
	}
	return d.Format("2006-01-02")
}
