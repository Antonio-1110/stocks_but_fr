package site

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

// Display windows. These pick what the page shows, not what a strategy trades;
// strategy parameters live in radar.toml.
const (
	monthTradingDays = 21  // "1-month change"
	yearCalendarDays = 372 // price history loaded for the 52-week high
	turnoverDays     = 20  // average traded value
	flowDays         = 10  // institutional net buying
	revenueMonths    = 3   // revenue YoY averaged over this many months
	staleTradingDays = 10  // hide companies with no price this close to the latest date
)

const dateLayout = "2006-01-02"

// Row is one company on the dashboard. Pointer fields are nil when the store
// has no data for them yet.
type Row struct {
	Rank         int
	Ticker       string
	Name         string
	Industry     string
	Exchange     string
	LastDate     string
	LastClose    *float64
	Change1M     *float64 // percent, from adjusted closes
	FromHigh     *float64 // percent below the 52-week adjusted high (<= 0)
	Turnover20   *float64 // average daily traded value, NTD
	RevYoY       *float64 // latest announced month, percent
	RevYoYAvg    *float64 // average of the last revenueMonths announced months
	RevMonth     string   // "2026-08"
	TrustNet10   *int64   // 投信 net, shares
	ForeignNet10 *int64   // 外資 net, shares
}

type Links struct {
	Goodinfo, Yahoo, MOPS string
}

func (r Row) Links() Links { return linksFor(r.Ticker) }

func linksFor(ticker string) Links {
	return Links{
		Goodinfo: "https://goodinfo.tw/tw/StockDetail.asp?STOCK_ID=" + ticker,
		Yahoo:    "https://tw.stock.yahoo.com/quote/" + ticker,
		MOPS:     "https://mops.twse.com.tw/mops/#/web/t05st03?companyId=" + ticker,
	}
}

// loadRows builds the company list for one market from whatever is in the
// store. asOf is the cut-off for revenue announcements (normally now).
func loadRows(st *store.Store, market string, asOf time.Time) (rows []Row, latest string, err error) {
	companies, err := st.Companies(market)
	if err != nil {
		return nil, "", fmt.Errorf("companies: %w", err)
	}
	byTicker := map[string]*Row{}
	for _, c := range companies {
		if !c.DelistedOn.IsZero() {
			continue
		}
		byTicker[c.Ticker] = &Row{Ticker: c.Ticker, Name: c.Name, Industry: c.Industry, Exchange: c.Exchange}
	}

	latest, err = latestPriceDate(st.DB, market)
	if err != nil {
		return nil, "", fmt.Errorf("latest price date: %w", err)
	}
	if latest != "" {
		if err := addPrices(st.DB, market, latest, byTicker); err != nil {
			return nil, "", fmt.Errorf("prices: %w", err)
		}
		if err := addFlows(st.DB, market, byTicker); err != nil {
			return nil, "", fmt.Errorf("flows: %w", err)
		}
	}
	if err := addRevenue(st.DB, market, asOf, byTicker); err != nil {
		return nil, "", fmt.Errorf("revenue: %w", err)
	}

	for _, r := range byTicker {
		rows = append(rows, *r)
	}
	if latest != "" {
		rows = dropStale(st.DB, market, latest, rows)
	}
	rank(rows)
	return rows, latest, nil
}

func latestPriceDate(db *sql.DB, market string) (string, error) {
	var d sql.NullString
	err := db.QueryRow(`SELECT MAX(date) FROM prices WHERE market = ?`, market).Scan(&d)
	return d.String, err
}

func addPrices(db *sql.DB, market, latest string, byTicker map[string]*Row) error {
	end, err := time.Parse(dateLayout, latest)
	if err != nil {
		return err
	}
	start := end.AddDate(0, 0, -yearCalendarDays).Format(dateLayout)
	q, err := db.Query(`SELECT ticker, date, close, adj_close, turnover FROM prices
		WHERE market = ? AND date >= ? ORDER BY ticker, date`, market, start)
	if err != nil {
		return err
	}
	defer q.Close()

	type bar struct {
		date            string
		close, adj, val float64
	}
	flush := func(ticker string, bars []bar) {
		r := byTicker[ticker]
		if r == nil || len(bars) == 0 {
			return
		}
		last := bars[len(bars)-1]
		r.LastDate = last.date
		r.LastClose = ptr(last.close)
		if i := len(bars) - 1 - monthTradingDays; i >= 0 && bars[i].adj > 0 {
			r.Change1M = ptr((last.adj/bars[i].adj - 1) * 100)
		}
		high := 0.0
		for _, b := range bars {
			high = math.Max(high, b.adj)
		}
		if high > 0 {
			r.FromHigh = ptr((last.adj/high - 1) * 100)
		}
		n, sum := 0, 0.0
		for i := len(bars) - 1; i >= 0 && n < turnoverDays; i-- {
			sum += bars[i].val
			n++
		}
		r.Turnover20 = ptr(sum / float64(n))
	}

	var cur string
	var bars []bar
	for q.Next() {
		var ticker, date string
		var cl, adj, val sql.NullFloat64
		if err := q.Scan(&ticker, &date, &cl, &adj, &val); err != nil {
			return err
		}
		if ticker != cur {
			flush(cur, bars)
			cur, bars = ticker, bars[:0]
		}
		a := adj.Float64
		if !adj.Valid || a == 0 {
			a = cl.Float64
		}
		bars = append(bars, bar{date, cl.Float64, a, val.Float64})
	}
	flush(cur, bars)
	return q.Err()
}

