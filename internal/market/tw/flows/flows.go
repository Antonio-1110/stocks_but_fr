// Package flows collects Taiwan institutional investor flows (三大法人): daily
// net shares bought by foreign investors, investment trusts (投信) and dealers.
//
// History comes from FinMind, one request per ticker covering the whole range.
// Recent days come from the exchanges' own daily reports (TWSE T86 and the
// TPEx equivalent), one request per exchange per day. A small bookkeeping
// table records what has been fetched, so no request is repeated.
package flows

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

const (
	userAgent = "stocks_but_fr-radar/0.1 (+https://github.com/Antonio-1110/stocks_but_fr)"

	// FinMind allows 600 requests/hour with a token and 300 without.
	// Stay under that and cap each run; the backfill resumes next run.
	finmindGapToken   = 6500 * time.Millisecond
	finmindGapNoToken = 13 * time.Second
	finmindPerRun     = 300

	// The exchanges block clients that hit them faster than a few per 5s.
	exchangeGap = 3 * time.Second
	// How many calendar days the daily reports look back. Older days are
	// FinMind's job.
	dailyLookback = 30
)

// Taiwan has no DST, so a fixed zone avoids depending on tzdata.
var taipei = time.FixedZone("Asia/Taipei", 8*60*60)

// Collector holds the endpoints and pacing so tests can point it at fakes.
type Collector struct {
	Client      *http.Client
	FinMindURL  string
	TWSEURL     string
	TPExURL     string
	Token       string // FinMind API token; optional
	FinMindGap  time.Duration
	ExchangeGap time.Duration
	FinMindMax  int
	Now         func() time.Time
}

func New() *Collector {
	token := os.Getenv("FINMIND_TOKEN")
	gap := finmindGapNoToken
	if token != "" {
		gap = finmindGapToken
	}
	return &Collector{
		Client:      &http.Client{Timeout: 60 * time.Second},
		FinMindURL:  "https://api.finmindtrade.com/api/v4/data",
		TWSEURL:     "https://www.twse.com.tw/rwd/zh/fund/T86",
		TPExURL:     "https://www.tpex.org.tw/www/zh-tw/insti/dailyTrade",
		Token:       token,
		FinMindGap:  gap,
		ExchangeGap: exchangeGap,
		FinMindMax:  finmindPerRun,
		Now:         time.Now,
	}
}

// Collect is the `radar collect` step.
func Collect(ctx context.Context, cfg config.Config, st *store.Store) error {
	return New().Run(ctx, cfg, st)
}

// Run backfills history, then fills recent days. Each part fails soft: an
// error is logged and the other part still runs.
func (c *Collector) Run(ctx context.Context, cfg config.Config, st *store.Store) error {
	if err := ensureSchema(st.DB); err != nil {
		return err
	}
	start, err := time.ParseInLocation("2006-01-02", cfg.Run.HistoryStart, taipei)
	if err != nil {
		return fmt.Errorf("run.history_start: %w", err)
	}
	errBackfill := c.backfill(ctx, st, start)
	if errBackfill != nil {
		log.Printf("flows: backfill: %v", errBackfill)
	}
	errDaily := c.daily(ctx, st)
	if errDaily != nil {
		log.Printf("flows: daily: %v", errDaily)
	}
	return errors.Join(errBackfill, errDaily)
}

// backfill fetches full history for each TW company not fetched before.
func (c *Collector) backfill(ctx context.Context, st *store.Store, start time.Time) error {
	companies, err := st.Companies(model.MarketTW)
	if err != nil {
		return err
	}
	if len(companies) == 0 {
		log.Printf("flows: no TW companies in the store yet; skipping FinMind backfill")
		return nil
	}
	done, err := fetchedKeys(st.DB, srcFinMind)
	if err != nil {
		return err
	}
	today := c.today()
	end := today.Format("2006-01-02")
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
	log.Printf("flows: FinMind backfill: %d of %d tickers left, up to %d this run", len(todo), len(companies), c.FinMindMax)

	var failed []error
	for i, co := range todo {
		if i >= c.FinMindMax {
			break
		}
		if i > 0 {
			if err := sleep(ctx, c.FinMindGap); err != nil {
				return err
			}
		}
		rows, err := c.fetchFinMind(ctx, co.Ticker, start.Format("2006-01-02"), end)
		if errors.Is(err, errQuota) {
			return fmt.Errorf("stopping after %d tickers: %w", i, err)
		}
		if err != nil {
			log.Printf("flows: FinMind %s: %v", co.Ticker, err)
			failed = append(failed, err)
			if len(failed) >= 10 {
				return fmt.Errorf("giving up after %d errors: %w", len(failed), errors.Join(failed...))
			}
			continue
		}
		if err := st.UpsertFlows(rows); err != nil {
			return err
		}
		if err := markFetched(st.DB, srcFinMind, co.Ticker, len(rows)); err != nil {
			return err
		}
	}
	return nil
}

// daily fetches each exchange's report for every recent day not yet recorded.
func (c *Collector) daily(ctx context.Context, st *store.Store) error {
	today := c.today()
	var errs []error
	for _, src := range []string{srcTWSE, srcTPEx} {
		done, err := fetchedKeys(st.DB, src)
		if err != nil {
			return err
		}
		first := true
		for d := today.AddDate(0, 0, -dailyLookback); !d.After(today); d = d.AddDate(0, 0, 1) {
			key := d.Format("2006-01-02")
			if done[key] {
				continue
			}
			if !first {
				if err := sleep(ctx, c.ExchangeGap); err != nil {
					return err
				}
			}
			first = false
			rows, err := c.fetchExchange(ctx, src, d)
			if err != nil {
				log.Printf("flows: %s %s: %v", src, key, err)
				errs = append(errs, fmt.Errorf("%s %s: %w", src, key, err))
				continue
			}
			if err := st.UpsertFlows(rows); err != nil {
				return err
			}
			// An empty report for today or yesterday may just not be out yet;
			// for older days it means the market was closed.
			if len(rows) > 0 || d.Before(today.AddDate(0, 0, -1)) {
				if err := markFetched(st.DB, src, key, len(rows)); err != nil {
					return err
				}
			}
		}
	}
	return errors.Join(errs...)
}

// today is midnight of the current date in Taipei.
func (c *Collector) today() time.Time {
	y, m, d := c.Now().In(taipei).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, taipei)
}

func sleep(ctx context.Context, d time.Duration) error {
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

// Bookkeeping. This table is private to this package; the backtester ignores it.
const (
	srcFinMind = "finmind" // key: ticker
	srcTWSE    = "twse"    // key: date
	srcTPEx    = "tpex"    // key: date
)

func ensureSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS flows_fetch_log (
		source     TEXT NOT NULL,
		key        TEXT NOT NULL,
		rows       INTEGER NOT NULL,
		fetched_at TEXT NOT NULL,
		PRIMARY KEY (source, key)
	)`)
	return err
}

func fetchedKeys(db *sql.DB, source string) (map[string]bool, error) {
	rs, err := db.Query(`SELECT key FROM flows_fetch_log WHERE source = ?`, source)
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

func markFetched(db *sql.DB, source, key string, rows int) error {
	_, err := db.Exec(`INSERT OR REPLACE INTO flows_fetch_log (source, key, rows, fetched_at)
		VALUES (?, ?, ?, ?)`, source, key, rows, time.Now().UTC().Format(time.RFC3339))
	return err
}
