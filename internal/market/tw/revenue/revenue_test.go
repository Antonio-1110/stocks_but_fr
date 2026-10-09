package revenue

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

// testdata/finmind_2330.json is a real TaiwanStockMonthRevenue response
// (TSMC, Jan 2023 to Feb 2024).

func month(y, m int) time.Time { return time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.UTC) }

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestParseAndConvert(t *testing.T) {
	rows, err := parseFinMind(readFixture(t, "finmind_2330.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 14 {
		t.Fatalf("got %d rows, want 14", len(rows))
	}
	// FinMind's date is the month after the revenue month.
	if rows[0].Date != "2023-02-01" || rows[0].RevenueYear != 2023 || rows[0].RevenueMonth != 1 {
		t.Fatalf("first row %+v", rows[0])
	}

	recs := toModel(rows, month(2023, 1))
	if len(recs) != 14 {
		t.Fatalf("got %d records", len(recs))
	}
	jan23, jan24, feb24 := recs[0], recs[12], recs[13]
	if !jan23.Month.Equal(month(2023, 1)) || jan23.Ticker != "2330" || jan23.Market != model.MarketTW {
		t.Fatalf("jan23 %+v", jan23)
	}
	if !math.IsNaN(jan23.YoYPct) || !math.IsNaN(jan23.MoMPct) {
		t.Errorf("jan23 has no base months, want NaN pcts: %+v", jan23)
	}
	if want := (215785127000.0/200050544000 - 1) * 100; !near(jan24.YoYPct, want) {
		t.Errorf("jan24 YoY = %v, want %v", jan24.YoYPct, want)
	}
	if want := (215785127000.0/176299866000 - 1) * 100; !near(jan24.MoMPct, want) {
		t.Errorf("jan24 MoM = %v, want %v", jan24.MoMPct, want)
	}
	if want := (181648270000.0/215785127000 - 1) * 100; !near(feb24.MoMPct, want) {
		t.Errorf("feb24 MoM = %v, want %v", feb24.MoMPct, want)
	}
	if !jan24.AnnouncedOn.Equal(time.Date(2024, 2, 10, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("jan24 announced %v, want 2024-02-10", jan24.AnnouncedOn)
	}
	if dec := recs[11]; !dec.AnnouncedOn.Equal(time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("dec23 announced %v, want 2024-01-10", dec.AnnouncedOn)
	}

	// Months before start are only bases for YoY, not stored.
	if recs := toModel(rows, month(2024, 1)); len(recs) != 2 || math.IsNaN(recs[0].YoYPct) {
		t.Errorf("start=2024-01: got %+v", recs)
	}
}

func TestParseQuota(t *testing.T) {
	if _, err := parseFinMind(readFixture(t, "finmind_quota.json")); !errors.Is(err, errQuota) {
		t.Fatalf("err = %v, want errQuota", err)
	}
}

func TestCollectEndToEnd(t *testing.T) {
	fixture := readFixture(t, "finmind_2330.json")
	var calls atomic.Int32
	var gotQuery, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotQuery, gotUA = r.URL.RawQuery, r.UserAgent()
		if r.URL.Query().Get("data_id") != "2330" {
			w.Write([]byte(`{"msg":"success","status":200,"data":[]}`))
			return
		}
		w.Write(fixture)
	}))
	defer srv.Close()
	old := finmindURL
	finmindURL = srv.URL
	defer func() { finmindURL = old }()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "radar.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	err = st.UpsertCompanies([]model.Company{
		{Market: model.MarketTW, Ticker: "0050", Name: "元大台灣50"}, // ETF: skipped
		{Market: model.MarketTW, Ticker: "2330", Name: "台積電"},
		{Market: model.MarketTW, Ticker: "9999", Name: "Gone Co", DelistedOn: month(2015, 1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := collector{
		st:       st,
		client:   &client{http: srv.Client()},
		cacheDir: filepath.Join(dir, "cache"),
		start:    month(2023, 1),
		now:      time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC),
		budget:   10,
	}
	if err := c.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("requests = %d, want 2 (2330 and 9999, not the ETF)", n)
	}
	if gotUA != userAgent || gotQuery == "" {
		t.Errorf("UA %q query %q", gotUA, gotQuery)
	}

	var count int
	var yoy sql.NullFloat64
	var announced string
	st.DB.QueryRow(`SELECT COUNT(*) FROM monthly_revenue WHERE ticker='2330'`).Scan(&count)
	if count != 14 {
		t.Errorf("stored %d rows, want 14", count)
	}
	st.DB.QueryRow(`SELECT yoy_pct, announced_on FROM monthly_revenue WHERE ticker='2330' AND month='2023-01-01'`).Scan(&yoy, &announced)
	if yoy.Valid || announced != "2023-02-10" {
		t.Errorf("2023-01: yoy %+v (want NULL), announced %q", yoy, announced)
	}
	st.DB.QueryRow(`SELECT yoy_pct FROM monthly_revenue WHERE ticker='2330' AND month='2024-02-01'`).Scan(&yoy)
	if !yoy.Valid || !near(yoy.Float64, (181648270000.0/163174097000-1)*100) {
		t.Errorf("2024-02 yoy %+v", yoy)
	}

	// Second run: 2330 already holds last month (Feb 2024), so nothing is
	// requested except 9999, which has no data yet and is answered from cache.
	if err := c.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("after rerun requests = %d, want still 2", n)
	}

	// A month later 2330 is stale: it refetches and keeps old YoY values intact.
	c.now = time.Date(2024, 4, 15, 0, 0, 0, 0, time.UTC)
	todo, err := c.todo()
	if err != nil {
		t.Fatal(err)
	}
	var stale *target
	for i := range todo {
		if todo[i].ticker == "2330" {
			stale = &todo[i]
		}
	}
	if stale == nil || !stale.storeFrom.Equal(month(2024, 2)) || !stale.from.Equal(month(2023, 3)) {
		t.Fatalf("2330 target %+v", stale)
	}
}

func TestQuotaStopsRun(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusPaymentRequired)
	}))
	defer srv.Close()
	old := finmindURL
	finmindURL = srv.URL
	defer func() { finmindURL = old }()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "radar.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.UpsertCompanies([]model.Company{
		{Market: model.MarketTW, Ticker: "1101", Name: "台泥"},
		{Market: model.MarketTW, Ticker: "2330", Name: "台積電"},
	})
	c := collector{st: st, client: &client{http: srv.Client()}, cacheDir: filepath.Join(dir, "cache"),
		start: month(2023, 1), now: time.Now(), budget: 10}
	if err := c.run(context.Background()); err != nil {
		t.Fatalf("quota should end the run softly, got %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}
