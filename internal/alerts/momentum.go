package alerts

import (
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
)

// common matches Taiwan common stocks (four digits; ETFs start with 00).
// Same as COMMON in backtest/strategies/revenue_momentum.py.
var common = regexp.MustCompile(`^[1-9]\d{3}$`)

// revenueMomentum ranks today's stocks by the entry filters and score of
// backtest/strategies/revenue_momentum.py (Strategy A, issue #18). Keep the
// two in step: the backtest is what says whether this rule is worth alerting on.
//
// Before the revenue deadline (the 10th) the strategy still uses the months
// it ranked on last time, so the list doesn't empty out while companies are
// announcing; it moves to last month's revenue the day after the deadline.
func revenueMomentum(db *sql.DB, now time.Time, p config.RevenueMomentum) ([]Hit, string, error) {
	dates, err := tradingDays(db, `prices`, p.HighDays)
	if err != nil {
		return nil, "", err
	}
	if len(dates) < p.HighDays || len(dates) < p.ADVDays {
		return nil, fmt.Sprintf("%d trading days stored, need %d", len(dates), p.HighDays), nil
	}
	latest, start, advStart := dates[0], dates[len(dates)-1], dates[p.ADVDays-1]

	newest := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
	if now.Day() <= p.RevenueDeadlineDay {
		newest = newest.AddDate(0, -1, 0)
	}
	asOf := fmt.Sprintf("prices %s · revenue to %s", latest, newest.Format("2006-01"))

	type stock struct {
		name, industry     string
		bars               int
		high, last, advSum float64
		lastDate           string
		minYoY             float64
		yoyMonths          int
		near               float64
	}
	stocks := map[string]*stock{}
	q, err := db.Query(`SELECT ticker, name, industry FROM companies WHERE market = 'TW' AND delisted_on = ''`)
	if err != nil {
		return nil, "", err
	}
	for q.Next() {
		var t, n, ind string
		if err := q.Scan(&t, &n, &ind); err != nil {
			q.Close()
			return nil, "", err
		}
		if common.MatchString(t) {
			stocks[t] = &stock{name: n, industry: ind}
		}
	}
	q.Close()

	q, err = db.Query(`SELECT ticker, date, close, adj_close, turnover FROM prices
		WHERE market = 'TW' AND date >= ? ORDER BY ticker, date`, start)
	if err != nil {
		return nil, "", err
	}
	for q.Next() {
		var t, d string
		var cl, adj, val sql.NullFloat64
		if err := q.Scan(&t, &d, &cl, &adj, &val); err != nil {
			q.Close()
			return nil, "", err
		}
		s := stocks[t]
		if s == nil {
			continue
		}
		a := adj.Float64
		if !adj.Valid || a == 0 {
			a = cl.Float64
		}
		if a <= 0 {
			continue
		}
		s.bars++
		s.high = max(s.high, a)
		s.last, s.lastDate = a, d
		if d >= advStart {
			s.advSum += val.Float64
		}
	}
	q.Close()
	if err := q.Err(); err != nil {
		return nil, "", err
	}

	months := make([]string, p.YoYMonths)
	for i := range months {
		months[i] = newest.AddDate(0, -i, 0).Format(dateLayout)
	}
	q, err = db.Query(`SELECT ticker, yoy_pct FROM monthly_revenue
		WHERE market = 'TW' AND yoy_pct IS NOT NULL AND announced_on != '' AND announced_on <= ?
		AND month IN (?`+strings.Repeat(",?", len(months)-1)+`)`,
		append([]any{now.Format(dateLayout)}, toAny(months)...)...)
	if err != nil {
		return nil, "", err
	}
	for q.Next() {
		var t string
		var yoy float64
		if err := q.Scan(&t, &yoy); err != nil {
			q.Close()
			return nil, "", err
		}
		if s := stocks[t]; s != nil {
			if s.yoyMonths == 0 || yoy < s.minYoY {
				s.minYoY = yoy
			}
			s.yoyMonths++
		}
	}
	q.Close()
	if err := q.Err(); err != nil {
		return nil, "", err
	}

	trust := map[string]int64{}
	if p.TrustFilter {
		if trust, err = trustNet(db, p.TrustDays); err != nil {
			return nil, "", err
		}
	}

	var pass []string
	for t, s := range stocks {
		if s.bars < int(0.9*float64(p.HighDays)) || s.lastDate != latest || s.high <= 0 {
			continue
		}
		s.near = s.last / s.high
		if s.near < 1-p.MaxBelowHigh || s.advSum/float64(p.ADVDays) < p.MinADVNTD {
			continue
		}
		if s.yoyMonths != p.YoYMonths || s.minYoY < p.MinYoYPct {
			continue
		}
		if p.TrustFilter && trust[t] <= 0 {
			continue
		}
		pass = append(pass, t)
	}

	yoyRank := pctRank(pass, func(t string) float64 { return stocks[t].minYoY })
	nearRank := pctRank(pass, func(t string) float64 { return stocks[t].near })
	score := map[string]float64{}
	for _, t := range pass {
		score[t] = (1-p.HighWeight)*yoyRank[t] + p.HighWeight*nearRank[t]
	}
	sort.Slice(pass, func(i, j int) bool {
		if score[pass[i]] != score[pass[j]] {
			return score[pass[i]] > score[pass[j]]
		}
		return pass[i] < pass[j]
	})

	hits := make([]Hit, len(pass))
	for i, t := range pass {
		s := stocks[t]
		hits[i] = Hit{Ticker: t, Name: s.name, Industry: s.industry,
			Detail: fmt.Sprintf("revenue YoY at least %+.0f%% each of the last %d months · %.1f%% below 52-week high",
				s.minYoY, p.YoYMonths, (1-s.near)*100)}
	}
	return hits, asOf, nil
}

// tradingDays returns the market's last n dates in table, newest first.
func tradingDays(db *sql.DB, table string, n int) ([]string, error) {
	q, err := db.Query(`SELECT DISTINCT date FROM `+table+` WHERE market = 'TW' ORDER BY date DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	var out []string
	for q.Next() {
		var d string
		if err := q.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, q.Err()
}

// trustNet sums 投信 net shares over the last days flow dates.
func trustNet(db *sql.DB, days int) (map[string]int64, error) {
	dates, err := tradingDays(db, `institutional_flows`, days)
	if err != nil || len(dates) == 0 {
		return map[string]int64{}, err
	}
	q, err := db.Query(`SELECT ticker, SUM(trust_net) FROM institutional_flows
		WHERE market = 'TW' AND date >= ? GROUP BY ticker`, dates[len(dates)-1])
	if err != nil {
		return nil, err
	}
	defer q.Close()
	out := map[string]int64{}
	for q.Next() {
		var t string
		var n sql.NullInt64
		if err := q.Scan(&t, &n); err != nil {
			return nil, err
		}
		out[t] = n.Int64
	}
	return out, q.Err()
}

// pctRank is pandas' rank(pct=True): average 1-based rank over n, ties shared.
func pctRank(keys []string, val func(string) float64) map[string]float64 {
	order := append([]string(nil), keys...)
	sort.SliceStable(order, func(a, b int) bool { return val(order[a]) < val(order[b]) })
	out := map[string]float64{}
	n := float64(len(order))
	for i := 0; i < len(order); {
		j := i
		for j+1 < len(order) && val(order[j+1]) == val(order[i]) {
			j++
		}
		r := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			out[order[k]] = r / n
		}
		i = j + 1
	}
	return out
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
