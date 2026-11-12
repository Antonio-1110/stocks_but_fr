package ratings

import (
	"math"
	"sort"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
)

const (
	yearDays  = 252 // 52-week high and 12-month momentum
	monthDays = 21
	weekDays  = 5
	maDays    = 200
	staleDays = 5 // a halted stock's last close counts for this many days
)

// Rating is one stock's ratings and their inputs on one day. Factor fields
// are NaN when there isn't enough history.
type Rating struct {
	Market, Ticker, Date string
	Backfill             bool
	Short, Long          int     // 1 (weakest fifth) .. 5 (strongest fifth)
	ShortScore           float64 // 0..1
	LongScore            float64
	High                 float64 // close / 52-week high close - 1, <= 0
	Month, Week          float64 // 1-month and 1-week return
	Year                 float64 // 12-month return, skipping the latest month
	MA                   float64 // close / 200-day average - 1
	RevYoY               float64 // latest known monthly revenue YoY, % (TW)
}

// universe returns the panel rows rated on day t: Taiwan's most traded common
// stocks above the price floor plus the watchlist; every US ticker. A stock
// needs a bar on t and a year of history.
func universe(p *panel, cfg config.Ratings, t int) []int {
	type cand struct {
		i   int
		adv float64
	}
	var cands []cand
	var out []int
	for _, tk := range p.rated {
		i := p.row[tk]
		if !p.has(i, t) || math.IsNaN(p.at(i, t-yearDays, staleDays)) {
			continue
		}
		if p.market != "TW" || p.watch[tk] {
			out = append(out, i)
			continue
		}
		if p.close[i][t] < cfg.TWMinPrice || !common.MatchString(tk) {
			continue
		}
		var sum float64
		n := 0
		for k := t; k > t-cfg.ADVDays && k >= 0; k-- {
			if v := p.value[i][k]; !math.IsNaN(v) {
				sum += v
			}
			n++
		}
		cands = append(cands, cand{i, sum / float64(max(n, 1))})
	}
	sort.Slice(cands, func(a, b int) bool {
		if cands[a].adv != cands[b].adv {
			return cands[a].adv > cands[b].adv
		}
		return p.tickers[cands[a].i] < p.tickers[cands[b].i]
	})
	for k, c := range cands {
		if k >= cfg.TWTop {
			break
		}
		out = append(out, c.i)
	}
	return out
}

// rank maps values to percentile ranks in 0..1 (ties share their average
// rank); NaN values get 0.5, neither good nor bad.
func rank(vals []float64) []float64 {
	type kv struct {
		k int
		v float64
	}
	var ok []kv
	out := make([]float64, len(vals))
	for k, v := range vals {
		out[k] = 0.5
		if !math.IsNaN(v) {
			ok = append(ok, kv{k, v})
		}
	}
	if len(ok) < 2 {
		return out
	}
	sort.Slice(ok, func(a, b int) bool { return ok[a].v < ok[b].v })
	for a := 0; a < len(ok); {
		b := a
		for b+1 < len(ok) && ok[b+1].v == ok[a].v {
			b++
		}
		r := float64(a+b) / 2 / float64(len(ok)-1)
		for k := a; k <= b; k++ {
			out[ok[k].k] = r
		}
		a = b + 1
	}
	return out
}

// quintile turns scores into ratings 1..5 by their order: the top fifth is 5.
func quintile(scores []float64) []int {
	order := make([]int, len(scores))
	for k := range order {
		order[k] = k
	}
	sort.SliceStable(order, func(a, b int) bool { return scores[order[a]] < scores[order[b]] })
	out := make([]int, len(scores))
	for pos, k := range order {
		out[k] = 1 + pos*5/len(scores)
	}
	return out
}

// rate rates the universe on day t. rev is monthly revenue YoY by ticker and
// month (Taiwan; nil elsewhere).
func rate(p *panel, cfg config.Ratings, t int, rev map[string]map[string]float64, backfill bool) []Rating {
	uni := universe(p, cfg, t)
	if len(uni) < 5 {
		return nil
	}
	d := p.dates[t]
	month := knownMonth(d, cfg.RevenueDeadlineDay)
	n := len(uni)
	out := make([]Rating, n)
	high, m1, w1, y1, ma, ry := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	ret := func(i, a, b int) float64 {
		x, y := p.at(i, a, staleDays), p.at(i, b, staleDays)
		if math.IsNaN(x) || math.IsNaN(y) || x <= 0 {
			return math.NaN()
		}
		return y/x - 1
	}
	for k, i := range uni {
		now := p.adj[i][t]
		top, sum, cnt := 0.0, 0.0, 0
		for j := t; j > t-yearDays && j >= 0; j-- {
			if v := p.adj[i][j]; !math.IsNaN(v) {
				top = math.Max(top, v)
				if j > t-maDays {
					sum += v
					cnt++
				}
			}
		}
		high[k] = now/top - 1
		m1[k] = ret(i, t-monthDays, t)
		w1[k] = ret(i, t-weekDays, t)
		y1[k] = ret(i, t-yearDays, t-monthDays)
		ma[k] = math.NaN()
		if cnt >= maDays*3/4 {
			ma[k] = now/(sum/float64(cnt)) - 1
		}
		ry[k] = math.NaN()
		if y, ok := rev[p.tickers[i]][month]; ok {
			ry[k] = y
		} else if y, ok := rev[p.tickers[i]][prevMonth(month)]; ok {
			ry[k] = y // filed late, or not yet collected: the month before
		}
		out[k] = Rating{Market: p.market, Ticker: p.tickers[i], Date: d, Backfill: backfill,
			High: high[k], Month: m1[k], Week: w1[k], Year: y1[k], MA: ma[k], RevYoY: ry[k]}
	}
	neg := make([]float64, n)
	for k := range w1 {
		neg[k] = -w1[k]
	}
	rh, rm, rw, ryr, rma, rrv := rank(high), rank(m1), rank(neg), rank(y1), rank(ma), rank(ry)
	short, long := make([]float64, n), make([]float64, n)
	lw := cfg.LongRevenueWeight
	if rev == nil {
		lw = 0
	}
	for k := range out {
		short[k] = weighted([]float64{rh[k], rm[k], rw[k]}, []float64{cfg.ShortHighWeight, cfg.ShortMonthWeight, cfg.ShortWeekWeight})
		long[k] = weighted([]float64{ryr[k], rma[k], rh[k], rrv[k]}, []float64{cfg.LongMomentumWeight, cfg.LongMAWeight, cfg.LongHighWeight, lw})
	}
	sq, lq := quintile(short), quintile(long)
	for k := range out {
		out[k].ShortScore, out[k].LongScore = short[k], long[k]
		out[k].Short, out[k].Long = sq[k], lq[k]
	}
	return out
}

func weighted(v, w []float64) float64 {
	var s, ws float64
	for k := range v {
		s += v[k] * w[k]
		ws += w[k]
	}
	if ws == 0 {
		return 0.5
	}
	return s / ws
}

func prevMonth(m string) string {
	t, err := time.Parse("2006-01", m)
	if err != nil {
		return ""
	}
	return t.AddDate(0, -1, 0).Format("2006-01")
}
