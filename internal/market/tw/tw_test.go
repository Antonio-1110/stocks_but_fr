package tw

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

// The testdata files are samples in FinMind v4's response format.
func load[T any](t *testing.T, name string) []T {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parseEnvelope[T](name, b)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestBuildUniverse(t *testing.T) {
	got := buildUniverse(load[stockInfo](t, "TaiwanStockInfo.json"), load[delisting](t, "TaiwanStockDelisting.json"))
	var tickers []string
	by := map[string]model.Company{}
	for _, c := range got {
		tickers = append(tickers, c.Ticker)
		by[c.Ticker] = c
	}
	if want := "0050 006201 2330 2888 3474 6488"; strings.Join(tickers, " ") != want {
		t.Fatalf("tickers = %v, want %s", tickers, want)
	}
	if c := by["2330"]; c.Industry != "半導體業" || c.Exchange != "TWSE" || c.Name != "台積電" || !c.DelistedOn.IsZero() {
		t.Errorf("2330 = %+v", c)
	}
	if c := by["6488"]; c.Exchange != "TPEx" {
		t.Errorf("6488 exchange = %q", c.Exchange)
	}
	if c := by["3474"]; c.DelistedOn.Format("2006-01-02") != "2016-12-06" || c.Name != "華亞科" {
		t.Errorf("3474 = %+v", c)
	}
	if c := by["2888"]; !c.DelistedOn.IsZero() {
		t.Errorf("2888 is listed again, got delisted %v", c.DelistedOn)
	}
}

func TestMergePrices(t *testing.T) {
	got := mergePrices("2330", load[priceRow](t, "TaiwanStockPrice_2330.json"), load[priceRow](t, "TaiwanStockPriceAdj_2330.json"))
	if len(got) != 4 {
		t.Fatalf("got %d rows", len(got))
	}
	p := got[0]
	if p.Date.Format("2006-01-02") != "2024-06-11" || p.Close != 885 || p.Open != 877 || p.High != 889 || p.Low != 876 ||
		p.Volume != 30116510 || p.Turnover != 26655064870 || math.Abs(p.AdjClose-881.15) > 1e-9 {
		t.Errorf("first row = %+v", p)
	}
	if p := got[3]; p.AdjClose != p.Close {
		t.Errorf("newest adj close %v != close %v", p.AdjClose, p.Close)
	}

	// A day missing from the adjusted series uses the next day's factor.
	adj := load[priceRow](t, "TaiwanStockPriceAdj_2330.json")
	got = mergePrices("2330", load[priceRow](t, "TaiwanStockPrice_2330.json"), adj[1:])
	if want := 885 * 916.0 / 920; math.Abs(got[0].AdjClose-want) > 1e-9 {
		t.Errorf("filled adj close = %v, want %v", got[0].AdjClose, want)
	}
}

func TestMergeDropsSuspendedDays(t *testing.T) {
	got := mergePrices("3474", load[priceRow](t, "TaiwanStockPrice_3474.json"), nil)
	if len(got) != 1 || got[0].AdjClose != 48 {
		t.Fatalf("got %+v", got)
	}
}

func TestRateLimitStops(t *testing.T) {
	b, _ := os.ReadFile("testdata/rate_limited.json")
	if _, err := parseEnvelope[priceRow]("x", b); !errors.Is(err, ErrStop) {
		t.Fatalf("err = %v, want ErrStop", err)
	}
}

// fakeFinMind serves testdata/<dataset>[_<data_id>].json, filtered by
// start_date, and answers 402 after limit requests.
func fakeFinMind(t *testing.T, limit int) (*FinMind, *[]string) {
	var calls []string
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Header.Get("User-Agent") == "" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing headers: %v", r.Header)
		}
		n++
		if n > limit {
			w.WriteHeader(http.StatusPaymentRequired)
			w.Write([]byte(`{"msg":"Requests reach the upper limit.","status":402}`))
			return
		}
		name := q.Get("dataset")
		if id := q.Get("data_id"); id != "" {
			name += "_" + id
		}
		calls = append(calls, name+"@"+q.Get("start_date"))
		b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
		if err != nil {
			w.Write([]byte(`{"msg":"success","status":200,"data":[]}`))
			return
		}
		if start := q.Get("start_date"); start != "" {
			var keep []map[string]any
			for _, row := range load[map[string]any](t, name+".json") {
				if row["date"].(string) >= start {
					keep = append(keep, row)
				}
			}
			b, _ = json.Marshal(map[string]any{"msg": "success", "status": 200, "data": keep})
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return &FinMind{BaseURL: srv.URL, Token: "tok", Budget: 1000, HTTP: srv.Client()}, &calls
}

func openStore(t *testing.T) *store.Store {
	st, err := store.Open(filepath.Join(t.TempDir(), "radar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestCollectBackfillsAndResumes(t *testing.T) {
	cfg := config.Default()
	st := openStore(t)
	ctx := context.Background()
	day1 := time.Date(2024, 6, 14, 18, 0, 0, 0, taipei)

	// Budget: universe (2) + 0050 (2) + one more ticker (2), then 402.
	c, calls := fakeFinMind(t, 6)
	if err := collect(ctx, cfg, st, c, day1); err != nil {
		t.Fatal(err)
	}
	cos, _ := st.Companies(model.MarketTW)
	if len(cos) != 6 {
		t.Fatalf("stored %d companies", len(cos))
	}
	if d, _ := st.LatestPriceDate(model.MarketTW, "0050"); d.Format("2006-01-02") != "2024-06-14" {
		t.Fatalf("0050 latest = %v", d)
	}
	if (*calls)[2] != "TaiwanStockPrice_0050@2010-01-01" {
		t.Errorf("benchmark not fetched first: %v", *calls)
	}

	// Next run resumes the rest; tickers done today are not fetched again.
	c, calls = fakeFinMind(t, 1000)
	if err := collect(ctx, cfg, st, c, day1); err != nil {
		t.Fatal(err)
	}
	for _, call := range *calls {
		if strings.Contains(call, "_0050") || strings.Contains(call, "_006201") {
			t.Errorf("refetched a ticker already checked today: %s", call)
		}
	}
	if d, _ := st.LatestPriceDate(model.MarketTW, "2330"); d.Format("2006-01-02") != "2024-06-14" {
		t.Errorf("2330 latest = %v", d)
	}
	if d, _ := st.LatestPriceDate(model.MarketTW, "3474"); d.Format("2006-01-02") != "2016-12-01" {
		t.Errorf("delisted 3474 latest = %v", d)
	}

	// A day later: up-to-date or delisted tickers are skipped, stale ones
	// fetch from their newest stored day.
	c, calls = fakeFinMind(t, 1000)
	if err := collect(ctx, cfg, st, c, day1.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	for _, call := range *calls {
		if strings.Contains(call, "_3474") || strings.Contains(call, "_2330") {
			t.Errorf("unexpected fetch: %s", call)
		}
	}
}

func TestDividendRescalesHistory(t *testing.T) {
	st := openStore(t)
	d := func(s string) time.Time { return parseDay(s) }
	p := func(date string, close, adj float64) model.Price {
		return model.Price{Market: model.MarketTW, Ticker: "2330", Date: d(date), Close: close, AdjClose: adj}
	}
	if err := writePrices(st, "2330", time.Time{}, []model.Price{p("2024-06-11", 885, 885), p("2024-06-12", 920, 920)}); err != nil {
		t.Fatal(err)
	}
	// Ex-dividend on 06-13: FinMind now reports 06-12 adjusted to 916.
	if err := writePrices(st, "2330", d("2024-06-12"), []model.Price{p("2024-06-12", 920, 916), p("2024-06-13", 914, 914)}); err != nil {
		t.Fatal(err)
	}
	var adj float64
	st.DB.QueryRow(`SELECT adj_close FROM prices WHERE ticker = '2330' AND date = '2024-06-11'`).Scan(&adj)
	if want := 885 * 916.0 / 920; math.Abs(adj-want) > 1e-9 {
		t.Errorf("rescaled adj close = %v, want %v", adj, want)
	}
}
