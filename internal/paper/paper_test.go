package paper

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "radar.db"))
	must(t, err)
	t.Cleanup(func() { st.Close() })
	must(t, migrate(st.DB))
	return st
}

func day(s string) time.Time {
	d, err := time.Parse(dateLayout, s)
	if err != nil {
		panic(err)
	}
	return d
}

func price(ticker, d string, open, close, adj float64) model.Price {
	return model.Price{Market: model.MarketTW, Ticker: ticker, Date: day(d),
		Open: open, Close: close, AdjClose: adj}
}

// script is a strategy that places the orders listed for each signal day.
type script map[string][]*order

func (s script) decide(m *market, _ *book, _ holdings) ([]*order, error) {
	var out []*order
	for _, o := range s[m.last()] {
		c := *o
		out = append(out, &c)
	}
	return out, nil
}

// step loads the market as it stands and runs one portfolio once.
func runOnce(t *testing.T, st *store.Store, cfg config.Config, s strategy) {
	t.Helper()
	m, err := loadMarket(st.DB, 60, "", cfg.Paper.MinCoverage)
	must(t, err)
	must(t, step(st.DB, cfg, m, "test", s, time.Now()))
}

func TestFillsAtNextOpenWithCostsAndDividends(t *testing.T) {
	st := openStore(t)
	cfg := config.Default()
	cfg.Paper.InitialCapital = 1_000_000
	s := script{
		"2026-10-01": {{Ticker: "1111", Side: "buy", Target: 500_000, Reason: "test"}},
		"2026-10-05": {{Ticker: "1111", Side: "sell", Reason: "test"}},
	}
	days := []model.Price{
		price("1111", "2026-10-01", 99, 100, 100), price("0050", "2026-10-01", 50, 50, 50),
	}
	must(t, st.UpsertPrices(days))
	runOnce(t, st, cfg, s) // starts, signals the buy

	// 10-02: buy fills at 100. 10-05: goes ex-dividend NT$5; the collector
	// rebuilds the adjusted series, scaling every earlier day by 95/100.
	must(t, st.UpsertPrices([]model.Price{
		price("1111", "2026-10-01", 99, 100, 95), price("0050", "2026-10-01", 50, 50, 50),
		price("1111", "2026-10-02", 100, 100, 95), price("0050", "2026-10-02", 50, 50, 50),
		price("1111", "2026-10-05", 95, 95, 95), price("0050", "2026-10-05", 50, 50, 50),
	}))
	runOnce(t, st, cfg, s) // fills the buy, signals the sell on 10-05
	runOnce(t, st, cfg, s) // a second run on the same data changes nothing
	must(t, st.UpsertPrices([]model.Price{
		price("1111", "2026-10-06", 96, 96, 96), price("0050", "2026-10-06", 50, 50, 50),
	}))
	runOnce(t, st, cfg, s) // fills the sell at 96

	b, err := load(st.DB, "test")
	must(t, err)
	c := cfg.Costs.TW
	buyComm := c.Commission(500_000)
	// 5,000 shares bought at 100; the dividend arrives as 100/95 more shares,
	// sold at 96 (the dividend is reinvested, as in the backtest).
	sellValue := 5000 * (100.0 / 95) * 96
	sellComm, tax := c.Commission(sellValue), math.Floor(sellValue*c.SellTax)
	want := 1_000_000 - 500_000 - buyComm + sellValue - sellComm - tax
	if math.Abs(b.Cash-want) > 1e-6 {
		t.Errorf("cash = %.2f, want %.2f", b.Cash, want)
	}
	if len(b.pos) != 0 {
		t.Errorf("still holding %v", b.pos)
	}
	var n int
	must(t, st.DB.QueryRow(`SELECT COUNT(*) FROM paper_orders WHERE status = 'filled'`).Scan(&n))
	if n != 2 {
		t.Errorf("%d filled orders, want 2 (a repeat run must not signal again)", n)
	}
	if b.BenchDate != "2026-10-02" {
		t.Errorf("benchmark bought on %q, want the first fill day", b.BenchDate)
	}
	var liq, bench float64
	must(t, st.DB.QueryRow(`SELECT liquidation, bench FROM paper_equity WHERE portfolio = 'test' AND date = '2026-10-06'`).
		Scan(&liq, &bench))
	if math.Abs(liq-want) > 1e-6 {
		t.Errorf("equity row = %.2f, want the cash %.2f", liq, want)
	}
	if bench >= 1_000_000 || bench < 990_000 {
		t.Errorf("benchmark = %.2f, want just under the capital after fees", bench)
	}
}

