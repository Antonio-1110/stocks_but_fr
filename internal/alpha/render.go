// Package alpha renders alpha.html, the alpha lab's leaderboard (issue #76).
// The Python lab (backtest/radar_backtest/alpha.py) owns the alpha_results
// and alpha_picks tables; this only reads the newest run.
package alpha

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
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

//go:embed templates/alpha.html
var templates embed.FS

var taipei = time.FixedZone("Asia/Taipei", 8*60*60)

// verdictOrder ranks the lab's verdicts, best first.
var verdictOrder = map[string]int{
	"passes": 0, "promising": 1, "recent only": 2, "faded": 3,
	"costs eat it": 4, "works backwards": 5, "no edge": 6,
}

// Stat is one period's numbers for a signal and horizon. Valid is false when
// the period had no observations (e.g. flows before 2013).
type Stat struct {
	Valid          bool
	N              int64
	Edge, EdgeT    float64
	Net, Hit, IC   float64
	HasIC, HasEdge bool
}

type Result struct {
	Signal, Family, Kind, Label, Verdict string
	Horizon                              int
	Design, Test, Recent                 Stat
	LateNet                              float64
	HasLate                              bool
}

type Pick struct {
	Ticker, Name, Link string
	Rank               int
}

type PickGroup struct {
	Result Result
	Date   string
	Picks  []Pick
}

type Page struct {
	GeneratedAt    string
	RunAt, DataEnd string
	Snapshot, Code string
	Hurdle, Cost   float64
	Best           []Result // each signal's best horizon
	All            []Result
	Picks          []PickGroup
	Counts         map[string]int
	Signals        int
}

// Render is the `radar render` step that writes public/alpha.html.
func Render(_ context.Context, cfg config.Config, st *store.Store) error {
	return render(st.DB, cfg.Run.PublicDir, time.Now())
}

func render(db *sql.DB, dir string, now time.Time) error {
	page, err := load(db)
	if err != nil {
		return err
	}
	page.GeneratedAt = now.In(taipei).Format("2006-01-02 15:04") + " (台北)"
	funcs := template.FuncMap{
		"pct":  func(f float64) string { return fmt.Sprintf("%+.2f%%", f*100) },
		"pct0": func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) },
		"num":  func(f float64) string { return fmt.Sprintf("%.1f", f) },
		"ic":   func(f float64) string { return fmt.Sprintf("%+.3f", f) },
		"count": func(n int64) string {
			s := fmt.Sprint(n)
			for i := len(s) - 3; i > 0; i -= 3 {
				s = s[:i] + "," + s[i:]
			}
			return s
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
		"good": func(v string) bool { return verdictOrder[v] <= 2 },
		"slug": func(v string) string {
			switch v {
			case "passes", "promising":
				return "ok"
			case "recent only", "faded":
				return "warn"
			}
			return "muted"
		},
	}
	tmpl, err := template.New("alpha.html").Funcs(funcs).ParseFS(templates, "templates/alpha.html")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".alpha.html.tmp")
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
	return os.Rename(tmp, filepath.Join(dir, "alpha.html"))
}

