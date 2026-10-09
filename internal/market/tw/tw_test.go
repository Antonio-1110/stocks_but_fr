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

// The testdata files are real FinMind v4 responses, trimmed to a few rows.
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
	delisted := load[delisting](t, "TaiwanStockDelisting.json")
	// Not in the saved response: a delisting older than a current listing,
	// as for a stock listed again.
	delisted = append(delisted, delisting{Date: "2001-01-02", StockID: "6488", Name: "環球晶"})
	got := buildUniverse(load[stockInfo](t, "TaiwanStockInfo.json"), delisted)
	var tickers []string
	by := map[string]model.Company{}
	for _, c := range got {
		tickers = append(tickers, c.Ticker)
		by[c.Ticker] = c
	}
	if want := "0050 006201 00732 2330 2888 3474 6488"; strings.Join(tickers, " ") != want {
		t.Fatalf("tickers = %v, want %s", tickers, want)
	}
	if c := by["2330"]; c.Industry != "半導體業" || c.Exchange != "TWSE" || c.Name != "台積電" || !c.DelistedOn.IsZero() {
		t.Errorf("2330 = %+v", c)
	}
	if c := by["6488"]; c.Exchange != "TPEx" || !c.DelistedOn.IsZero() {
		t.Errorf("6488 = %+v, want listed on TPEx", c)
	}
	if c := by["3474"]; c.DelistedOn.Format("2006-01-02") != "2016-12-06" || c.Name != "華亞科" {
		t.Errorf("3474 = %+v", c)
	}
	// FinMind still lists 2888 with a date after its delisting, but not the
	// current one: it is delisted.
	if c := by["2888"]; c.DelistedOn.Format("2006-01-02") != "2025-07-24" {
		t.Errorf("2888 delisted %v, want 2025-07-24", c.DelistedOn)
	}
}

func TestMergePricesDividend(t *testing.T) {
	events := buildEvents(load[dividendRow](t, "TaiwanStockDividendResult_2330.json"), nil, nil)
	got := mergePrices("2330", load[priceRow](t, "TaiwanStockPrice_2330.json"), events)
	if len(got) != 4 {
		t.Fatalf("got %d rows", len(got))
	}
	// Ex-dividend on 2024-06-13: the reference price was 905.5 against a
	// 909 close the day before.
	p := got[0]
	if p.Date.Format("2006-01-02") != "2024-06-11" || p.Close != 883 || p.Open != 892 || p.High != 895 || p.Low != 883 ||
		p.Volume != 57435637 || p.Turnover != 51091497348 || math.Abs(p.AdjClose-883*905.5/909) > 1e-9 {
		t.Errorf("first row = %+v", p)
	}
	if p := got[1]; math.Abs(p.AdjClose-905.5) > 1e-9 {
		t.Errorf("day before ex-dividend adj close = %v, want 905.5", p.AdjClose)
	}
	for _, p := range got[2:] {
		if p.AdjClose != p.Close {
			t.Errorf("%s adj close %v != close %v", p.Date.Format("2006-01-02"), p.AdjClose, p.Close)
		}
	}
}

func TestMergePricesSplit(t *testing.T) {
	var splits []splitRow
	for _, r := range load[splitRow](t, "TaiwanStockSplitPrice.json") {
		if r.StockID == "0050" {
			splits = append(splits, r)
		}
	}
	// 0050 split 1:4 on 2025-06-18 (halted 06-11 to 06-17).
	got := mergePrices("0050", load[priceRow](t, "split_0050_price.json"), buildEvents(nil, nil, splits))
	if len(got) != 4 || math.Abs(got[1].AdjClose-47.16) > 1e-9 || math.Abs(got[0].AdjClose-183.7*47.16/188.65) > 1e-9 ||
		got[2].AdjClose != got[2].Close {
		t.Fatalf("got %+v", got)
	}
}

func TestMergePricesCapitalReduction(t *testing.T) {
	events := buildEvents(nil, load[reductionRow](t, "reduction_3481.json"), nil)
	got := mergePrices("3481", load[priceRow](t, "reduction_3481_price.json"), events)
	// Cash-refund reduction on 2022-10-11: last close 10.45, reference 10.49.
	if len(got) != 4 || math.Abs(got[1].AdjClose-10.49) > 1e-9 || got[2].AdjClose != got[2].Close {
		t.Fatalf("got %+v", got)
	}
}

