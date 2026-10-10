package alerts

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "radar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.DB.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return st
}

func day(s string) time.Time {
	d, err := time.Parse(dateLayout, s)
	if err != nil {
		panic(err)
	}
	return d
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type fakeSender struct {
	sent []string
	err  error
}

func (f *fakeSender) Send(_ context.Context, msg string) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, msg)
	return nil
}

func hits(tickers ...string) []Hit {
	out := make([]Hit, len(tickers))
	for i, t := range tickers {
		out[i] = Hit{Ticker: t, Name: "N" + t, Detail: "why"}
	}
	return out
}

func TestCheckSendsOnlyNewEntriesWithHysteresis(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	now := day("2026-10-11")
	var ranking []Hit
	r := rule{name: "test", title: "Test", enter: 2, exit: 3,
		rank: func(time.Time) ([]Hit, string, error) { return ranking, "prices x", nil }}
	s := &fakeSender{}
	step := func(tickers ...string) []string {
		t.Helper()
		ranking = hits(tickers...)
		s.sent = nil
		must(t, check(ctx, st.DB, r, s, now))
		return s.sent
	}

	// First run records and only says hello.
	if sent := step("A", "B", "C"); len(sent) != 1 || !strings.Contains(sent[0], "alerts are on") || strings.Contains(sent[0], "NA") {
		t.Fatalf("first run sent %v", sent)
	}
	// A slips to 3rd (still within exit): no alert. D enters the top 2.
	sent := step("B", "D", "A", "C")
	if len(sent) != 1 || !strings.Contains(sent[0], "D ND") || strings.Contains(sent[0], "A NA") {
		t.Fatalf("want only D, got %q", sent)
	}
	// Same ranking again: nothing new.
	if sent := step("B", "D", "A"); len(sent) != 0 {
		t.Fatalf("repeat run sent %v", sent)
	}
	// A falls past exit, then comes back: alerts again.
	step("B", "D", "C", "A")
	if sent := step("A", "B", "D"); len(sent) != 1 || !strings.Contains(sent[0], "A NA") {
		t.Fatalf("want A again, got %q", sent)
	}
	// An empty ranking (missing data) keeps the state.
	if sent := step(); len(sent) != 0 {
		t.Fatal("empty ranking sent something")
	}
	if on, _ := loadOn(st.DB, "test"); len(on) != 3 {
		t.Fatalf("state after empty ranking: %v", on)
	}
	// A failed send saves nothing, so the next run retries.
	ranking = hits("E", "A", "B")
	s.err = errors.New("down")
	if err := check(ctx, st.DB, r, s, now); err == nil {
		t.Fatal("want send error")
	}
	s.err = nil
	if sent := step("E", "A", "B"); len(sent) != 1 || !strings.Contains(sent[0], "E NE") {
		t.Fatalf("want E retried, got %q", sent)
	}
}

func TestCheckWithoutTelegramStillRecords(t *testing.T) {
	st := openStore(t)
	r := rule{name: "test", enter: 1, exit: 1,
		rank: func(time.Time) ([]Hit, string, error) { return hits("A"), "", nil }}
	must(t, check(context.Background(), st.DB, r, nil, day("2026-10-11")))
	must(t, check(context.Background(), st.DB, r, nil, day("2026-10-11")))
	if on, _ := loadOn(st.DB, "test"); on["A"] != "2026-10-11" {
		t.Fatalf("state %v", on)
	}
}

// seedMarket: 25 trading days. 1101 and 2330 pass; 2603 is too far below its
// high; 3008's revenue growth is too weak; 0050 is an ETF; 4904 is delisted.
func seedMarket(t *testing.T, st *store.Store) {
	tw := model.MarketTW
	must(t, st.UpsertCompanies([]model.Company{
		{Market: tw, Ticker: "1101", Name: "台泥", Industry: "水泥工業"},
		{Market: tw, Ticker: "2330", Name: "台積電", Industry: "半導體業"},
		{Market: tw, Ticker: "2603", Name: "長榮", Industry: "航運業"},
		{Market: tw, Ticker: "3008", Name: "大立光", Industry: "光電業"},
		{Market: tw, Ticker: "0050", Name: "元大台灣50", Industry: "ETF"},
		{Market: tw, Ticker: "4904", Name: "遠傳", DelistedOn: day("2026-01-01")},
	}))
	var prices []model.Price
	for i := 0; i < 25; i++ {
		d := day("2026-09-01").AddDate(0, 0, i)
		up := 100 + float64(i)
		for _, tk := range []string{"1101", "2330", "3008", "0050", "4904"} {
			prices = append(prices, model.Price{Market: tw, Ticker: tk, Date: d, Close: up, AdjClose: up, Turnover: 2e7})
		}
		down := 200 - 4*float64(i)
		prices = append(prices, model.Price{Market: tw, Ticker: "2603", Date: d, Close: down, AdjClose: down, Turnover: 2e7})
	}
	must(t, st.UpsertPrices(prices))

	var rev []model.MonthlyRevenue
	add := func(tk string, yoys ...float64) {
		for i, y := range yoys { // months 2026-06..2026-09
			m := day("2026-06-01").AddDate(0, i, 0)
			rev = append(rev, model.MonthlyRevenue{Market: tw, Ticker: tk, Month: m, Revenue: 1e9, YoYPct: y,
				AnnouncedOn: m.AddDate(0, 1, 9)})
		}
	}
	add("1101", 50, 40, 30, 20)
	add("2330", 5, 15, 25, 35)
	add("2603", 50, 50, 50, 50)
	add("3008", 50, 50, 50, 5)
	add("0050", 50, 50, 50, 50)
	add("4904", 50, 50, 50, 50)
	must(t, st.UpsertMonthlyRevenue(rev))
}

