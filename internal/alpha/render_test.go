package alpha

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

func TestPageBeforeFirstRun(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "radar.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := render(st.DB, dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "alpha.html"))
	if !strings.Contains(string(b), "No run yet") {
		t.Fatal("expected the empty state")
	}
}

// The tables as backtest/radar_backtest/alpha.py writes them, cut to the
// columns the page reads.
const schema = `
CREATE TABLE alpha_results (run_at TEXT, snapshot TEXT, code TEXT, signal TEXT, family TEXT, kind TEXT,
  label TEXT, horizon INTEGER, verdict TEXT, hurdle REAL, cost REAL, late_net REAL, data_end TEXT,
  design_n INTEGER, design_edge REAL, design_edge_t REAL, design_net REAL, design_hit REAL, design_ic REAL,
  test_n INTEGER, test_edge REAL, test_edge_t REAL, test_net REAL, test_hit REAL, test_ic REAL,
  recent_n INTEGER, recent_edge REAL, recent_edge_t REAL, recent_net REAL, recent_hit REAL, recent_ic REAL);
CREATE TABLE alpha_picks (run_at TEXT, date TEXT, signal TEXT, horizon INTEGER, rank INTEGER,
  ticker TEXT, name TEXT, value REAL);
`

func TestLeaderboard(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "radar.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB.Exec(schema); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO alpha_results VALUES (?, 's', 'c', ?, 'flows', 'score', ?, ?, ?, 3.3, 0.0047, ?, '2026-10-22',
		1000, 0.01, ?, 0.005, 0.55, 0.05, 900, 0.012, ?, 0.007, 0.56, 0.06, NULL, NULL, NULL, NULL, NULL, NULL)`
	for _, r := range []struct {
		run, sig, label string
		h               int
		verdict         string
		late, dt, tt    any
	}{
		{"2026-10-17 10:00:00", "old_signal", "Old run", 5, "passes", 0.001, 5.0, 3.0},
		{"2026-10-24 10:00:00", "trust_5d", "投信 net buying, 5 days", 5, "promising", 0.002, 2.5, 2.4},
		{"2026-10-24 10:00:00", "trust_5d", "投信 net buying, 5 days", 1, "no edge", nil, 0.5, 0.2},
		{"2026-10-24 10:00:00", "foreign_1d", "外資 net buying today", 1, "passes", -0.001, 4.0, 3.1},
		{"2026-10-24 10:00:00", "reversal_1d", "Yesterday's losers", 1, "no edge", nil, 0.1, nil},
	} {
		if _, err := st.DB.Exec(ins, r.run, r.sig, r.label, r.h, r.verdict, r.late, r.dt, r.tt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB.Exec(`INSERT INTO alpha_picks VALUES
		('2026-10-24 10:00:00', '2026-10-22', 'foreign_1d', 1, 1, '2330', '台積電', 1.2),
		('2026-10-24 10:00:00', '2026-10-22', 'foreign_1d', 1, 2, '2317', '鴻海', 1.1)`); err != nil {
		t.Fatal(err)
	}

	p, err := load(st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if p.RunAt != "2026-10-24 10:00:00" || len(p.All) != 4 || p.Signals != 3 {
		t.Fatalf("run %q, %d rows, %d signals", p.RunAt, len(p.All), p.Signals)
	}
	if p.Best[0].Signal != "foreign_1d" || p.Best[1].Signal != "trust_5d" || p.Best[1].Horizon != 5 {
		t.Fatalf("order: %+v", p.Best)
	}
	if p.Best[2].Test.HasEdge || p.Best[1].HasLate != true {
		t.Fatalf("missing values: %+v", p.Best[2])
	}
	if len(p.Picks) != 1 || len(p.Picks[0].Picks) != 2 || p.Picks[0].Result.Verdict != "passes" {
		t.Fatalf("picks: %+v", p.Picks)
	}

	if err := render(st.DB, dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "alpha.html"))
	for _, want := range []string{"外資 net buying today", "台積電", "0.70%", "t ≥ 3.3", `aria-current="page">Alpha`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("page lacks %q", want)
		}
	}
}
