package paper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
)

// paper.html is the scoreboard; each portfolio gets its own page (issue #74).
func TestRenderWritesAPagePerPortfolio(t *testing.T) {
	st := openStore(t)
	cfg := config.Default()
	cfg.Paper.InitialCapital = 1_000_000
	cfg.Paper.Strategies = []string{"test"}
	s := script{"2026-10-01": {{Ticker: "1111", Side: "buy", Target: 500_000, Reason: "test"}}}
	must(t, st.UpsertPrices([]model.Price{price("1111", "2026-10-01", 99, 100, 100), price("0050", "2026-10-01", 50, 50, 50)}))
	runOnce(t, st, cfg, s)
	must(t, st.UpsertPrices([]model.Price{price("1111", "2026-10-02", 100, 101, 101), price("0050", "2026-10-02", 50, 50, 50)}))
	runOnce(t, st, cfg, s)

	dir := t.TempDir()
	must(t, render(st.DB, cfg, dir, time.Now()))
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		must(t, err)
		return string(b)
	}
	board := read("paper.html")
	for _, want := range []string{"Scoreboard", `href="paper-test.html"`} {
		if !strings.Contains(board, want) {
			t.Errorf("paper.html missing %q", want)
		}
	}
	if strings.Contains(board, "<h3>Holdings") {
		t.Error("paper.html should only show the scoreboard")
	}
	own := read(PageFile("test"))
	for _, want := range []string{`href="paper-test.html" aria-current="page"`, "<h3>Holdings", "1111", `href="paper.html"`} {
		if !strings.Contains(own, want) {
			t.Errorf("%s missing %q", PageFile("test"), want)
		}
	}
	if strings.Contains(own, "Scoreboard</h2>") {
		t.Errorf("%s should not repeat the scoreboard", PageFile("test"))
	}
}
