package validate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

func open(t *testing.T) (*store.Store, config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.Run.DBPath = filepath.Join(t.TempDir(), "radar.db")
	st, err := store.Open(cfg.Run.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO companies (market, ticker, name) VALUES ('TW', '2330', 'TSMC'), ('TW', '0050', 'ETF')`)
	for _, d := range []string{"2026-10-07", "2026-10-08"} {
		for _, tk := range []string{"2330", "0050"} {
			exec(`INSERT INTO prices (market, ticker, date, open, high, low, close, adj_close, volume)
				VALUES ('TW', ?, ?, 10, 11, 9, 10, 10, 100)`, tk, d)
		}
	}
	exec(`INSERT INTO monthly_revenue (market, ticker, month, revenue, announced_on)
		VALUES ('TW', '2330', '2026-09-01', 100, '2026-10-10')`)
	return st, cfg
}

var day = time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)

func hardFailed(m Manifest) []string {
	var out []string
	for _, c := range m.Hard {
		if !c.OK {
			out = append(out, c.Name)
		}
	}
	return out
}

func TestValidatePassesAndRecordsSnapshot(t *testing.T) {
	st, cfg := open(t)
	m, err := Validate(context.Background(), st.DB, "0050", day, "abc123")
	if err != nil {
		t.Fatalf("err %v, failed %v", err, hardFailed(m))
	}
	if m.Rows["prices"] != 4 || m.Latest["prices"] != "2026-10-08" || m.Latest["revenue_month"] != "2026-09-01" {
		t.Errorf("manifest %+v", m)
	}
	if len(m.Coverage) != 2 || m.Coverage[1].Month != "2026-09" || m.Coverage[1].Have != 1 || m.Coverage[1].Listed != 1 {
		t.Errorf("coverage %+v", m.Coverage)
	}

	if err := Run(context.Background(), cfg, st); err != nil {
		t.Fatal(err)
	}
	var n int
	st.DB.QueryRow(`SELECT COUNT(*) FROM snapshots`).Scan(&n)
	if n != 1 {
		t.Errorf("snapshots = %d, want 1", n)
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(cfg.Run.DBPath), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got Manifest
	if err := json.Unmarshal(body, &got); err != nil || got.Rows["companies"] != 2 {
		t.Errorf("manifest.json %s (%v)", body, err)
	}
}

func TestValidateBlocksBadData(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string
	}{
		{"zero close", `UPDATE prices SET close = 0 WHERE date = '2026-10-08' AND ticker = '2330'`,
			"price close or adj_close missing or not positive"},
		{"high below low", `UPDATE prices SET high = 8 WHERE date = '2026-10-08' AND ticker = '2330'`,
			"price high below low"},
		{"future date", `INSERT INTO prices (market, ticker, date, open, high, low, close, adj_close)
			VALUES ('TW', '2330', '2026-12-01', 10, 11, 9, 10, 10)`, "price dated in the future"},
		{"lost prices", `DELETE FROM prices WHERE date = '2026-10-08'`, "prices rows not lost"},
		{"lost revenue", `DELETE FROM monthly_revenue`, "monthly_revenue rows not lost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, cfg := open(t)
			if err := Run(context.Background(), cfg, st); err != nil { // previous snapshot
				t.Fatal(err)
			}
			if _, err := st.DB.Exec(tc.sql); err != nil {
				t.Fatal(err)
			}
			m, err := Validate(context.Background(), st.DB, "0050", day.Add(time.Hour), "")
			if !errors.Is(err, ErrFailed) {
				t.Fatalf("err = %v, want ErrFailed", err)
			}
			found := false
			for _, name := range hardFailed(m) {
				found = found || name == tc.want
			}
			if !found {
				t.Errorf("failed %v, want %q among them", hardFailed(m), tc.want)
			}
			// Nothing is stored for a failed snapshot.
			if err := Run(context.Background(), cfg, st); !errors.Is(err, ErrFailed) {
				t.Errorf("Run err = %v, want ErrFailed", err)
			}
			var n int
			st.DB.QueryRow(`SELECT COUNT(*) FROM snapshots`).Scan(&n)
			if n != 1 {
				t.Errorf("snapshots = %d, want only the first", n)
			}
		})
	}
}

// Soft checks are reported but never block.
func TestSoftChecksDoNotBlock(t *testing.T) {
	st, _ := open(t)
	st.DB.Exec(`UPDATE prices SET open = 20 WHERE ticker = '2330'`)
	st.DB.Exec(`INSERT INTO companies (market, ticker, name) VALUES ('TW', '1101', 'No revenue')`)
	m, err := Validate(context.Background(), st.DB, "0050", day, "")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for _, c := range m.Soft {
		counts[c.Name] = c.Count
	}
	if counts["open outside the day's low-high range"] != 2 || counts["listed stocks with no revenue"] != 1 {
		t.Errorf("soft checks %+v", m.Soft)
	}
}

func TestHealthPage(t *testing.T) {
	st, cfg := open(t)
	dir := t.TempDir()
	// Before any snapshot the page still renders.
	if err := renderHealth(st.DB, cfg, dir, day); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "health.html"))
	if !strings.Contains(string(body), "No snapshot yet") {
		t.Errorf("empty page: %s", body)
	}
	if err := Run(context.Background(), cfg, st); err != nil {
		t.Fatal(err)
	}
	if err := renderHealth(st.DB, cfg, dir, day); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(filepath.Join(dir, "health.html"))
	for _, want := range []string{"all pass", "Revenue 2026-09", "100%", "listed stocks with no revenue", "Recent snapshots"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("page lacks %q", want)
		}
	}
}
