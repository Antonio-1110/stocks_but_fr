package margin

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/market/tw"
	"github.com/Antonio-1110/stocks_but_fr/internal/market/tw/dataset"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var nov13 = time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)

func find(t *testing.T, rows []Row, ticker string) Row {
	t.Helper()
	for _, r := range rows {
		if r.Ticker == ticker {
			return r
		}
	}
	t.Fatalf("no row for %s in %d rows", ticker, len(rows))
	return Row{}
}

func TestParseTWSE(t *testing.T) {
	rows, err := parseTWSE(readFile(t, "twse_mi_margn_20261113.json"), nov13)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4 (the summary table must be skipped)", len(rows))
	}
	// 2330: 融資 680 bought, 477 sold, 32,376 open; 融券 20 bought back, 5 sold, 50 open; 3 offset.
	want := Row{Ticker: "2330", Date: nov13, MarginBuy: 680, MarginSell: 477, MarginBalance: 32376,
		ShortSell: 5, ShortBuy: 20, ShortBalance: 50, Offset: 3}
	if got := find(t, rows, "2330"); got != want {
		t.Errorf("2330 = %+v\nwant  %+v", got, want)
	}
	if got := find(t, rows, "0050"); got.ShortBalance != 481 || got.MarginBalance != 37150 {
		t.Errorf("0050 = %+v", got)
	}
}

func TestParseTPEx(t *testing.T) {
	rows, err := parseTPEx(readFile(t, "tpex_margin_20261113.json"), nov13)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4", len(rows))
	}
	// 6488: 資買 350, 資賣 312, 資餘額 9,021; 券賣 4, 券買 9, 券餘額 198; 相抵 7.
	want := Row{Ticker: "6488", Date: nov13, MarginBuy: 350, MarginSell: 312, MarginBalance: 9021,
		ShortSell: 4, ShortBuy: 9, ShortBalance: 198, Offset: 7, Note: "11   A"}
	if got := find(t, rows, "6488"); got != want {
		t.Errorf("6488 = %+v\nwant  %+v", got, want)
	}
	if got := find(t, rows, "00679B"); got.Note != "X" {
		t.Errorf("00679B note = %q, want X (short selling stopped)", got.Note)
	}
}

func TestParseOtherDay(t *testing.T) {
	// A report dated another day than asked is no data, not that day's data.
	for name, parse := range map[string]func([]byte, time.Time) ([]Row, error){
		"twse_mi_margn_20261113.json": parseTWSE, "tpex_margin_20261113.json": parseTPEx,
	} {
		rows, err := parse(readFile(t, name), nov13.AddDate(0, 0, 1))
		if err != nil || rows != nil {
			t.Errorf("%s for Nov 14: %d rows, err %v; want none", name, len(rows), err)
		}
	}
}

func TestParseFinMind(t *testing.T) {
	rows, err := parseFinMind(readFile(t, "finmind_2330.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("got %d rows, want 5", len(rows))
	}
	want := Row{Ticker: "2330", Date: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
		MarginBuy: 884, MarginSell: 608, MarginBalance: 35219, ShortSell: 8, ShortBuy: 4, ShortBalance: 13, Offset: 2}
	if rows[0] != want {
		t.Errorf("first row = %+v\nwant        %+v", rows[0], want)
	}
}

// TestRun runs the engine against fake FinMind and exchange servers: the
// backfill fills history, the exchanges fill trading days the prices table
// knows, and a second run asks for nothing again.
func TestRun(t *testing.T) {
	hits := map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("/finmind", func(w http.ResponseWriter, r *http.Request) {
		hits["finmind "+r.URL.Query().Get("data_id")]++
		if r.URL.Query().Get("data_id") == "2330" {
			w.Write(readFile(t, "finmind_2330.json"))
			return
		}
		w.Write([]byte(`{"msg":"success","status":200,"data":[]}`))
	})
	mux.HandleFunc("/twse", func(w http.ResponseWriter, r *http.Request) {
		hits["twse "+r.URL.Query().Get("date")]++
		w.Write(readFile(t, "twse_mi_margn_20261113.json"))
	})
	mux.HandleFunc("/tpex", func(w http.ResponseWriter, r *http.Request) {
		hits["tpex "+r.URL.Query().Get("d")]++
		w.Write(readFile(t, "tpex_margin_20261113.json"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "radar.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertCompanies([]model.Company{
		{Market: model.MarketTW, Ticker: "2330", Name: "台積電"},
		{Market: model.MarketTW, Ticker: "6488", Name: "環球晶"},
	}); err != nil {
		t.Fatal(err)
	}
	// Nov 13 (Friday) is a trading day; the weekend has no prices.
	var prices []model.Price
	for i := 0; i < 400; i++ {
		prices = append(prices, model.Price{Market: model.MarketTW, Ticker: string(rune('A'+i/26)) + string(rune('a'+i%26)), Date: nov13, Close: 10})
	}
	if err := st.UpsertPrices(prices); err != nil {
		t.Fatal(err)
	}

	d := Dataset
	d.Exchanges = append([]dataset.Exchange[Row](nil), d.Exchanges...)
	d.Exchanges[0].URL = srv.URL + "/twse"
	d.Exchanges[1].URL = srv.URL + "/tpex"
	run := func() {
		r := dataset.New(d, &tw.FinMind{BaseURL: srv.URL + "/finmind", Budget: 100, HTTP: srv.Client()})
		r.Client, r.Gap = srv.Client(), 0
		r.Now = func() time.Time { return time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC) }
		if err := r.Run(context.Background(), st, time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
	}
	run()
	run()
	for k, n := range hits {
		if n != 1 {
			t.Errorf("%s asked %d times, want once", k, n)
		}
	}
	for _, k := range []string{"finmind 2330", "finmind 6488", "twse 20261113", "tpex 115/11/13"} {
		if hits[k] != 1 {
			t.Errorf("%s not asked", k)
		}
	}
	if len(hits) != 4 {
		t.Errorf("asked %v; want only Nov 13 from the exchanges", hits)
	}

	count := func(q string, args ...any) int {
		var n int
		if err := st.DB.QueryRow(q, args...).Scan(&n); err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM tw_margin`); n != 5+4+4 {
		t.Errorf("tw_margin has %d rows, want 13 (5 FinMind days + 4 TWSE + 4 TPEx)", n)
	}
	if n := count(`SELECT short_balance FROM tw_margin WHERE ticker = '6488' AND date = '2026-11-13'`); n != 198 {
		t.Errorf("6488 short balance = %d, want 198", n)
	}
}
