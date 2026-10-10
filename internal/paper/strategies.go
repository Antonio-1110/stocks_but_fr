package paper

import (
	"database/sql"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/alerts"
	"github.com/Antonio-1110/stocks_but_fr/internal/config"
)

// --- revenue momentum ----------------------------------------------------

// momentum follows backtest/strategies/revenue_momentum.py with the ranking
// the alerts send (internal/alerts). On the first trading day after the
// revenue deadline it keeps holdings still ranked within exit_rank, sells the
// rest, and buys the best new names into the free slots at 1/top_n of equity
// each. Between rebalances it sells a holding that is stop_loss below what
// it cost (and, with stop_ma_days, one that closes below that average).
//
// One difference from the backtest: kept holdings are not trimmed or topped
// up to equal weight every month, which saves a round of fees and tax.
type momentum struct {
	p  config.RevenueMomentum
	db *sql.DB
}

func (s momentum) decide(m *market, b *book, h holdings) ([]*order, error) {
	day := m.last()
	month := day[:7]
	if b.LastRebalance == month || !rebalanceDay(m, day, s.p.RevenueDeadlineDay) {
		return s.stops(m, b, h), nil
	}
	at, _ := time.ParseInLocation(dateLayout, day, taipei)
	hits, asOf, err := alerts.RevenueMomentum(s.db, at.Add(18*time.Hour), s.p)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		// Missing data, not an empty market: try again on the next close.
		log.Printf("paper: revenue_momentum: nothing ranked (%s); rebalance waits", asOf)
		return s.stops(m, b, h), nil
	}
	b.LastRebalance = month

	rank := map[string]int{}
	for i, x := range hits {
		rank[x.Ticker] = i + 1
	}
	var out []*order
	var keep []string
	for t := range b.pos {
		if h.leave[t] {
			continue
		}
		if r := rank[t]; r > 0 && r <= s.p.ExitRank {
			keep = append(keep, t)
		}
	}
	sort.Slice(keep, func(i, j int) bool { return rank[keep[i]] < rank[keep[j]] })
	if len(keep) > s.p.TopN {
		keep = keep[:s.p.TopN]
	}
	kept := map[string]bool{}
	for _, t := range keep {
		kept[t] = true
	}
	for _, t := range sortedKeys(b.pos) {
		if kept[t] || h.leave[t] {
			continue
		}
		why := "no longer passes the filters"
		if r := rank[t]; r > 0 {
			why = fmt.Sprintf("ranked #%d, below the top %d", r, s.p.ExitRank)
		}
		out = append(out, &order{Ticker: t, Side: "sell", Reason: "rebalance: " + why})
	}
	free := s.p.TopN - len(keep) - h.buys
	for _, x := range hits {
		if free <= 0 {
			break
		}
		if b.pos[x.Ticker] != nil {
			continue
		}
		out = append(out, &order{Ticker: x.Ticker, Side: "buy", Target: h.equity / float64(s.p.TopN),
			Reason: fmt.Sprintf("rebalance: ranked #%d · %s", rank[x.Ticker], x.Detail)})
		free--
	}
	return out, nil
}

func (s momentum) stops(m *market, b *book, h holdings) []*order {
	var out []*order
	day := m.last()
	for _, t := range sortedKeys(b.pos) {
		if h.leave[t] {
			continue
		}
		p := b.pos[t]
		if s.p.StopLoss > 0 && h.value[t] <= p.Gross*(1-s.p.StopLoss) {
			out = append(out, &order{Ticker: t, Side: "sell",
				Reason: fmt.Sprintf("stop loss: %s since bought", pct(h.value[t]/p.Gross-1))})
			continue
		}
		if s.p.StopMADays > 0 {
			if px, ok := m.on(t, day); ok {
				if ma, n := average(m, t, day, s.p.StopMADays); n >= s.p.StopMADays && px.adj < ma {
					out = append(out, &order{Ticker: t, Side: "sell",
						Reason: fmt.Sprintf("closed below its %d-day average", s.p.StopMADays)})
				}
			}
		}
	}
	return out
}

