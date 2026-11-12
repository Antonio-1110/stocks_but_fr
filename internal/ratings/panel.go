package ratings

import (
	"database/sql"
	"math"
	"regexp"
	"sort"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
)

// common matches Taiwan common stocks (four digits; ETFs start with 00).
var common = regexp.MustCompile(`^[1-9]\d{3}$`)

// panel is one market's daily prices on a shared date axis. Missing bars are NaN.
type panel struct {
	market  string
	bench   string
	dates   []string // trading days, oldest first, up to the newest fully collected one
	idx     map[string]int
	tickers []string
	row     map[string]int
	adj     [][]float64 // [ticker][date] dividend-adjusted close
	close   [][]float64 // raw close
	value   [][]float64 // traded value (TW turnover, US close x volume)
	names   map[string]string
	rated   []string // tickers that may be rated (excludes the benchmark)
	watch   map[string]bool
}

// at is the ticker's adjusted close on day t, or its last one within the
// previous `stale` days (a halt), or NaN.
func (p *panel) at(i, t, stale int) float64 {
	for k := t; k >= 0 && k >= t-stale; k-- {
		if v := p.adj[i][k]; !math.IsNaN(v) {
			return v
		}
	}
	return math.NaN()
}

func (p *panel) has(i, t int) bool { return t >= 0 && t < len(p.dates) && !math.IsNaN(p.adj[i][t]) }

type bar struct {
	ticker, date      string
	close, adj, value float64
}

// build turns rows into a panel. A newest day with fewer rows than
// minCoverage of the day before is still being collected and is dropped.
func build(market, bench string, rows []bar, minCoverage float64) *panel {
	p := &panel{market: market, bench: bench, idx: map[string]int{}, row: map[string]int{}, names: map[string]string{}, watch: map[string]bool{}}
	count := map[string]int{}
	for _, b := range rows {
		count[b.date]++
		if _, ok := p.row[b.ticker]; !ok {
			p.row[b.ticker] = len(p.tickers)
			p.tickers = append(p.tickers, b.ticker)
		}
	}
	for d := range count {
		p.dates = append(p.dates, d)
	}
	sort.Strings(p.dates)
	if n := len(p.dates); n >= 2 && float64(count[p.dates[n-1]]) < minCoverage*float64(count[p.dates[n-2]]) {
		p.dates = p.dates[:n-1]
	}
	for i, d := range p.dates {
		p.idx[d] = i
	}
	nan := func() []float64 {
		s := make([]float64, len(p.dates))
		for i := range s {
			s[i] = math.NaN()
		}
		return s
	}
	for range p.tickers {
		p.adj = append(p.adj, nan())
		p.close = append(p.close, nan())
		p.value = append(p.value, nan())
	}
	for _, b := range rows {
		t, ok := p.idx[b.date]
		if !ok || b.close <= 0 {
			continue
		}
		i := p.row[b.ticker]
		adj := b.adj
		if adj <= 0 || math.IsNaN(adj) {
			adj = b.close
		}
		p.adj[i][t], p.close[i][t], p.value[i][t] = adj, b.close, b.value
	}
	return p
}

// newestDate is the market's newest stored day ("" when there is none) and
// how many rows it has: a cheap fingerprint to skip runs with nothing new.
func newestDate(db *sql.DB, market string) (string, int, error) {
	var d sql.NullString
	var n int
	var err error
	switch market {
	case "TW":
		err = db.QueryRow(`SELECT MAX(date) FROM prices WHERE market = 'TW'`).Scan(&d)
		if err == nil && d.Valid {
			err = db.QueryRow(`SELECT COUNT(*) FROM prices WHERE market = 'TW' AND date = ?`, d.String).Scan(&n)
		}
	case "US":
		if !tableExists(db, "us_prices") {
			return "", 0, nil
		}
		err = db.QueryRow(`SELECT MAX(date) FROM us_prices`).Scan(&d)
		if err == nil && d.Valid {
			err = db.QueryRow(`SELECT COUNT(*) FROM us_prices WHERE date = ?`, d.String).Scan(&n)
		}
	}
	return d.String, n, err
}

func tableExists(db *sql.DB, name string) bool {
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	return n > 0
}

