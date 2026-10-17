// Package revenue collects Taiwan monthly revenue (月營收) from FinMind's
// TaiwanStockMonthRevenue dataset for every company in the universe,
// delisted ones included, back to run.history_start.
//
// MOPS/TWSE would publish the newest month a few hours earlier, but FinMind
// picks it up within a day, which is fine for a daily run.
package revenue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/market/tw"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

// A cached response younger than this is reused instead of refetched.
// Pacing and the per-run request budget come from the shared FinMind client
// (tw.ForStep); a full backfill takes several runs, each resuming from what
// the database holds.
const cacheTTL = 20 * time.Hour

// checksTable records when each ticker was last fetched and how many rows came
// back. Without it a ticker FinMind has no revenue for (most companies
// delisted before history_start) looks unfetched forever and is asked again
// every run ahead of everything else (issue #42). It is this collector's
// bookkeeping, not part of the shared schema.
const checksTable = `CREATE TABLE IF NOT EXISTS tw_revenue_checks (
	ticker     TEXT PRIMARY KEY,
	checked_on TEXT NOT NULL,
	rows       INTEGER NOT NULL
)`

// revisionsTable keeps every figure FinMind gives for a month that differs
// from the one stored (issue #64). monthly_revenue keeps the figure as first
// stored, so a backtest never uses a revision before it was published; this
// table records when each revision was first seen.
const revisionsTable = `CREATE TABLE IF NOT EXISTS tw_revenue_revisions (
	ticker         TEXT NOT NULL,
	month          TEXT NOT NULL,
	stored_revenue REAL NOT NULL,
	new_revenue    REAL NOT NULL,
	seen_on        TEXT NOT NULL,
	PRIMARY KEY (ticker, month, new_revenue)
)`

// A listed ticker FinMind returned no revenue for is asked again after this
// many days; a delisted one is not asked again.
const emptyRecheckDays = 7

// Collect is the `radar collect` step.
func Collect(ctx context.Context, cfg config.Config, st *store.Store) error {
	start, err := time.Parse("2006-01-02", cfg.Run.HistoryStart)
	if err != nil {
		return fmt.Errorf("run.history_start: %w", err)
	}
	c := collector{
		st:       st,
		finmind:  tw.ForStep("tw-revenue", 1), // tw-flows runs after this step
		cacheDir: filepath.Join(filepath.Dir(cfg.Run.DBPath), "cache", "tw-revenue"),
		start:    start,
		now:      time.Now(),
	}
	return c.run(ctx)
}

type collector struct {
	st       *store.Store
	finmind  *tw.FinMind
	cacheDir string
	start    time.Time // first revenue month to store
	now      time.Time
}

func (c *collector) run(ctx context.Context) error {
	for _, q := range []string{checksTable, revisionsTable} {
		if _, err := c.st.DB.Exec(q); err != nil {
			return err
		}
	}
	todo, err := c.todo()
	if err != nil {
		return err
	}
	if len(todo) == 0 {
		log.Printf("tw-revenue: nothing to fetch (empty universe or up to date)")
		return nil
	}
	fetched, failed, stored, revised := 0, 0, 0, 0
	for i, t := range todo {
		rows, hit, err := c.load(ctx, t)
		if errors.Is(err, errQuota) {
			log.Printf("tw-revenue: FinMind budget or quota reached, %d tickers left for next run", len(todo)-i)
			break
		}
		if !hit {
			fetched++
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("tw-revenue: %s: %v (skipped)", t.ticker, err)
			failed++
			continue
		}
		// Every month in the response is checked for revisions; only months
		// from storeFrom on are written, the year before is the YoY base.
		recs := toModel(rows, c.start)
		fresh, n, err := c.newMonths(t.ticker, recs)
		if err != nil {
			return err
		}
		fresh = slices.DeleteFunc(fresh, func(r model.MonthlyRevenue) bool { return r.Month.Before(t.storeFrom) })
		if err := c.st.UpsertMonthlyRevenue(fresh); err != nil {
			return err
		}
		if _, err := c.st.DB.Exec(`INSERT OR REPLACE INTO tw_revenue_checks (ticker, checked_on, rows) VALUES (?, ?, ?)`,
			t.ticker, c.now.Format("2006-01-02"), len(recs)); err != nil {
			return err
		}
		stored += len(fresh)
		revised += n
	}
	log.Printf("tw-revenue: %d requests, %d failed, %d rows stored, %d revisions recorded", fetched, failed, stored, revised)
	return nil
}