// rebalanceDay is true on the first trading day after this month's revenue
// deadline (the deadline itself moves to the next trading day on a holiday),
// and on any later day of the month until the rebalance has happened.
func rebalanceDay(m *market, day string, deadline int) bool {
	n := 0
	for _, d := range m.dates {
		if d[:7] != day[:7] || d > day {
			continue
		}
		var dd int
		fmt.Sscanf(d[8:], "%d", &dd)
		if dd >= deadline {
			n++
		}
	}
	return n >= 2
}

// --- cycle bottom ----------------------------------------------------------

// cycle follows backtest/strategies/cycle_bottom.py: buy cyclicals as their
// revenue YoY turns up from a long negative stretch, sell when it rolls over
// or the price is stop_loss below cost. Checked on every close; each buy gets
// 1/max_positions of equity and then runs. A stock stopped out can't come back
// until a newer revenue month is out.
type cycle struct {
	p  config.CycleBottom
	db *sql.DB
}

func (s cycle) decide(m *market, b *book, h holdings) ([]*order, error) {
	day := m.last()
	universe, err := s.universe()
	if err != nil {
		return nil, err
	}
	for t := range b.pos {
		universe[t] = true
	}
	yoy, latest, err := s.growth(day, universe)
	if err != nil {
		return nil, err
	}
	if latest == "" {
		return nil, nil
	}

	var out []*order
	left := 0
	for _, t := range sortedKeys(b.pos) {
		if h.leave[t] {
			continue
		}
		p := b.pos[t]
		switch {
		case h.value[t] < p.Gross*(1-s.p.StopLoss):
			b.blocked[t] = latest
			out = append(out, &order{Ticker: t, Side: "sell",
				Reason: fmt.Sprintf("stop loss: %s since bought", pct(h.value[t]/p.Gross-1))})
		case falling(yoy[t], s.p.RolloverMonths):
			y := yoy[t]
			out = append(out, &order{Ticker: t, Side: "sell",
				Reason: fmt.Sprintf("revenue YoY fell %d months in a row, now %+.1f%%", s.p.RolloverMonths, y[len(y)-1])})
		default:
			left++
		}
	}

	slots := s.p.MaxPositions - left - h.buys
	if slots <= 0 {
		return out, nil
	}
	type cand struct {
		t     string
		score float64
	}
	var cands []cand
	turnDays := m.window(day, s.p.TurnoverDays)
	for t := range universe {
		if b.pos[t] != nil || (b.blocked[t] != "" && b.blocked[t] >= latest) {
			continue
		}
		today, ok := m.on(t, day)
		if !ok || len(turnDays) < s.p.TurnoverDays {
			continue
		}
		var turn float64
		for _, d := range turnDays {
			turn += m.bars[t][d].turnout
		}
		if turn/float64(len(turnDays)) < s.p.MinAvgTurnover {
			continue
		}
		if s.p.PriceMADays > 0 {
			ma, n := average(m, t, day, s.p.PriceMADays)
			if len(m.window(day, s.p.PriceMADays)) < s.p.PriceMADays || n == 0 || !(today.adj > ma) {
				continue
			}
		}
		if score, ok := s.entry(yoy[t]); ok {
			cands = append(cands, cand{t, score})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].t < cands[j].t
	})
	for _, c := range cands[:min(slots, len(cands))] {
		y := yoy[c.t]
		out = append(out, &order{Ticker: c.t, Side: "buy", Target: h.equity / float64(s.p.MaxPositions),
			Reason: fmt.Sprintf("revenue YoY turned up: %+.1f%% → %+.1f%% over %d months (to %s)",
				y[len(y)-1-s.p.ImproveMonths], y[len(y)-1], s.p.ImproveMonths, latest[:7])})
	}
	return out, nil
}

func (s cycle) universe() (map[string]bool, error) {
	inds := map[string]bool{}
	for _, i := range s.p.Industries {
		inds[i] = true
	}
	extra := map[string]bool{}
	for _, t := range s.p.Tickers {
		extra[t] = true
	}
	q, err := s.db.Query(`SELECT ticker, industry FROM companies WHERE market = 'TW' AND delisted_on = ''`)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	out := map[string]bool{}
	for q.Next() {
		var t, ind string
		if err := q.Scan(&t, &ind); err != nil {
			return nil, err
		}
		if (inds[ind] || extra[t]) && !strings.HasPrefix(t, "00") {
			out[t] = true
		}
	}
	return out, q.Err()
}