// addFlows sums net buying over the market's last flowDays trading days.
func addFlows(db *sql.DB, market string, byTicker map[string]*Row) error {
	q, err := db.Query(`SELECT ticker, SUM(trust_net), SUM(foreign_net) FROM institutional_flows
		WHERE market = ? AND date IN (
			SELECT DISTINCT date FROM institutional_flows WHERE market = ? ORDER BY date DESC LIMIT ?)
		GROUP BY ticker`, market, market, flowDays)
	if err != nil {
		return err
	}
	defer q.Close()
	for q.Next() {
		var ticker string
		var trust, foreign sql.NullInt64
		if err := q.Scan(&ticker, &trust, &foreign); err != nil {
			return err
		}
		if r := byTicker[ticker]; r != nil {
			r.TrustNet10, r.ForeignNet10 = ptr(trust.Int64), ptr(foreign.Int64)
		}
	}
	return q.Err()
}

// addRevenue uses only months announced on or before asOf, so the page never
// shows something the market didn't know yet.
func addRevenue(db *sql.DB, market string, asOf time.Time, byTicker map[string]*Row) error {
	q, err := db.Query(`SELECT ticker, month, yoy_pct FROM monthly_revenue
		WHERE market = ? AND (announced_on = '' OR announced_on <= ?)
		ORDER BY ticker, month DESC`, market, asOf.Format(dateLayout))
	if err != nil {
		return err
	}
	defer q.Close()
	seen := map[string]int{}
	sums := map[string]float64{}
	for q.Next() {
		var ticker, month string
		var yoy sql.NullFloat64
		if err := q.Scan(&ticker, &month, &yoy); err != nil {
			return err
		}
		r := byTicker[ticker]
		if r == nil || seen[ticker] >= revenueMonths || !yoy.Valid {
			continue
		}
		if seen[ticker] == 0 {
			r.RevYoY = ptr(yoy.Float64)
			if len(month) >= 7 {
				r.RevMonth = month[:7]
			}
		}
		seen[ticker]++
		sums[ticker] += yoy.Float64
		if seen[ticker] == revenueMonths {
			r.RevYoYAvg = ptr(sums[ticker] / revenueMonths)
		}
	}
	return q.Err()
}

// dropStale hides companies that have no price within the market's last
// staleTradingDays trading days (suspended, or missing from the collector).
// Companies with no price at all stay, so an empty price table still shows the universe.
func dropStale(db *sql.DB, market, latest string, rows []Row) []Row {
	var cutoff sql.NullString
	db.QueryRow(`SELECT MIN(date) FROM (SELECT DISTINCT date FROM prices WHERE market = ?
		ORDER BY date DESC LIMIT ?)`, market, staleTradingDays).Scan(&cutoff)
	out := rows[:0]
	for _, r := range rows {
		if r.LastDate == "" || r.LastDate >= cutoff.String {
			out = append(out, r)
		}
	}
	return out
}

// rank orders by average revenue YoY (the signal Strategy A leads with), then
// latest YoY, then ticker; companies without revenue go last.
func rank(rows []Row) {
	val := func(p *float64) float64 {
		if p == nil {
			return math.Inf(-1)
		}
		return *p
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if va, vb := val(a.RevYoYAvg), val(b.RevYoYAvg); va != vb {
			return va > vb
		}
		if va, vb := val(a.RevYoY), val(b.RevYoY); va != vb {
			return va > vb
		}
		return a.Ticker < b.Ticker
	})
	for i := range rows {
		rows[i].Rank = i + 1
	}
}

func ptr[T any](v T) *T { return &v }
