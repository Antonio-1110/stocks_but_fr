package paper

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

//go:embed templates/paper.html
var templates embed.FS

// ordersShown caps the trade list per portfolio; pending orders always show.
const ordersShown = 60

type Page struct {
	Title       string
	GeneratedAt string
	PriceDate   string
	Portfolios  []View
	Costs       config.TWCosts
	Benchmark   string
}

// View is one portfolio as the page shows it.
type View struct {
	Name, Title, Rules string
	StartedOn          string
	Capital            float64
	Value              float64 // liquidation value at the last close
	Return             float64
	Bench              float64 // benchmark liquidation value; 0 before it is bought
	BenchReturn        float64
	HasBench           bool
	Cash               float64
	Fees, Tax          float64
	Holdings           []Holding
	Orders             []OrderRow
	Chart              Chart
}

type Holding struct {
	Ticker, Name, Link string
	Opened             string
	Cost, Value        float64
	Return             float64 // after the costs of selling now
	Weight             float64
}

type OrderRow struct {
	Ticker, Name, Link   string
	Side, Reason, Status string
	SignalDate, FillDate string
	Price, Value         float64
	Costs                float64
	PnL                  float64
	HasPnL               bool
	Note                 string
}

// Chart is the equity curve: return since start, portfolio vs benchmark.
type Chart struct {
	Points          int
	Path, BenchPath string
	Ticks           []Tick
	ZeroY           float64
	First, Last     string
	EndY, BenchEndY float64
	EndLabel        string
	BenchEndLabel   string
	Data            string // JSON for the hover readout
}

type Tick struct {
	Y     float64
	Label string
}

var titles = map[string]struct{ title, rules string }{
	"revenue_momentum": {"Revenue momentum",
		"Monthly, the day after revenue is due: hold the top names with strong revenue YoY 3 months running and a price near its 52-week high. Stop loss between rebalances."},
	"cycle_bottom": {"Cycle bottom",
		"Daily: buy cyclicals (semis, panels, shipping, steel, plastics) when revenue YoY turns up after a long negative stretch; sell when it rolls over or on the stop loss."},
}

// Render is the `radar render` step: paper.html next to the dashboard.
func Render(_ context.Context, cfg config.Config, st *store.Store) error {
	return render(st.DB, cfg, cfg.Run.PublicDir, time.Now())
}

func render(db *sql.DB, cfg config.Config, dir string, now time.Time) error {
	if err := migrate(db); err != nil {
		return err
	}
	page := Page{
		Title:       "Paper portfolios",
		GeneratedAt: now.In(taipei).Format("2006-01-02 15:04") + " (台北)",
		Costs:       cfg.Costs.TW,
		Benchmark:   cfg.Paper.Benchmark,
	}
	m, err := loadMarket(db, 5, "", cfg.Paper.MinCoverage)
	if err != nil {
		return err
	}
	if len(m.dates) > 0 {
		page.PriceDate = m.last()
	}
	for _, name := range cfg.Paper.Strategies {
		b, err := load(db, name)
		if err != nil {
			return err
		}
		if b == nil || len(m.dates) == 0 {
			continue
		}
		v, err := view(db, m, b, cfg.Costs.TW)
		if err != nil {
			return err
		}
		page.Portfolios = append(page.Portfolios, v)
	}

	tmpl, err := template.New("paper.html").Funcs(funcs).ParseFS(templates, "templates/paper.html")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Write to a temp file and rename, so a failed render never leaves a half page.
	tmp := filepath.Join(dir, ".paper.html.tmp")
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
	return os.Rename(tmp, filepath.Join(dir, "paper.html"))
}

