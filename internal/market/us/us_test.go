package us

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

func sample(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseChart(t *testing.T) {
	c, err := parseChart(sample(t, "yahoo_chart_AAPL_range.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Apple Inc." || c.Symbol != "AAPL" || c.Currency != "USD" || c.Events != 0 {
		t.Fatalf("meta: %+v", c)
	}
	if len(c.Bars) != 23 {
		t.Fatalf("bars: got %d, want 23", len(c.Bars))
	}
	b := c.Bars[0]
	if b.Date != "2025-10-01" || math.Abs(b.Close-255.45) > 0.01 || math.Abs(b.AdjClose-254.51) > 0.01 || b.Volume != 48713900 {
		t.Fatalf("first bar: %+v", b)
	}
	if last := c.Bars[len(c.Bars)-1]; last.Date != "2025-10-31" {
		t.Fatalf("last bar date %s", last.Date)
	}

	// NVDA June 2024: a 10:1 split and a dividend in the window.
	c, err = parseChart(sample(t, "yahoo_chart_NVDA_split.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Events != 2 {
		t.Fatalf("events: got %d, want 2", c.Events)
	}

	if _, err := parseChart(sample(t, "yahoo_chart_BAD.json")); err == nil || !strings.Contains(err.Error(), "delisted") {
		t.Fatalf("unknown symbol: got %v", err)
	}
}

func TestLastSession(t *testing.T) {
	for _, c := range []struct{ utc, want string }{
		{"2026-10-30T20:00:00Z", "2026-10-29"}, // Fri 16:00 New York: not settled yet
		{"2026-10-30T21:00:00Z", "2026-10-30"}, // Fri 17:00
		{"2026-10-31T12:00:00Z", "2026-10-30"}, // Saturday
		{"2026-11-02T15:00:00Z", "2026-10-30"}, // Monday morning (EST from Nov 1)
		{"2026-11-02T22:00:00Z", "2026-11-02"}, // Monday 17:00 EST
	} {
		now, _ := time.Parse(time.RFC3339, c.utc)
		if got := lastSession(now); got != c.want {
			t.Errorf("lastSession(%s) = %s, want %s", c.utc, got, c.want)
		}
	}
}

// server serves testdata/yahoo_chart_<SYMBOL>.json for /<SYMBOL>.
func server(t *testing.T, files map[string]string) (*yahoo, *atomic.Int32) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if r.Header.Get("User-Agent") != userAgent {
			t.Errorf("no User-Agent")
		}
		f, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write(sample(t, "yahoo_chart_BAD.json"))
			return
		}
		w.Write(sample(t, f))
	}))
	t.Cleanup(srv.Close)
	return &yahoo{base: srv.URL + "/", http: srv.Client()}, &n
}

