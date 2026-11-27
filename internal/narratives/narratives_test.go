package narratives

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "radar.db"))
	must(t, err)
	t.Cleanup(func() { st.Close() })
	must(t, migrate(st.DB))
	return st.DB
}

func params(t *testing.T, cards string) config.Narratives {
	t.Helper()
	p := config.Default().Narratives
	p.CardsFile = filepath.Join(t.TempDir(), "narratives.toml")
	must(t, os.WriteFile(p.CardsFile, []byte(cards), 0o644))
	return p
}

// server serves the saved samples like the two APIs.
func server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != userAgent {
			t.Errorf("missing User-Agent on %s", r.URL)
		}
		switch {
		case r.URL.Path == "/pm/markets" && r.URL.Query().Get("slug") != "":
			w.Write([]byte("[]"))
		case r.URL.Path == "/pm/markets":
			b, _ := os.ReadFile("testdata/polymarket_markets.json")
			w.Write(b)
		case r.URL.Path == "/k/markets":
			b, _ := os.ReadFile("testdata/kalshi_markets.json")
			w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testSources(srv *httptest.Server) []source {
	return []source{
		&polymarket{base: srv.URL + "/pm", client: srv.Client(), limit: 1000},
		&kalshi{base: srv.URL + "/k", client: srv.Client(), pages: 2},
	}
}

func TestParseSamples(t *testing.T) {
	var pm []pmMarket
	b, _ := os.ReadFile("testdata/polymarket_markets.json")
	must(t, json.Unmarshal(b, &pm))
	got := parsePolymarket(pm)
	if len(got) != 3 { // the Lakers market isn't yes/no
		t.Fatalf("polymarket: %d markets, want 3: %+v", len(got), got)
	}
	fed := got[0]
	if fed.Prob != 0.875 || fed.EndDate != "2026-12-09" || fed.Volume24h != 1250000.5 ||
		fed.URL != "https://polymarket.com/event/fed-decision-in-december-2026" {
		t.Errorf("fed market: %+v", fed)
	}
	if got[2].Volume24h != 120.5 {
		t.Errorf("volume as a string: %v", got[2].Volume24h)
	}

	var kp kPage
	b, _ = os.ReadFile("testdata/kalshi_markets.json")
	must(t, json.Unmarshal(b, &kp))
	ks := parseKalshi(kp.Markets)
	if len(ks) != 2 {
		t.Fatalf("kalshi: %d markets, want 2 (one never traded): %+v", len(ks), ks)
	}
	if k := ks[0]; math.Abs(k.Prob-0.70) > 1e-9 || k.Question != "Fed decision in Dec 2026? · Hike 25bps" ||
		k.URL != "https://kalshi.com/markets/kxfeddecision" || k.EndDate != "2026-12-09" || k.Closed {
		t.Errorf("kalshi fed: %+v", k)
	}
	if k := ks[1]; math.Abs(k.Prob-0.22) > 1e-9 { // no quotes: last trade, in cents
		t.Errorf("kalshi recession prob %v", k.Prob)
	}
}

func TestCollectStoresAndWaits(t *testing.T) {
	db := openDB(t)
	srv := server(t)
	p := params(t, "")
	now := time.Date(2026, 11, 27, 3, 0, 0, 0, time.UTC)
	must(t, collect(context.Background(), p, db, testSources(srv), nil, now))
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM pm_markets`).Scan(&n)
	if n != 4 { // tiny market (volume 120) left out; 2 Polymarket + 2 Kalshi
		t.Errorf("%d markets stored, want 4", n)
	}
	db.QueryRow(`SELECT COUNT(*) FROM pm_odds WHERE date = '2026-11-27'`).Scan(&n)
	if n != 4 {
		t.Errorf("%d odds rows, want 4", n)
	}
	// An hour later: within fetch_hours, nothing is fetched.
	srv.Close()
	must(t, collect(context.Background(), p, db, testSources(srv), nil, now.Add(time.Hour)))
	// After fetch_hours the closed server makes the step fail, softly.
	if err := collect(context.Background(), p, db, testSources(srv), nil, now.Add(4*time.Hour)); err == nil {
		t.Error("want an error when the sources are down")
	}
}

func TestCardsInRepo(t *testing.T) {
	cards, err := LoadCards("../../narratives.toml")
	must(t, err)
	if len(cards) == 0 {
		t.Fatal("no cards in narratives.toml")
	}
	if _, err := LoadCards(filepath.Join(t.TempDir(), "none.toml")); err != nil {
		t.Errorf("a missing file should mean no cards: %v", err)
	}
	bad := params(t, "[[card]]\nid = \"x\"\ntitle = \"X\"\nkeywords = [\"a\"]\nview = 1.5\n")
	if _, err := LoadCards(bad.CardsFile); err == nil || !strings.Contains(err.Error(), "view") {
		t.Errorf("want a view error, got %v", err)
	}
}

func addMarket(t *testing.T, db *sql.DB, src, id, q, end string, odds map[string]float64) {
	t.Helper()
	var last string
	var prob float64
	for d, p := range odds {
		_, err := db.Exec(`INSERT INTO pm_odds VALUES (?,?,?,?)`, src, id, d, p)
		must(t, err)
		if d > last {
			last, prob = d, p
		}
	}
	_, err := db.Exec(`INSERT INTO pm_markets VALUES (?,?,?,?,?,?,?,0,?)`, src, id, q, "https://x/"+id, end, prob, 50000.0, "2026-11-27T00:00:00Z")
	must(t, err)
}

const testCards = `
[[card]]
id = "fed"
title = "Fed hikes"
market = "polymarket:fed-hike"
view = 0.70
yes_tw = ["2881", "2882"]
no_tw = ["00679B"]

[[card]]
id = "huang"
title = "Huang returns"
keywords = ["huang", "rejoin"]
yes_tw = ["2382"]

[[card]]
id = "settled"
title = "Settled"
keywords = ["settled"]
yes_tw = ["2330"]

[[card]]
id = "nowhere"
title = "No market"
keywords = ["nothing like this"]
yes_tw = ["2330"]
`

func TestSignals(t *testing.T) {
	db := openDB(t)
	p := params(t, testCards)
	addMarket(t, db, "polymarket", "fed-hike", "Fed hike in Dec?", "2026-12-09",
		map[string]float64{"2026-11-20": 0.85, "2026-11-27": 0.87})
	addMarket(t, db, "polymarket", "huang-2026", "Jensen Huang rejoins NVIDIA by Dec 31?", "2026-12-31",
		map[string]float64{"2026-11-20": 0.05, "2026-11-27": 0.20})
	addMarket(t, db, "kalshi", "SETTLED", "Already settled?", "2027-01-01",
		map[string]float64{"2026-11-27": 0.99})

	sigs, err := Signals(db, p, "2026-11-27", nil)
	must(t, err)
	by := map[string]Signal{}
	for _, s := range sigs {
		by[s.ID] = s
	}
	if s := by["fed"]; s.Side != "no" || math.Abs(s.Weight-0.17) > 1e-9 || strings.Join(s.Tickers, ",") != "00679B" {
		t.Errorf("fed: view 70%% vs 87%% should hold the no side with weight 0.17: %+v", s)
	}
	if s := by["huang"]; s.Side != "yes" || math.Abs(s.Weight-0.075) > 1e-9 {
		t.Errorf("huang: a 15-point jump should hold yes at 0.15 x 0.5: %+v", s)
	}
	if s := by["settled"]; s.Side != "" || !strings.HasPrefix(s.Basis, "settled") {
		t.Errorf("settled: %+v", s)
	}
	if s := by["nowhere"]; s.Side != "" || s.Market != nil {
		t.Errorf("nowhere: %+v", s)
	}

	// Hysteresis: a 5-point gap doesn't open a trade but keeps a held one.
	_, err = db.Exec(`UPDATE pm_odds SET prob = 0.75 WHERE id = 'fed-hike' AND date = '2026-11-27'`)
	must(t, err)
	sigs, _ = Signals(db, p, "2026-11-27", nil)
	if s := sigs[0]; s.Side != "" {
		t.Errorf("5-point gap, not held: want no trade, got %+v", s)
	}
	sigs, _ = Signals(db, p, "2026-11-27", map[string]bool{"00679B": true})
	if s := sigs[0]; s.Side != "no" || !s.Held {
		t.Errorf("5-point gap, held: want to stay, got %+v", s)
	}
}

type fakeScanner struct {
	answer string
	calls  int
}

func (f *fakeScanner) scan(_ context.Context, prompt string) (string, error) {
	f.calls++
	if !strings.Contains(prompt, "polymarket:fed-hike") || !strings.Contains(prompt, "2330 台積電") {
		return "", os.ErrInvalid
	}
	return f.answer, nil
}
func (f *fakeScanner) model() string { return "test-model" }

func TestNewsScan(t *testing.T) {
	db := openDB(t)
	p := params(t, testCards)
	addMarket(t, db, "polymarket", "fed-hike", "Fed hike in Dec?", "2026-12-09", map[string]float64{"2026-11-27": 0.87})
	_, err := db.Exec(`INSERT INTO companies (market, ticker, name) VALUES ('TW', '2330', '台積電'), ('TW', '2881', '富邦金')`)
	must(t, err)
	_, err = db.Exec(`INSERT INTO prices (market, ticker, date, close, turnover) VALUES
		('TW', '2330', '2026-11-26', 1000, 9e10), ('TW', '2881', '2026-11-26', 90, 1e9)`)
	must(t, err)
	f := &fakeScanner{answer: "Here you go:\n```json\n" + `{"summary": "Hike odds firmed.", "cards": [
		{"id": "Fed-Hike-News", "title": "Hike sticks", "market": "polymarket:fed-hike", "view": 0.95,
		 "yes_tw": ["2881", "9999", "2330.TW"], "no_tw": []},
		{"id": "made-up", "title": "Bad market", "market": "polymarket:nope", "view": 0.5}]}` + "\n```"}
	now := time.Date(2026, 11, 27, 3, 0, 0, 0, time.UTC)
	hand, _ := LoadCards(p.CardsFile)
	must(t, scanIfDue(context.Background(), p, db, f, hand, now))
	cards, at, err := latestScan(db)
	must(t, err)
	if len(cards) != 1 || cards[0].ID != "fed-hike-news" || strings.Join(cards[0].YesTW, ",") != "2881,2330" || at == "" {
		t.Fatalf("scan cards: %+v", cards)
	}
	// Not due again for a week.
	must(t, scanIfDue(context.Background(), p, db, f, hand, now.Add(48*time.Hour)))
	if f.calls != 1 {
		t.Errorf("scanned %d times, want 1", f.calls)
	}
	// The news card trades next to the hand ones.
	sigs, err := Signals(db, p, "2026-11-27", nil)
	must(t, err)
	if s := sigs[len(sigs)-1]; s.ID != "fed-hike-news" || s.Side != "yes" || s.Source != "news" {
		t.Errorf("news signal: %+v", s)
	}
	p.TradeNews = false
	if sigs, _ := Signals(db, p, "2026-11-27", nil); len(sigs) != 4 {
		t.Errorf("trade_news off: %d signals, want the 4 hand cards", len(sigs))
	}
}

func TestRender(t *testing.T) {
	db := openDB(t)
	p := params(t, testCards)
	addMarket(t, db, "polymarket", "fed-hike", "Fed hike in Dec?", "2026-12-09",
		map[string]float64{"2026-11-20": 0.60, "2026-11-27": 0.87})
	dir := t.TempDir()
	now := time.Date(2026, 11, 27, 3, 0, 0, 0, time.UTC)
	must(t, render(db, p, dir, now, true))
	b, err := os.ReadFile(filepath.Join(dir, "narratives.html"))
	must(t, err)
	html := string(b)
	if os.Getenv("DUMP") != "" {
		os.WriteFile(os.Getenv("DUMP"), b, 0o644)
	}
	for _, want := range []string{"Fed hikes", "our 70% vs market 87%", "Huang returns", "ANTHROPIC_API_KEY", "Fed hike in Dec?", "&#43;27"} {
		if !strings.Contains(html, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}
