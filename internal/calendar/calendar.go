// Package calendar collects dated company events in Taiwan (法說會, ex-dividend
// and ex-rights days, shareholder meetings) from the exchanges' and MOPS's
// free official pages, and renders them as a calendar page next to the
// dashboard. Report deadlines are computed from the rules, not fetched.
//
// It makes no FinMind requests, so it never eats into the price backfill's
// budget. Each source is fetched at most once per refetchAfter.
package calendar

import (
	"database/sql"
	"time"
)

// Event kinds. The order here is the order the page lists them in a day.
const (
	KindConference = "conference" // 法說會
	KindExDividend = "exdiv"      // 除權息
	KindMeeting    = "meeting"    // 股東會
	KindDeadline   = "deadline"   // market-wide filing deadline
)

var kindOrder = map[string]int{KindConference: 0, KindExDividend: 1, KindMeeting: 2, KindDeadline: 3}

// Event is one dated item on the calendar. Ticker is empty for market-wide
// items such as filing deadlines.
type Event struct {
	Market  string
	Ticker  string
	Name    string
	Date    time.Time
	EndDate time.Time // zero unless the event spans several days
	Kind    string
	Title   string // short label, e.g. "除息" or "法說會 14:00"
	Detail  string // amounts, place, summary
	Source  string // which fetch wrote it
}

// Taiwan has no DST, so a fixed zone avoids depending on tzdata.
var taipei = time.FixedZone("Asia/Taipei", 8*60*60)

const dateLayout = "2006-01-02"

// The table lives with this package rather than internal/store: nothing else
// reads it, and the backtester has no use for it.
const schema = `
CREATE TABLE IF NOT EXISTS corporate_events (
	market   TEXT NOT NULL,
	ticker   TEXT NOT NULL,
	date     TEXT NOT NULL,
	kind     TEXT NOT NULL,
	title    TEXT NOT NULL,
	name     TEXT NOT NULL DEFAULT '',
	end_date TEXT NOT NULL DEFAULT '',
	detail   TEXT NOT NULL DEFAULT '',
	source   TEXT NOT NULL,
	PRIMARY KEY (market, ticker, date, kind, title)
);
CREATE TABLE IF NOT EXISTS calendar_fetches (
	source     TEXT PRIMARY KEY,
	fetched_at TEXT NOT NULL
);
`

func migrate(db *sql.DB) error {
	_, err := db.Exec(schema)
	return err
}

func fmtDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(dateLayout)
}

// replaceSource stores one source's latest snapshot. Its rows dated from
// `from` on are dropped first, so an event that was moved or cancelled
// doesn't linger; older rows stay as history.
func replaceSource(db *sql.DB, source string, from time.Time, events []Event) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM corporate_events WHERE source = ? AND date >= ?`, source, fmtDate(from)); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO corporate_events
		(market, ticker, date, kind, title, name, end_date, detail, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range events {
		if _, err := stmt.Exec(e.Market, e.Ticker, fmtDate(e.Date), e.Kind, e.Title, e.Name, fmtDate(e.EndDate), e.Detail, source); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// loadEvents returns stored events dated in [from, to], plus multi-day
// events that started earlier and are still running.
func loadEvents(db *sql.DB, market string, from, to time.Time) ([]Event, error) {
	rows, err := db.Query(`SELECT market, ticker, name, date, end_date, kind, title, detail, source
		FROM corporate_events
		WHERE market = ? AND date <= ? AND (date >= ? OR end_date >= ?)
		ORDER BY date, ticker`, market, fmtDate(to), fmtDate(from), fmtDate(from))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var d, end string
		if err := rows.Scan(&e.Market, &e.Ticker, &e.Name, &d, &end, &e.Kind, &e.Title, &e.Detail, &e.Source); err != nil {
			return nil, err
		}
		e.Date, _ = time.Parse(dateLayout, d)
		e.EndDate, _ = time.Parse(dateLayout, end)
		out = append(out, e)
	}
	return out, rows.Err()
}

func lastFetched(db *sql.DB, source string) (time.Time, error) {
	var s string
	err := db.QueryRow(`SELECT fetched_at FROM calendar_fetches WHERE source = ?`, source).Scan(&s)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	t, _ := time.Parse(time.RFC3339, s)
	return t, nil
}

func markFetched(db *sql.DB, source string, at time.Time) error {
	_, err := db.Exec(`INSERT OR REPLACE INTO calendar_fetches (source, fetched_at) VALUES (?, ?)`,
		source, at.UTC().Format(time.RFC3339))
	return err
}
