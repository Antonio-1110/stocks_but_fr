package tw

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

var taipei = time.FixedZone("Asia/Taipei", 8*60*60)

// checksTable records when each ticker was last fetched from FinMind, so a
// ticker FinMind has no prices for is tried once a day, and a delisted one
// only once. It is this collector's bookkeeping, not part of the shared schema.
const checksTable = `CREATE TABLE IF NOT EXISTS tw_price_checks (
	ticker     TEXT PRIMARY KEY,
	checked_on TEXT NOT NULL,
	rows       INTEGER NOT NULL
)`

// eventsFullTable lists tickers whose FinMind events are stored back to
// history_start.
const eventsFullTable = `CREATE TABLE IF NOT EXISTS tw_events_full (ticker TEXT PRIMARY KEY)`

// Collect is the `radar collect` step. OTC (TPEx) stocks come from TPEx's
// whole-market daily files, one request per day for every stock. TWSE blocks
// cloud addresses after a request or two, so TWSE-listed stocks still come
// from FinMind, one stock at a time. Both run at once, then adjusted closes
// are rebuilt where events or backfilled days changed them.
func Collect(ctx context.Context, cfg config.Config, st *store.Store) error {
	// Revenue and flows run after this step and share the FinMind budget.
	return collect(ctx, cfg, st, ForStep("tw-prices", 2), NewTPEx(), time.Now().In(taipei))
}

func collect(ctx context.Context, cfg config.Config, st *store.Store, c *FinMind, tp *TPEx, now time.Time) error {
	for _, q := range []string{checksTable, eventsTable, eventsFullTable, daysTable, eventYearsTable} {
		if _, err := st.DB.Exec(q); err != nil {
			return err
		}
	}
	start := parseDay(cfg.Run.HistoryStart)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	dirty := newDirty()

	if err := updateUniverse(ctx, cfg, st, c); err != nil {
		log.Printf("tw: %v (keeping the stored universe)", err)
	}
	// Splits come for the whole market in one request.
	if rows, err := Fetch[splitRow](ctx, c, "TaiwanStockSplitPrice", map[string]string{"start_date": cfg.Run.HistoryStart}); err != nil {
		log.Printf("tw: splits unavailable this run: %v", err)
	} else if err := writeEvents(st, dirty, splitEvents(rows), today); err != nil {
		return err
	}

	var wg sync.WaitGroup
	var tpexErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := tpexEvents(ctx, tp, st, dirty, start.Year(), now); err != nil {
			log.Printf("tw: TPEx events: %v", err)
		}
		fetched, left, err := tpexDays(ctx, tp, st, dirty, start, now)
		log.Printf("tw: TPEx: fetched %d days, %d left for later runs", fetched, left)
		tpexErr = err
	}()
	finmindErr := finmindTickers(ctx, cfg, st, c, dirty, now)
	wg.Wait()
	if tpexErr != nil {
		log.Printf("tw: TPEx: %v", tpexErr)
	}
	if finmindErr != nil {
		return finmindErr
	}

	n, err := rebuild(st, dirty)
	if err != nil {
		return fmt.Errorf("rebuild adjusted closes: %w", err)
	}
	log.Printf("tw: rebuilt adjusted closes for %d tickers", n)
	return nil
}

// finmindTickers backfills or updates TWSE-listed stocks one at a time from
// FinMind, until they are current or the FinMind budget is spent. OTC stocks
// are left to the TPEx files. A delisted stock is fetched once, unless its
// stored prices already reach its delisting (it traded on TPEx).
func finmindTickers(ctx context.Context, cfg config.Config, st *store.Store, c *FinMind, dirty *dirtySet, now time.Time) error {
	companies, err := st.Companies(model.MarketTW)
	if err != nil {
		return err
	}
	today := now.Format("2006-01-02")
	checked, err := loadChecks(st)
	if err != nil {
		return err
	}

	// The benchmark goes first; its newest date is the latest trading day.
	bench := cfg.Backtest.Benchmark
	if checked[bench] != today {
		if err := updateTicker(ctx, cfg, st, c, dirty, bench, now); err != nil {
			if errors.Is(err, ErrStop) || ctx.Err() != nil {
				log.Printf("tw: FinMind: stopping: %v", err)
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
		if co.Ticker == bench || co.Exchange == "TPEx" || checked[co.Ticker] == today {
			continue
		}
		latest, err := st.LatestPriceDate(model.MarketTW, co.Ticker)
		if err != nil {
			return err
		}
		if !co.DelistedOn.IsZero() {
			if checked[co.Ticker] != "" || (!latest.IsZero() && latest.AddDate(0, 0, 10).After(co.DelistedOn)) {
				continue
			}
		} else if !latest.IsZero() && !latest.Before(calendar) {
			continue
		}
		jobs = append(jobs, job{co.Ticker, latest})
	}
	// Never-fetched tickers first (backfill), then the stalest.
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].latest.Before(jobs[j].latest) })

	done := 0
	for _, j := range jobs {
		err := updateTicker(ctx, cfg, st, c, dirty, j.ticker, now)
		if errors.Is(err, ErrStop) || ctx.Err() != nil {
			log.Printf("tw: FinMind: stopping: %v", err)
			break
		}
		if err != nil {
			log.Printf("tw: %s: %v (skipped)", j.ticker, err)
			continue
		}
		done++
	}
	log.Printf("tw: FinMind: prices updated for %d of %d stale TWSE tickers; the rest resume next run", done, len(jobs))
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

// updateTicker fetches one stock's raw prices and adjustment events from
// FinMind, from its newest stored day (or from history_start). The newest
// stored day is fetched again; that is harmless, and adjusted closes are
// rebuilt afterwards if an event or an older day changed.
func updateTicker(ctx context.Context, cfg config.Config, st *store.Store, c *FinMind, dirty *dirtySet, ticker string, now time.Time) error {
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
	// Events are stored since this collector rebuilds adjusted closes from
	// them. A ticker priced before that has none stored, so its events are
	// fetched from history_start once (same number of requests).
	var full int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM tw_events_full WHERE ticker = ?`, ticker).Scan(&full); err != nil {
		return err
	}
	evParams := params
	if full == 0 {
		evParams = map[string]string{"data_id": ticker, "start_date": cfg.Run.HistoryStart}
	}
	divs, err := Fetch[dividendRow](ctx, c, "TaiwanStockDividendResult", evParams)
	if err != nil {
		return err
	}
	reds, err := Fetch[reductionRow](ctx, c, "TaiwanStockCapitalReductionReferencePrice", evParams)
	if err != nil {
		return err
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if err := writeEvents(st, dirty, finmindEvents(ticker, divs, reds), today); err != nil {
		return err
	}
	prices := mergePrices(ticker, raw, nil)
	if err := writeRaw(st, dirty, prices); err != nil {
		return err
	}
	if _, err := st.DB.Exec(`INSERT OR IGNORE INTO tw_events_full (ticker) VALUES (?)`, ticker); err != nil {
		return err
	}
	_, err = st.DB.Exec(`INSERT OR REPLACE INTO tw_price_checks (ticker, checked_on, rows) VALUES (?, ?, ?)`,
		ticker, now.Format("2006-01-02"), len(prices))
	return err
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