func TestBuySkippedWhenStockDoesNotTrade(t *testing.T) {
	st := openStore(t)
	cfg := config.Default()
	s := script{"2026-10-01": {{Ticker: "2222", Side: "buy", Target: 100_000, Reason: "test"}}}
	must(t, st.UpsertPrices([]model.Price{price("2222", "2026-10-01", 10, 10, 10), price("0050", "2026-10-01", 50, 50, 50)}))
	runOnce(t, st, cfg, s)
	must(t, st.UpsertPrices([]model.Price{price("0050", "2026-10-02", 50, 50, 50)}))
	cfg.Paper.MinCoverage = 0 // one of two stocks is "complete" for this test
	runOnce(t, st, cfg, s)
	var status, note string
	must(t, st.DB.QueryRow(`SELECT status, note FROM paper_orders`).Scan(&status, &note))
	if status != "skipped" {
		t.Errorf("status = %s (%s), want skipped", status, note)
	}
}

func TestWaitsForADayToBeCollected(t *testing.T) {
	st := openStore(t)
	cfg := config.Default()
	var rows []model.Price
	for _, tk := range []string{"1101", "1102", "1103", "1104", "0050"} {
		rows = append(rows, price(tk, "2026-10-01", 10, 10, 10))
	}
	rows = append(rows, price("1101", "2026-10-02", 10, 10, 10)) // 1 of 5 so far
	must(t, st.UpsertPrices(rows))
	m, err := loadMarket(st.DB, 60, "", cfg.Paper.MinCoverage)
	must(t, err)
	if m.ready || m.last() != "2026-10-01" {
		t.Errorf("ready=%v last=%s, want the half-collected 10-02 left out", m.ready, m.last())
	}
}

func TestRebalanceDay(t *testing.T) {
	m := &market{dates: []string{"2026-10-07", "2026-10-08", "2026-10-13", "2026-10-14"}}
	// 10-10 is a holiday: the deadline moves to 10-13, the rebalance to 10-14.
	for d, want := range map[string]bool{"2026-10-08": false, "2026-10-13": false, "2026-10-14": true} {
		if got := rebalanceDay(m, d, 10); got != want {
			t.Errorf("rebalanceDay(%s) = %v, want %v", d, got, want)
		}
	}
}

func TestCycleEntryAndExit(t *testing.T) {
	s := cycle{p: config.Default().Strategy.CycleBottom}
	nan := math.NaN()
	// 12 months down, then two months up to +5%.
	y := []float64{-5, -8, -10, -12, -15, -20, -18, -16, -14, -12, -10, -9, -4, 5}
	if gain, ok := s.entry(y); !ok || gain != 14 {
		t.Errorf("entry = %v, %v; want 14, true", gain, ok)
	}
	tooFar := append(append([]float64{}, y[:13]...), 15)
	if _, ok := s.entry(tooFar); ok {
		t.Error("bought above max_entry_yoy")
	}
	gap := append(append([]float64{}, y[:12]...), nan, 5)
	if _, ok := s.entry(gap); ok {
		t.Error("bought across a missing month")
	}
	if !falling([]float64{10, 8, 3}, 2) || falling([]float64{10, 12, 3}, 2) {
		t.Error("falling wrong")
	}
}
