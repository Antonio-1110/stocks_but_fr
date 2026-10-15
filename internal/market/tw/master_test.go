package tw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/model"
)

// twse_t187ap03_L.json and tpex_t187ap03_O.json are real responses of
// 2026-10-14/15, trimmed to a few companies.
func TestParseCompanyLists(t *testing.T) {
	tw, err := parseTWSECompanies(readTestdata(t, "twse_t187ap03_L.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]listing{}
	for _, l := range tw {
		got[l.Ticker] = l
	}
	if l := got["2330"]; l.ListedOn != "1994-09-05" || l.Industry != "24" {
		t.Errorf("2330 = %+v", l)
	}
	// Traded on the emerging board until it listed in 2026.
	if l := got["4169"]; l.ListedOn != "2026-04-08" {
		t.Errorf("4169 = %+v", l)
	}
	op, err := parseTPExCompanies(readTestdata(t, "tpex_t187ap03_O.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(op) != 3 || op[1].Ticker != "6488" || op[1].ListedOn != "2015-09-25" || op[1].Industry != "24" {
		t.Errorf("tpex = %+v", op)
	}
	if _, err := parseTWSECompanies([]byte(`[{"code":"1101"}]`)); err == nil {
		t.Error("a list without the expected fields parsed")
	}
}

func fakeOpenData(t *testing.T, twse string) (*OpenData, *[]string) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Errorf("no User-Agent: %v", r.URL)
		}
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/twse":
			w.Write(readTestdata(t, twse))
		case "/tpex":
			w.Write(readTestdata(t, "tpex_t187ap03_O.json"))
		}
	}))
	t.Cleanup(srv.Close)
	return &OpenData{TWSE: srv.URL + "/twse", TPEx: srv.URL + "/tpex", HTTP: srv.Client()}, &calls
}

func TestCollectMaster(t *testing.T) {
	st := openStore(t)
	day := func(s string) time.Time { return parseDay(s) }
	st.UpsertCompanies([]model.Company{
		{Market: model.MarketTW, Ticker: "2330", Name: "台積電", Industry: "半導體業", Exchange: "TWSE"},
		{Market: model.MarketTW, Ticker: "4169", Name: "4169", Industry: "生技醫療業", Exchange: "TWSE"},
		{Market: model.MarketTW, Ticker: "0050", Name: "元大台灣50", Industry: "ETF", Exchange: "TWSE"},
		{Market: model.MarketTW, Ticker: "3474", Name: "華亞科", Exchange: "TWSE", DelistedOn: day("2016-12-06")},
	})
	st.UpsertPrices([]model.Price{
		{Market: model.MarketTW, Ticker: "0050", Date: day("2010-01-04"), Close: 50, AdjClose: 50},
		{Market: model.MarketTW, Ticker: "3474", Date: day("2010-01-05"), Close: 20, AdjClose: 20},
		{Market: model.MarketTW, Ticker: "4169", Date: day("2012-05-07"), Close: 30, AdjClose: 30},
	})
	o, calls := fakeOpenData(t, "twse_t187ap03_L.json")
	now := time.Date(2026, 10, 15, 18, 0, 0, 0, taipei)
	if err := collectMaster(context.Background(), st, o, now); err != nil {
		t.Fatal(err)
	}
	cos, _ := st.Companies(model.MarketTW)
	want := map[string]string{"2330": "1994-09-05", "4169": "2026-04-08", "0050": "2010-01-04", "3474": "2010-01-05"}
	for _, c := range cos {
		if got := c.ListedOn.Format("2006-01-02"); got != want[c.Ticker] {
			t.Errorf("%s listed %s, want %s", c.Ticker, got, want[c.Ticker])
		}
	}
	var src string
	st.DB.QueryRow(`SELECT source FROM tw_listings WHERE ticker = '0050'`).Scan(&src)
	if src != "first_price" {
		t.Errorf("0050 source %q", src)
	}
	st.DB.QueryRow(`SELECT source FROM tw_listings WHERE ticker = '4169'`).Scan(&src)
	if src != "twse" {
		t.Errorf("4169 source %q", src)
	}

	// Same day again: nothing fetched. Next day the industry changes for
	// one source: a second row, the first one kept.
	*calls = nil
	if err := collectMaster(context.Background(), st, o, now); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Errorf("fetched again on the same day: %v", *calls)
	}
	st.DB.Exec(`UPDATE companies SET industry = '其他業' WHERE ticker = '4169'`)
	if err := collectMaster(context.Background(), st, o, now.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	rows, err := st.DB.Query(`SELECT source, industry, first_seen, last_seen FROM tw_industry_history
		WHERE ticker = '4169' ORDER BY source, first_seen`)
	if err != nil {
		t.Fatal(err)
	}
	var hist []string
	for rows.Next() {
		var s, i, f, l string
		rows.Scan(&s, &i, &f, &l)
		hist = append(hist, strings.Join([]string{s, i, f, l}, " "))
	}
	rows.Close()
	wantHist := []string{
		"finmind 生技醫療業 2026-10-15 2026-10-15",
		"finmind 其他業 2026-10-16 2026-10-16",
		"twse 22 2026-10-15 2026-10-16",
	}
	if strings.Join(hist, "|") != strings.Join(wantHist, "|") {
		t.Errorf("history:\n%s\nwant\n%s", strings.Join(hist, "\n"), strings.Join(wantHist, "\n"))
	}
}

// A blocked or broken source fails soft: dates already stored stay, and
// the fallback still fills the rest.
func TestCollectMasterBlocked(t *testing.T) {
	st := openStore(t)
	st.UpsertCompanies([]model.Company{{Market: model.MarketTW, Ticker: "0050", Name: "0050", Exchange: "TWSE"}})
	st.UpsertPrices([]model.Price{{Market: model.MarketTW, Ticker: "0050", Date: parseDay("2010-01-04"), Close: 50, AdjClose: 50}})
	o, _ := fakeOpenData(t, "twse_blocked.html")
	if err := collectMaster(context.Background(), st, o, time.Date(2026, 10, 15, 18, 0, 0, 0, taipei)); err != nil {
		t.Fatal(err)
	}
	var fetched int
	st.DB.QueryRow(`SELECT COUNT(*) FROM tw_master_fetches WHERE source = 'twse'`).Scan(&fetched)
	if fetched != 0 {
		t.Error("a blocked fetch was recorded as done")
	}
	cos, _ := st.Companies(model.MarketTW)
	if cos[0].ListedOn.Format("2006-01-02") != "2010-01-04" {
		t.Errorf("0050 listed %v", cos[0].ListedOn)
	}
}
