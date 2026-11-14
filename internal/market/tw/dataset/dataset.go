// Package dataset is a small engine for Taiwan datasets that come one row
// per stock per trading day (issue #84): history from FinMind, one request
// per ticker over the whole range, and recent days from the exchanges' own
// daily reports, one request per exchange per day. A shared bookkeeping
// table records what has been fetched, so no request is repeated.
//
// A dataset package (margin, valuation, ...) describes its table, its
// FinMind dataset and its exchange reports; Run does the rest. Every part
// fails soft: an error is logged and the other parts still run.
package dataset

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/market/tw"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

const (
	UserAgent = "stocks_but_fr-radar/0.1 (+https://github.com/Antonio-1110/stocks_but_fr)"

	// The exchanges block clients that hit them faster than a few per 5s.
	// The gap is kept per host across every dataset in the process.
	DefaultGap = 3 * time.Second
	// How many calendar days the exchange reports look back. Older days are
	// FinMind's job.
	DefaultLookback = 10
	// A day counts as a trading day once the prices table holds this many
	// Taiwan rows for it. Exchange reports are only asked for such days:
	// some reports (TWSE MI_QFIIS) answer a weekend date with the last
	// trading day's figures, and skipping closed days saves requests.
	minPriceRows = 300
)

// Taiwan has no DST, so a fixed zone avoids depending on tzdata.
var taipei = time.FixedZone("Asia/Taipei", 8*60*60)

// ErrBlocked means TWSE answered with its "FOR SECURITY REASONS" page
// instead of data. It blocks an address for a while after a burst of
// requests, so the rest of the run leaves that exchange alone.
var ErrBlocked = errors.New("blocked by the exchange's firewall")

// Exchange is one exchange's daily report for a dataset.
type Exchange[T any] struct {
	Name  string                                        // fetch-log source, e.g. "twse"
	URL   string                                        // base URL; tests point it at a fake
	Query func(day time.Time) url.Values                // query string for one day
	Parse func(body []byte, day time.Time) ([]T, error) // nil rows = no report for that day
}

// Dataset describes one daily per-stock dataset.
type Dataset[T any] struct {
	Name         string // log prefix and fetch-log dataset key, e.g. "margin"
	Schema       string // CREATE TABLE IF NOT EXISTS ... for the data table
	FinMind      string // FinMind dataset name for the history backfill
	ParseFinMind func(body []byte) ([]T, error)
	Exchanges    []Exchange[T]
	Save         func(db *sql.DB, rows []T) error
}

// Runner fetches one dataset. Fields are exported so tests can use fakes.
type Runner[T any] struct {
	D        Dataset[T]
	Client   *http.Client // for the exchanges' reports
	FinMind  *tw.FinMind  // paced and budgeted with the run's other FinMind steps
	Gap      time.Duration
	Lookback int
	Now      func() time.Time
}

// New returns a runner with production pacing.
func New[T any](d Dataset[T], fm *tw.FinMind) *Runner[T] {
	return &Runner[T]{
		D:        d,
		Client:   &http.Client{Timeout: 60 * time.Second},
		FinMind:  fm,
		Gap:      DefaultGap,
		Lookback: DefaultLookback,
		Now:      time.Now,
	}
}

const fetchLog = `CREATE TABLE IF NOT EXISTS tw_dataset_fetches (
	dataset    TEXT NOT NULL,
	source     TEXT NOT NULL,  -- "finmind" (key: ticker) or an exchange (key: date)
	key        TEXT NOT NULL,
	rows       INTEGER NOT NULL,
	fetched_at TEXT NOT NULL,
	PRIMARY KEY (dataset, source, key)
)`

// Run backfills history from start, then fills recent days from the
// exchanges. Both parts run even if the other fails.
func (r *Runner[T]) Run(ctx context.Context, st *store.Store, start time.Time) error {
	for _, q := range []string{fetchLog, r.D.Schema} {
		if _, err := st.DB.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%s: schema: %w", r.D.Name, err)
		}
	}
	errBackfill := r.backfill(ctx, st, start)
	if errBackfill != nil {
		log.Printf("%s: backfill: %v", r.D.Name, errBackfill)
	}
	errDaily := r.daily(ctx, st)
	if errDaily != nil {
		log.Printf("%s: daily: %v", r.D.Name, errDaily)
	}
	return errors.Join(errBackfill, errDaily)
}

const srcFinMind = "finmind"

