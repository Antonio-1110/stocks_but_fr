package paper

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// bar is one stock's day. Adjusted and raw closes give the adjustment factor
// (adj/close): holding a stock from day a to day b is worth
// factor(b)/factor(a) times as many shares, which is how dividends and splits
// reach a position without tracking the events themselves. The ratio stays
// right when the collector later rebuilds the adjusted series, because a
// rebuild scales every day before an event by the same amount.
type bar struct {
	date                      string
	open, close, adj, turnout float64
}

func (b bar) factor() float64 {
	if b.close > 0 && b.adj > 0 {
		return b.adj / b.close
	}
	return 1
}

// market is the recent Taiwan price window the portfolios trade on.
type market struct {
	db *sql.DB
	// dates are the trading days in the window, oldest first, up to the
	// newest day whose prices are fully collected.
	dates []string
	// ready is true when the newest stored day is fully collected, so it may
	// be traded on. Otherwise dates stop before it and nothing new is signalled.
	ready bool
	bars  map[string]map[string]bar // ticker -> date -> bar
	names map[string]string
}

func (m *market) last() string { return m.dates[len(m.dates)-1] }

// after is the first usable trading day after d, or "".
func (m *market) after(d string) string {
	for _, x := range m.dates {
		if x > d {
			return x
		}
	}
	return ""
}

// on returns a ticker's bar on d.
func (m *market) on(ticker, d string) (bar, bool) {
	b, ok := m.bars[ticker][d]
	return b, ok
}

// lastOn returns a ticker's newest bar on or before d, from the window or,
// failing that, the whole table.
func (m *market) lastOn(ticker, d string) (bar, bool) {
	for i := len(m.dates) - 1; i >= 0; i-- {
		if m.dates[i] > d {
			continue
		}
		if b, ok := m.bars[ticker][m.dates[i]]; ok {
			return b, true
		}
	}
	return m.query(`SELECT date, open, close, adj_close, turnover FROM prices
		WHERE market = 'TW' AND ticker = ? AND date <= ? ORDER BY date DESC LIMIT 1`, ticker, d)
}

// exact returns a ticker's bar on d even outside the window (a position's fill day).
func (m *market) exact(ticker, d string) (bar, bool) {
	if b, ok := m.on(ticker, d); ok {
		return b, true
	}
	return m.query(`SELECT date, open, close, adj_close, turnover FROM prices
		WHERE market = 'TW' AND ticker = ? AND date = ?`, ticker, d)
}

func (m *market) query(q string, args ...any) (bar, bool) {
	var b bar
	var o, c, a, t sql.NullFloat64
	if err := m.db.QueryRow(q, args...).Scan(&b.date, &o, &c, &a, &t); err != nil {
		return bar{}, false
	}
	b.open, b.close, b.adj, b.turnout = o.Float64, c.Float64, a.Float64, t.Float64
	if !a.Valid {
		b.adj = b.close
	}
	return b, b.close > 0
}

// sinceLast counts usable trading days after a ticker's last bar up to the
// end of the window: how long it has not traded.
func (m *market) sinceLast(ticker string) int {
	n := 0
	for i := len(m.dates) - 1; i >= 0; i-- {
		if _, ok := m.bars[ticker][m.dates[i]]; ok {
			return n
		}
		n++
	}
	return n
}

// window returns the last n usable dates up to and including d.
func (m *market) window(d string, n int) []string {
	end := len(m.dates)
	for end > 0 && m.dates[end-1] > d {
		end--
	}
	return m.dates[max(0, end-n):end]
}

// loadMarket reads the last `days` trading days of Taiwan prices, plus
// whatever is newer than `since` (the oldest pending order), and company names.
// minCoverage decides when the newest day counts as collected: it needs this
// share of the previous day's stock count.
func loadMarket(db *sql.DB, days int, since string, minCoverage float64) (*market, error) {
	m := &market{db: db, bars: map[string]map[string]bar{}, names: map[string]string{}}
	q, err := db.Query(`SELECT DISTINCT date FROM prices WHERE market = 'TW' ORDER BY date DESC LIMIT ?`, days)
	if err != nil {
		return nil, err
	}
	var dates []string
	for q.Next() {
		var d string
		if err := q.Scan(&d); err != nil {
			q.Close()
			return nil, err
		}
		dates = append(dates, d)
	}
	q.Close()
	if err := q.Err(); err != nil {
		return nil, err
	}
	if len(dates) == 0 {
		return m, nil
	}
	from := dates[len(dates)-1]
	if since != "" && since < from {
		from = since
	}

	q, err = db.Query(`SELECT ticker, date, open, close, adj_close, turnover FROM prices
		WHERE market = 'TW' AND date >= ?`, from)
	if err != nil {
		return nil, err
	}
	count := map[string]int{}
	for q.Next() {
		var t string
		var b bar
		var o, c, a, v sql.NullFloat64
		if err := q.Scan(&t, &b.date, &o, &c, &a, &v); err != nil {
			q.Close()
			return nil, err
		}
		if !c.Valid || c.Float64 <= 0 {
			continue
		}
		b.open, b.close, b.adj, b.turnout = o.Float64, c.Float64, a.Float64, v.Float64
		if !a.Valid || a.Float64 <= 0 {
			b.adj = b.close
		}
		if m.bars[t] == nil {
			m.bars[t] = map[string]bar{}
		}
		m.bars[t][b.date] = b
		count[b.date]++
	}
	q.Close()
	if err := q.Err(); err != nil {
		return nil, err
	}
	for d := range count {
		m.dates = append(m.dates, d)
	}
	sort.Strings(m.dates)

	// The collectors fill a day over several runs; trade on it only once it
	// looks complete next to the day before.
	n := len(m.dates)
	m.ready = n < 2 || float64(count[m.dates[n-1]]) >= minCoverage*float64(count[m.dates[n-2]])
	if !m.ready {
		m.dates = m.dates[:n-1]
	}

	rows, err := db.Query(`SELECT ticker, name FROM companies WHERE market = 'TW'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t, name string
		if err := rows.Scan(&t, &name); err != nil {
			return nil, err
		}
		m.names[t] = name
	}
	return m, rows.Err()
}

func placeholders(n int) string {
	if n == 0 {
		return ""
	}
	return "?" + strings.Repeat(",?", n-1)
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func pct(x float64) string { return fmt.Sprintf("%+.1f%%", x*100) }