// loadTW reads Taiwan common stocks, the watchlist and the benchmark from
// `from` on.
func loadTW(db *sql.DB, cfg config.Config, from string) (*panel, error) {
	bench := cfg.Paper.Benchmark
	watch := map[string]bool{}
	for _, t := range cfg.Ratings.TWWatch {
		watch[t] = true
	}
	q, err := db.Query(`SELECT ticker, date, close, adj_close, turnover FROM prices
		WHERE market = 'TW' AND date >= ? AND close > 0`, from)
	if err != nil {
		return nil, err
	}
	var rows []bar
	for q.Next() {
		var b bar
		var a, v sql.NullFloat64
		if err := q.Scan(&b.ticker, &b.date, &b.close, &a, &v); err != nil {
			q.Close()
			return nil, err
		}
		if !common.MatchString(b.ticker) && !watch[b.ticker] && b.ticker != bench {
			continue
		}
		b.adj, b.value = a.Float64, v.Float64
		rows = append(rows, b)
	}
	q.Close()
	if err := q.Err(); err != nil {
		return nil, err
	}
	p := build("TW", bench, rows, cfg.Ratings.MinCoverage)
	p.watch = watch
	for _, t := range p.tickers {
		if t != bench || watch[t] {
			p.rated = append(p.rated, t)
		}
	}
	n, err := db.Query(`SELECT ticker, name FROM companies WHERE market = 'TW'`)
	if err != nil {
		return nil, err
	}
	defer n.Close()
	for n.Next() {
		var t, name string
		if err := n.Scan(&t, &name); err != nil {
			return nil, err
		}
		p.names[t] = name
	}
	return p, n.Err()
}

// loadUS reads the [us] tickers and the first benchmark from `from` on.
func loadUS(db *sql.DB, cfg config.Config, from string) (*panel, error) {
	bench := "SPY"
	if len(cfg.US.Benchmarks) > 0 {
		bench = cfg.US.Benchmarks[0]
	}
	want := map[string]bool{bench: true}
	for _, t := range cfg.US.Tickers {
		want[t] = true
	}
	q, err := db.Query(`SELECT ticker, date, close, adj_close, volume FROM us_prices
		WHERE date >= ? AND close > 0`, from)
	if err != nil {
		return nil, err
	}
	var rows []bar
	for q.Next() {
		var b bar
		var a sql.NullFloat64
		var v sql.NullInt64
		if err := q.Scan(&b.ticker, &b.date, &b.close, &a, &v); err != nil {
			q.Close()
			return nil, err
		}
		if !want[b.ticker] {
			continue
		}
		b.adj, b.value = a.Float64, b.close*float64(v.Int64)
		rows = append(rows, b)
	}
	q.Close()
	if err := q.Err(); err != nil {
		return nil, err
	}
	p := build("US", bench, rows, cfg.Ratings.MinCoverage)
	for _, t := range cfg.US.Tickers {
		if _, ok := p.row[t]; ok && t != bench {
			p.rated = append(p.rated, t)
		}
	}
	if tableExists(db, "us_companies") {
		n, err := db.Query(`SELECT ticker, name FROM us_companies`)
		if err != nil {
			return nil, err
		}
		defer n.Close()
		for n.Next() {
			var t, name string
			if err := n.Scan(&t, &name); err != nil {
				return nil, err
			}
			p.names[t] = name
		}
		return p, n.Err()
	}
	return p, nil
}

// revenueYoY returns, for each Taiwan ticker, its monthly revenue YoY (%) by
// month ("YYYY-MM"), from `from` (a date) minus a few months on.
func revenueYoY(db *sql.DB, from string) (map[string]map[string]float64, error) {
	out := map[string]map[string]float64{}
	start := from
	if t, err := time.Parse("2006-01-02", from); err == nil {
		start = t.AddDate(0, -3, 0).Format("2006-01")
	}
	q, err := db.Query(`SELECT ticker, month, yoy_pct FROM monthly_revenue
		WHERE market = 'TW' AND month >= ? AND yoy_pct IS NOT NULL`, start)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	for q.Next() {
		var t, m string
		var y float64
		if err := q.Scan(&t, &m, &y); err != nil {
			return nil, err
		}
		if out[t] == nil {
			out[t] = map[string]float64{}
		}
		if len(m) >= 7 {
			out[t][m[:7]] = y
		}
	}
	return out, q.Err()
}

// knownMonth is the newest revenue month whose filing deadline (day `deadline`
// of the following month) is on or before d.
func knownMonth(d string, deadline int) string {
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return ""
	}
	m := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
	if t.Day() < deadline {
		m = m.AddDate(0, -1, 0)
	}
	return m.Format("2006-01")
}