// backfill fetches the full history of each TW company not fetched before.
func (r *Runner[T]) backfill(ctx context.Context, st *store.Store, start time.Time) error {
	if r.FinMind == nil || r.D.FinMind == "" {
		return nil
	}
	companies, err := st.Companies(model.MarketTW)
	if err != nil {
		return err
	}
	if len(companies) == 0 {
		log.Printf("%s: no TW companies in the store yet; skipping FinMind backfill", r.D.Name)
		return nil
	}
	done, err := r.fetched(st.DB, srcFinMind)
	if err != nil {
		return err
	}
	var todo []model.Company
	for _, co := range companies {
		if done[co.Ticker] {
			continue
		}
		if !co.DelistedOn.IsZero() && co.DelistedOn.Before(start) {
			continue
		}
		todo = append(todo, co)
	}
	if len(todo) == 0 {
		return nil
	}
	log.Printf("%s: FinMind backfill: %d of %d tickers left, up to %d this run",
		r.D.Name, len(todo), len(companies), r.FinMind.Left())
	end := r.today().Format("2006-01-02")
	var failed []error
	for i, co := range todo {
		body, err := r.FinMind.Get(ctx, r.D.FinMind, map[string]string{
			"data_id":    co.Ticker,
			"start_date": start.Format("2006-01-02"),
			"end_date":   end,
		})
		var rows []T
		if err == nil {
			rows, err = r.D.ParseFinMind(body)
		}
		if errors.Is(err, tw.ErrStop) {
			// The run's budget being spent is the normal end of a backfill run.
			log.Printf("%s: FinMind budget or quota reached after %d tickers; resumes next run", r.D.Name, i)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.Printf("%s: FinMind %s: %v", r.D.Name, co.Ticker, err)
			failed = append(failed, err)
			if len(failed) >= 10 {
				return fmt.Errorf("giving up after %d errors: %w", len(failed), errors.Join(failed...))
			}
			continue
		}
		if err := r.D.Save(st.DB, rows); err != nil {
			return err
		}
		if err := r.mark(st.DB, srcFinMind, co.Ticker, len(rows)); err != nil {
			return err
		}
	}
	return nil
}

// daily fetches each exchange's report for every recent trading day not yet
// recorded.
func (r *Runner[T]) daily(ctx context.Context, st *store.Store) error {
	today := r.today()
	from := today.AddDate(0, 0, -r.Lookback)
	days, err := tradingDays(st.DB, from)
	if err != nil {
		return err
	}
	var errs []error
	for _, ex := range r.D.Exchanges {
		done, err := r.fetched(st.DB, ex.Name)
		if err != nil {
			return err
		}
		for _, d := range days {
			key := d.Format("2006-01-02")
			if done[key] {
				continue
			}
			rows, err := r.fetchExchange(ctx, ex, d)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				log.Printf("%s: %s %s: %v", r.D.Name, ex.Name, key, err)
				errs = append(errs, fmt.Errorf("%s %s: %w", ex.Name, key, err))
				if errors.Is(err, ErrBlocked) {
					break // retrying now only extends the block
				}
				continue
			}
			if err := r.D.Save(st.DB, rows); err != nil {
				return err
			}
			// Prices say the market was open, so an empty report for today
			// or yesterday is just not out yet; ask again next run. Older
			// empty days are recorded so they aren't asked forever.
			if len(rows) > 0 || d.Before(today.AddDate(0, 0, -1)) {
				if err := r.mark(st.DB, ex.Name, key, len(rows)); err != nil {
					return err
				}
			}
		}
	}
	return errors.Join(errs...)
}

// tradingDays returns the dates on or after from that the prices table
// shows as Taiwan trading days, oldest first.
func tradingDays(db *sql.DB, from time.Time) ([]time.Time, error) {
	rs, err := db.Query(`SELECT date FROM prices WHERE market = ? AND date >= ?
		GROUP BY date HAVING COUNT(*) >= ? ORDER BY date`,
		model.MarketTW, from.Format("2006-01-02"), minPriceRows)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []time.Time
	for rs.Next() {
		var s string
		if err := rs.Scan(&s); err != nil {
			return nil, err
		}
		d, err := time.ParseInLocation("2006-01-02", s, taipei)
		if err != nil {
			return nil, fmt.Errorf("prices date %q: %w", s, err)
		}
		out = append(out, d)
	}
	return out, rs.Err()
}

func (r *Runner[T]) fetchExchange(ctx context.Context, ex Exchange[T], day time.Time) ([]T, error) {
	u, err := url.Parse(ex.URL)
	if err != nil {
		return nil, err
	}
	u.RawQuery = ex.Query(day).Encode()
	if err := wait(ctx, u.Host, r.Gap); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if bytes.Contains(body, []byte("FOR SECURITY REASONS")) {
		return nil, ErrBlocked
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, body)
	}
	return ex.Parse(body, day)
}

