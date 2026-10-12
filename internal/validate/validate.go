// Package validate checks the database before it is published and records
// what it holds (issue #44). `radar validate` runs after collect: hard checks
// that fail stop the run before the database is saved; soft checks are
// recorded for the data-health page. On a pass the manifest is stored in the
// snapshots table, so every copy of the database says which snapshot it is,
// and written next to the database as manifest.json for the release.
package validate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

const dateLayout = "2006-01-02"

// snapshotsTable keeps one row per published snapshot, newest last. It is
// this package's bookkeeping, not part of the shared schema.
const snapshotsTable = `CREATE TABLE IF NOT EXISTS snapshots (
	id        TEXT PRIMARY KEY, -- UTC time the snapshot was validated, RFC 3339
	code_sha  TEXT NOT NULL,    -- commit that collected it ('' if unknown)
	manifest  TEXT NOT NULL     -- the Manifest as JSON
)`

// A drop in a table's row count larger than this share, against the previous
// snapshot, is treated as data loss.
const maxRowDrop = 0.01

// tables whose row counts are recorded and must not shrink. (The calendar's
// corporate_events is left out: it replaces upcoming events on each fetch.)
var tables = []string{"companies", "prices", "monthly_revenue", "institutional_flows"}

// Manifest describes one snapshot of the database.
type Manifest struct {
	ID       string            `json:"id"`
	CodeSHA  string            `json:"code_sha"`
	Rows     map[string]int64  `json:"rows"`
	Latest   map[string]string `json:"latest"` // prices, benchmark, revenue_month, flows
	Coverage []Coverage        `json:"revenue_coverage"`
	Hard     []Check           `json:"hard_checks"`
	Soft     []Check           `json:"soft_checks"`
}

// Coverage is the share of listed common stocks with revenue for a month.
type Coverage struct {
	Month  string  `json:"month"`
	Have   int64   `json:"have"`
	Listed int64   `json:"listed"`
	Share  float64 `json:"share"`
}

// Check is one rule's outcome. Count is how many rows break it; a hard check
// passes only at zero.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Count  int64  `json:"count"`
	Detail string `json:"detail,omitempty"`
}

// ErrFailed means a hard check failed; the run must not publish.
var ErrFailed = errors.New("validate: hard checks failed")

