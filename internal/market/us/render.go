package us

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

//go:embed templates/us.html
var templates embed.FS

var taipei = time.FixedZone("Asia/Taipei", 8*60*60)

// Row is one ticker on the US page. Changes use the dividend-adjusted close
// (total return); NaN means not enough history.
type Row struct {
	Order               int // position in [us] tickers, roughly by size
	Ticker, Name, Link  string
	Date                string
	Stale               bool // newest bar older than the page's newest
	Close               float64
	D1, W1, M1, YTD, Y1 float64
	FromHigh            float64 // vs the 52-week high close, <= 0
	DollarVol           float64 // 20-day average close x volume, USD
}

type Page struct {
	GeneratedAt string
	DataEnd     string
	Benchmarks  []Row
	Rows        []Row
	Missing     []string // configured but no prices yet
}

// Render is the `radar render` step that writes public/us.html.
func Render(_ context.Context, cfg config.Config, st *store.Store) error {
	return render(st.DB, cfg.US, cfg.Run.PublicDir, time.Now())
}

func render(db *sql.DB, cfg config.US, dir string, now time.Time) error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	page, err := load(db, cfg)
	if err != nil {
		return err
	}
	page.GeneratedAt = now.In(taipei).Format("2006-01-02 15:04") + " (台北)"
	funcs := template.FuncMap{
		"pct": func(f float64) string {
			if math.IsNaN(f) {
				return "–"
			}
			return fmt.Sprintf("%+.1f%%", f*100)
		},
		"price": func(f float64) string { return fmt.Sprintf("%.2f", f) },
		"usd": func(f float64) string {
			switch {
			case math.IsNaN(f) || f <= 0:
				return "–"
			case f >= 1e9:
				return fmt.Sprintf("$%.1fB", f/1e9)
			}
			return fmt.Sprintf("$%.0fM", f/1e6)
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
	}
	tmpl, err := template.New("us.html").Funcs(funcs).ParseFS(templates, "templates/us.html")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".us.html.tmp")
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
	return os.Rename(tmp, filepath.Join(dir, "us.html"))
}

func load(db *sql.DB, cfg config.US) (Page, error) {
	var p Page
	build := func(list []string) ([]Row, error) {
		var out []Row
		for i, t := range list {
			r, ok, err := row(db, t)
			if err != nil {
				return nil, err
			}
			if !ok {
				p.Missing = append(p.Missing, t)
				continue
			}
			r.Order = i + 1
			if r.Date > p.DataEnd {
				p.DataEnd = r.Date
			}
			out = append(out, r)
		}
		return out, nil
	}
	var err error
	if p.Benchmarks, err = build(cfg.Benchmarks); err != nil {
		return p, err
	}
	if p.Rows, err = build(cfg.Tickers); err != nil {
		return p, err
	}
	for _, rs := range [][]Row{p.Benchmarks, p.Rows} {
		for i := range rs {
			rs[i].Stale = rs[i].Date < p.DataEnd
		}
	}
	return p, nil
}

// row computes one ticker's figures from its last year of bars.
func row(db *sql.DB, ticker string) (Row, bool, error) {
	r := Row{Ticker: ticker, Link: "https://finance.yahoo.com/quote/" + ticker}
	db.QueryRow(`SELECT name FROM us_companies WHERE ticker = ?`, ticker).Scan(&r.Name)
	q, err := db.Query(`SELECT date, close, adj_close, volume FROM us_prices
		WHERE ticker = ? ORDER BY date DESC LIMIT 400`, ticker)
	if err != nil {
		return r, false, err
	}
	defer q.Close()
	type bar struct {
		date       string
		close, adj float64
		vol        int64
	}
	var bars []bar // newest first
	for q.Next() {
		var b bar
		if err := q.Scan(&b.date, &b.close, &b.adj, &b.vol); err != nil {
			return r, false, err
		}
		bars = append(bars, b)
	}
	if err := q.Err(); err != nil || len(bars) == 0 {
		return r, false, err
	}
	last := bars[0]
	r.Date, r.Close = last.date, last.close
	back := func(n int) float64 {
		if n >= len(bars) || bars[n].adj <= 0 {
			return math.NaN()
		}
		return last.adj/bars[n].adj - 1
	}
	r.D1, r.W1, r.M1, r.Y1 = back(1), back(5), back(21), back(252)
	r.YTD = math.NaN()
	year := last.date[:4]
	for _, b := range bars {
		if b.date[:4] < year {
			r.YTD = last.adj/b.adj - 1
			break
		}
	}
	high := 0.0
	for i := 0; i < len(bars) && i < 252; i++ {
		high = math.Max(high, bars[i].adj)
	}
	r.FromHigh = last.adj/high - 1
	var sum float64
	n := 0
	for i := 0; i < len(bars) && i < 20; i++ {
		sum += bars[i].close * float64(bars[i].vol)
		n++
	}
	r.DollarVol = sum / float64(n)
	return r, true, nil
}