func view(db *sql.DB, m *market, b *book, costs config.TWCosts) (View, error) {
	t := titles[b.Name]
	if t.title == "" {
		t.title = b.Name
	}
	v := View{Name: b.Name, Title: t.title, Rules: t.rules, StartedOn: b.StartedOn,
		Capital: b.Capital, Cash: b.Cash}
	e := equityRow(m, b, costs)
	v.Value = e.liquidation
	v.Return = e.liquidation/b.Capital - 1
	if e.bench.Valid {
		v.HasBench, v.Bench = true, e.bench.Float64
		v.BenchReturn = e.bench.Float64/b.Capital - 1
	}
	pos, err := loadPositions(db, b.Name)
	if err != nil {
		return View{}, err
	}
	for _, p := range pos {
		val := worth(m, p.Ticker, p.Opened, p.Shares, m.last())
		net := val - costs.Commission(val) - sellTax(costs, p.Ticker, val)
		h := Holding{Ticker: p.Ticker, Name: m.names[p.Ticker], Link: goodinfo(p.Ticker),
			Opened: p.Opened, Cost: p.Cost, Value: val, Return: net/p.Cost - 1}
		if e.holdings+e.cash > 0 {
			h.Weight = val / (e.holdings + e.cash)
		}
		v.Holdings = append(v.Holdings, h)
	}

	if err := db.QueryRow(`SELECT COALESCE(SUM(commission), 0), COALESCE(SUM(tax), 0) FROM paper_orders
		WHERE portfolio = ? AND status = 'filled'`, b.Name).Scan(&v.Fees, &v.Tax); err != nil {
		return View{}, err
	}
	pending, err := loadOrders(db, b.Name, `status = 'pending'`, 0)
	if err != nil {
		return View{}, err
	}
	done, err := loadOrders(db, b.Name, `status != 'pending'`, ordersShown)
	if err != nil {
		return View{}, err
	}
	for _, o := range append(pending, done...) {
		r := OrderRow{Ticker: o.Ticker, Name: m.names[o.Ticker], Link: goodinfo(o.Ticker),
			Side: o.Side, Reason: o.Reason, Status: o.Status, SignalDate: o.SignalDate,
			FillDate: o.FillDate, Price: o.Price, Value: o.Value, Costs: o.Commission + o.Tax, Note: o.Note}
		if o.Status == "pending" && o.Side == "buy" {
			r.Value = o.Target
		}
		if o.Side == "sell" && o.Status == "filled" {
			r.PnL, r.HasPnL = o.PnL, true
		}
		v.Orders = append(v.Orders, r)
	}

	v.Chart, err = chart(db, b)
	return v, err
}

func goodinfo(t string) string { return "https://goodinfo.tw/tw/StockDetail.asp?STOCK_ID=" + t }

// Chart geometry, in SVG user units.
const (
	chartW, chartH = 640.0, 220.0
	padL, padR     = 46.0, 64.0
	padT, padB     = 10.0, 22.0
)

