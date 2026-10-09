package tw

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

var taipei = time.FixedZone("Asia/Taipei", 8*60*60)

// checksTable records when each ticker was last fetched, so a ticker FinMind
// has no prices for is tried once a day, and a delisted one only once.
// It is this collector's bookkeeping, not part of the shared schema.
const checksTable = `CREATE TABLE IF NOT EXISTS tw_price_checks (
	ticker     TEXT PRIMARY KEY,
	checked_on TEXT NOT NULL,
	rows       INTEGER NOT NULL
)`

// Collect is the `radar collect` step: refresh the universe, then backfill or
// update prices until everything is current or this run's budget is spent.
func Collect(ctx context.Context, cfg config.Config, st *store.Store) error {
	// Revenue and flows run after this step and share the FinMind budget.
	return collect(ctx, cfg, st, ForStep("tw-prices", 2), time.Now().In(taipei))
}

func collect(ctx context.Context, cfg config.Config, st *store.Store, c *FinMind, now time.Time) error {
	if _, err := st.DB.Exec(checksTable); err != nil {
		return err
	}
	if err := updateUniverse(ctx, cfg, st, c); err != nil {
		return err
	}
	companies, err := st.Companies(model.MarketTW)
	if err != nil {
		return err
	}
	// Splits come for the whole market in one request. Without them adjusted
	// closes would be wrong, so prices wait for the next run.
	splitRows, err := Fetch[splitRow](ctx, c, "TaiwanStockSplitPrice", map[string]string{"start_date": cfg.Run.HistoryStart})
	if err != nil {
		log.Printf("tw: splits unavailable, skipping prices this run: %v", err)
		return nil
	}
	splits := map[string][]splitRow{}
	for _, r := range splitRows {
		splits[r.StockID] = append(splits[r.StockID], r)
	}
	today := now.Format("2006-01-02")
	checked, err := loadChecks(st)
	if err != nil {
		return err
	}

	// The benchmark goes first; its newest date is the latest trading day.
	bench := cfg.Backtest.Benchmark
	if checked[bench] != today {
		if err := updateTicker(ctx, cfg, st, c, bench, today, splits[bench]); err != nil {
			if errors.Is(err, ErrStop) || ctx.Err() != nil {
				log.Printf("tw: stopping: %v", err)
				return nil
			}
			log.Printf("tw: %s: %v", bench, err)
		}
	}
	calendar, err := st.LatestPriceDate(model.MarketTW, bench)
	if err != nil {
		return err
	}
	if calendar.IsZero() {
		calendar = parseDay(today)
	}

	type job struct {
		ticker string
		latest time.Time
	}
	var jobs []job
	for _, co := range companies {
		if co.Ticker == bench || checked[co.Ticker] == today {
			continue
		}
		if !co.DelistedOn.IsZero() && checked[co.Ticker] != "" {
			continue // a delisted stock's history is complete after one fetch
		}
		latest, err := st.LatestPriceDate(model.MarketTW, co.Ticker)
		if err != nil {
			return err
		}
		if !latest.IsZero() && !latest.Before(calendar) {
			continue
		}
		jobs = append(jobs, job{co.Ticker, latest})
	}
	// Never-fetched tickers first (backfill), then the stalest.
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].latest.Before(jobs[j].latest) })

	done := 0
	for _, j := range jobs {
		err := updateTicker(ctx, cfg, st, c, j.ticker, today, splits[j.ticker])
		if errors.Is(err, ErrStop) || ctx.Err() != nil {
			log.Printf("tw: stopping: %v", err)
			break
		}
		if err != nil {
			log.Printf("tw: %s: %v (skipped)", j.ticker, err)
			continue
		}
		done++
	}
	log.Printf("tw: prices updated for %d of %d stale tickers; the rest resume next run", done, len(jobs))
	return nil
}

