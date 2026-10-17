package paper

import (
	"strings"
	"testing"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

// Issue #53: the same price-limit rule as the backtester.

func orderRow(t *testing.T, st *store.Store, side string) (status, fillDate string, price float64, note string) {
	t.Helper()
	must(t, st.DB.QueryRow(`SELECT status, fill_date, price, note FROM paper_orders WHERE side = ?`, side).
		Scan(&status, &fillDate, &price, &note))
	return
}

func TestBuySkippedAtLimitUpOpen(t *testing.T) {
	st := openStore(t)
	cfg := config.Default()
	s := script{"2026-10-01": {{Ticker: "1111", Side: "buy", Target: 100_000, Reason: "test"}}}
	must(t, st.UpsertPrices([]model.Price{price("1111", "2026-10-01", 100, 100, 100), price("0050", "2026-10-01", 50, 50, 50)}))
	runOnce(t, st, cfg, s)
	// 10-02 opens at 110: +10%, locked limit-up.
	must(t, st.UpsertPrices([]model.Price{price("1111", "2026-10-02", 110, 110, 110), price("0050", "2026-10-02", 50, 50, 50)}))
	runOnce(t, st, cfg, s)
	status, _, _, note := orderRow(t, st, "buy")
	if status != "skipped" || !strings.Contains(note, "limit-up") {
		t.Errorf("buy %s (%s), want skipped at limit-up", status, note)
	}
	b, err := load(st.DB, "test")
	must(t, err)
	if len(b.pos) != 0 || b.Cash != cfg.Paper.InitialCapital {
		t.Errorf("cash %.0f, positions %v: want nothing bought", b.Cash, b.pos)
	}
}

func TestBuyFillsBelowTheLimit(t *testing.T) {
	st := openStore(t)
	cfg := config.Default()
	s := script{"2026-10-01": {{Ticker: "1111", Side: "buy", Target: 100_000, Reason: "test"}}}
	must(t, st.UpsertPrices([]model.Price{price("1111", "2026-10-01", 100, 100, 100), price("0050", "2026-10-01", 50, 50, 50)}))
	runOnce(t, st, cfg, s)
	must(t, st.UpsertPrices([]model.Price{price("1111", "2026-10-02", 109, 110, 110), price("0050", "2026-10-02", 50, 50, 50)}))
	runOnce(t, st, cfg, s)
	if status, _, px, note := orderRow(t, st, "buy"); status != "filled" || px != 109 {
		t.Errorf("buy %s at %v (%s), want filled at 109 (+9%%)", status, px, note)
	}
}

func TestSellWaitsOutLimitDownOpens(t *testing.T) {
	st := openStore(t)
	cfg := config.Default()
	s := script{
		"2026-10-01": {{Ticker: "1111", Side: "buy", Target: 100_000, Reason: "test"}},
		"2026-10-02": {{Ticker: "1111", Side: "sell", Reason: "stop"}},
	}
	steps := [][]model.Price{
		{price("1111", "2026-10-01", 100, 100, 100), price("0050", "2026-10-01", 50, 50, 50)},
		{price("1111", "2026-10-02", 100, 100, 100), price("0050", "2026-10-02", 50, 50, 50)}, // buy fills, sell signalled
		{price("1111", "2026-10-05", 90, 90, 90), price("0050", "2026-10-05", 50, 50, 50)},    // locked: waits
		{price("1111", "2026-10-06", 81, 81, 81), price("0050", "2026-10-06", 50, 50, 50)},    // locked again
		{price("1111", "2026-10-07", 80, 82, 82), price("0050", "2026-10-07", 50, 50, 50)},    // -1.2%: fills
	}
	for i, rows := range steps {
		must(t, st.UpsertPrices(rows))
		runOnce(t, st, cfg, s)
		if i == 0 {
			continue
		}
		status, fillDate, px, note := orderRow(t, st, "sell")
		if i < 4 && status != "pending" {
			t.Fatalf("step %d: sell %s on %s at %v, want still pending", i, status, fillDate, px)
		}
		if i == 4 && (status != "filled" || fillDate != "2026-10-07" || px != 80 || !strings.Contains(note, "limit-down 2 day")) {
			t.Errorf("sell %s on %s at %v (%s), want filled 2026-10-07 at 80 after 2 locked opens", status, fillDate, px, note)
		}
	}
	// The wait adds no ledger rows: one order, one fill.
	if got, want := ledgerKinds(t, st), "start order:buy bench:buy fill:buy order:sell fill:sell"; got != want {
		t.Errorf("ledger %q, want %q", got, want)
	}
}

func TestExDividendOpenIsNotLimitDown(t *testing.T) {
	st := openStore(t)
	cfg := config.Default()
	s := script{
		"2026-10-01": {{Ticker: "1111", Side: "buy", Target: 100_000, Reason: "test"}},
		"2026-10-02": {{Ticker: "1111", Side: "sell", Reason: "test"}},
	}
	must(t, st.UpsertPrices([]model.Price{price("1111", "2026-10-01", 100, 100, 100), price("0050", "2026-10-01", 50, 50, 50)}))
	runOnce(t, st, cfg, s)
	must(t, st.UpsertPrices([]model.Price{price("1111", "2026-10-02", 100, 100, 100), price("0050", "2026-10-02", 50, 50, 50)}))
	runOnce(t, st, cfg, s)
	// 10-05 goes ex-dividend NT$10: the raw open is -10%, but the rebuilt
	// adjusted series scales earlier days by 0.9, so it is no move at all.
	must(t, st.UpsertPrices([]model.Price{
		price("1111", "2026-10-01", 100, 100, 90), price("1111", "2026-10-02", 100, 100, 90),
		price("1111", "2026-10-05", 90, 90, 90), price("0050", "2026-10-05", 50, 50, 50),
	}))
	runOnce(t, st, cfg, s)
	if status, fillDate, _, note := orderRow(t, st, "sell"); status != "filled" || fillDate != "2026-10-05" {
		t.Errorf("sell %s on %s (%s), want filled on the ex-dividend day", status, fillDate, note)
	}
}

// Issue #58: a sell whose first day after the signal is a trading halt fills
// at the first open the stock trades again, instead of waiting forever.
func TestSellWaitsOutAHalt(t *testing.T) {
	st := openStore(t)
	cfg := config.Default()
	cfg.Paper.MinCoverage = 0 // a two-stock market: one halted stock isn't a partly collected day
	s := script{
		"2026-10-01": {{Ticker: "1111", Side: "buy", Target: 100_000, Reason: "test"}},
		"2026-10-02": {{Ticker: "1111", Side: "sell", Reason: "stop"}},
	}
	steps := [][]model.Price{
		{price("1111", "2026-10-01", 100, 100, 100), price("0050", "2026-10-01", 50, 50, 50)},
		{price("1111", "2026-10-02", 100, 100, 100), price("0050", "2026-10-02", 50, 50, 50)}, // buy fills, sell signalled
		{price("0050", "2026-10-05", 50, 50, 50)},                                             // 1111 halted: waits
		{price("0050", "2026-10-06", 50, 50, 50)},                                             // still halted
		{price("1111", "2026-10-07", 95, 96, 96), price("0050", "2026-10-07", 50, 50, 50)},    // trades again: fills
		{price("1111", "2026-10-08", 97, 97, 97), price("0050", "2026-10-08", 50, 50, 50)},
	}
	for i, rows := range steps {
		must(t, st.UpsertPrices(rows))
		runOnce(t, st, cfg, s)
		if i == 0 {
			continue
		}
		status, fillDate, px, note := orderRow(t, st, "sell")
		if i < 4 && status != "pending" {
			t.Fatalf("step %d: sell %s on %s at %v, want still pending", i, status, fillDate, px)
		}
		if i >= 4 && (status != "filled" || fillDate != "2026-10-07" || px != 95 || !strings.Contains(note, "didn't trade 2 day")) {
			t.Errorf("step %d: sell %s on %s at %v (%s), want filled 2026-10-07 at 95 after 2 halted days", i, status, fillDate, px, note)
		}
	}
}
