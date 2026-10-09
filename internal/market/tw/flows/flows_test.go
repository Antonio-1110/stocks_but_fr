package flows

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
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

func day(s string) time.Time {
	d, _ := time.Parse("2006-01-02", s)
	return d
}

func find(rows []model.InstitutionalFlow, ticker, date string) *model.InstitutionalFlow {
	for i := range rows {
		if rows[i].Ticker == ticker && rows[i].Date.Equal(day(date)) {
			return &rows[i]
		}
	}
	return nil
}

func check(t *testing.T, rows []model.InstitutionalFlow, ticker, date string, foreign, trust, dealer int64) {
	t.Helper()
	f := find(rows, ticker, date)
	if f == nil {
		t.Fatalf("no row for %s on %s", ticker, date)
	}
	if f.Market != model.MarketTW || f.ForeignNet != foreign || f.TrustNet != trust || f.DealerNet != dealer {
		t.Errorf("%s %s = %+v, want foreign %d trust %d dealer %d", ticker, date, *f, foreign, trust, dealer)
	}
}

func TestParseFinMind(t *testing.T) {
	rows, err := parseFinMind(readFile(t, "finmind_2330.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	// Same 2024-10-01 numbers as the TWSE fixture, so both sources agree.
	check(t, rows, "2330", "2024-10-01", -7_608_187, 829_000, -63_000)
	check(t, rows, "2330", "2024-10-04", 8_705_223+1_000, -209_000, -20_000)
}

func TestParseFinMindQuota(t *testing.T) {
	_, err := parseFinMind(readFile(t, "finmind_quota.json"))
	if !errors.Is(err, errQuota) {
		t.Fatalf("err = %v, want errQuota", err)
	}
}

func TestParseFinMindUnknownGroup(t *testing.T) {
	body := `{"msg":"success","status":200,"data":[{"date":"2024-10-01","stock_id":"2330","buy":1,"name":"Martians","sell":0}]}`
	if _, err := parseFinMind([]byte(body)); err == nil {
		t.Fatal("want an error for an unknown investor group")
	}
}

func TestParseTWSE(t *testing.T) {
	rows, err := parseTWSE(readFile(t, "twse_t86_20241001.json"), day("2024-10-01"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	check(t, rows, "2330", "2024-10-01", -7_608_187, 829_000, -63_000)
	check(t, rows, "0050", "2024-10-01", 2_099_500+1_000, 0, -215_000)
	check(t, rows, "2317", "2024-10-01", -123_457, -50_000, 0)
}

func TestParseTWSEHoliday(t *testing.T) {
	rows, err := parseTWSE(readFile(t, "twse_t86_holiday.json"), day("2024-10-02"))
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows %v, err %v; want none, nil", rows, err)
	}
}

// Before 2017-12-18 T86 had one foreign column and one dealer column.
func TestParseTWSEOldLayout(t *testing.T) {
	body := `{"stat":"OK","fields":["證券代號","證券名稱","外資買進股數","外資賣出股數","外資買賣超股數","投信買進股數","投信賣出股數","投信買賣超股數","自營商買賣超股數","自營商買進股數(自行買賣)","自營商賣出股數(自行買賣)","自營商買賣超股數(自行買賣)","自營商買進股數(避險)","自營商賣出股數(避險)","自營商買賣超股數(避險)","三大法人買賣超股數"],
	"data":[["2330","台積電","5,000","1,000","4,000","0","300","-300","700","800","0","800","0","100","-100","4,400"]]}`
	rows, err := parseTWSE([]byte(body), day("2015-03-02"))
	if err != nil {
		t.Fatal(err)
	}
	check(t, rows, "2330", "2015-03-02", 4_000, -300, 700)
}

func TestParseTPEx(t *testing.T) {
	rows, err := parseTPEx(readFile(t, "tpex_20241001.json"), day("2024-10-01"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	check(t, rows, "6488", "2024-10-01", 40_000, 10_000, -2_000)
	check(t, rows, "8069", "2024-10-01", -390_000, -20_000, 0)
}

func TestParseTPExUnlabelledColumns(t *testing.T) {
	body := strings.Replace(string(readFile(t, "tpex_20241001.json")), "外資", "X", -1)
	body = strings.Replace(body, "投信", "Y", -1)
	rows, err := parseTPEx([]byte(body), day("2024-10-01"))
	if err != nil {
		t.Fatal(err)
	}
	check(t, rows, "6488", "2024-10-01", 40_000, 10_000, -2_000)
}

func TestParseTPExHoliday(t *testing.T) {
	rows, err := parseTPEx(readFile(t, "tpex_holiday.json"), day("2024-10-02"))
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows %v, err %v; want none, nil", rows, err)
	}
}

// fakeSources serves the fixtures the way the real sites would.
func fakeSources(t *testing.T, finmindHits *int32) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/finmind", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(finmindHits, 1)
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("FinMind request without token")
		}
		if r.URL.Query().Get("data_id") == "2330" {
			w.Write(readFile(t, "finmind_2330.json"))
			return
		}
		w.Write([]byte(`{"msg":"success","status":200,"data":[]}`))
	})
	mux.HandleFunc("/twse", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("date") == "20241001" {
			w.Write(readFile(t, "twse_t86_20241001.json"))
			return
		}
		w.Write(readFile(t, "twse_t86_holiday.json"))
	})
	mux.HandleFunc("/tpex", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("date") == "2024/10/01" {
			w.Write(readFile(t, "tpex_20241001.json"))
			return
		}
		w.Write(readFile(t, "tpex_holiday.json"))
	})
	return httptest.NewServer(mux)
}

