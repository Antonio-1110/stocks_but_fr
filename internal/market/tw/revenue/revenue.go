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
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

const (
	// FinMind allows 600 requests/hour with a token (300 without), so one
	// request every 6s stays under it. A full backfill of ~2,000 companies
	// takes several runs; each run resumes from what the database holds.
	requestInterval = 6 * time.Second
	maxRequestsRun  = 550
	// A cached response younger than this is reused instead of refetched.
	cacheTTL = 20 * time.Hour
)

// Collect is the `radar collect` step.
func Collect(ctx context.Context, cfg config.Config, st *store.Store) error {
	start, err := time.Parse("2006-01-02", cfg.Run.HistoryStart)
	if err != nil {
		return fmt.Errorf("run.history_start: %w", err)
	}
	token := os.Getenv("FINMIND_TOKEN")
	if token == "" {
		log.Printf("tw-revenue: FINMIND_TOKEN not set, using FinMind's lower anonymous limit")
	}
	c := collector{
		st:       st,
		client:   &client{http: &http.Client{Timeout: 60 * time.Second}, token: token},
		cacheDir: filepath.Join(filepath.Dir(cfg.Run.DBPath), "cache", "tw-revenue"),
		start:    start,
		now:      time.Now(),
		interval: requestInterval,
		budget:   maxRequestsRun,
	}
	return c.run(ctx)
}

type collector struct {
	st       *store.Store
	client   *client
	cacheDir string
	start    time.Time // first revenue month to store
	now      time.Time
	interval time.Duration
	budget   int
}

func (c *collector) run(ctx context.Context) error {
	todo, err := c.todo()
	if err != nil {
		return err
	}
	if len(todo) == 0 {
		log.Printf("tw-revenue: nothing to fetch (empty universe or up to date)")
		return nil
	}
	fetched, failed, stored := 0, 0, 0
	for i, t := range todo {
		if fetched >= c.budget {
			log.Printf("tw-revenue: request budget used, %d tickers left for next run", len(todo)-i)
			break
		}
		rows, hit, err := c.load(ctx, t)
		if !hit {
			fetched++
		}
		if errors.Is(err, errQuota) {
			log.Printf("tw-revenue: FinMind quota reached, stopping; resumes next run")
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("tw-revenue: %s: %v (skipped)", t.ticker, err)
			failed++
			continue
		}
		recs := toModel(rows, t.storeFrom)
		if err := c.st.UpsertMonthlyRevenue(recs); err != nil {
			return err
		}
		stored += len(recs)
	}
	log.Printf("tw-revenue: %d requests, %d failed, %d rows stored", fetched, failed, stored)
	return nil
}

type target struct {
	ticker    string
	from      time.Time // FinMind start_date to request
	storeFrom time.Time // first revenue month to write
}

// todo lists the tickers to fetch, those with no revenue yet first. Tickers
// already holding last month's figure, and delisted ones with any data, are
// skipped. ETFs (tickers starting "00") report no revenue and are skipped too.
func (c *collector) todo() ([]target, error) {
	companies, err := c.st.Companies(model.MarketTW)
	if err != nil {
		return nil, err
	}
	latest, err := latestMonths(c.st.DB)
	if err != nil {
		return nil, err
	}
	lastMonth := firstOfMonth(c.now).AddDate(0, -1, 0)
	// YoY needs the 12 months before the first stored month. FinMind dates
	// are one month after the revenue month, hence -11 rather than -12.
	start := firstOfMonth(c.start)

	var fresh, stale []target
	for _, co := range companies {
		if strings.HasPrefix(co.Ticker, "00") {
			continue
		}
		have, ok := latest[co.Ticker]
		switch {
		case !ok:
			fresh = append(fresh, target{co.Ticker, start.AddDate(0, -11, 0), start})
		case !co.DelistedOn.IsZero(), !have.Before(lastMonth):
			continue
		default:
			// Rewrite from the newest stored month (it may have been revised)
			// with the year before it fetched as the base for YoY and MoM.
			stale = append(stale, target{co.Ticker, have.AddDate(0, -11, 0), have})
		}
	}
	sort.SliceStable(stale, func(i, j int) bool { return stale[i].from.Before(stale[j].from) })
	return append(fresh, stale...), nil
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
// otherwise from FinMind, waiting out the request interval first.
func (c *collector) load(ctx context.Context, t target) (rows []finmindRow, hit bool, err error) {
	path := filepath.Join(c.cacheDir, fmt.Sprintf("%s_%s.json", t.ticker, t.from.Format("2006-01")))
	if fi, err := os.Stat(path); err == nil && c.now.Sub(fi.ModTime()) < cacheTTL {
		if body, err := os.ReadFile(path); err == nil {
			if rows, err := parseFinMind(body); err == nil {
				return rows, true, nil
			}
		}
	}
	select {
	case <-ctx.Done():
		return nil, false, ctx.Err()
	case <-time.After(c.interval):
	}
	body, err := c.client.fetch(ctx, t.ticker, t.from)
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
