// Package site renders the static dashboard into public/ from the SQLite store.
// Company details are not collected: every row links out to Goodinfo,
// Yahoo股市 and MOPS instead.
package site

import (
	"context"
	"fmt"
	"html/template"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
	"github.com/Antonio-1110/stocks_but_fr/web"
)

var taipei = time.FixedZone("Asia/Taipei", 8*60*60)

// Page is everything the home template sees.
type Page struct {
	Title       string
	GeneratedAt string // when render ran, Taipei time
	PriceDate   string // latest trading day in the store
	Rows        []Row
	Industries  []string
	Backtest    []Field
}

// Render is the `radar render` step: it writes index.html and the static
// assets into cfg.Run.PublicDir.
func Render(_ context.Context, cfg config.Config, st *store.Store) error {
	return render(st, cfg.Run.PublicDir, time.Now())
}

func render(st *store.Store, dir string, now time.Time) error {
	rows, latest, err := loadRows(st, model.MarketTW, now)
	if err != nil {
		return err
	}
	bt, err := loadBacktest(st.DB)
	if err != nil {
		return fmt.Errorf("backtest summary: %w", err)
	}
	page := Page{
		Title:       "Taiwan radar",
		GeneratedAt: now.In(taipei).Format("2006-01-02 15:04") + " (台北)",
		PriceDate:   latest,
		Rows:        rows,
		Industries:  industries(rows),
		Backtest:    bt,
	}

	tmpl, err := template.New("").Funcs(funcs).ParseFS(web.FS, "templates/*.html")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := copyStatic(dir); err != nil {
		return err
	}
	// Write to a temp file and rename, so a failed render never leaves a half page.
	tmp := filepath.Join(dir, ".index.html.tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := tmpl.ExecuteTemplate(f, "index.html", page); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "index.html"))
}

func copyStatic(dir string) error {
	return fs.WalkDir(web.FS, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, path)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := web.FS.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
}

func industries(rows []Row) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		if r.Industry != "" && !seen[r.Industry] {
			seen[r.Industry] = true
			out = append(out, r.Industry)
		}
	}
	sort.Strings(out)
	return out
}

var funcs = template.FuncMap{
	// pct formats a percentage with a sign; nil renders as a dash.
	"pct": func(p *float64) string {
		if p == nil {
			return "–"
		}
		return fmt.Sprintf("%+.1f%%", *p)
	},
	"price": func(p *float64) string {
		if p == nil {
			return "–"
		}
		return strconv.FormatFloat(*p, 'f', 2, 64)
	},
	// yi formats NTD in 億 (100 million).
	"yi": func(p *float64) string {
		if p == nil {
			return "–"
		}
		return fmt.Sprintf("%.1f億", *p/1e8)
	},
	// lots formats shares as 張 (1,000 shares), signed.
	"lots": func(p *int64) string {
		if p == nil {
			return "–"
		}
		return withCommas(*p/1000, true)
	},
	// sortVal is the data-sort attribute: a raw number, or empty for no data
	// so the script can push missing values to the bottom.
	"sortVal": func(v any) string {
		switch x := v.(type) {
		case *float64:
			if x != nil {
				return strconv.FormatFloat(*x, 'f', -1, 64)
			}
		case *int64:
			if x != nil {
				return strconv.FormatInt(*x, 10)
			}
		}
		return ""
	},
	"sign": func(v any) string {
		var f float64
		switch x := v.(type) {
		case *float64:
			if x == nil {
				return ""
			}
			f = *x
		case *int64:
			if x == nil {
				return ""
			}
			f = float64(*x)
		}
		switch {
		case f > 0:
			return "up"
		case f < 0:
			return "down"
		}
		return ""
	},
}

func withCommas(n int64, signed bool) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	switch {
	case neg:
		return "-" + b.String()
	case signed && n > 0:
		return "+" + b.String()
	}
	return b.String()
}
