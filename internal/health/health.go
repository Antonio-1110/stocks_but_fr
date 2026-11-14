// Package health records how each `radar collect` step did on every run
// (issue #14), so a source that gets blocked or changes format shows up as
// failed instead of silently giving less data. `radar health` prints the
// latest run of each step and the newest date in each data table.
package health

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

// Schema is one row per step per run. It lives in the market database, so
// it travels with the data it describes.
const Schema = `CREATE TABLE IF NOT EXISTS source_runs (
	run_started TEXT NOT NULL,  -- UTC RFC 3339, the same for every step of one run
	step        TEXT NOT NULL,  -- collect step name, e.g. "tw-margin"
	ok          INTEGER NOT NULL,
	seconds     REAL NOT NULL,
	error       TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (run_started, step)
)`

// Record saves one step's outcome. It never fails the run: a write error
// is returned for logging only.
func Record(db *sql.DB, run time.Time, step string, started time.Time, stepErr error) error {
	if _, err := db.Exec(Schema); err != nil {
		return err
	}
	msg := ""
	if stepErr != nil {
		msg = stepErr.Error()
		msg = truncate(msg, 2000)
	}
	_, err := db.Exec(`INSERT OR REPLACE INTO source_runs (run_started, step, ok, seconds, error)
		VALUES (?, ?, ?, ?, ?)`, run.UTC().Format(time.RFC3339), step, stepErr == nil,
		time.Since(started).Seconds(), msg)
	return err
}

// StepStatus is one step's latest run and its streak.
type StepStatus struct {
	Step       string
	LastRun    string // run_started of the latest run
	OK         bool
	Seconds    float64
	Error      string
	LastOK     string // run_started of the latest successful run ("" if none)
	FailStreak int    // consecutive failed runs up to the latest
}

// Steps returns every recorded step's latest status, by step name.
func Steps(db *sql.DB) ([]StepStatus, error) {
	if _, err := db.Exec(Schema); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT step, run_started, ok, seconds, error FROM source_runs
		ORDER BY step, run_started DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StepStatus
	streakDone := false
	for rows.Next() {
		var step, run, msg string
		var ok bool
		var secs float64
		if err := rows.Scan(&step, &run, &ok, &secs, &msg); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Step != step {
			out = append(out, StepStatus{Step: step, LastRun: run, OK: ok, Seconds: secs, Error: msg})
			streakDone = false
		}
		s := &out[len(out)-1]
		if ok {
			if s.LastOK == "" {
				s.LastOK = run
			}
			streakDone = true
		} else if !streakDone {
			s.FailStreak++
		}
	}
	return out, rows.Err()
}

// Freshness is a data table's newest date and row count.
type Freshness struct {
	Table  string
	Latest string // "" if the table is missing or empty
	Rows   int64
}

// dataTables are the market tables and their date column. Tables a run
// hasn't created yet are reported as missing, not as an error.
var dataTables = []struct{ table, col string }{
	{"prices", "date"},
	{"monthly_revenue", "month"},
	{"institutional_flows", "date"},
	{"tw_margin", "date"},
	{"tw_valuations", "date"},
	{"tw_foreign_holdings", "date"},
	{"us_prices", "date"},
}

// Tables returns each data table's newest date and row count.
func Tables(db *sql.DB) ([]Freshness, error) {
	var out []Freshness
	for _, t := range dataTables {
		f := Freshness{Table: t.table}
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM main.sqlite_master WHERE type = 'table' AND name = ?`, t.table).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			var latest sql.NullString
			q := fmt.Sprintf(`SELECT MAX("%s"), COUNT(*) FROM "%s"`, t.col, t.table)
			if err := db.QueryRow(q).Scan(&latest, &f.Rows); err != nil {
				// A table whose shape differs (no such column) is reported, not fatal.
				f.Latest = "error: " + err.Error()
			} else {
				f.Latest = latest.String
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// Print is the `radar health` command: a Markdown report on stdout.
func Print(ctx context.Context, cfg config.Config, st *store.Store) error {
	return Write(os.Stdout, st.DB)
}

// Write renders the report as Markdown, failing steps first.
func Write(w io.Writer, db *sql.DB) error {
	steps, err := Steps(db)
	if err != nil {
		return err
	}
	tables, err := Tables(db)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("## Collect steps (latest run)\n\n| Step | Status | Last run | Last OK | Seconds | Error |\n|---|---|---|---|---|---|\n")
	if len(steps) == 0 {
		b.WriteString("| (no runs recorded yet) | | | | | |\n")
	}
	for _, failedFirst := range []bool{true, false} {
		for _, s := range steps {
			if s.OK == failedFirst {
				continue
			}
			status := "ok"
			if !s.OK {
				status = fmt.Sprintf("FAILED (%d in a row)", s.FailStreak)
			}
			lastOK := s.LastOK
			if lastOK == "" {
				lastOK = "never"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %.0f | %s |\n", s.Step, status, s.LastRun, lastOK, s.Seconds, cell(s.Error))
		}
	}
	b.WriteString("\n## Data tables\n\n| Table | Newest | Rows |\n|---|---|---|\n")
	for _, t := range tables {
		latest := t.Latest
		if latest == "" {
			latest = "missing or empty"
		}
		fmt.Fprintf(&b, "| %s | %s | %d |\n", t.Table, cell(latest), t.Rows)
	}
	_, err = io.WriteString(w, b.String())
	return err
}

// cell keeps an error message on one table row.
func cell(s string) string {
	s = strings.NewReplacer("\n", " ", "|", "/").Replace(s)
	return truncate(s, 200)
}

// truncate cuts s to at most n runes.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
