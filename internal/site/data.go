package site

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
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
	Rank       int
	Ticker     string
	Name       string
	Industry   string
	Exchange   string
	LastDate   string
	LastClose  *float64
	Change1M   *float64 // percent, from adjusted closes
	FromHigh   *float64 // percent below the 52-week adjusted high (<= 0)
	Turnover20 *float64 // average daily traded value, NTD
	RevYoY     *float64 // latest announced month, percent
	RevYoYAvg  *float64 // average of the last revenueMonths announced months
	RevMonth   string   // "2026-08"
	// LowBase is set when any month in the YoY window is off a tiny base
	// (config.LowBase); such rows are shown but left out of the ranking.
	LowBase       bool
	RevYoYLowBase bool     // the latest month itself is off a tiny base
	LowBaseNotes  []string // why, one line per flagged month
	TrustNet10    *int64   // 投信 net, shares
	ForeignNet10  *int64   // 外資 net, shares
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
func loadRows(st *store.Store, market string, asOf time.Time, lb config.LowBase) (rows []Row, latest string, err error) {
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
	if err := addRevenue(st.DB, market, asOf, lb, byTicker); err != nil {
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
// shows something the market didn't know yet. Each month's YoY is checked
// against last year's base (see lowBase); a flagged month is still shown.
func addRevenue(db *sql.DB, market string, asOf time.Time, lb config.LowBase, byTicker map[string]*Row) error {
	// Enough history for the YoY window, its base months, and the months before those.
	from := asOf.AddDate(0, -(revenueMonths + 12 + lb.TypicalMonths + 2), 0)
	q, err := db.Query(`SELECT ticker, month, revenue, yoy_pct, announced_on FROM monthly_revenue
		WHERE market = ? AND month >= ? ORDER BY ticker, month`, market, from.Format(dateLayout))
	if err != nil {
		return err
	}
	defer q.Close()

	type month struct {
		month     string
		rev       sql.NullFloat64
		yoy       sql.NullFloat64
		announced bool
	}
	cutoff := asOf.Format(dateLayout)
	flush := func(ticker string, months []month) {
		r := byTicker[ticker]
		if r == nil {
			return
		}
		revByMonth := map[string]float64{}
		for _, m := range months {
			if m.rev.Valid {
				revByMonth[m.month] = m.rev.Float64
			}
		}
		n, sum := 0, 0.0
		for i := len(months) - 1; i >= 0 && n < revenueMonths; i-- {
			m := months[i]
			if !m.announced || !m.yoy.Valid {
				continue
			}
			low, note := lowBase(m.month, m.rev, m.yoy.Float64, revByMonth, lb)
			if n == 0 {
				r.RevYoY = ptr(m.yoy.Float64)
				r.RevMonth = m.month[:min(7, len(m.month))]
				r.RevYoYLowBase = low
			}
			if low {
				r.LowBase = true
				r.LowBaseNotes = append(r.LowBaseNotes, note)
			}
			n++
			sum += m.yoy.Float64
		}
		if n == revenueMonths {
			r.RevYoYAvg = ptr(sum / revenueMonths)
		}
	}

	var cur string
	var months []month
	for q.Next() {
		var ticker, m, ann string
		var rev, yoy sql.NullFloat64
		if err := q.Scan(&ticker, &m, &rev, &yoy, &ann); err != nil {
			return err
		}
		if ticker != cur {
			flush(cur, months)
			cur, months = ticker, months[:0]
		}
		months = append(months, month{m, rev, yoy, ann == "" || ann <= cutoff})
	}
	flush(cur, months)
	return q.Err()
}

// lowBase reports whether the YoY for month m is measured against a base too
// small to mean anything: last year's same-month revenue below lb.MinBaseNTD,
// or below lb.MinBaseRatio of the company's average monthly revenue over the
// lb.TypicalMonths months before that base month. The average, not the median,
// so lumpy businesses (builders booking a project in one month) count their
// big months as typical and a quiet month as the low base it is. The base comes from the
// stored row, or is backed out of the YoY when that row is missing.
func lowBase(m string, rev sql.NullFloat64, yoy float64, revByMonth map[string]float64, lb config.LowBase) (bool, string) {
	t, err := time.Parse(dateLayout, m)
	if err != nil {
		return false, ""
	}
	baseMonth := t.AddDate(-1, 0, 0)
	base, ok := revByMonth[baseMonth.Format(dateLayout)]
	if !ok {
		if !rev.Valid || yoy <= -100 {
			return false, ""
		}
		base = rev.Float64 / (1 + yoy/100)
	}
	label := baseMonth.Format("2006-01")
	if lb.MinBaseNTD > 0 && base < lb.MinBaseNTD {
		return true, fmt.Sprintf("%s revenue %s, under the %s floor", label, million(base), million(lb.MinBaseNTD))
	}
	var prior []float64
	for i := 1; i <= lb.TypicalMonths; i++ {
		if v, ok := revByMonth[baseMonth.AddDate(0, -i, 0).Format(dateLayout)]; ok {
			prior = append(prior, v)
		}
	}
	// Half the window is enough to call something typical.
	if lb.MinBaseRatio > 0 && len(prior) > 0 && len(prior)*2 >= lb.TypicalMonths {
		if avg := mean(prior); base < lb.MinBaseRatio*avg {
			return true, fmt.Sprintf("%s revenue %s, %.0f%% of its usual %s", label, million(base), base/avg*100, million(avg))
		}
	}
	return false, ""
}

func mean(v []float64) float64 {
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

// million formats NTD in 百萬 (millions).
func million(v float64) string { return fmt.Sprintf("%.1f百萬", v/1e6) }

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
// latest YoY, then ticker; companies without revenue go last. Low-base rows
// follow all the others and get no rank number.
func rank(rows []Row) {
	val := func(p *float64) float64 {
		if p == nil {
			return math.Inf(-1)
		}
		return *p
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.LowBase != b.LowBase {
			return b.LowBase
		}
		if va, vb := val(a.RevYoYAvg), val(b.RevYoYAvg); va != vb {
			return va > vb
		}
		if va, vb := val(a.RevYoY), val(b.RevYoY); va != vb {
			return va > vb
		}
		return a.Ticker < b.Ticker
	})
	n := 0
	for i := range rows {
		if !rows[i].LowBase {
			n++
			rows[i].Rank = n
		}
	}
}

func ptr[T any](v T) *T { return &v }