func updateUniverse(ctx context.Context, cfg config.Config, st *store.Store, c *FinMind) error {
	info, err := Fetch[stockInfo](ctx, c, "TaiwanStockInfo", nil)
	if err != nil {
		return fmt.Errorf("universe: %w", err)
	}
	delisted, err := Fetch[delisting](ctx, c, "TaiwanStockDelisting", nil)
	if err != nil {
		log.Printf("tw: delisting table unavailable, keeping stored delisting dates: %v", err)
	}
	universe := buildUniverse(info, delisted)

	stored, err := st.Companies(model.MarketTW)
	if err != nil {
		return err
	}
	prev := map[string]model.Company{}
	for _, co := range stored {
		prev[co.Ticker] = co
	}
	hasBench, nDelisted := false, 0
	for i := range universe {
		co := &universe[i]
		if delisted == nil && co.DelistedOn.IsZero() {
			co.DelistedOn = prev[co.Ticker].DelistedOn
		}
		if !co.DelistedOn.IsZero() {
			nDelisted++
		}
		hasBench = hasBench || co.Ticker == cfg.Backtest.Benchmark
	}
	if !hasBench {
		universe = append(universe, model.Company{Market: model.MarketTW, Ticker: cfg.Backtest.Benchmark,
			Name: cfg.Backtest.Benchmark, Exchange: "TWSE", Industry: "ETF"})
	}
	log.Printf("tw: universe %d stocks and ETFs, %d of them delisted", len(universe), nDelisted)
	return st.UpsertCompanies(universe)
}

// updateTicker fetches prices and adjustment events from the newest stored
// day (or from history_start) and writes them. The first fetched day overlaps
// the newest stored day: if its adjusted close changed, a dividend or split
// happened since, and all older adjusted closes are rescaled by the same
// ratio, which keeps the series exactly back-adjusted.
func updateTicker(ctx context.Context, cfg config.Config, st *store.Store, c *FinMind, ticker, today string, splits []splitRow) error {
	latest, err := st.LatestPriceDate(model.MarketTW, ticker)
	if err != nil {
		return err
	}
	from := cfg.Run.HistoryStart
	if !latest.IsZero() {
		from = latest.Format("2006-01-02")
	}
	params := map[string]string{"data_id": ticker, "start_date": from}
	raw, err := Fetch[priceRow](ctx, c, "TaiwanStockPrice", params)
	if err != nil {
		return err
	}
	divs, err := Fetch[dividendRow](ctx, c, "TaiwanStockDividendResult", params)
	if err != nil {
		return err
	}
	reds, err := Fetch[reductionRow](ctx, c, "TaiwanStockCapitalReductionReferencePrice", params)
	if err != nil {
		return err
	}
	var recent []splitRow
	for _, r := range splits {
		if r.Date >= from {
			recent = append(recent, r)
		}
	}
	prices := mergePrices(ticker, raw, buildEvents(divs, reds, recent))
	if err := writePrices(st, ticker, latest, prices); err != nil {
		return err
	}
	_, err = st.DB.Exec(`INSERT OR REPLACE INTO tw_price_checks (ticker, checked_on, rows) VALUES (?, ?, ?)`,
		ticker, today, len(prices))
	return err
}

// writePrices rescales older adjusted closes (see updateTicker) and upserts
// the new rows in one transaction, so a failure can't apply the ratio twice.
func writePrices(st *store.Store, ticker string, latest time.Time, prices []model.Price) error {
	tx, err := st.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !latest.IsZero() {
		day := latest.Format("2006-01-02")
		var old sql.NullFloat64
		err := tx.QueryRow(`SELECT adj_close FROM prices WHERE market = ? AND ticker = ? AND date = ?`,
			model.MarketTW, ticker, day).Scan(&old)
		if err != nil {
			return err
		}
		for _, p := range prices {
			if !p.Date.Equal(latest) {
				continue
			}
			if old.Valid && old.Float64 > 0 {
				if ratio := p.AdjClose / old.Float64; math.Abs(ratio-1) > 1e-6 {
					if _, err := tx.Exec(`UPDATE prices SET adj_close = adj_close * ?
						WHERE market = ? AND ticker = ? AND date < ?`, ratio, model.MarketTW, ticker, day); err != nil {
						return err
					}
				}
			}
			break
		}
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO prices
		(market, ticker, date, open, high, low, close, adj_close, volume, turnover)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range prices {
		if _, err := stmt.Exec(p.Market, p.Ticker, p.Date.Format("2006-01-02"),
			p.Open, p.High, p.Low, p.Close, p.AdjClose, p.Volume, p.Turnover); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func loadChecks(st *store.Store) (map[string]string, error) {
	rows, err := st.DB.Query(`SELECT ticker, checked_on FROM tw_price_checks`)
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