// Run is the `radar validate` command.
func Run(ctx context.Context, cfg config.Config, st *store.Store) error {
	m, err := Validate(ctx, st.DB, cfg.Backtest.Benchmark, time.Now().UTC(), codeSHA())
	if err != nil && !errors.Is(err, ErrFailed) {
		return err
	}
	logChecks("FAIL", m.Hard)
	logChecks("warn", m.Soft)
	if err != nil {
		return err
	}
	body, jerr := json.MarshalIndent(m, "", "  ")
	if jerr != nil {
		return jerr
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO snapshots (id, code_sha, manifest) VALUES (?, ?, ?)`,
		m.ID, m.CodeSHA, string(body)); err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(cfg.Run.DBPath), "manifest.json")
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return err
	}
	log.Printf("validate: snapshot %s written to %s", m.ID, path)
	return nil
}

func logChecks(bad string, checks []Check) {
	for _, c := range checks {
		mark := "ok  "
		if !c.OK {
			mark = bad
		}
		log.Printf("validate: %s %s: %d %s", mark, c.Name, c.Count, c.Detail)
	}
}

// Validate builds the manifest for db at now and runs the checks against the
// previous snapshot. It returns ErrFailed (with the manifest) when a hard
// check fails, and stores nothing.
func Validate(ctx context.Context, db *sql.DB, benchmark string, now time.Time, sha string) (Manifest, error) {
	if _, err := db.ExecContext(ctx, snapshotsTable); err != nil {
		return Manifest{}, err
	}
	m := Manifest{ID: now.Format(time.RFC3339), CodeSHA: sha, Rows: map[string]int64{}, Latest: map[string]string{}}
	q := func(dst any, query string, args ...any) error {
		return db.QueryRowContext(ctx, query, args...).Scan(dst)
	}
	for _, t := range tables {
		var n int64
		if err := q(&n, `SELECT COUNT(*) FROM `+t); err != nil {
			return m, fmt.Errorf("count %s: %w", t, err)
		}
		m.Rows[t] = n
	}
	latest := map[string]string{
		"prices":        `SELECT COALESCE(MAX(date), '') FROM prices WHERE market = 'TW'`,
		"benchmark":     `SELECT COALESCE(MAX(date), '') FROM prices WHERE market = 'TW' AND ticker = ?`,
		"revenue_month": `SELECT COALESCE(MAX(month), '') FROM monthly_revenue WHERE market = 'TW'`,
		"flows":         `SELECT COALESCE(MAX(date), '') FROM institutional_flows WHERE market = 'TW'`,
	}
	for k, query := range latest {
		var s string
		var args []any
		if k == "benchmark" {
			args = []any{benchmark}
		}
		if err := q(&s, query, args...); err != nil {
			return m, fmt.Errorf("latest %s: %w", k, err)
		}
		m.Latest[k] = s
	}

	prev, err := previous(ctx, db)
	if err != nil {
		return m, err
	}
	if err := m.hard(q, prev, now); err != nil {
		return m, err
	}
	if err := m.soft(ctx, db, q, now); err != nil {
		return m, err
	}
	for _, c := range m.Hard {
		if !c.OK {
			return m, ErrFailed
		}
	}
	return m, nil
}

type queryFunc func(dst any, query string, args ...any) error

func (m *Manifest) hard(q queryFunc, prev *Manifest, now time.Time) error {
	rowChecks := []struct{ name, query string }{
		{"price close or adj_close missing or not positive",
			`SELECT COUNT(*) FROM prices WHERE close IS NULL OR close <= 0 OR adj_close IS NULL OR adj_close <= 0`},
		{"price high below low", `SELECT COUNT(*) FROM prices WHERE high < low`},
		{"price dated in the future", `SELECT COUNT(*) FROM prices WHERE date > ?`},
	}
	for _, rc := range rowChecks {
		var n int64
		var args []any
		if strings.Contains(rc.query, "?") {
			args = []any{now.AddDate(0, 0, 1).Format(dateLayout)}
		}
		if err := q(&n, rc.query, args...); err != nil {
			return fmt.Errorf("%s: %w", rc.name, err)
		}
		m.Hard = append(m.Hard, Check{Name: rc.name, OK: n == 0, Count: n})
	}
	if prev == nil {
		m.Hard = append(m.Hard, Check{Name: "compared with previous snapshot", OK: true, Detail: "none yet"})
		return nil
	}
	for _, t := range tables {
		was, cur := prev.Rows[t], m.Rows[t]
		c := Check{Name: t + " rows not lost", OK: true, Detail: fmt.Sprintf("%d, was %d in %s", cur, was, prev.ID)}
		if was > 0 && float64(was-cur)/float64(was) > maxRowDrop {
			c.OK, c.Count = false, was-cur
		}
		m.Hard = append(m.Hard, c)
	}
	for _, k := range []string{"prices", "revenue_month", "flows"} {
		was, cur := prev.Latest[k], m.Latest[k]
		c := Check{Name: "latest " + k + " not earlier", OK: cur >= was, Detail: fmt.Sprintf("%s, was %s", cur, was)}
		if !c.OK {
			c.Count = 1
		}
		m.Hard = append(m.Hard, c)
	}
	return nil
}

// listedCommon selects TW common stocks still listed; c is the companies alias.
const listedCommon = `c.market = 'TW' AND c.delisted_on = '' AND c.ticker GLOB '[1-9][0-9][0-9][0-9]'`

func (m *Manifest) soft(ctx context.Context, db *sql.DB, q queryFunc, now time.Time) error {
	recent := now.AddDate(0, 0, -30).Format(dateLayout)
	if latest := m.Latest["revenue_month"]; latest != "" {
		month, err := time.Parse(dateLayout, latest)
		if err != nil {
			return err
		}
		for _, mo := range []time.Time{month.AddDate(0, -1, 0), month} {
			c := Coverage{Month: mo.Format("2006-01")}
			var have sql.NullInt64
			err := db.QueryRowContext(ctx, `SELECT COUNT(*), SUM(EXISTS(SELECT 1 FROM monthly_revenue r
					WHERE r.market = 'TW' AND r.ticker = c.ticker AND r.month = ?))
				FROM companies c WHERE `+listedCommon+`
				AND EXISTS(SELECT 1 FROM prices p WHERE p.market = 'TW' AND p.ticker = c.ticker AND p.date >= ?)`,
				mo.Format(dateLayout), recent).Scan(&c.Listed, &have)
			if err != nil {
				return fmt.Errorf("revenue coverage: %w", err)
			}
			c.Have = have.Int64
			if c.Listed > 0 {
				c.Share = float64(c.Have) / float64(c.Listed)
			}
			m.Coverage = append(m.Coverage, c)
		}
	}

	stale := now.AddDate(0, 0, -14).Format(dateLayout)
	checks := []struct {
		name, query string
		args        []any
	}{
		{"open outside the day's low-high range",
			`SELECT COUNT(*) FROM prices WHERE high > 0 AND low > 0 AND (open > high OR open < low)`, nil},
		{"listed stocks with no price in 14 days",
			`SELECT COUNT(*) FROM companies c WHERE ` + listedCommon + `
			AND NOT EXISTS(SELECT 1 FROM prices p WHERE p.market = 'TW' AND p.ticker = c.ticker AND p.date >= ?)`,
			[]any{stale}},
		{"listed stocks with no revenue",
			`SELECT COUNT(*) FROM companies c WHERE ` + listedCommon + `
			AND NOT EXISTS(SELECT 1 FROM monthly_revenue r WHERE r.market = 'TW' AND r.ticker = c.ticker)`, nil},
		{"negative revenue rows", `SELECT COUNT(*) FROM monthly_revenue WHERE revenue < 0`, nil},
		{"companies with no listing date", `SELECT COUNT(*) FROM companies WHERE market = 'TW' AND listed_on = ''`, nil},
	}
	for _, sc := range checks {
		var n int64
		if err := q(&n, sc.query, sc.args...); err != nil {
			return fmt.Errorf("%s: %w", sc.name, err)
		}
		m.Soft = append(m.Soft, Check{Name: sc.name, OK: n == 0, Count: n})
	}
	for _, c := range m.Coverage {
		m.Soft = append(m.Soft, Check{Name: "revenue coverage " + c.Month, OK: c.Share >= 0.9, Count: c.Listed - c.Have,
			Detail: fmt.Sprintf("%d of %d listed stocks (%.0f%%)", c.Have, c.Listed, c.Share*100)})
	}
	bench := m.Latest["benchmark"]
	c := Check{Name: "benchmark up to date", OK: bench >= now.AddDate(0, 0, -5).Format(dateLayout), Detail: "latest " + bench}
	if !c.OK {
		c.Count = 1
	}
	m.Soft = append(m.Soft, c)
	return nil
}

// previous returns the newest stored snapshot, or nil if there is none.
func previous(ctx context.Context, db *sql.DB) (*Manifest, error) {
	var body string
	err := db.QueryRowContext(ctx, `SELECT manifest FROM snapshots ORDER BY id DESC LIMIT 1`).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return nil, fmt.Errorf("previous snapshot: %w", err)
	}
	return &m, nil
}

// codeSHA is the commit being run: GITHUB_SHA in Actions, else empty.
func codeSHA() string { return os.Getenv("GITHUB_SHA") }
