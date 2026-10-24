package paper

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
)

// marks writes paper_equity rows: date -> {portfolio, benchmark}.
func marks(t *testing.T, db *sql.DB, name string, rows map[string][2]float64) {
	t.Helper()
	must(t, migrate(db))
	_, err := db.Exec(`INSERT OR IGNORE INTO paper_portfolios (name, started_on, initial_capital, cash, bench_ticker)
		VALUES (?, '2026-08-01', 100, 100, '0050')`, name)
	must(t, err)
	for d, v := range rows {
		_, err := db.Exec(`INSERT INTO paper_equity (portfolio, date, cash, holdings, liquidation, bench)
			VALUES (?, ?, 0, 0, ?, ?)`, name, d, v[0], v[1])
		must(t, err)
	}
}

func TestReviewFlagsPortfoliosBehindTheBenchmark(t *testing.T) {
	st := openStore(t)
	// "lag": ahead in September, behind in October (+2% vs +5%).
	marks(t, st.DB, "lag", map[string][2]float64{
		"2026-08-31": {100, 100}, "2026-09-30": {110, 101}, "2026-10-15": {108, 103}, "2026-10-30": {112.2, 106.05},
	})
	// "lead": behind in September, ahead in October.
	marks(t, st.DB, "lead", map[string][2]float64{
		"2026-08-31": {100, 100}, "2026-09-30": {99, 101}, "2026-10-30": {110, 106.05},
	})
	// "new": started mid-October and behind since.
	marks(t, st.DB, "new", map[string][2]float64{"2026-10-24": {100, 100}, "2026-10-30": {99, 101}})
	// "none": no mark with a benchmark yet.
	marks(t, st.DB, "none", nil)
	cfg := config.Default()
	cfg.Paper.Strategies = []string{"lag", "lead", "new", "none"}

	var out bytes.Buffer
	must(t, review(st.DB, cfg, &out, time.Date(2026, 11, 1, 9, 0, 0, 0, taipei)))
	got := out.String()
	for _, want := range []string{
		"# Paper review 2026-10\n",
		"| lag **behind** | +2.00% | +5.00% | -3.00 pts | +9.00 pts | +12.20% | +6.05% | 0 |",
		"| lead | +11.11% | +5.00% | +6.11 pts | -2.00 pts |",
		"| new **behind** | -1.00% (from 2026-10-24) | +1.00% |",
		"| none | – |",
		"**Behind 0050 this month:** lag, new.",
		"Not measured yet (no mark with 0050 before the month's end): none.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("review missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "two months running") {
		t.Errorf("no portfolio was behind in both months\n%s", got)
	}
}

func TestReviewPrintsNothingBeforeAnyPortfolioTraded(t *testing.T) {
	st := openStore(t)
	marks(t, st.DB, "p", map[string][2]float64{"2026-10-24": {100, 100}, "2026-10-30": {99, 101}})
	cfg := config.Default()
	cfg.Paper.Strategies = []string{"p"}
	var out bytes.Buffer
	must(t, review(st.DB, cfg, &out, time.Date(2026, 10, 25, 9, 0, 0, 0, taipei))) // reviews September
	if out.Len() != 0 {
		t.Errorf("want no report for a month before any mark, got:\n%s", out.String())
	}
}

func TestLast30MeasuresFromTheMarkAMonthBack(t *testing.T) {
	st := openStore(t)
	marks(t, st.DB, "p", map[string][2]float64{
		"2026-09-20": {100, 100}, "2026-09-30": {120, 100}, "2026-10-28": {108, 110}, "2026-10-30": {114, 110},
	})
	c, err := last30(st.DB, "p")
	must(t, err)
	// 30 days back from 2026-10-30 is 2026-09-30: +14/120 = -5% vs +10%.
	if !c.OK || c.From != "2026-09-30" || c.To != "2026-10-30" || c.Partial || !c.Behind() {
		t.Fatalf("got %+v", c)
	}
	if d := c.Gap() - (-0.05 - 0.10); d > 1e-9 || d < -1e-9 {
		t.Errorf("gap %.4f, want -0.15", c.Gap())
	}
}

func TestPaperPageShowsTheBehindFlag(t *testing.T) {
	st := openStore(t)
	marks(t, st.DB, "behind_one", map[string][2]float64{"2026-10-01": {100, 100}, "2026-10-02": {99, 101}})
	cfg := config.Default()
	cfg.Paper.Strategies = []string{"behind_one"}
	// The page needs a price day to show a portfolio.
	must(t, st.UpsertPrices([]model.Price{price("0050", "2026-10-02", 50, 50, 50)}))
	dir := t.TempDir()
	must(t, render(st.DB, cfg, dir, time.Now()))
	b, err := os.ReadFile(filepath.Join(dir, "paper.html"))
	must(t, err)
	if !strings.Contains(string(b), `class="behind"`) || !strings.Contains(string(b), "-2.0 pts*") {
		t.Errorf("paper.html has no behind flag:\n%s", b)
	}
}