// today is midnight of the current date in Taipei.
func (r *Runner[T]) today() time.Time {
	y, m, d := r.Now().In(taipei).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, taipei)
}

var (
	hostMu   sync.Mutex
	hostLast = map[string]time.Time{}
)

// wait keeps at least gap between requests to the same host, across every
// dataset in the process.
func wait(ctx context.Context, host string, gap time.Duration) error {
	hostMu.Lock()
	next := hostLast[host].Add(gap)
	now := time.Now()
	if next.Before(now) {
		next = now
	}
	hostLast[host] = next
	hostMu.Unlock()
	d := time.Until(next)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *Runner[T]) fetched(db *sql.DB, source string) (map[string]bool, error) {
	rs, err := db.Query(`SELECT key FROM tw_dataset_fetches WHERE dataset = ? AND source = ?`, r.D.Name, source)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[string]bool{}
	for rs.Next() {
		var k string
		if err := rs.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rs.Err()
}

func (r *Runner[T]) mark(db *sql.DB, source, key string, rows int) error {
	_, err := db.Exec(`INSERT OR REPLACE INTO tw_dataset_fetches (dataset, source, key, rows, fetched_at)
		VALUES (?, ?, ?, ?, ?)`, r.D.Name, source, key, rows, time.Now().UTC().Format(time.RFC3339))
	return err
}

// Helpers for dataset packages.

// FinMindData decodes FinMind's v4 envelope. An in-body 402 is tw.ErrStop.
func FinMindData[R any](body []byte) ([]R, error) {
	var env struct {
		Msg    string `json:"msg"`
		Status int    `json:"status"`
		Data   []R    `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode FinMind response: %w", err)
	}
	switch {
	case env.Status == http.StatusPaymentRequired:
		return nil, fmt.Errorf("%w: %s", tw.ErrStop, env.Msg)
	case env.Status != http.StatusOK:
		return nil, fmt.Errorf("FinMind status %d: %s", env.Status, env.Msg)
	}
	return env.Data, nil
}

// Upsert writes rows with one prepared statement in one transaction.
func Upsert[T any](db *sql.DB, query string, rows []T, args func(T) []any) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(query)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, row := range rows {
		if _, err := stmt.Exec(args(row)...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Date is a day as stored: midnight UTC, formatted "YYYY-MM-DD" on save.
func Date(day time.Time) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
}

// ROCDate formats a day the way TPEx's report URLs want it: "115/11/13".
func ROCDate(day time.Time) string {
	return fmt.Sprintf("%d/%02d/%02d", day.Year()-1911, day.Month(), day.Day())
}

// Int reads "1,234,567" or "-1,234". Blank, "-" or "--" is zero.
func Int(s string) (int64, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if s == "" || s == "--" || s == "-" {
		return 0, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad number %q", s)
	}
	return v, nil
}

// Float reads "12.34", "1,234.5" or "12.3%". Blank, "-", "--" or "N/A" is
// unknown (ok = false), stored as NULL.
func Float(s string) (v float64, ok bool, err error) {
	s = strings.TrimSuffix(strings.ReplaceAll(strings.TrimSpace(s), ",", ""), "%")
	switch s {
	case "", "-", "--", "N/A", "n/a":
		return 0, false, nil
	}
	v, err = strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false, fmt.Errorf("bad number %q", s)
	}
	return v, true, nil
}

// Cell turns a JSON cell (string or number) into text, for reports that mix
// them (TWSE sends some ratios as numbers).
type Cell string

func (c *Cell) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*c = Cell(s)
		return nil
	}
	if string(b) == "null" {
		*c = ""
		return nil
	}
	*c = Cell(b)
	return nil
}

// Table is the exchanges' common report shape.
type Table struct {
	Fields []string `json:"fields"`
	Data   [][]Cell `json:"data"`
}

// Columns finds each wanted header's index by exact match after removing
// spaces. Headers that repeat (TWSE's margin report has two "買進") are
// matched in order: the n-th occurrence of a name in want takes the n-th
// matching field.
func Columns(fields []string, want ...string) ([]int, error) {
	norm := func(s string) string { return strings.ReplaceAll(strings.TrimSpace(s), " ", "") }
	used := make([]bool, len(fields))
	out := make([]int, len(want))
	for i, w := range want {
		out[i] = -1
		for j, f := range fields {
			if !used[j] && norm(f) == norm(w) {
				out[i], used[j] = j, true
				break
			}
		}
		if out[i] < 0 {
			return nil, fmt.Errorf("no %q column in %q", w, fields)
		}
	}
	return out, nil
}