func openStore(t *testing.T) *sql.DB {
	st, err := store.Open(filepath.Join(t.TempDir(), "radar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st.DB
}

func count(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCollect(t *testing.T) {
	db := openStore(t)
	y, reqs := server(t, map[string]string{
		"AAPL": "yahoo_chart_AAPL_range.json",
		"NVDA": "yahoo_chart_NVDA_split.json",
	})
	cfg := config.US{Tickers: []string{"AAPL", "nvda", "GONE", "AAPL"}, HistoryStart: "2015-01-01"}
	ctx := context.Background()

	// Friday 2025-10-31, 15:00 New York: that day's bar is still trading.
	now, _ := time.Parse(time.RFC3339, "2025-10-31T19:00:00Z")
	if err := collect(ctx, cfg, db, y, now); err != nil {
		t.Fatal(err)
	}
	if got := reqs.Load(); got != 3 {
		t.Fatalf("requests: got %d, want 3 (AAPL, NVDA, GONE once each)", got)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM us_prices WHERE ticker = 'AAPL'`); n != 22 {
		t.Fatalf("AAPL rows: got %d, want 22 (no live bar)", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM us_prices WHERE ticker = 'GONE'`); n != 0 {
		t.Fatal("rows for an unknown symbol")
	}
	var name string
	db.QueryRow(`SELECT name FROM us_companies WHERE ticker = 'AAPL'`).Scan(&name)
	if name != "Apple Inc." {
		t.Fatalf("name %q", name)
	}

	// An hour later nothing is due: everyone was fetched recently.
	if err := collect(ctx, cfg, db, y, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := reqs.Load(); got != 3 {
		t.Fatalf("refetched within %v: %d requests", minRefetch, got)
	}

	// Saturday: the 31st has closed. AAPL is incremental (one request, no
	// events); NVDA's window has a split, so it is refetched in full (two).
	sat, _ := time.Parse(time.RFC3339, "2025-11-01T16:00:00Z")
	if err := collect(ctx, cfg, db, y, sat); err != nil {
		t.Fatal(err)
	}
	if got := reqs.Load(); got != 3+1+2+1 {
		t.Fatalf("requests: got %d, want 7", got)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM us_prices WHERE ticker = 'AAPL'`); n != 23 {
		t.Fatalf("AAPL rows: got %d, want 23", n)
	}
	// Up to date now: no more AAPL requests, even past the refetch gap.
	if err := collect(ctx, cfg, db, y, sat.Add(8*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := reqs.Load(); got != 7+2+1 {
		t.Fatalf("requests: got %d, want 10 (NVDA full refetch + GONE)", got)
	}
}

func TestCollectAllFail(t *testing.T) {
	db := openStore(t)
	y, _ := server(t, nil)
	now, _ := time.Parse(time.RFC3339, "2025-11-01T16:00:00Z")
	err := collect(context.Background(), config.US{Tickers: []string{"X", "Y"}, HistoryStart: "2015-01-01"}, db, y, now)
	if err == nil {
		t.Fatal("want an error when every ticker fails")
	}
}

func TestRender(t *testing.T) {
	db := openStore(t)
	dir := t.TempDir()
	cfg := config.US{Tickers: []string{"AAPL", "MSFT"}, Benchmarks: []string{"SPY"}, HistoryStart: "2015-01-01"}

	// Before any prices the page says so.
	if err := render(db, cfg, dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "us.html"))
	if !strings.Contains(string(b), "No prices yet") {
		t.Fatal("expected the empty state")
	}

	y, _ := server(t, map[string]string{"AAPL": "yahoo_chart_AAPL_range.json", "SPY": "yahoo_chart_AAPL_range.json"})
	now, _ := time.Parse(time.RFC3339, "2025-11-01T16:00:00Z")
	if err := collect(context.Background(), cfg, db, y, now); err != nil {
		t.Fatal(err)
	}
	// SPY one day behind: flagged stale.
	db.Exec(`DELETE FROM us_prices WHERE ticker = 'SPY' AND date = '2025-10-31'`)
	if err := render(db, cfg, dir, now); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "us.html"))
	page := string(b)

	p, err := load(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Rows) != 1 || len(p.Benchmarks) != 1 || len(p.Missing) != 1 || p.DataEnd != "2025-10-31" {
		t.Fatalf("page: %+v", p)
	}
	r := p.Rows[0]
	var last, prev, first float64
	db.QueryRow(`SELECT adj_close FROM us_prices WHERE ticker = 'AAPL' AND date = '2025-10-31'`).Scan(&last)
	db.QueryRow(`SELECT adj_close FROM us_prices WHERE ticker = 'AAPL' AND date = '2025-10-30'`).Scan(&prev)
	db.QueryRow(`SELECT adj_close FROM us_prices WHERE ticker = 'AAPL' AND date = '2025-10-02'`).Scan(&first)
	if math.Abs(r.D1-(last/prev-1)) > 1e-9 || math.Abs(r.M1-(last/first-1)) > 1e-9 {
		t.Fatalf("changes: %+v", r)
	}
	if !math.IsNaN(r.Y1) || !math.IsNaN(r.YTD) || r.FromHigh > 0 || r.DollarVol <= 0 {
		t.Fatalf("row: %+v", r)
	}
	if !p.Benchmarks[0].Stale || r.Stale {
		t.Fatal("stale flags")
	}
	for _, want := range []string{
		"Apple Inc.", "prices to 2025-10-31", "STALE", "No prices yet for: MSFT",
		fmt.Sprintf("%+.1f%%", r.D1*100), `href="https://finance.yahoo.com/quote/AAPL"`, `aria-current="page">US<`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}
