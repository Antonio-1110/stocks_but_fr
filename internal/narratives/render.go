package narratives

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

//go:embed templates/narratives.html
var templates embed.FS

// Portfolio is the paper portfolio that trades the cards ([paper] strategies).
const Portfolio = "narrative"

type Stock struct {
	Ticker, Name string
	Held         bool
}

type CardView struct {
	Signal
	Odds         string // "53%"
	Move         string // "+12" points over jump_days, or ""
	MoveSign     string
	ViewText     string
	EdgeText     string
	Spark        string // SVG polyline points, 0..100 x 0..30
	YesW, NoW    []Stock
	Live         bool
	MarketURL    string
	MarketTitle  string
	MarketSource string
}

type MoverView struct {
	Market
	Odds, Move string
	Up         bool
	d          float64
}

type Page struct {
	GeneratedAt, Day string
	JumpDays         int
	MinEdge, MinJump float64
	Live, Quiet      []CardView
	ScanAt, Summary  string
	ScanError        string
	NoKey            bool
	Movers, Busiest  []MoverView
	Markets          int
}

// Render is the `radar render` step that writes public/narratives.html.
func Render(_ context.Context, cfg config.Config, st *store.Store) error {
	// The key is passed to collect only, so the page can't tell "no key" from
	// "no scan yet"; it says both.
	return render(st.DB, cfg.Narratives, cfg.Run.PublicDir, time.Now(), true)
}

func render(db *sql.DB, p config.Narratives, dir string, now time.Time, noKey bool) error {
	if err := migrate(db); err != nil {
		return err
	}
	pg, err := load(db, p, now)
	if err != nil {
		return err
	}
	pg.NoKey = noKey
	tmpl, err := template.New("narratives.html").ParseFS(templates, "templates/narratives.html")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".narratives.html.tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := tmpl.Execute(f, pg); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "narratives.html"))
}

func load(db *sql.DB, p config.Narratives, now time.Time) (Page, error) {
	day := now.In(taipei).Format("2006-01-02")
	pg := Page{GeneratedAt: now.In(taipei).Format("2006-01-02 15:04") + " (台北)", Day: day,
		JumpDays: p.JumpDays, MinEdge: p.MinEdge * 100, MinJump: p.MinJump * 100}
	held, err := heldTickers(db)
	if err != nil {
		return pg, err
	}
	names, err := twNames(db)
	if err != nil {
		return pg, err
	}
	sigs, err := Signals(db, p, day, held)
	if err != nil {
		return pg, err
	}
	from := now.In(taipei).AddDate(0, 0, -60).Format("2006-01-02")
	for _, s := range sigs {
		v := CardView{Signal: s, Live: s.Side != ""}
		if s.View != nil {
			v.ViewText = fmt.Sprintf("%.0f%%", *s.View*100)
		}
		if m := s.Market; m != nil {
			v.Odds = fmt.Sprintf("%.0f%%", s.Prob*100)
			v.MarketURL, v.MarketTitle, v.MarketSource = m.URL, m.Question, m.Source
			if !math.IsNaN(s.Then) {
				d := (s.Prob - s.Then) * 100
				v.Move, v.MoveSign = fmt.Sprintf("%+.0f", d), sign(d)
			}
			if s.View != nil {
				v.EdgeText = fmt.Sprintf("%+.0f", s.Edge*100)
			}
			hist, err := history(db, m.Source, m.ID, from)
			if err != nil {
				return pg, err
			}
			v.Spark = spark(hist)
		}
		stocks := func(ts []string, side string) []Stock {
			var out []Stock
			for _, t := range ts {
				out = append(out, Stock{t, names[t], held[t] && s.Side == side})
			}
			return out
		}
		v.YesW, v.NoW = stocks(s.YesTW, "yes"), stocks(s.NoTW, "no")
		if v.Live {
			pg.Live = append(pg.Live, v)
		} else {
			pg.Quiet = append(pg.Quiet, v)
		}
	}
	sort.SliceStable(pg.Live, func(i, j int) bool { return pg.Live[i].Weight > pg.Live[j].Weight })

	var at, summary, errText string
	if db.QueryRow(`SELECT created_at, summary, error FROM narrative_scans ORDER BY id DESC LIMIT 1`).Scan(&at, &summary, &errText) == nil {
		pg.ScanAt, pg.ScanError = at, errText
		if t, err := time.Parse(time.RFC3339, at); err == nil {
			pg.ScanAt = t.In(taipei).Format("2006-01-02 15:04")
		}
		if errText != "" {
			db.QueryRow(`SELECT summary FROM narrative_scans WHERE error = '' ORDER BY id DESC LIMIT 1`).Scan(&summary)
		}
		pg.Summary = summary
	}

	ms, err := Markets(db)
	if err != nil {
		return pg, err
	}
	thenDay := now.In(taipei).AddDate(0, 0, -p.JumpDays).Format("2006-01-02")
	var movers []MoverView
	for _, m := range ms {
		if m.Closed || (m.EndDate != "" && m.EndDate < day) {
			continue
		}
		pg.Markets++
		mv := MoverView{Market: m, Odds: fmt.Sprintf("%.0f%%", m.Prob*100)}
		if len(pg.Busiest) < 25 {
			pg.Busiest = append(pg.Busiest, mv)
		}
		if then, _, ok := oddsOn(db, m.Source, m.ID, thenDay); ok && m.Volume24h >= 10_000 {
			d := m.Prob - then
			if math.Abs(d) >= 0.05 {
				mv.Move, mv.Up, mv.d = fmt.Sprintf("%+.0f", d*100), d > 0, d
				movers = append(movers, mv)
			}
		}
	}
	sort.SliceStable(movers, func(i, j int) bool {
		return math.Abs(movers[i].d) > math.Abs(movers[j].d)
	})
	pg.Movers = movers[:min(len(movers), 25)]
	return pg, nil
}

func sign(f float64) string {
	switch {
	case f > 0:
		return "up"
	case f < 0:
		return "down"
	}
	return ""
}

// spark is an SVG polyline over a 100 x 30 box, odds 0 at the bottom.
func spark(ps []float64) string {
	if len(ps) < 2 {
		return ""
	}
	var b strings.Builder
	for i, p := range ps {
		fmt.Fprintf(&b, "%.1f,%.1f ", float64(i)*100/float64(len(ps)-1), 30-p*30)
	}
	return strings.TrimSpace(b.String())
}

// heldTickers is what the narrative paper portfolio holds now (none before it starts).
func heldTickers(db *sql.DB) (map[string]bool, error) {
	out := map[string]bool{}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM state.sqlite_master WHERE type = 'table' AND name = 'paper_positions'`).Scan(&n); err != nil || n == 0 {
		return out, err
	}
	q, err := db.Query(`SELECT ticker FROM state.paper_positions WHERE portfolio = ?`, Portfolio)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	for q.Next() {
		var t string
		if err := q.Scan(&t); err != nil {
			return nil, err
		}
		out[t] = true
	}
	return out, q.Err()
}

func twNames(db *sql.DB) (map[string]string, error) {
	out := map[string]string{}
	q, err := db.Query(`SELECT ticker, name FROM companies WHERE market = 'TW'`)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	for q.Next() {
		var t, n string
		if err := q.Scan(&t, &n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, q.Err()
}
