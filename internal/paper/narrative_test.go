package paper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/narratives"
)

func TestNarrative(t *testing.T) {
	n := 30
	m, b := synthetic(t, map[string][]float64{
		"00679B": flat(30, n), "2382": flat(300, n), "2881": flat(90, n), "3008": flat(2000, n),
	}, map[string][]float64{"3008": {1e6}}) // 3008 trades too little
	day := m.last()
	p := config.Default().Narratives
	p.CardsFile = filepath.Join(t.TempDir(), "narratives.toml")
	must(t, os.WriteFile(p.CardsFile, []byte(`
[[card]]
id = "fed"
title = "Fed hikes"
market = "polymarket:fed"
view = 0.70
yes_tw = ["2881"]
no_tw = ["00679B", "3008"]

[[card]]
id = "huang"
title = "Huang returns"
market = "polymarket:huang"
yes_tw = ["2382"]
`), 0o644))
	// The tables exist once the narratives package has run.
	_, err := narratives.Signals(m.db, p, day, nil)
	must(t, err)
	odds := func(id string, then, now float64) {
		_, err := m.db.Exec(`DELETE FROM pm_odds WHERE id = ?`, id)
		must(t, err)
		_, err = m.db.Exec(`INSERT OR REPLACE INTO pm_markets VALUES ('polymarket', ?, ?, '', '2027-01-01', ?, 1e6, 0, '')`, id, id, now)
		must(t, err)
		_, err = m.db.Exec(`INSERT INTO pm_odds VALUES ('polymarket', ?, date(?, '-7 days'), ?), ('polymarket', ?, ?, ?)`,
			id, day, then, id, day, now)
		must(t, err)
	}
	odds("fed", 0.86, 0.87)   // our 70% vs 87%: hold the "no" side
	odds("huang", 0.05, 0.20) // +15 points: hold the "yes" side

	s := narrative{p, m.db}
	orders, err := s.decide(m, b, mark(m, b, day))
	must(t, err)
	if got := tickers(orders); got != "buy:00679B buy:2382" {
		t.Fatalf("orders %q, want the two narratives' liquid winners", got)
	}
	for _, o := range orders {
		// Both gaps are big enough to hit max_narrative_share (30% of 5M).
		if o.Target != 1.5e6 {
			t.Errorf("%s target %.0f, want 1500000", o.Ticker, o.Target)
		}
	}
	if !strings.Contains(orders[0].Reason, "our 70% vs market 87%") {
		t.Errorf("reason %q", orders[0].Reason)
	}

	// Held: Huang's odds settle back (move under half of min_jump): sell 2382;
	// 00679B drops 20% below cost: stop loss, then not bought again.
	b.pos["2382"] = &position{Ticker: "2382", Opened: m.dates[n-5], Shares: 1000, Cost: 300_000, Gross: 300_000}
	b.pos["00679B"] = &position{Ticker: "00679B", Opened: m.dates[n-5], Shares: 1000, Cost: 37_500, Gross: 37_500}
	odds("huang", 0.17, 0.20)
	orders, err = s.decide(m, b, mark(m, b, day))
	must(t, err)
	if got := tickers(orders); got != "sell:00679B sell:2382" {
		t.Fatalf("orders %q, want the stop and the faded narrative sold", got)
	}
	if !strings.HasPrefix(orders[0].Reason, "stop loss") || !strings.Contains(orders[1].Reason, "Huang returns") {
		t.Errorf("reasons %q / %q", orders[0].Reason, orders[1].Reason)
	}
	delete(b.pos, "00679B")
	delete(b.pos, "2382")
	orders, err = s.decide(m, b, mark(m, b, day))
	must(t, err)
	if got := tickers(orders); got != "" {
		t.Errorf("after the stop: orders %q, want none (00679B cools off)", got)
	}
}
