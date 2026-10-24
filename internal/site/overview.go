package site

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// The overview (index.html) shows a short summary of each page and links to
// the full one. These read the other packages' tables directly, so they only
// ever show what those packages already stored; a missing table is no data.

const (
	topShown      = 10 // rows per ranking card
	upcomingDays  = 7  // calendar card: today and the next days
	upcomingShown = 8  // calendar card: events listed
)

// PaperCard is one paper portfolio's line on the overview.
type PaperCard struct {
	Name, Title   string
	Date          string // latest equity day
	Return        float64
	BenchReturn   float64
	HasBench      bool
	Holdings      int
	PendingOrders int
}

// UpcomingEvent is one calendar entry on the overview.
type UpcomingEvent struct {
	Date, Kind, Ticker, Name, Title string
}

// HealthCard is the latest data snapshot's verdict.
type HealthCard struct {
	Stamp      string
	HardFailed int
	SoftFailed int
	Prices     string
}

func tableExists(db *sql.DB, name string) (bool, error) {
	schema, table := "main", name
	if s, t, ok := strings.Cut(name, "."); ok {
		schema, table = s, t
	}
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM `+schema+`.sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n)
	return n > 0, err
}

// paperTitles are the overview's short names; the paper page has the full
// rules. A strategy missing here shows its config name, tidied.
var paperTitles = map[string]string{
	"revenue_momentum":         "Revenue momentum",
	"revenue_momentum_foreign": "Revenue momentum + 外資",
	"cycle_bottom":             "Cycle bottom",
	"record_revenue":           "Record revenue",
	"breakout":                 "Breakout swing",
	"window_dressing":          "投信 window dressing",
	"pullback":                 "Pullback buys",
}

func paperTitle(name string) string {
	if t, ok := paperTitles[name]; ok {
		return t
	}
	t := strings.ReplaceAll(name, "_", " ")
	if t != "" {
		t = strings.ToUpper(t[:1]) + t[1:]
	}
	return t
}

// loadPaper returns each configured portfolio that has started, in config order.
func loadPaper(db *sql.DB, names []string) ([]PaperCard, error) {
	for _, t := range []string{"state.paper_portfolios", "state.paper_equity", "state.paper_positions", "state.paper_orders"} {
		if ok, err := tableExists(db, t); err != nil || !ok {
			return nil, err
		}
	}
	var out []PaperCard
	for _, name := range names {
		var c PaperCard
		var capital, liq float64
		var bench sql.NullFloat64
		err := db.QueryRow(`SELECT p.initial_capital, e.date, e.liquidation, e.bench
			FROM state.paper_portfolios p JOIN state.paper_equity e ON e.portfolio = p.name
			WHERE p.name = ? ORDER BY e.date DESC LIMIT 1`, name).Scan(&capital, &c.Date, &liq, &bench)
		if err == sql.ErrNoRows || (err == nil && capital <= 0) {
			continue
		}
		if err != nil {
			return nil, err
		}
		c.Name, c.Title = name, paperTitle(name)
		c.Return = liq/capital - 1
		if bench.Valid {
			c.HasBench, c.BenchReturn = true, bench.Float64/capital-1
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM state.paper_positions WHERE portfolio = ?`, name).Scan(&c.Holdings); err != nil {
			return nil, err
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM state.paper_orders WHERE portfolio = ? AND status = 'pending'`, name).Scan(&c.PendingOrders); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

var kindLabels = map[string]string{
	"conference": "法說會",
	"exdiv":      "除權息",
	"meeting":    "股東會",
}

// loadUpcoming returns company events from today through the next
// upcomingDays-1 days, ETFs left out (as the calendar page does by default).
func loadUpcoming(db *sql.DB, market string, now time.Time) (events []UpcomingEvent, total int, err error) {
	if ok, err := tableExists(db, "corporate_events"); err != nil || !ok {
		return nil, 0, err
	}
	today := now.In(taipei).Format(dateLayout)
	until := now.In(taipei).AddDate(0, 0, upcomingDays-1).Format(dateLayout)
	q, err := db.Query(`SELECT date, kind, ticker, name, title FROM corporate_events
		WHERE market = ? AND date >= ? AND date <= ? AND ticker NOT LIKE '00%'
		ORDER BY date, CASE kind WHEN 'conference' THEN 0 WHEN 'exdiv' THEN 1 ELSE 2 END, ticker`, market, today, until)
	if err != nil {
		return nil, 0, err
	}
	defer q.Close()
	for q.Next() {
		var e UpcomingEvent
		if err := q.Scan(&e.Date, &e.Kind, &e.Ticker, &e.Name, &e.Title); err != nil {
			return nil, 0, err
		}
		total++
		if len(events) < upcomingShown {
			if l, ok := kindLabels[e.Kind]; ok {
				e.Kind = l
			}
			e.Date = e.Date[5:] // MM-DD
			events = append(events, e)
		}
	}
	return events, total, q.Err()
}

// loadHealth reads the newest validation snapshot; nil before the first one.
func loadHealth(db *sql.DB) (*HealthCard, error) {
	if ok, err := tableExists(db, "snapshots"); err != nil || !ok {
		return nil, err
	}
	var body string
	err := db.QueryRow(`SELECT manifest FROM snapshots ORDER BY id DESC LIMIT 1`).Scan(&body)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type check struct {
		OK bool `json:"ok"`
	}
	var m struct {
		ID     string            `json:"id"`
		Latest map[string]string `json:"latest"`
		Hard   []check           `json:"hard_checks"`
		Soft   []check           `json:"soft_checks"`
	}
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return nil, err
	}
	h := &HealthCard{Stamp: m.ID, Prices: m.Latest["prices"]}
	if t, err := time.Parse(time.RFC3339, m.ID); err == nil {
		h.Stamp = t.In(taipei).Format("2006-01-02 15:04")
	}
	for _, c := range m.Hard {
		if !c.OK {
			h.HardFailed++
		}
	}
	for _, c := range m.Soft {
		if !c.OK {
			h.SoftFailed++
		}
	}
	return h, nil
}

// topGrowth is the first n ranked rows (low-base rows have no rank).
func topGrowth(rows []Row, n int) []Row {
	var out []Row
	for _, r := range rows {
		if r.Rank > 0 {
			out = append(out, r)
			if len(out) == n {
				break
			}
		}
	}
	return out
}

// topUnpriced is the first n rows of the "not yet priced in" ranking.
func topUnpriced(rows []Row, n int) []Row {
	out := make([]Row, n)
	k := 0
	for _, r := range rows {
		if r.UnpricedRank > 0 && r.UnpricedRank <= n {
			out[r.UnpricedRank-1] = r
			k++
		}
	}
	return out[:k]
}