func TestRevenueMomentum(t *testing.T) {
	st := openStore(t)
	seedMarket(t, st)
	p := config.Default().Strategy.RevenueMomentum
	p.HighDays, p.ADVDays = 20, 5

	// After the deadline: months 07..09. 2330's weakest is 15, 1101's is 20.
	got, asOf, err := revenueMomentum(st.DB, day("2026-10-11"), p)
	must(t, err)
	if tickers(got) != "1101,2330" || !strings.Contains(asOf, "revenue to 2026-09") {
		t.Fatalf("after deadline: %s (%s)", tickers(got), asOf)
	}
	// On the 10th it still ranks on 06..08: 2330's 5% in June fails, 3008 passes (and leads).
	got, _, err = revenueMomentum(st.DB, day("2026-10-10"), p)
	must(t, err)
	if tickers(got) != "3008,1101" {
		t.Fatalf("before deadline: %s", tickers(got))
	}
	// Not enough history: nothing ranked, no error.
	p.HighDays = 30
	if got, _, err := revenueMomentum(st.DB, day("2026-10-11"), p); err != nil || len(got) != 0 {
		t.Fatalf("short history: %v %v", got, err)
	}
}

func TestRunEndToEnd(t *testing.T) {
	st := openStore(t)
	seedMarket(t, st)
	cfg := config.Default()
	cfg.Strategy.RevenueMomentum.HighDays, cfg.Strategy.RevenueMomentum.ADVDays = 20, 5
	cfg.Alerts.Rules = []string{"revenue_momentum", "unpriced"}
	s := &fakeSender{}
	must(t, run(context.Background(), cfg, st, s, day("2026-10-10")))
	must(t, run(context.Background(), cfg, st, s, day("2026-10-11")))
	// 2330 enters revenue momentum once September revenue is out; 1101 was already on.
	var momentum []string
	for _, m := range s.sent {
		if strings.HasPrefix(m, "<b>Revenue momentum</b>") {
			momentum = append(momentum, m)
		}
	}
	if len(momentum) != 2 || !strings.Contains(momentum[0], "alerts are on") || !strings.Contains(momentum[1], "2330 台積電") || strings.Contains(momentum[1], "1101") {
		t.Fatalf("sent %q", s.sent)
	}
	if on, _ := loadOn(st.DB, "unpriced"); len(on) == 0 {
		t.Error("unpriced rule recorded nothing")
	}
	cfg.Alerts.Rules = []string{"nope"}
	if err := run(context.Background(), cfg, st, s, day("2026-10-11")); err == nil {
		t.Fatal("unknown rule should fail")
	}
}

func TestTelegramSend(t *testing.T) {
	var form map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTOK/sendMessage" || r.Header.Get("User-Agent") == "" {
			t.Errorf("request %s", r.URL.Path)
		}
		r.ParseForm()
		form = map[string]string{"chat_id": r.Form.Get("chat_id"), "text": r.Form.Get("text"), "parse_mode": r.Form.Get("parse_mode")}
		if r.Form.Get("chat_id") == "bad" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"ok":false,"description":"Bad Request: chat not found"}`))
			return
		}
		w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()

	tg := &telegram{token: "TOK", chatID: "42", base: srv.URL, client: srv.Client()}
	must(t, tg.Send(context.Background(), "<b>hi</b>"))
	if form["chat_id"] != "42" || form["text"] != "<b>hi</b>" || form["parse_mode"] != "HTML" {
		t.Errorf("form %v", form)
	}
	tg.chatID = "bad"
	if err := tg.Send(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("err %v", err)
	}
	tg.base = "http://127.0.0.1:1"
	if err := tg.Send(context.Background(), "x"); err == nil || strings.Contains(err.Error(), "TOK") {
		t.Errorf("error leaks token or is nil: %v", err)
	}
}

func TestFormatEscapesAndSplits(t *testing.T) {
	r := rule{title: "A & B"}
	var many []Hit
	for i := 0; i < 60; i++ {
		many = append(many, Hit{Ticker: "1101", Name: "<x>", Detail: strings.Repeat("d", 80)})
	}
	msgs := format(r, many, "prices 2026-10-09")
	if len(msgs) < 2 {
		t.Fatalf("want split, got %d", len(msgs))
	}
	for _, m := range msgs {
		if len(m) > maxMessage || !strings.HasPrefix(m, "<b>A &amp; B</b>: 60 new") || strings.Contains(m, "<x>") {
			t.Fatalf("bad message %q", m[:80])
		}
	}
}

func tickers(h []Hit) string {
	var s []string
	for _, x := range h {
		s = append(s, x.Ticker)
	}
	return strings.Join(s, ",")
}
