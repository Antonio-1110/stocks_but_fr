package tw

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

// Tests never reach TWSE: collect() skips the file unless a test sets twseURL.
func TestMain(m *testing.M) {
	twseURL = ""
	os.Exit(m.Run())
}

// twse_stock_day_all.json is a live STOCK_DAY_ALL response (2026-10-16),
// trimmed to six rows; 020011 didn't trade that day. twse_twt48u_all.json is
// a live TWT48U_ALL response (2026-10-17), trimmed to three rows: 00851 and
// 00904 went ex-dividend (cash) on 10-16, 1711 has rights on 10-20.
func TestParseTWSEDayAll(t *testing.T) {
	day, quotes, err := parseTWSEDayAll(readTestdata(t, "twse_stock_day_all.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !day.Equal(time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)) || len(quotes) != 5 {
		t.Fatalf("day %v, %d quotes, want 2026-10-16 and 5", day, len(quotes))
	}
	ex, err := parseTWSEEx(readTestdata(t, "twse_twt48u_all.json"), day)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 2 || ex["00851"] != (twseExDay{Cash: 2.5, CashOnly: true}) {
		t.Errorf("ex-days %+v, want 00851 and 00904", ex)
	}
	for _, q := range quotes {
		if q.Ticker == "2330" && (q.Open != 2430 || q.High != 2445 || q.Low != 2420 || q.Close != 2420 ||
			q.Ref != 2440 || q.Volume != 43046982 || q.Turnover != 104583063462) {
			t.Errorf("2330 %+v", q)
		}
	}
}

func seedTWSE(t *testing.T, days []string) *store.Store {
	t.Helper()
	st := openStore(t)
	for _, q := range []string{daysTable, eventsTable, eventChecksTable} {
		_, err := st.DB.Exec(q)
		must(t, err)
	}
	must(t, st.UpsertCompanies([]model.Company{
		{Market: model.MarketTW, Ticker: "0050", Exchange: "TWSE"},
		{Market: model.MarketTW, Ticker: "2330", Exchange: "TWSE"},
		{Market: model.MarketTW, Ticker: "1101", Exchange: "TWSE"},
		{Market: model.MarketTW, Ticker: "00851", Exchange: "TWSE"},
		{Market: model.MarketTW, Ticker: "2317", Exchange: "TWSE"},
	}))
	p := func(ticker, day string, close float64) model.Price {
		return model.Price{Market: model.MarketTW, Ticker: ticker, Date: parseDay(day),
			Open: close, High: close, Low: close, Close: close, AdjClose: close}
	}
	must(t, st.UpsertPrices([]model.Price{
		p("0050", "2026-10-15", 108.9),
		p("2330", "2026-10-15", 2440),
		p("1101", "2026-10-15", 28),                                // file's close minus change is 26.80: doesn't match
		p("00851", "2026-10-14", 72), p("00851", "2026-10-15", 72), // NT$2.5 cash dividend on 10-16
		p("2317", "2026-10-14", 240), // behind: left to FinMind, or 10-15 would be skipped
	}))
	for _, d := range days {
		_, err := st.DB.Exec(`INSERT INTO tw_price_days (exchange, date, rows) VALUES ('TPEx', ?, 800)`, d)
		must(t, err)
	}
	return st
}

func TestTWSEDayExtendsCurrentStocks(t *testing.T) {
	s := seedTWSE(t, []string{"2026-10-14", "2026-10-15"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != userAgent {
			t.Errorf("User-Agent %q", r.UserAgent())
		}
		if r.URL.Path == "/ex" {
			w.Write(readTestdata(t, "twse_twt48u_all.json"))
			return
		}
		w.Write(readTestdata(t, "twse_stock_day_all.json"))
	}))
	defer srv.Close()
	twseURL, twseExListURL = srv.URL+"/day", srv.URL+"/ex"
	defer func() { twseURL = "" }()

	_, err := s.DB.Exec(`INSERT INTO tw_event_checks (ticker, checked_on) VALUES ('1101', '2026-10-15')`)
	must(t, err)
	d := newDirty()
	now := time.Date(2026, 10, 16, 20, 0, 0, 0, taipei)
	twseLatest(context.Background(), s, d, now)
	if _, err := rebuild(s, d); err != nil {
		t.Fatal(err)
	}
	for ticker, want := range map[string]string{"0050": "2026-10-16", "2330": "2026-10-16", "00851": "2026-10-16",
		"1101": "2026-10-15", "2317": "2026-10-14"} {
		latest, err := s.LatestPriceDate(model.MarketTW, ticker)
		must(t, err)
		if latest.Format("2006-01-02") != want {
			t.Errorf("%s latest %s, want %s", ticker, latest.Format("2006-01-02"), want)
		}
	}
	// 00851's NT$2.5 dividend makes the reference price 72 - 2.5 = 69.5, so
	// older adjusted closes scale by 69.5 / 72. (The file's change is 0 on an
	// ex-day, so close minus change can't be used.)
	var adj float64
	must(t, s.DB.QueryRow(`SELECT adj_close FROM prices WHERE ticker = '00851' AND date = '2026-10-15'`).Scan(&adj))
	if math.Abs(adj-69.5) > 1e-6 {
		t.Errorf("00851 adj_close on 10-15 = %v, want 69.5", adj)
	}
	var n int
	must(t, s.DB.QueryRow(`SELECT COUNT(*) FROM tw_adj_events`).Scan(&n))
	if n != 1 {
		t.Errorf("%d events, want only 00851's", n)
	}
	// 1101 didn't match its stored close: left to FinMind, which refetches
	// its events this run.
	must(t, s.DB.QueryRow(`SELECT COUNT(*) FROM tw_event_checks WHERE ticker = '1101'`).Scan(&n))
	if n != 0 {
		t.Errorf("1101 event check kept, want it reset")
	}
}

// With a weekday before the file's day not fetched yet, the previous trading
// day isn't known, so nothing is written: a gap is never stepped over.
func TestTWSEDayNeedsAKnownPreviousDay(t *testing.T) {
	s := seedTWSE(t, []string{"2026-10-14"})
	_, quotes, err := parseTWSEDayAll(readTestdata(t, "twse_stock_day_all.json"))
	must(t, err)
	n, err := twseDay(s, newDirty(), parseDay("2026-10-16"), quotes, nil, time.Date(2026, 10, 16, 20, 0, 0, 0, taipei))
	must(t, err)
	if n != 0 {
		t.Errorf("wrote %d stocks, want 0", n)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
