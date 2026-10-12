package validate

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

//go:embed templates/health.html
var templates embed.FS

var taipei = time.FixedZone("Asia/Taipei", 8*60*60)

// historyLen is how many recent snapshots the page lists.
const historyLen = 14

// HealthPage is what health.html shows (issue #45).
type HealthPage struct {
	GeneratedAt string
	Latest      *Manifest // nil before the first validated run
	MinCoverage float64
	History     []Manifest // newest first
}

// Render is the `radar render` step that writes public/health.html.
func Render(_ context.Context, cfg config.Config, st *store.Store) error {
	return renderHealth(st.DB, cfg, cfg.Run.PublicDir, time.Now())
}

func renderHealth(db *sql.DB, cfg config.Config, dir string, now time.Time) error {
	page := HealthPage{
		GeneratedAt: now.In(taipei).Format("2006-01-02 15:04") + " (台北)",
		MinCoverage: cfg.Revenue.MinCoverage,
	}
	hist, err := history(db, historyLen)
	if err != nil {
		return err
	}
	page.History = hist
	if len(hist) > 0 {
		page.Latest = &hist[0]
	}

	funcs := template.FuncMap{
		"pct": func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) },
		"num": func(n int64) string { return commas(n) },
		"short": func(s string) string {
			if len(s) > 7 {
				return s[:7]
			}
			return s
		},
		"stamp": func(id string) string {
			t, err := time.Parse(time.RFC3339, id)
			if err != nil {
				return id
			}
			return t.In(taipei).Format("2006-01-02 15:04")
		},
		"failed": func(cs []Check) int {
			n := 0
			for _, c := range cs {
				if !c.OK {
					n++
				}
			}
			return n
		},
		"lastCov": func(m Manifest) string {
			if len(m.Coverage) == 0 {
				return "–"
			}
			c := m.Coverage[len(m.Coverage)-1]
			return fmt.Sprintf("%s %.0f%%", c.Month, c.Share*100)
		},
	}
	tmpl, err := template.New("health.html").Funcs(funcs).ParseFS(templates, "templates/health.html")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Write to a temp file and rename, so a failed render never leaves a half page.
	tmp := filepath.Join(dir, ".health.html.tmp")
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
	return os.Rename(tmp, filepath.Join(dir, "health.html"))
}

// history returns up to n stored snapshots, newest first. A database that has
// never been validated has none.
func history(db *sql.DB, n int) ([]Manifest, error) {
	rows, err := db.Query(`SELECT manifest FROM snapshots ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		var exists int
		if qerr := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'snapshots'`).Scan(&exists); qerr == nil && exists == 0 {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []Manifest
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var m Manifest
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			return nil, errors.Join(fmt.Errorf("snapshot manifest"), err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func commas(n int64) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return "-" + commas(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
