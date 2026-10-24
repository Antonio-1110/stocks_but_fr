package paper

import (
	"database/sql"
	"fmt"
	"log"
	"math"
	"regexp"
	"sort"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/alerts"
	"github.com/Antonio-1110/stocks_but_fr/internal/config"
)

// The active strategies (issues #70-#73) follow backtest/strategies/
// record_revenue.py, breakout.py, window_dressing.py and pullback.py. Each
// holds a few stocks for days or weeks, gives every buy 1/max_positions of
// equity, and is checked on every close. Indicators are read from the
// market window (activeDays covers their longest lookback), so a trailing
// stop's "highest close since buying" only sees the window: fine for holds
// shorter than a year.

var common = regexp.MustCompile(`^[1-9]\d{3}$`)

// activeDays is how many trading days of prices the active strategies need.
func activeDays(cfg config.Config) int {
	s := cfg.Strategy
	return max(s.Breakout.RegimeDays, s.Breakout.BreakoutDays+1, s.Breakout.VolumeDays+1, s.Breakout.ATRDays+1,
		s.Pullback.TrendDays, s.Pullback.StrengthDays+1, s.RecordRevenue.ADVDays, s.WindowDressing.FlowDays) + 60
}

// liquid returns the common stocks listed and traded on day whose average
// traded value over the last n trading days (no trade counts as 0) is at least min.
func liquid(db *sql.DB, m *market, day string, min float64, n int) ([]string, map[string]float64, error) {
	listed, err := listedOn(db, day)
	if err != nil {
		return nil, nil, err
	}
	days := m.window(day, n)
	adv := map[string]float64{}
	var out []string
	if len(days) < n {
		return nil, adv, nil
	}
	for t, bars := range m.bars {
		if _, ok := bars[day]; !ok || !listed[t] || !common.MatchString(t) {
			continue
		}
		var sum float64
		for _, d := range days {
			sum += bars[d].turnout
		}
		if a := sum / float64(n); a >= min {
			adv[t] = a
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out, adv, nil
}

func listedOn(db *sql.DB, day string) (map[string]bool, error) {
	q, err := db.Query(`SELECT ticker FROM companies WHERE market = 'TW'
		AND (listed_on = '' OR listed_on <= ?) AND (delisted_on = '' OR delisted_on > ?)`, day, day)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	out := map[string]bool{}
	for q.Next() {
		var t string
		if err := q.Scan(&t); err != nil {
			return nil, err
		}
		out[t] = true
	}
	return out, q.Err()
}

// peak is the highest adjusted close from opened to day.
func peak(m *market, t, opened, day string) float64 {
	hi := math.Inf(-1)
	for _, d := range m.dates {
		if d < opened || d > day {
			continue
		}
		if b, ok := m.bars[t][d]; ok && b.adj > hi {
			hi = b.adj
		}
	}
	return hi
}

// heldDays counts trading days after opened up to day.
func heldDays(m *market, opened, day string) int {
	if len(m.dates) == 0 || opened < m.dates[0] {
		return math.MaxInt32 // older than the window: long past any time limit
	}
	n := 0
	for _, d := range m.dates {
		if d > opened && d <= day {
			n++
		}
	}
	return n
}

// stats over the bars a ticker has in a list of dates.
func window(m *market, t string, days []string, f func(bar) float64) (sum, hi float64, n int) {
	hi = math.Inf(-1)
	for _, d := range days {
		if b, ok := m.bars[t][d]; ok {
			v := f(b)
			sum += v
			hi = math.Max(hi, v)
			n++
		}
	}
	return
}

func adjOf(b bar) float64  { return b.adj }
func turnOf(b bar) float64 { return b.turnout }

// previous returns the n dates before day (not including it).
func previous(m *market, day string, n int) []string {
	w := m.window(day, n+1)
	if len(w) > 0 && w[len(w)-1] == day {
		w = w[:len(w)-1]
	}
	if len(w) > n {
		w = w[len(w)-n:]
	}
	return w
}

type ranked struct {
	t     string
	score float64
}

// best sorts by score, highest first (lowest with asc), ties by ticker.
func best(xs []ranked, asc bool) {
	sort.Slice(xs, func(i, j int) bool {
		if xs[i].score != xs[j].score {
			return (xs[i].score < xs[j].score) == asc
		}
		return xs[i].t < xs[j].t
	})
}

func buys(xs []ranked, free int, h holdings, n int, why func(ranked) string, b *book) []*order {
	var out []*order
	for _, x := range xs {
		if free <= 0 {
			break
		}
		if b.pos[x.t] != nil {
			continue
		}
		out = append(out, &order{Ticker: x.t, Side: "buy", Target: h.equity / float64(n), Reason: why(x)})
		free--
	}
	return out
}

// --- record revenue (issue #70) ----------------------------------------------

// recordRevenue: on the first trading day after the revenue deadline, buy
// last month's record-revenue stocks; sell after hold_days unless the stock
// posts another record, or on the trailing stop. The day a holding last
// qualified again is kept in the book's blocked map (ticker -> date), which
// this strategy doesn't otherwise use.
type recordRevenue struct {
	p      config.RecordRevenue
	db     *sql.DB
	minCov float64
}

func (s recordRevenue) decide(m *market, b *book, h holdings) ([]*order, error) {
	day := m.last()
	var hits []ranked
	if b.LastRebalance != day[:7] && rebalanceDay(m, day, s.p.RevenueDeadlineDay) {
		at, _ := time.ParseInLocation(dateLayout, day, taipei)
		ready, note, err := alerts.RevenueReady(s.db, alerts.RevenueMonth(at, s.p.RevenueDeadlineDay), at, s.minCov)
		if err != nil {
			return nil, err
		}
		if ready {
			if hits, err = s.records(m, day); err != nil {
				return nil, err
			}
			b.LastRebalance = day[:7]
			for _, x := range hits {
				if b.pos[x.t] != nil {
					b.blocked[x.t] = day
				}
			}
		} else {
			log.Printf("paper: record_revenue: %s; buys wait", note)
		}
	}
	for t := range b.blocked {
		if b.pos[t] == nil {
			delete(b.blocked, t)
		}
	}

	var out []*order
	for _, t := range sortedKeys(b.pos) {
		p := b.pos[t]
		px, ok := m.on(t, day)
		if h.leave[t] || !ok {
			continue
		}
		start := p.Opened
		if r := b.blocked[t]; r > start {
			start = r
		}
		if hi := peak(m, t, p.Opened, day); px.adj <= hi*(1-s.p.TrailStop) {
			out = append(out, &order{Ticker: t, Side: "sell",
				Reason: fmt.Sprintf("trailing stop: %s from its high since bought", pct(px.adj/hi-1))})
		} else if heldDays(m, start, day) >= s.p.HoldDays {
			out = append(out, &order{Ticker: t, Side: "sell",
				Reason: fmt.Sprintf("held %d trading days since its last record", s.p.HoldDays)})
		}
	}
	if len(hits) == 0 {
		return out, nil
	}
	free := s.p.MaxPositions - len(b.pos) - h.buys + len(out)
	month := ""
	if at, err := time.Parse(dateLayout, day); err == nil {
		month = at.AddDate(0, -1, 0).Format("2006-01")
	}
	return append(out, buys(hits, free, h, s.p.MaxPositions, func(x ranked) string {
		return fmt.Sprintf("record revenue for %s, YoY %+.1f%%", month, x.score)
	}, b)...), nil
}

// records are the liquid stocks whose revenue for the month before day is
// above each of the record_months-1 months before it, with YoY of at least
// min_yoy_pct off a base of at least min_base_ntd; best YoY first.
func (s recordRevenue) records(m *market, day string) ([]ranked, error) {
	cands, _, err := liquid(s.db, m, day, s.p.MinADVNTD, s.p.ADVDays)
	if err != nil || len(cands) == 0 {
		return nil, err
	}
	at, _ := time.Parse(dateLayout, day)
	newest := time.Date(at.Year(), at.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	first := newest.AddDate(0, -(s.p.RecordMonths - 1), 0)
	ok := map[string]bool{}
	for _, t := range cands {
		ok[t] = true
	}
	q, err := s.db.Query(`SELECT ticker, month, revenue, yoy_pct FROM monthly_revenue WHERE market = 'TW'
		AND announced_on != '' AND announced_on < ? AND month >= ? AND month <= ? AND revenue IS NOT NULL`,
		day, first.Format(dateLayout), newest.Format(dateLayout))
	if err != nil {
		return nil, err
	}
	defer q.Close()
	type row struct {
		cur, before float64
		n           int
		yoy         sql.NullFloat64
		has         bool
	}
	rows := map[string]*row{}
	nm := newest.Format(dateLayout)
	for q.Next() {
		var t, mo string
		var r float64
		var y sql.NullFloat64
		if err := q.Scan(&t, &mo, &r, &y); err != nil {
			return nil, err
		}
		if !ok[t] {
			continue
		}
		x := rows[t]
		if x == nil {
			x = &row{before: math.Inf(-1)}
			rows[t] = x
		}
		if mo == nm {
			x.cur, x.yoy, x.has = r, y, true
		} else {
			x.before = math.Max(x.before, r)
			x.n++
		}
	}
	if err := q.Err(); err != nil {
		return nil, err
	}
	var out []ranked
	for t, x := range rows {
		if !x.has || !x.yoy.Valid || x.n < s.p.RecordMonths-1 || !(x.cur > x.before) {
			continue
		}
		base := x.cur / (1 + x.yoy.Float64/100)
		if x.yoy.Float64 >= s.p.MinYoYPct && base >= s.p.MinBaseNTD {
			out = append(out, ranked{t, x.yoy.Float64})
		}
	}
	best(out, false)
	return out, nil
}

// --- breakout (issue #71) ----------------------------------------------------

// breakout: while the regime ticker is above its average, buy new
// breakout_days highs on volume_mult x normal traded value; sell on a
// trailing stop of atr_mult average daily moves below the high since bought.
type breakout struct {
	p  config.Breakout
	db *sql.DB
}

func (s breakout) decide(m *market, b *book, h holdings) ([]*order, error) {
	day := m.last()
	var out []*order
	for _, t := range sortedKeys(b.pos) {
		px, ok := m.on(t, day)
		atr, aok := s.atr(m, t, day)
		if h.leave[t] || !ok || !aok {
			continue
		}
		hi := peak(m, t, b.pos[t].Opened, day)
		if px.adj < hi-s.p.ATRMult*atr {
			out = append(out, &order{Ticker: t, Side: "sell",
				Reason: fmt.Sprintf("trailing stop: %s from its high since bought (%.1f x its average daily move)",
					pct(px.adj/hi-1), (hi-px.adj)/atr)})
		}
	}
	free := s.p.MaxPositions - len(b.pos) - h.buys + len(out)
	if free <= 0 || !s.uptrend(m, day) {
		return out, nil
	}
	cands, _, err := liquid(s.db, m, day, s.p.MinADVNTD, s.p.ADVDays)
	if err != nil {
		return nil, err
	}
	prevHigh := previous(m, day, s.p.BreakoutDays)
	prevVol := previous(m, day, s.p.VolumeDays)
	if len(prevHigh) < s.p.BreakoutDays || len(prevVol) < s.p.VolumeDays {
		return out, nil
	}
	var hits []ranked
	for _, t := range cands {
		if b.pos[t] != nil {
			continue
		}
		today := m.bars[t][day]
		_, hi, n := window(m, t, prevHigh, adjOf)
		if n < int(0.8*float64(s.p.BreakoutDays)) || !(today.adj > hi) {
			continue
		}
		sum, _, k := window(m, t, prevVol, turnOf)
		if k < int(0.8*float64(s.p.VolumeDays)) || sum <= 0 {
			continue
		}
		if r := today.turnout / (sum / float64(k)); r >= s.p.VolumeMult {
			hits = append(hits, ranked{t, r})
		}
	}
	best(hits, false)
	return append(out, buys(hits, free, h, s.p.MaxPositions, func(x ranked) string {
		return fmt.Sprintf("new %d-day high on %.1fx its usual traded value", s.p.BreakoutDays, x.score)
	}, b)...), nil
}

// atr is the average absolute change of the adjusted close over the last
// atr_days dates, carrying the last close over days without a trade.
func (s breakout) atr(m *market, t, day string) (float64, bool) {
	days := m.window(day, s.p.ATRDays+1)
	var sum float64
	n := 0
	prev := math.NaN()
	if len(days) > 0 {
		if b, ok := m.before(t, days[0]); ok {
			prev = b.adj
		}
	}
	for i, d := range days {
		cur := prev
		if b, ok := m.bars[t][d]; ok {
			cur = b.adj
		}
		if i > 0 && !math.IsNaN(prev) && !math.IsNaN(cur) {
			sum += math.Abs(cur - prev)
			n++
		}
		prev = cur
	}
	if n < int(0.8*float64(s.p.ATRDays)) || n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

func (s breakout) uptrend(m *market, day string) bool {
	if s.p.RegimeDays <= 0 {
		return true
	}
	px, ok := m.on(s.p.RegimeTicker, day)
	if !ok {
		return false
	}
	ma, n := average(m, s.p.RegimeTicker, day, s.p.RegimeDays)
	return n >= s.p.RegimeDays && px.adj > ma
}

// --- window dressing (issue #72) -----------------------------------------------

// windowDressing: once a traded quarter end is entry_days calendar days away,
// buy once the stocks 投信 bought hardest over flow_days; sell everything
// once it is exit_days away, or a holding on the stop. The quarter end bought
// for is kept in the book's LastRebalance.
type windowDressing struct {
	p  config.WindowDressing
	db *sql.DB
}

func quarterEnd(day time.Time) time.Time {
	q := (int(day.Month())-1)/3*3 + 3
	return time.Date(day.Year(), time.Month(q)+1, 0, 0, 0, 0, 0, time.UTC)
}

func (s windowDressing) decide(m *market, b *book, h holdings) ([]*order, error) {
	day := m.last()
	at, err := time.Parse(dateLayout, day)
	if err != nil {
		return nil, err
	}
	qend := quarterEnd(at)
	left := int(qend.Sub(at).Hours() / 24)
	var out []*order
	for _, t := range sortedKeys(b.pos) {
		if h.leave[t] {
			continue
		}
		p := b.pos[t]
		if left <= s.p.ExitDays {
			out = append(out, &order{Ticker: t, Side: "sell",
				Reason: fmt.Sprintf("quarter end %s is %d day(s) away", qend.Format(dateLayout), left)})
		} else if _, ok := m.on(t, day); ok && h.value[t] <= p.Cost*(1-s.p.StopLoss) {
			out = append(out, &order{Ticker: t, Side: "sell",
				Reason: fmt.Sprintf("stop loss: %s since bought", pct(h.value[t]/p.Cost-1))})
		}
	}
	traded := false
	for _, q := range s.p.Quarters {
		traded = traded || int(qend.Month()) == q
	}
	key := qend.Format(dateLayout)
	if !traded || left <= s.p.ExitDays || left > s.p.EntryDays || b.LastRebalance == key {
		return out, nil
	}
	b.LastRebalance = key
	hits, err := s.ranked(m, day)
	if err != nil {
		return nil, err
	}
	free := s.p.MaxPositions - len(b.pos) - h.buys + len(out)
	return append(out, buys(hits, free, h, s.p.MaxPositions, func(x ranked) string {
		return fmt.Sprintf("投信 net bought %.1f%% of its traded value over %d days, before the %s quarter end",
			x.score*100, s.p.FlowDays, key)
	}, b)...), nil
}

func (s windowDressing) ranked(m *market, day string) ([]ranked, error) {
	cands, adv, err := liquid(s.db, m, day, s.p.MinADVNTD, s.p.ADVDays)
	if err != nil || len(cands) == 0 {
		return nil, err
	}
	sort.SliceStable(cands, func(i, j int) bool { return adv[cands[i]] > adv[cands[j]] })
	if s.p.SkipLargest >= len(cands) {
		return nil, nil
	}
	cands = cands[s.p.SkipLargest:]
	days := m.window(day, s.p.FlowDays)
	if len(days) == 0 {
		return nil, nil
	}
	q, err := s.db.Query(`SELECT ticker, date, trust_net FROM institutional_flows WHERE market = 'TW'
		AND date >= ? AND date <= ? AND trust_net IS NOT NULL`, days[0], day)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	want := map[string]bool{}
	for _, t := range cands {
		want[t] = true
	}
	bought := map[string]float64{}
	seen := map[string]bool{}
	for q.Next() {
		var t, d string
		var net float64
		if err := q.Scan(&t, &d, &net); err != nil {
			return nil, err
		}
		if !want[t] {
			continue
		}
		if b, ok := m.bars[t][d]; ok {
			bought[t] += net * b.close
			seen[t] = true
		}
	}
	if err := q.Err(); err != nil {
		return nil, err
	}
	var out []ranked
	for _, t := range cands {
		sum, _, _ := window(m, t, days, turnOf)
		if !seen[t] || sum <= 0 {
			continue
		}
		if x := bought[t] / sum; x > 0 {
			out = append(out, ranked{t, x})
		}
	}
	best(out, false)
	return out, nil
}

// --- pullback (issue #73) --------------------------------------------------------

// pullback: among liquid leaders above their trend average, buy RSI(rsi_days)
// below entry_rsi; sell back above the exit_ma_days average, after
// max_hold_days, or on the stop.
type pullback struct {
	p  config.Pullback
	db *sql.DB
}

func (s pullback) decide(m *market, b *book, h holdings) ([]*order, error) {
	day := m.last()
	var out []*order
	for _, t := range sortedKeys(b.pos) {
		px, ok := m.on(t, day)
		if h.leave[t] || !ok {
			continue
		}
		p := b.pos[t]
		sum, _, n := window(m, t, m.window(day, s.p.ExitMADays), adjOf)
		switch {
		case h.value[t] <= p.Cost*(1-s.p.StopLoss):
			out = append(out, &order{Ticker: t, Side: "sell",
				Reason: fmt.Sprintf("stop loss: %s since bought", pct(h.value[t]/p.Cost-1))})
		case n == s.p.ExitMADays && px.adj > sum/float64(n):
			out = append(out, &order{Ticker: t, Side: "sell",
				Reason: fmt.Sprintf("bounced above its %d-day average", s.p.ExitMADays)})
		case heldDays(m, p.Opened, day) >= s.p.MaxHoldDays:
			out = append(out, &order{Ticker: t, Side: "sell",
				Reason: fmt.Sprintf("held %d trading days", s.p.MaxHoldDays)})
		}
	}
	free := s.p.MaxPositions - len(b.pos) - h.buys + len(out)
	if free <= 0 {
		return out, nil
	}
	cands, _, err := liquid(s.db, m, day, s.p.MinADVNTD, s.p.ADVDays)
	if err != nil {
		return nil, err
	}
	w := m.window(day, s.p.StrengthDays+1)
	if len(w) < s.p.StrengthDays+1 {
		return out, nil
	}
	var strength []ranked
	for _, t := range cands {
		now, ok1 := m.lastOn(t, day)
		then, ok2 := m.lastOn(t, w[0])
		if ok1 && ok2 && then.adj > 0 {
			strength = append(strength, ranked{t, now.adj/then.adj - 1})
		}
	}
	strong := topShare(strength, s.p.StrengthPct)
	trendDays := m.window(day, s.p.TrendDays)
	var hits []ranked
	for _, t := range strong {
		if b.pos[t] != nil {
			continue
		}
		sum, _, n := window(m, t, trendDays, adjOf)
		if n < int(0.9*float64(s.p.TrendDays)) || !(m.bars[t][day].adj > sum/float64(n)) {
			continue
		}
		if r, ok := rsi(m, t, day, s.p.RSIDays); ok && r < s.p.EntryRSI {
			hits = append(hits, ranked{t, r})
		}
	}
	best(hits, true)
	return append(out, buys(hits, free, h, s.p.MaxPositions, func(x ranked) string {
		return fmt.Sprintf("pullback in a leader: RSI(%d) %.1f", s.p.RSIDays, x.score)
	}, b)...), nil
}

// topShare returns the tickers whose percentile rank (pandas rank(pct=True),
// ties averaged) is at least 1-share.
func topShare(xs []ranked, share float64) []string {
	sort.Slice(xs, func(i, j int) bool { return xs[i].score < xs[j].score })
	n := float64(len(xs))
	var out []string
	for i := 0; i < len(xs); {
		j := i
		for j < len(xs) && xs[j].score == xs[i].score {
			j++
		}
		rank := (float64(i+1) + float64(j)) / 2 / n
		if rank >= 1-share {
			for _, x := range xs[i:j] {
				out = append(out, x.t)
			}
		}
		i = j
	}
	sort.Strings(out)
	return out
}

// rsi is Wilder's RSI over the ticker's bars in the window up to day, as
// pullback.py computes it (pandas ewm, alpha 1/n, adjust=False).
func rsi(m *market, t, day string, n int) (float64, bool) {
	if _, ok := m.bars[t][day]; !ok {
		return 0, false
	}
	var up, down float64
	k := 0
	prev := math.NaN()
	a := 1 / float64(n)
	for _, d := range m.dates {
		if d > day {
			break
		}
		b, ok := m.bars[t][d]
		if !ok {
			continue
		}
		if !math.IsNaN(prev) {
			ch := b.adj - prev
			u, dn := math.Max(ch, 0), math.Max(-ch, 0)
			if k == 0 {
				up, down = u, dn
			} else {
				up, down = (1-a)*up+a*u, (1-a)*down+a*dn
			}
			k++
		}
		prev = b.adj
	}
	if k < n {
		return 0, false
	}
	switch {
	case down == 0 && up > 0:
		return 100, true
	case down == 0:
		return 50, true
	}
	return 100 - 100/(1+up/down), true
}