// newMonths returns the records for months not stored yet. A stored month is
// never overwritten: when the fetched figure differs, it is recorded in
// tw_revenue_revisions instead, once per distinct figure (n counts new ones).
func (c *collector) newMonths(ticker string, recs []model.MonthlyRevenue) (fresh []model.MonthlyRevenue, n int, err error) {
	rows, err := c.st.DB.Query(`SELECT month, revenue FROM monthly_revenue WHERE market = ? AND ticker = ?`,
		model.MarketTW, ticker)
	if err != nil {
		return nil, 0, err
	}
	have := map[string]sql.NullFloat64{}
	for rows.Next() {
		var m string
		var v sql.NullFloat64
		if err := rows.Scan(&m, &v); err != nil {
			rows.Close()
			return nil, 0, err
		}
		have[m] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	for _, r := range recs {
		old, ok := have[r.Month.Format("2006-01-02")]
		if !ok {
			fresh = append(fresh, r)
			continue
		}
		if !old.Valid || math.IsNaN(r.Revenue) || math.Abs(r.Revenue-old.Float64) < 0.5 {
			continue
		}
		res, err := c.st.DB.Exec(`INSERT OR IGNORE INTO tw_revenue_revisions
			(ticker, month, stored_revenue, new_revenue, seen_on) VALUES (?, ?, ?, ?, ?)`,
			ticker, r.Month.Format("2006-01-02"), old.Float64, r.Revenue, c.now.Format("2006-01-02"))
		if err != nil {
			return nil, 0, err
		}
		if k, _ := res.RowsAffected(); k > 0 {
			n++
			log.Printf("tw-revenue: %s %s revised: %.0f stored, %.0f now (kept the stored figure)",
				ticker, r.Month.Format("2006-01"), old.Float64, r.Revenue)
		}
	}
	return fresh, n, nil
}

type target struct {
	ticker    string
	from      time.Time // FinMind start_date to request
	storeFrom time.Time // first revenue month to write
}

// todo lists the tickers to fetch: listed ones with no revenue yet first, then
// delisted ones with none, then stale ones. Tickers already holding last
// month's figure, and delisted ones with any data, are skipped, as are ETFs
// (tickers starting "00", which report no revenue), companies delisted before
// the first month stored, delisted tickers already fetched once, and listed
// ones that came back empty in the last emptyRecheckDays.
func (c *collector) todo() ([]target, error) {
	companies, err := c.st.Companies(model.MarketTW)
	if err != nil {
		return nil, err
	}
	latest, err := latestMonths(c.st.DB)
	if err != nil {
		return nil, err
	}
	checks, err := loadChecks(c.st.DB)
	if err != nil {
		return nil, err
	}
	recheck := c.now.AddDate(0, 0, -emptyRecheckDays).Format("2006-01-02")
	lastMonth := firstOfMonth(c.now).AddDate(0, -1, 0)
	// YoY needs the 12 months before the first stored month. FinMind dates
	// are one month after the revenue month, hence -11 rather than -12.
	start := firstOfMonth(c.start)

	var fresh, gone, stale []target
	for _, co := range companies {
		if strings.HasPrefix(co.Ticker, "00") {
			continue
		}
		have, ok := latest[co.Ticker]
		delisted := !co.DelistedOn.IsZero()
		checked, wasChecked := checks[co.Ticker]
		switch {
		case !ok && delisted && (co.DelistedOn.Before(start) || wasChecked):
			continue
		case !ok && delisted:
			gone = append(gone, target{co.Ticker, start.AddDate(0, -11, 0), start})
		case !ok && wasChecked && checked > recheck:
			continue
		case !ok:
			fresh = append(fresh, target{co.Ticker, start.AddDate(0, -11, 0), start})
		case delisted, !have.Before(lastMonth):
			continue
		default:
			// Fetch from the newest stored month (to see a revision of it,
			// #64) with the year before it as the base for YoY and MoM.
			stale = append(stale, target{co.Ticker, have.AddDate(0, -11, 0), have})
		}
	}
	sort.SliceStable(stale, func(i, j int) bool { return stale[i].from.Before(stale[j].from) })
	return append(append(fresh, gone...), stale...), nil
}

func loadChecks(db *sql.DB) (map[string]string, error) {
	rows, err := db.Query(`SELECT ticker, checked_on FROM tw_revenue_checks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var t, d string
		if err := rows.Scan(&t, &d); err != nil {
			return nil, err
		}
		out[t] = d
	}
	return out, rows.Err()
}

func latestMonths(db *sql.DB) (map[string]time.Time, error) {
	rows, err := db.Query(`SELECT ticker, MAX(month) FROM monthly_revenue WHERE market = ? GROUP BY ticker`, model.MarketTW)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var ticker, month string
		if err := rows.Scan(&ticker, &month); err != nil {
			return nil, err
		}
		if m, err := time.Parse("2006-01-02", month); err == nil {
			out[ticker] = m
		}
	}
	return out, rows.Err()
}

// load returns a ticker's rows from the on-disk cache when fresh (hit=true),
// otherwise from FinMind.
func (c *collector) load(ctx context.Context, t target) (rows []finmindRow, hit bool, err error) {
	path := filepath.Join(c.cacheDir, fmt.Sprintf("%s_%s.json", t.ticker, t.from.Format("2006-01")))
	if fi, err := os.Stat(path); err == nil && c.now.Sub(fi.ModTime()) < cacheTTL {
		if body, err := os.ReadFile(path); err == nil {
			if rows, err := parseFinMind(body); err == nil {
				return rows, true, nil
			}
		}
	}
	body, err := c.finmind.Get(ctx, "TaiwanStockMonthRevenue", map[string]string{
		"data_id":    t.ticker,
		"start_date": t.from.Format("2006-01-02"),
	})
	if err != nil {
		return nil, false, err
	}
	rows, err = parseFinMind(body)
	if err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(c.cacheDir, 0o755); err == nil {
		if err := os.WriteFile(path, body, 0o644); err != nil {
			log.Printf("tw-revenue: cache %s: %v", path, err)
		}
	}
	return rows, false, nil
}

// toModel turns FinMind rows into MonthlyRevenue records from start onward,
// computing YoY and MoM from the rows themselves. A percentage whose base
// month is missing or zero is NaN, which SQLite stores as NULL.
func toModel(rows []finmindRow, start time.Time) []model.MonthlyRevenue {
	byMonth := map[time.Time]finmindRow{}
	for _, r := range rows {
		if r.RevenueYear == 0 || r.RevenueMonth < 1 || r.RevenueMonth > 12 {
			continue
		}
		byMonth[time.Date(r.RevenueYear, time.Month(r.RevenueMonth), 1, 0, 0, 0, 0, time.UTC)] = r
	}
	start = firstOfMonth(start)
	var out []model.MonthlyRevenue
	for m, r := range byMonth {
		if m.Before(start) {
			continue
		}
		rec := model.MonthlyRevenue{
			Market:      model.MarketTW,
			Ticker:      r.StockID,
			Month:       m,
			Revenue:     r.Revenue,
			YoYPct:      pctChange(r.Revenue, byMonth, m.AddDate(-1, 0, 0)),
			MoMPct:      pctChange(r.Revenue, byMonth, m.AddDate(0, -1, 0)),
			AnnouncedOn: announcedOn(m),
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Month.Before(out[j].Month) })
	return out
}

func pctChange(v float64, byMonth map[time.Time]finmindRow, base time.Time) float64 {
	b, ok := byMonth[base]
	if !ok || b.Revenue == 0 {
		return math.NaN()
	}
	return (v/b.Revenue - 1) * 100
}

// announcedOn is when a month's revenue is assumed public. FinMind does not
// give the filing date, so this uses the legal deadline: companies must file
// by the 10th of the following month (most file earlier). Using the deadline
// is conservative: a backtest never sees a figure before it could have been
// known. If the 10th is a holiday the deadline moves to the next business
// day, so the backtester should act on the first trading day after this date.
func announcedOn(month time.Time) time.Time {
	return firstOfMonth(month).AddDate(0, 1, 9)
}

func firstOfMonth(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}
