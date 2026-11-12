package ratings

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

//go:embed templates/ratings.html
var templates embed.FS

var taipei = time.FixedZone("Asia/Taipei", 8*60*60)

// Bucket is how one rating level did over one horizon.
type Bucket struct {
	Rating int
	N      int
	Ret    float64 // average return
	Excess float64 // average return minus the benchmark's
	Up     float64 // share that rose
}

// Acc is the accuracy of one market's ratings over one horizon.
type Acc struct {
	Horizon              int
	Term                 string // "Short" or "Long"
	N                    int    // scored ratings
	Days                 int    // distinct rating days scored
	UpCalls, UpRight     int    // ratings 4-5 and how many rose
	DownCalls, DownRight int    // ratings 1-2 and how many fell
	BaseUp               float64
	BeatCalls, BeatRight int // ratings 1-2 and 4-5; right = 4-5 beat the benchmark, 1-2 lagged it
	Buckets              [5]Bucket
	Spread               float64 // average excess of 5s minus 1s
	First, Last          string  // rating days covered
}

func (a Acc) UpRate() float64   { return ratio(a.UpRight, a.UpCalls) }
func (a Acc) DownRate() float64 { return ratio(a.DownRight, a.DownCalls) }
func (a Acc) BeatRate() float64 { return ratio(a.BeatRight, a.BeatCalls) }
func (a Acc) DirRate() float64  { return ratio(a.UpRight+a.DownRight, a.UpCalls+a.DownCalls) }

func ratio(a, b int) float64 {
	if b == 0 {
		return math.NaN()
	}
	return float64(a) / float64(b)
}

// Record is one stock's past calls over one horizon.
type Record struct{ Right, Calls int }

// Row is one stock's ratings today.
type Row struct {
	Rating
	Name, Link        string
	ShortRec, LongRec Record
}

// Market is one market's section of the page.
type Market struct {
	Code, Title, Bench string
	Date               string // newest rating day
	Rows               []Row
	Live, Backtest     []Acc
	NextScore          string // when the first live rating gets scored, if none has been
	ShortH, LongH      int    // horizons the per-stock records use
	HasRevenue         bool
}

type accView struct {
	Accs  []Acc
	Bench string
}

type Page struct {
	GeneratedAt string
	Markets     []Market
	Cfg         config.Ratings
}

// Render is the `radar render` step that writes public/ratings.html.
func Render(_ context.Context, cfg config.Config, st *store.Store) error {
	return render(st.DB, cfg, cfg.Run.PublicDir, time.Now())
}

func render(db *sql.DB, cfg config.Config, dir string, now time.Time) error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	page := Page{GeneratedAt: now.In(taipei).Format("2006-01-02 15:04") + " (台北)", Cfg: cfg.Ratings}
	usBench := "SPY"
	if len(cfg.US.Benchmarks) > 0 {
		usBench = cfg.US.Benchmarks[0]
	}
	for _, m := range []Market{
		{Code: "TW", Title: "Taiwan", Bench: cfg.Paper.Benchmark, HasRevenue: true},
		{Code: "US", Title: "US large caps", Bench: usBench},
	} {
		if err := load(db, cfg.Ratings, &m); err != nil {
			return err
		}
		page.Markets = append(page.Markets, m)
	}
	funcs := template.FuncMap{
		"pct": func(f float64) string {
			if math.IsNaN(f) {
				return "–"
			}
			return fmt.Sprintf("%+.1f%%", f*100)
		},
		"share": func(f float64) string {
			if math.IsNaN(f) {
				return "–"
			}
			return fmt.Sprintf("%.0f%%", f*100)
		},
		"yoy": func(f float64) string {
			if math.IsNaN(f) {
				return "–"
			}
			return fmt.Sprintf("%+.0f%%", f)
		},
		"sortVal": func(f float64) string {
			if math.IsNaN(f) {
				return ""
			}
			return fmt.Sprintf("%.6f", f)
		},
		"sign": func(f float64) string {
			switch {
			case f > 0:
				return "up"
			case f < 0:
				return "down"
			}
			return ""
		},
		"label": label,
		"rec": func(r Record) string {
			if r.Calls == 0 {
				return "–"
			}
			return fmt.Sprintf("%d/%d", r.Right, r.Calls)
		},
		"recSort": func(r Record) string {
			if r.Calls == 0 {
				return ""
			}
			return fmt.Sprintf("%.4f", float64(r.Right)/float64(r.Calls))
		},
		"better": func(rate, base float64) string { // colour a hit rate against its base rate
			switch {
			case math.IsNaN(rate) || math.IsNaN(base):
				return ""
			case rate > base+0.02:
				return "good"
			case rate < base-0.02:
				return "bad"
			}
			return ""
		},
		"sub":  func(a, b float64) float64 { return a - b },
		"half": func() float64 { return 0.5 },
		"dict": func(a []Acc, bench string) accView { return accView{a, bench} },
	}
	tmpl, err := template.New("ratings.html").Funcs(funcs).ParseFS(templates, "templates/ratings.html")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".ratings.html.tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := tmpl.Execute(f, page); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "ratings.html"))
}

