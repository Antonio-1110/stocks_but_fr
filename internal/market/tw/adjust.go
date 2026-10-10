package tw

import (
	"database/sql"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

// Raw prices are written with adj_close = close. Afterwards each ticker
// whose adjusted closes may have moved is rebuilt from its stored raw closes
// and stored events (see mergePrices).

// dirtySet collects, per ticker, the oldest day whose raw price or event
// changed this run. It is shared by the TPEx and FinMind loops.
type dirtySet struct {
	mu     sync.Mutex
	prices map[string]time.Time // oldest new raw price
	events map[string]bool      // an event was added or changed
}

func newDirty() *dirtySet {
	return &dirtySet{prices: map[string]time.Time{}, events: map[string]bool{}}
}

func (d *dirtySet) price(ticker string, day time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if old, ok := d.prices[ticker]; !ok || day.Before(old) {
		d.prices[ticker] = day
	}
}

func (d *dirtySet) event(ticker string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events[ticker] = true
}

// writeRaw stores raw prices; adj_close is set to close until rebuilt.
func writeRaw(st *store.Store, d *dirtySet, prices []model.Price) error {
	for i := range prices {
		prices[i].AdjClose = prices[i].Close
	}
	if err := st.UpsertPrices(prices); err != nil {
		return err
	}
	for _, p := range prices {
		d.price(p.Ticker, p.Date)
	}
	return nil
}

// writeEvents stores events dated up to today (a future ex-date must not
// adjust prices yet) and marks tickers whose events changed.
func writeEvents(st *store.Store, d *dirtySet, events []tickerEvent, today time.Time) error {
	tx, err := st.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range events {
		if e.Date.After(today) {
			continue
		}
		// Write-only, so this transaction never has to upgrade a read
		// snapshot while the other loop writes (SQLite would fail it).
		res, err := tx.Exec(`INSERT INTO tw_adj_events (ticker, date, kind, ratio) VALUES (?, ?, ?, ?)
			ON CONFLICT (ticker, date, kind) DO UPDATE SET ratio = excluded.ratio
			WHERE abs(ratio - excluded.ratio) > 1e-9`, e.Ticker, e.Date.Format("2006-01-02"), e.Kind, e.Ratio)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		d.event(e.Ticker)
	}
	return tx.Commit()
}

// rebuild recomputes adjusted closes for tickers that need it: those with a
// changed event, and those with a new raw price older than their newest
// event (a backfilled day, or a refetched day whose adj_close was reset).
// Rows already right are left alone. It returns how many tickers it rebuilt.
func rebuild(st *store.Store, d *dirtySet) (int, error) {
	newest := map[string]time.Time{}
	rows, err := st.DB.Query(`SELECT ticker, MAX(date) FROM tw_adj_events GROUP BY ticker`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var t, day string
		if err := rows.Scan(&t, &day); err != nil {
			rows.Close()
			return 0, err
		}
		newest[t] = parseDay(day)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	need := map[string]bool{}
	for t := range d.events {
		need[t] = true
	}
	for t, day := range d.prices {
		if newest[t].After(day) {
			need[t] = true
		}
	}
	tickers := make([]string, 0, len(need))
	for t := range need {
		tickers = append(tickers, t)
	}
	sort.Strings(tickers)
	for _, t := range tickers {
		if err := rebuildTicker(st, t); err != nil {
			return 0, err
		}
	}
	return len(tickers), nil
}

func rebuildTicker(st *store.Store, ticker string) error {
	var events []adjEvent
	rows, err := st.DB.Query(`SELECT date, ratio FROM tw_adj_events WHERE ticker = ? ORDER BY date`, ticker)
	if err != nil {
		return err
	}
	for rows.Next() {
		var day string
		var e adjEvent
		if err := rows.Scan(&day, &e.Ratio); err != nil {
			rows.Close()
			return err
		}
		e.Date = parseDay(day)
		events = append(events, e)
	}
	rows.Close()

	var raw []priceRow
	old := map[string]float64{}
	rows, err = st.DB.Query(`SELECT date, close, adj_close FROM prices WHERE market = ? AND ticker = ?`, model.MarketTW, ticker)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r priceRow
		var adj sql.NullFloat64
		if err := rows.Scan(&r.Date, &r.Close, &adj); err != nil {
			rows.Close()
			return err
		}
		raw = append(raw, r)
		old[r.Date] = adj.Float64
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	tx, err := st.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`UPDATE prices SET adj_close = ? WHERE market = ? AND ticker = ? AND date = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range mergePrices(ticker, raw, events) {
		day := p.Date.Format("2006-01-02")
		if math.Abs(old[day]-p.AdjClose) <= 1e-9*p.Close {
			continue
		}
		if _, err := stmt.Exec(p.AdjClose, model.MarketTW, ticker, day); err != nil {
			return err
		}
	}
	return tx.Commit()
}