func TestMergeDropsSuspendedDays(t *testing.T) {
	raw := load[priceRow](t, "TaiwanStockPrice_3474.json")
	raw = append(raw, priceRow{Date: "2016-11-30", StockID: "3474"}) // a day with no trades
	got := mergePrices("3474", raw, nil)
	if len(got) != 2 || got[1].AdjClose != 29.8 {
		t.Fatalf("got %+v", got)
	}
}

func TestPaidDatasetIsAnError(t *testing.T) {
	b, _ := os.ReadFile("testdata/paid_only.json")
	_, err := parseEnvelope[priceRow]("x", b)
	if err == nil || errors.Is(err, ErrStop) {
		t.Fatalf("err = %v, want a non-stop error", err)
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

	// Budget: universe (2) + splits (1) + 0050 (3) + one more ticker (3), then 402.
	c, calls := fakeFinMind(t, 9)
	if err := collect(ctx, cfg, st, c, day1); err != nil {
		t.Fatal(err)
	}
	cos, _ := st.Companies(model.MarketTW)
	if len(cos) != 7 {
		t.Fatalf("stored %d companies", len(cos))
	}
	if d, _ := st.LatestPriceDate(model.MarketTW, "0050"); d.Format("2006-01-02") != "2024-06-14" {
		t.Fatalf("0050 latest = %v", d)
	}
	if (*calls)[3] != "TaiwanStockPrice_0050@2010-01-01" {
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
	if d, _ := st.LatestPriceDate(model.MarketTW, "3474"); d.Format("2006-01-02") != "2016-11-29" {
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
	if err := writePrices(st, "2330", time.Time{}, []model.Price{p("2024-06-11", 883, 883), p("2024-06-12", 909, 909)}); err != nil {
		t.Fatal(err)
	}
	// Ex-dividend on 06-13: the next run adjusts 06-12 to 905.5.
	if err := writePrices(st, "2330", d("2024-06-12"), []model.Price{p("2024-06-12", 909, 905.5), p("2024-06-13", 919, 919)}); err != nil {
		t.Fatal(err)
	}
	var adj float64
	st.DB.QueryRow(`SELECT adj_close FROM prices WHERE ticker = '2330' AND date = '2024-06-11'`).Scan(&adj)
	if want := 883 * 905.5 / 909; math.Abs(adj-want) > 1e-9 {
		t.Errorf("rescaled adj close = %v, want %v", adj, want)
	}
}


// Steps get slices of one run budget. A step's unused requests stay with the
// run, and FinMind's 402 ends the run for every step, not just the one hit.
func TestSharedBudget(t *testing.T) {
	n, limit := 0, 5
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n > limit {
			// FinMind also sends its limit message with HTTP 200.
			w.Write([]byte(`{"msg":"Requests reach the upper limit.","status":402}`))
			return
		}
		w.Write([]byte(`{"msg":"success","status":200,"data":[]}`))
	}))
	defer srv.Close()
	ctx := context.Background()
	run := &FinMind{BaseURL: srv.URL, Budget: 10, HTTP: srv.Client()}

	first := run.Allot(run.Left() / 3)
	for i := 0; i < 3; i++ {
		if _, err := first.Get(ctx, "x", nil); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if _, err := first.Get(ctx, "x", nil); !errors.Is(err, ErrStop) {
		t.Fatalf("4th request of a 3-request slice: err = %v, want ErrStop", err)
	}
	if run.Left() != 7 || n != 3 {
		t.Fatalf("run left %d after %d requests, want 7 after 3", run.Left(), n)
	}

	second := run.Allot(run.Left() / 2) // 3 of 7
	if _, err := second.Get(ctx, "x", nil); err != nil {
		t.Fatal(err)
	}
	third := run.Allot(run.Left()) // the 6 left, unused ones included
	if third.Left() != 6 {
		t.Fatalf("last step gets %d, want 6", third.Left())
	}
	if _, err := third.Get(ctx, "x", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := third.Get(ctx, "x", nil); !errors.Is(err, ErrStop) {
		t.Fatalf("over FinMind's limit: err = %v, want ErrStop", err)
	}
	if run.Left() != 0 || run.Allot(5).Left() != 0 {
		t.Errorf("after a 402 the run has %d requests left, want 0", run.Left())
	}
}
