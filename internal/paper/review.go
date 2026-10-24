package paper

// The monthly review (issue #75): each portfolio against the same money in the
// benchmark over the last full calendar month, so a strategy that falls
// behind gets looked at. `radar review` prints it as markdown; the daily run
// opens it as a GitHub issue once a month. The paper pages show the same
// comparison over the last 30 days.

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

// Compare is a portfolio against its benchmark over a window, from the marks
// in paper_equity, both after the costs of selling.
type Compare struct {
	OK          bool   // two marks with a benchmark in the window
	From, To    string // the marks compared
	Partial     bool   // the portfolio started inside the window: From is its first mark
	Return      float64
	BenchReturn float64
	Trades      int // fills in the window
}

// Behind: the portfolio did worse than the benchmark over the window.
func (c Compare) Behind() bool { return c.OK && c.Return < c.BenchReturn }

// Gap is the portfolio's return minus the benchmark's.
func (c Compare) Gap() float64 { return c.Return - c.BenchReturn }

// compare measures from the last mark before `before` to the last mark
// before `until` (dates as YYYY-MM-DD, both exclusive). A portfolio with no
// mark before `before` is measured from its first mark with a benchmark.
func compare(db *sql.DB, name, before, until string) (Compare, error) {
	var c Compare
	var endP, endB float64
	err := db.QueryRow(`SELECT date, liquidation, bench FROM paper_equity
		WHERE portfolio = ? AND date < ? AND bench IS NOT NULL ORDER BY date DESC LIMIT 1`, name, until).
		Scan(&c.To, &endP, &endB)
	if err == sql.ErrNoRows {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	var startP, startB float64
	err = db.QueryRow(`SELECT date, liquidation, bench FROM paper_equity
		WHERE portfolio = ? AND date < ? AND bench IS NOT NULL ORDER BY date DESC LIMIT 1`, name, before).
		Scan(&c.From, &startP, &startB)
	if err == sql.ErrNoRows {
		c.Partial = true
		err = db.QueryRow(`SELECT date, liquidation, bench FROM paper_equity
			WHERE portfolio = ? AND bench IS NOT NULL ORDER BY date LIMIT 1`, name).
			Scan(&c.From, &startP, &startB)
	}
	if err != nil {
		return c, err
	}
	if c.From >= c.To || startP <= 0 || startB <= 0 {
		return c, nil
	}
	c.OK = true
	c.Return, c.BenchReturn = endP/startP-1, endB/startB-1
	err = db.QueryRow(`SELECT COUNT(*) FROM paper_orders WHERE portfolio = ? AND status = 'filled'
		AND fill_date > ? AND fill_date <= ?`, name, c.From, c.To).Scan(&c.Trades)
	return c, err
}

// last30 is the comparison over the 30 calendar days to the newest mark.
func last30(db *sql.DB, name string) (Compare, error) {
	var last sql.NullString
	if err := db.QueryRow(`SELECT MAX(date) FROM paper_equity WHERE portfolio = ? AND bench IS NOT NULL`, name).
		Scan(&last); err != nil || !last.Valid {
		return Compare{}, err
	}
	end, err := time.Parse(dateLayout, last.String)
	if err != nil {
		return Compare{}, err
	}
	// Exclusive bounds: the mark on or before 30 days back, and the newest.
	return compare(db, name, end.AddDate(0, 0, -29).Format(dateLayout), end.AddDate(0, 0, 1).Format(dateLayout))
}

// Review is the `radar review` command: the report for the last full month
// in Taipei, on stdout. It prints nothing when no portfolio has a mark in
// that month.
func Review(_ context.Context, cfg config.Config, st *store.Store) error {
	return review(st.DB, cfg, os.Stdout, time.Now().In(taipei))
}

func review(db *sql.DB, cfg config.Config, w io.Writer, now time.Time) error {
	if err := migrate(db); err != nil {
		return err
	}
	thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	month := thisMonth.AddDate(0, -1, 0)
	prev := month.AddDate(0, -1, 0)
	bench := cfg.Paper.Benchmark

	var b strings.Builder
	fmt.Fprintf(&b, "# Paper review %s\n\n", month.Format("2006-01"))
	fmt.Fprintf(&b, "Each paper portfolio over %s, from the last close of the month before to the month's last close, "+
		"against the same money held in %s. Values are after selling everything, fees and tax included.\n\n",
		month.Format("January 2006"), bench)
	fmt.Fprintf(&b, "| Strategy | Month | %s | Gap | Prev. month gap | Since start | %s since start | Trades |\n", bench, bench)
	b.WriteString("|---|--:|--:|--:|--:|--:|--:|--:|\n")

	var behind, twice, waiting []string
	for _, name := range cfg.Paper.Strategies {
		title := titles[name].title
		if title == "" {
			title = name
		}
		m, err := compare(db, name, month.Format(dateLayout), thisMonth.Format(dateLayout))
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if !m.OK {
			fmt.Fprintf(&b, "| %s | – | – | – | – | – | – | – |\n", title)
			waiting = append(waiting, title)
			continue
		}
		p, err := compare(db, name, prev.Format(dateLayout), month.Format(dateLayout))
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		all, err := compare(db, name, "0000-00-00", thisMonth.Format(dateLayout))
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		mark := ""
		if m.Behind() {
			mark = " **behind**"
			behind = append(behind, title)
			if p.Behind() {
				twice = append(twice, title)
			}
		}
		since := ""
		if m.Partial {
			since = fmt.Sprintf(" (from %s)", m.From)
		}
		prevGap := "–"
		if p.OK {
			prevGap = pts(p.Gap())
		}
		fmt.Fprintf(&b, "| %s%s | %s%s | %s | %s | %s | %s | %s | %d |\n", title, mark,
			pctS(m.Return), since, pctS(m.BenchReturn), pts(m.Gap()), prevGap,
			pctS(all.Return), pctS(all.BenchReturn), m.Trades)
	}

	if len(waiting) == len(cfg.Paper.Strategies) {
		return nil // nothing traded through the month: no report, so no issue
	}
	b.WriteString("\n")
	if len(behind) == 0 {
		fmt.Fprintf(&b, "No portfolio fell behind %s this month.\n", bench)
	} else {
		fmt.Fprintf(&b, "**Behind %s this month:** %s.\n", bench, strings.Join(behind, ", "))
		if len(twice) > 0 {
			fmt.Fprintf(&b, "\n**Behind two months running:** %s.\n", strings.Join(twice, ", "))
		}
	}
	if len(waiting) > 0 {
		fmt.Fprintf(&b, "\nNot measured yet (no mark with %s before the month's end): %s.\n", bench, strings.Join(waiting, ", "))
	}
	b.WriteString("\nA month is noisy, and a strategy that barely traded (see Trades) or started mid-month says little yet. " +
		"Each one behind gets a look: a rule change that backtests better in both periods, or a proposal to retire it. " +
		"Retiring a portfolio needs the owner's OK; its record stays in the state database.\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func pctS(v float64) string { return fmt.Sprintf("%+.2f%%", v*100) }

// pts is a return difference in percentage points.
func pts(v float64) string { return fmt.Sprintf("%+.2f pts", v*100) }