func chart(db *sql.DB, b *book) (Chart, error) {
	q, err := db.Query(`SELECT date, liquidation, bench FROM paper_equity WHERE portfolio = ? ORDER BY date`, b.Name)
	if err != nil {
		return Chart{}, err
	}
	defer q.Close()
	type pt struct {
		Date string   `json:"d"`
		P    float64  `json:"p"`
		B    *float64 `json:"b,omitempty"`
	}
	var pts []pt
	for q.Next() {
		var d string
		var liq float64
		var bench sql.NullFloat64
		if err := q.Scan(&d, &liq, &bench); err != nil {
			return Chart{}, err
		}
		p := pt{Date: d, P: liq/b.Capital - 1}
		if bench.Valid {
			x := bench.Float64/b.Capital - 1
			p.B = &x
		}
		pts = append(pts, p)
	}
	if err := q.Err(); err != nil || len(pts) == 0 {
		return Chart{}, err
	}

	lo, hi := 0.0, 0.0
	for _, p := range pts {
		lo, hi = math.Min(lo, p.P), math.Max(hi, p.P)
		if p.B != nil {
			lo, hi = math.Min(lo, *p.B), math.Max(hi, *p.B)
		}
	}
	step := niceStep((hi - lo) / 4)
	lo, hi = math.Floor(lo/step)*step, math.Ceil(hi/step)*step
	if hi-lo < step {
		hi = lo + step
	}
	y := func(r float64) float64 { return padT + (hi-r)/(hi-lo)*(chartH-padT-padB) }
	x := func(i int) float64 {
		if len(pts) == 1 {
			return chartW - padR
		}
		return padL + float64(i)/float64(len(pts)-1)*(chartW-padL-padR)
	}

	c := Chart{Points: len(pts), ZeroY: y(0), First: pts[0].Date, Last: pts[len(pts)-1].Date}
	for r := lo; r <= hi+step/2; r += step {
		c.Ticks = append(c.Ticks, Tick{Y: y(r), Label: fmt.Sprintf("%+.0f%%", r*100)})
	}
	var path, bpath strings.Builder
	for i, p := range pts {
		fmt.Fprintf(&path, "%s%.1f %.1f ", map[bool]string{true: "M", false: "L"}[i == 0], x(i), y(p.P))
		if p.B != nil {
			cmd := "L"
			if bpath.Len() == 0 {
				cmd = "M"
			}
			fmt.Fprintf(&bpath, "%s%.1f %.1f ", cmd, x(i), y(*p.B))
			c.BenchEndY, c.BenchEndLabel = y(*p.B), fmt.Sprintf("%+.1f%%", *p.B*100)
		}
	}
	last := pts[len(pts)-1]
	c.Path, c.BenchPath = strings.TrimSpace(path.String()), strings.TrimSpace(bpath.String())
	c.EndY, c.EndLabel = y(last.P), fmt.Sprintf("%+.1f%%", last.P*100)
	// Keep the two end labels from overlapping.
	if c.BenchPath != "" && math.Abs(c.EndY-c.BenchEndY) < 12 {
		if c.EndY <= c.BenchEndY {
			c.EndY, c.BenchEndY = c.EndY-6, c.BenchEndY+6
		} else {
			c.EndY, c.BenchEndY = c.EndY+6, c.BenchEndY-6
		}
	}
	js, err := json.Marshal(pts)
	if err != nil {
		return Chart{}, err
	}
	c.Data = string(js)
	return c, nil
}

func niceStep(raw float64) float64 {
	if raw <= 0 {
		return 0.01
	}
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, s := range []float64{1, 2, 2.5, 5, 10} {
		if s*mag >= raw {
			return s * mag
		}
	}
	return 10 * mag
}

var funcs = template.FuncMap{
	"ntd": func(v float64) string { return groupThousands(math.Round(v)) },
	"pct": func(v float64) string { return fmt.Sprintf("%+.2f%%", v*100) },
	"money": func(v float64) string {
		if v < 0 {
			return "-NT$" + groupThousands(math.Round(-v))
		}
		return "+NT$" + groupThousands(math.Round(v))
	},
	"rate":  func(v float64) string { return strconv.FormatFloat(v*100, 'f', -1, 64) + "%" },
	"pct0":  func(v float64) string { return fmt.Sprintf("%.1f%%", v*100) },
	"price": func(v float64) string { return fmt.Sprintf("%.2f", v) },
	"sign": func(v float64) string {
		switch {
		case v > 0:
			return "up"
		case v < 0:
			return "down"
		}
		return ""
	},
	"f1":     func(v float64) string { return fmt.Sprintf("%.1f", v) },
	"js":     func(s string) template.JS { return template.JS(s) },
	"chartW": func() float64 { return chartW },
	"chartH": func() float64 { return chartH },
	"padL":   func() float64 { return padL },
	"padR":   func() float64 { return padR },
	"padB":   func() float64 { return padB },
	"sub":    func(a, b float64) float64 { return a - b },
	"diff":   func(a, b float64) float64 { return a - b },
	"inc":    func(i int) int { return i + 1 },
}

func groupThousands(v float64) string {
	neg := v < 0
	s := fmt.Sprintf("%.0f", math.Abs(v))
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}