// load reads the newest run. Before the lab's first run the page says so.
func load(db *sql.DB) (Page, error) {
	var p Page
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'alpha_results'`).Scan(&n); err != nil || n == 0 {
		return p, err
	}
	var runAt sql.NullString
	if err := db.QueryRow(`SELECT MAX(run_at) FROM alpha_results`).Scan(&runAt); err != nil || !runAt.Valid {
		return p, err
	}
	p.RunAt = runAt.String
	rows, err := db.Query(`SELECT signal, family, kind, label, horizon, verdict,
		COALESCE(hurdle, 0), COALESCE(cost, 0), late_net, COALESCE(data_end, ''), snapshot, code,
		design_n, design_edge, design_edge_t, design_net, design_hit, design_ic,
		test_n, test_edge, test_edge_t, test_net, test_hit, test_ic,
		recent_n, recent_edge, recent_edge_t, recent_net, recent_hit, recent_ic
		FROM alpha_results WHERE run_at = ?`, p.RunAt)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var r Result
		var late sql.NullFloat64
		var s [3][6]sql.NullFloat64
		dest := []any{&r.Signal, &r.Family, &r.Kind, &r.Label, &r.Horizon, &r.Verdict,
			&p.Hurdle, &p.Cost, &late, &p.DataEnd, &p.Snapshot, &p.Code}
		for i := range s {
			for j := range s[i] {
				dest = append(dest, &s[i][j])
			}
		}
		if err := rows.Scan(dest...); err != nil {
			return p, err
		}
		r.LateNet, r.HasLate = late.Float64, late.Valid && !math.IsNaN(late.Float64)
		r.Design, r.Test, r.Recent = stat(s[0]), stat(s[1]), stat(s[2])
		p.All = append(p.All, r)
	}
	if err := rows.Err(); err != nil {
		return p, err
	}
	sort.SliceStable(p.All, func(i, j int) bool { return better(p.All[i], p.All[j]) })
	p.Counts = map[string]int{}
	seen := map[string]bool{}
	for _, r := range p.All {
		if !seen[r.Signal] {
			seen[r.Signal] = true
			p.Best = append(p.Best, r)
			p.Counts[r.Verdict]++
		}
	}
	p.Signals = len(p.Best)
	p.Picks, err = loadPicks(db, p.RunAt, p.All)
	return p, err
}

func stat(v [6]sql.NullFloat64) Stat {
	ok := func(x sql.NullFloat64) bool { return x.Valid && !math.IsNaN(x.Float64) }
	s := Stat{N: int64(v[0].Float64), Edge: v[1].Float64, EdgeT: v[2].Float64, Net: v[3].Float64,
		Hit: v[4].Float64, IC: v[5].Float64}
	s.Valid = ok(v[0]) && v[0].Float64 > 0
	s.HasEdge = s.Valid && ok(v[1]) && ok(v[2])
	s.HasIC = s.Valid && ok(v[5])
	return s
}

// better orders by verdict, then by the test period's t-stat.
func better(a, b Result) bool {
	if va, vb := verdictOrder[a.Verdict], verdictOrder[b.Verdict]; va != vb {
		return va < vb
	}
	ta, tb := a.Test.EdgeT, b.Test.EdgeT
	if !a.Test.HasEdge {
		ta = math.Inf(-1)
	}
	if !b.Test.HasEdge {
		tb = math.Inf(-1)
	}
	return ta > tb
}

func loadPicks(db *sql.DB, runAt string, all []Result) ([]PickGroup, error) {
	rows, err := db.Query(`SELECT signal, horizon, date, rank, ticker, name FROM alpha_picks
		WHERE run_at = ? ORDER BY signal, rank`, runAt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := map[string]*PickGroup{}
	var order []string
	for rows.Next() {
		var sig, date string
		var h int
		var pk Pick
		if err := rows.Scan(&sig, &h, &date, &pk.Rank, &pk.Ticker, &pk.Name); err != nil {
			return nil, err
		}
		pk.Link = "https://goodinfo.tw/tw/StockDetail.asp?STOCK_ID=" + pk.Ticker
		g := groups[sig]
		if g == nil {
			g = &PickGroup{Date: date}
			for _, r := range all {
				if r.Signal == sig && r.Horizon == h {
					g.Result = r
				}
			}
			groups[sig] = g
			order = append(order, sig)
		}
		g.Picks = append(g.Picks, pk)
	}
	out := make([]PickGroup, 0, len(order))
	for _, s := range order {
		out = append(out, *groups[s])
	}
	sort.SliceStable(out, func(i, j int) bool { return better(out[i].Result, out[j].Result) })
	return out, rows.Err()
}