// growth returns, per ticker, YoY % of the trailing yoy_window-month revenue
// sum for the months up to the newest one announced before day (oldest
// first, NaN where a month is missing), and that newest month.
func (s cycle) growth(day string, tickers map[string]bool) (map[string][]float64, string, error) {
	var latest sql.NullString
	if err := s.db.QueryRow(`SELECT MAX(month) FROM monthly_revenue WHERE market = 'TW'
		AND announced_on != '' AND announced_on < ?`, day).Scan(&latest); err != nil || !latest.Valid {
		return nil, "", err
	}
	end, err := time.Parse(dateLayout, latest.String)
	if err != nil {
		return nil, "", err
	}
	n := s.p.TroughMonths + s.p.ImproveMonths + 1
	n = max(n, s.p.RolloverMonths+1)
	w := s.p.YoYWindow
	first := end.AddDate(0, -(n-1)-(w-1)-12, 0)

	q, err := s.db.Query(`SELECT ticker, month, revenue FROM monthly_revenue WHERE market = 'TW'
		AND announced_on != '' AND announced_on < ? AND month >= ? AND month <= ? AND revenue IS NOT NULL`,
		day, first.Format(dateLayout), latest.String)
	if err != nil {
		return nil, "", err
	}
	defer q.Close()
	rev := map[string]map[string]float64{}
	for q.Next() {
		var t, mo string
		var r float64
		if err := q.Scan(&t, &mo, &r); err != nil {
			return nil, "", err
		}
		if !tickers[t] {
			continue
		}
		if rev[t] == nil {
			rev[t] = map[string]float64{}
		}
		rev[t][mo] = r
	}
	if err := q.Err(); err != nil {
		return nil, "", err
	}

	sum := func(t string, last time.Time) (float64, bool) {
		var total float64
		for i := 0; i < w; i++ {
			r, ok := rev[t][last.AddDate(0, -i, 0).Format(dateLayout)]
			if !ok {
				return 0, false
			}
			total += r
		}
		return total, true
	}
	out := map[string][]float64{}
	for t := range rev {
		y := make([]float64, n)
		for i := range y {
			mo := end.AddDate(0, -(n - 1 - i), 0)
			cur, ok1 := sum(t, mo)
			base, ok2 := sum(t, mo.AddDate(-1, 0, 0))
			if ok1 && ok2 && base > 0 {
				y[i] = (cur/base - 1) * 100
			} else {
				y[i] = math.NaN()
			}
		}
		out[t] = y
	}
	return out, latest.String, nil
}

// entry is _entry_ok in cycle_bottom.py: the YoY improvement over the turn
// when the series qualifies.
func (s cycle) entry(y []float64) (float64, bool) {
	k, n := s.p.ImproveMonths, s.p.TroughMonths
	if !rising(y, k) || y[len(y)-1] > s.p.MaxEntryYoY {
		return 0, false
	}
	start := len(y) - 1 - k
	neg := 0
	for i := max(0, start-n+1); i <= start; i++ {
		if y[i] < 0 {
			neg++
		}
	}
	if neg < s.p.MinNegativeMonths {
		return 0, false
	}
	return y[len(y)-1] - y[len(y)-1-k], true
}

// rising: the last n steps of y all went up (needs n+1 values, none NaN).
func rising(y []float64, n int) bool { return steps(y, n, func(a, b float64) bool { return b > a }) }

func falling(y []float64, n int) bool { return steps(y, n, func(a, b float64) bool { return b < a }) }

func steps(y []float64, n int, ok func(a, b float64) bool) bool {
	if n <= 0 || len(y) < n+1 {
		return false
	}
	tail := y[len(y)-n-1:]
	for i, v := range tail {
		if math.IsNaN(v) {
			return false
		}
		if i > 0 && !ok(tail[i-1], v) {
			return false
		}
	}
	return true
}

// average is the mean adjusted close over the last n usable days up to day,
// skipping days without a trade, and how many days it used.
func average(m *market, t, day string, n int) (float64, int) {
	var sum float64
	k := 0
	for _, d := range m.window(day, n) {
		if b, ok := m.bars[t][d]; ok {
			sum += b.adj
			k++
		}
	}
	if k == 0 {
		return 0, 0
	}
	return sum / float64(k), k
}

func sortedKeys[V any](mp map[string]V) []string {
	out := make([]string, 0, len(mp))
	for k := range mp {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