func TestRun(t *testing.T) {
	var hits int32
	srv := fakeSources(t, &hits)
	defer srv.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "radar.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	err = st.UpsertCompanies([]model.Company{
		{Market: model.MarketTW, Ticker: "2330", Name: "台積電"},
		{Market: model.MarketTW, Ticker: "6488", Name: "環球晶"},
		{Market: model.MarketTW, Ticker: "1101", Name: "gone", DelistedOn: day("2005-01-01")},
	})
	if err != nil {
		t.Fatal(err)
	}

	c := &Collector{
		Client:     srv.Client(),
		FinMindURL: srv.URL + "/finmind",
		TWSEURL:    srv.URL + "/twse",
		TPExURL:    srv.URL + "/tpex",
		Token:      "tok",
		FinMindMax: 100,
		Now:        func() time.Time { return time.Date(2024, 10, 5, 10, 0, 0, 0, taipei) },
	}
	cfg := config.Default()
	for run := 0; run < 2; run++ {
		if err := c.Run(context.Background(), cfg, st); err != nil {
			t.Fatal(err)
		}
	}
	// 2330 and 6488 once each; 1101 delisted before history_start; second run
	// finds both already fetched.
	if hits != 2 {
		t.Errorf("FinMind hit %d times, want 2", hits)
	}

	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM institutional_flows`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	// 2330 on 10-01 and 10-04, 0050 and 2317 on 10-01 (TWSE), 6488 and 8069 (TPEx).
	if n != 6 {
		t.Errorf("stored %d flows, want 6", n)
	}
	var foreign, trust, dealer int64
	err = st.DB.QueryRow(`SELECT foreign_net, trust_net, dealer_net FROM institutional_flows
		WHERE market = 'TW' AND ticker = '2330' AND date = '2024-10-01'`).Scan(&foreign, &trust, &dealer)
	if err != nil {
		t.Fatal(err)
	}
	if foreign != -7_608_187 || trust != 829_000 || dealer != -63_000 {
		t.Errorf("2330 2024-10-01 = %d %d %d", foreign, trust, dealer)
	}

	// Empty reports for today and yesterday are retried next run; older
	// empty days count as closed and are not.
	done, err := fetchedKeys(st.DB, srcTWSE)
	if err != nil {
		t.Fatal(err)
	}
	if !done["2024-10-02"] || !done["2024-10-01"] || done["2024-10-04"] || done["2024-10-05"] {
		t.Errorf("TWSE fetch log = %v", done)
	}
}