func label(r int) string {
	switch r {
	case 5:
		return "Strong up"
	case 4:
		return "Up"
	case 3:
		return "Neutral"
	case 2:
		return "Down"
	case 1:
		return "Strong down"
	}
	return "–"
}

func f(n sql.NullFloat64) float64 {
	if !n.Valid {
		return math.NaN()
	}
	return n.Float64
}

func load(db *sql.DB, cfg config.Ratings, m *Market) error {
	if len(cfg.ShortHorizons) > 0 {
		m.ShortH = slices.Max(cfg.ShortHorizons)
	}
	if len(cfg.LongHorizons) > 0 {
		m.LongH = slices.Max(cfg.LongHorizons)
	}
	short := map[int]bool{}
	for _, h := range cfg.ShortHorizons {
		short[h] = true
	}

	// Accuracy, and each stock's record, from every scored rating.
	type key struct {
		backfill bool
		h        int
	}
	type agg struct {
		acc  Acc
		ret  [5]float64
		exc  [5]float64
		up   [5]int
		rose int
		days map[string]bool
	}
	aggs := map[key]*agg{}
	recs := map[string]*[2]Record{}
	q, err := db.Query(`SELECT r.ticker, r.made_on, r.backfill, r.short_rating, r.long_rating, o.horizon, o.ret, o.bench
		FROM state.ratings r JOIN state.rating_outcomes o
		ON o.market = r.market AND o.ticker = r.ticker AND o.made_on = r.made_on
		WHERE r.market = ? AND o.ret IS NOT NULL`, m.Code)
	if err != nil {
		return err
	}
	for q.Next() {
		var t, d string
		var bf, sr, lr, h int
		var ret float64
		var bench sql.NullFloat64
		if err := q.Scan(&t, &d, &bf, &sr, &lr, &h, &ret, &bench); err != nil {
			q.Close()
			return err
		}
		r := lr
		if short[h] {
			r = sr
		}
		if r < 1 || r > 5 {
			continue
		}
		k := key{bf == 1, h}
		a := aggs[k]
		if a == nil {
			a = &agg{days: map[string]bool{}}
			a.acc.Horizon, a.acc.First, a.acc.Last = h, d, d
			a.acc.Term = "Long"
			if short[h] {
				a.acc.Term = "Short"
			}
			aggs[k] = a
		}
		a.days[d] = true
		a.acc.First, a.acc.Last = min(a.acc.First, d), max(a.acc.Last, d)
		a.acc.N++
		b := r - 1
		a.acc.Buckets[b].N++
		a.ret[b] += ret
		if ret > 0 {
			a.up[b]++
			a.rose++
		}
		exc := math.NaN()
		if bench.Valid {
			exc = ret - bench.Float64
			a.exc[b] += exc
		}
		right := (r >= 4 && ret > 0) || (r <= 2 && ret < 0)
		switch {
		case r >= 4:
			a.acc.UpCalls++
			if ret > 0 {
				a.acc.UpRight++
			}
		case r <= 2:
			a.acc.DownCalls++
			if ret < 0 {
				a.acc.DownRight++
			}
		}
		if r != 3 && !math.IsNaN(exc) {
			a.acc.BeatCalls++
			if (r >= 4 && exc > 0) || (r <= 2 && exc < 0) {
				a.acc.BeatRight++
			}
		}
		if r != 3 && (h == m.ShortH || h == m.LongH) {
			rc := recs[t]
			if rc == nil {
				rc = &[2]Record{}
				recs[t] = rc
			}
			slot := 0
			if h == m.LongH {
				slot = 1
			}
			rc[slot].Calls++
			if right {
				rc[slot].Right++
			}
		}
	}
	q.Close()
	if err := q.Err(); err != nil {
		return err
	}
	var keys []key
	for k := range aggs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].h < keys[j].h })
	for _, k := range keys {
		a := aggs[k]
		a.acc.Days = len(a.days)
		a.acc.BaseUp = ratio(a.rose, a.acc.N)
		for b := range a.acc.Buckets {
			bk := &a.acc.Buckets[b]
			bk.Rating = b + 1
			bk.Ret, bk.Excess, bk.Up = math.NaN(), math.NaN(), math.NaN()
			if bk.N > 0 {
				bk.Ret = a.ret[b] / float64(bk.N)
				bk.Excess = a.exc[b] / float64(bk.N)
				bk.Up = float64(a.up[b]) / float64(bk.N)
			}
		}
		a.acc.Spread = a.acc.Buckets[4].Excess - a.acc.Buckets[0].Excess
		if k.backfill {
			m.Backtest = append(m.Backtest, a.acc)
		} else {
			m.Live = append(m.Live, a.acc)
		}
	}

	// Today's ratings: the newest live day.
	var d sql.NullString
	db.QueryRow(`SELECT MAX(made_on) FROM state.ratings WHERE market = ? AND backfill = 0`, m.Code).Scan(&d)
	if !d.Valid {
		return nil
	}
	m.Date = d.String
	if len(m.Live) == 0 && len(cfg.ShortHorizons) > 0 {
		var first string
		db.QueryRow(`SELECT MIN(made_on) FROM state.ratings WHERE market = ? AND backfill = 0`, m.Code).Scan(&first)
		m.NextScore = fmt.Sprintf("about %d trading days after %s", slices.Min(cfg.ShortHorizons)+1, first)
	}
	names := map[string]string{}
	nq := `SELECT ticker, name FROM companies WHERE market = 'TW'`
	if m.Code == "US" {
		nq = `SELECT ticker, name FROM us_companies`
	}
	if n, err := db.Query(nq); err == nil {
		for n.Next() {
			var t, name string
			if n.Scan(&t, &name) == nil {
				names[t] = name
			}
		}
		n.Close()
	}
	rows, err := db.Query(`SELECT ticker, short_rating, short_score, long_rating, long_score,
		high, month_ret, week_ret, year_ret, ma200, rev_yoy FROM state.ratings
		WHERE market = ? AND made_on = ? ORDER BY long_score DESC`, m.Code, m.Date)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r Row
		var ss, ls, hi, mo, wk, yr, ma, rv sql.NullFloat64
		if err := rows.Scan(&r.Ticker, &r.Short, &ss, &r.Long, &ls, &hi, &mo, &wk, &yr, &ma, &rv); err != nil {
			return err
		}
		r.Market, r.Date = m.Code, m.Date
		r.ShortScore, r.LongScore, r.High, r.Month, r.Week, r.Year, r.MA, r.RevYoY = f(ss), f(ls), f(hi), f(mo), f(wk), f(yr), f(ma), f(rv)
		r.Name = names[r.Ticker]
		if m.Code == "TW" {
			r.Link = "https://goodinfo.tw/tw/StockDetail.asp?STOCK_ID=" + r.Ticker
		} else {
			r.Link = "https://finance.yahoo.com/quote/" + r.Ticker
		}
		if rc := recs[r.Ticker]; rc != nil {
			r.ShortRec, r.LongRec = rc[0], rc[1]
		}
		m.Rows = append(m.Rows, r)
	}
	return rows.Err()
}
