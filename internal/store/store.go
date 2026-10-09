// Package store owns the SQLite file that the Go pipeline writes and the
// Python backtester reads. The schema below is the contract between them.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Antonio-1110/stocks_but_fr/internal/model"
)

// Dates are stored as TEXT "YYYY-MM-DD"; an empty string means unknown.
const dateLayout = "2006-01-02"

const schema = `
CREATE TABLE IF NOT EXISTS companies (
	market        TEXT NOT NULL,
	ticker        TEXT NOT NULL,
	name          TEXT NOT NULL,
	industry      TEXT NOT NULL DEFAULT '',
	exchange      TEXT NOT NULL DEFAULT '',
	listed_on     TEXT NOT NULL DEFAULT '',
	delisted_on   TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (market, ticker)
);
CREATE TABLE IF NOT EXISTS prices (
	market    TEXT NOT NULL,
	ticker    TEXT NOT NULL,
	date      TEXT NOT NULL,
	open      REAL, high REAL, low REAL, close REAL, adj_close REAL,
	volume    INTEGER,
	turnover  REAL,
	PRIMARY KEY (market, ticker, date)
);
CREATE TABLE IF NOT EXISTS monthly_revenue (
	market       TEXT NOT NULL,
	ticker       TEXT NOT NULL,
	month        TEXT NOT NULL,
	revenue      REAL,
	yoy_pct      REAL,
	mom_pct      REAL,
	announced_on TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (market, ticker, month)
);
CREATE TABLE IF NOT EXISTS institutional_flows (
	market      TEXT NOT NULL,
	ticker      TEXT NOT NULL,
	date        TEXT NOT NULL,
	foreign_net INTEGER,
	trust_net   INTEGER,
	dealer_net  INTEGER,
	PRIMARY KEY (market, ticker, date)
);
`

type Store struct {
	DB *sql.DB
}

// Open creates the file and its directory if needed and applies the schema.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return &Store{DB: db}, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func fmtDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(dateLayout)
}

func parseDate(s string) time.Time {
	t, _ := time.Parse(dateLayout, s)
	return t
}

// upsert runs one statement per row inside a single transaction.
func upsert[T any](s *Store, query string, rows []T, args func(T) []any) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(query)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.Exec(args(r)...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) UpsertCompanies(rows []model.Company) error {
	return upsert(s, `INSERT OR REPLACE INTO companies
		(market, ticker, name, industry, exchange, listed_on, delisted_on)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, rows, func(c model.Company) []any {
		return []any{c.Market, c.Ticker, c.Name, c.Industry, c.Exchange, fmtDate(c.ListedOn), fmtDate(c.DelistedOn)}
	})
}

func (s *Store) UpsertPrices(rows []model.Price) error {
	return upsert(s, `INSERT OR REPLACE INTO prices
		(market, ticker, date, open, high, low, close, adj_close, volume, turnover)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, rows, func(p model.Price) []any {
		return []any{p.Market, p.Ticker, fmtDate(p.Date), p.Open, p.High, p.Low, p.Close, p.AdjClose, p.Volume, p.Turnover}
	})
}

func (s *Store) UpsertMonthlyRevenue(rows []model.MonthlyRevenue) error {
	return upsert(s, `INSERT OR REPLACE INTO monthly_revenue
		(market, ticker, month, revenue, yoy_pct, mom_pct, announced_on)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, rows, func(r model.MonthlyRevenue) []any {
		return []any{r.Market, r.Ticker, fmtDate(r.Month), r.Revenue, r.YoYPct, r.MoMPct, fmtDate(r.AnnouncedOn)}
	})
}

func (s *Store) UpsertFlows(rows []model.InstitutionalFlow) error {
	return upsert(s, `INSERT OR REPLACE INTO institutional_flows
		(market, ticker, date, foreign_net, trust_net, dealer_net)
		VALUES (?, ?, ?, ?, ?, ?)`, rows, func(f model.InstitutionalFlow) []any {
		return []any{f.Market, f.Ticker, fmtDate(f.Date), f.ForeignNet, f.TrustNet, f.DealerNet}
	})
}

// Companies returns every company in a market, listed and delisted.
func (s *Store) Companies(market string) ([]model.Company, error) {
	rows, err := s.DB.Query(`SELECT market, ticker, name, industry, exchange, listed_on, delisted_on
		FROM companies WHERE market = ? ORDER BY ticker`, market)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Company
	for rows.Next() {
		var c model.Company
		var listed, delisted string
		if err := rows.Scan(&c.Market, &c.Ticker, &c.Name, &c.Industry, &c.Exchange, &listed, &delisted); err != nil {
			return nil, err
		}
		c.ListedOn, c.DelistedOn = parseDate(listed), parseDate(delisted)
		out = append(out, c)
	}
	return out, rows.Err()
}

// LatestPriceDate returns the newest stored date for a ticker (zero if none),
// so collectors can resume a backfill where they stopped.
func (s *Store) LatestPriceDate(market, ticker string) (time.Time, error) {
	var d sql.NullString
	err := s.DB.QueryRow(`SELECT MAX(date) FROM prices WHERE market = ? AND ticker = ?`, market, ticker).Scan(&d)
	if err != nil || !d.Valid {
		return time.Time{}, err
	}
	return parseDate(d.String), nil
}
