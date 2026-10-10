package tw

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
)

// The tpex_* files are live TPEx responses (2026-10-10), trimmed to a few rows.
func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseTPExDaily(t *testing.T) {
	day := parseDay("2024-06-13")
	got, err := parseDaily(readTestdata(t, "tpex_otc_20240613.json"), day, tpexColumns)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]model.Price{}
	for _, p := range got {
		by[p.Ticker] = p
	}
	// 00679B is a bond ETF (letter suffix, kept by the ticker rule); 2924
	// had no trades and is skipped.
	if len(got) != 5 || by["2924"].Ticker != "" {
		t.Fatalf("got %d rows: %+v", len(got), got)
	}
	// Same day from FinMind: open 547, high 547, low 536, close 543. TPEx's
	// volume leaves out fixed-price trades (FinMind: 1,930,405 shares).
	p := by["6488"]
	if p.Open != 547 || p.High != 547 || p.Low != 536 || p.Close != 543 || p.AdjClose != 543 ||
		p.Volume != 1895000 || p.Turnover != 1026673000 || !p.Date.Equal(day) || p.Market != model.MarketTW {
		t.Errorf("6488 = %+v", p)
	}

	// 2010 has fewer columns; mapping is by name.
	got, err = parseDaily(readTestdata(t, "tpex_otc_20100104.json"), parseDay("2010-01-04"), tpexColumns)
	if err != nil || len(got) != 3 || got[0].Ticker != "1107" || got[0].Close != 0.95 || got[0].Volume != 189000 {
		t.Fatalf("2010: %v %+v", err, got)
	}

	// Typhoon closure: an empty table, no error.
	got, err = parseDaily(readTestdata(t, "tpex_otc_20241002.json"), parseDay("2024-10-02"), tpexColumns)
	if err != nil || len(got) != 0 {
		t.Fatalf("closed day: %v %+v", err, got)
	}

	if _, err := parseDaily(readTestdata(t, "twse_blocked.html"), day, tpexColumns); !errors.Is(err, errBlocked) {
		t.Fatalf("security page: err = %v, want errBlocked", err)
	}
}

func TestParseTPExEvents(t *testing.T) {
	divs, err := parseTPExDividends(readTestdata(t, "tpex_exdailyq_202406.json"))
	if err != nil || len(divs) == 0 {
		t.Fatalf("%v %+v", err, divs)
	}
	e := divs[0]
	if e.Ticker != "6788" || e.Kind != kindDividend || e.Date.Format("2006-01-02") != "2024-06-04" || math.Abs(e.Ratio-140.5/146.5) > 1e-12 {
		t.Errorf("first dividend = %+v", e)
	}
	reds, err := parseTPExReductions(readTestdata(t, "tpex_revivt_2024.json"))
	if err != nil || len(reds) != 3 {
		t.Fatalf("%v %+v", err, reds)
	}
	// 3064 resumed 2024-02-05 after a 70% loss-offset reduction.
	if e := reds[0]; e.Ticker != "3064" || e.Date.Format("2006-01-02") != "2024-02-05" || math.Abs(e.Ratio-35.5/10.65) > 1e-12 {
		t.Errorf("first reduction = %+v", e)
	}
}

func TestRebuildAdjustedCloses(t *testing.T) {
	st := openStore(t)
	for _, q := range []string{eventsTable} {
		st.DB.Exec(q)
	}
	d := newDirty()
	p := func(day string, close float64) model.Price {
		return model.Price{Market: model.MarketTW, Ticker: "6488", Date: parseDay(day), Close: close}
	}
	today := parseDay("2024-07-20")
	if err := writeRaw(st, d, []model.Price{p("2024-07-17", 576), p("2024-07-18", 565)}); err != nil {
		t.Fatal(err)
	}
	// Ex-dividend 2024-07-18: reference 565 against a 576 close. An event
	// dated after today is ignored.
	events := []tickerEvent{
		{"6488", kindDividend, adjEvent{parseDay("2024-07-18"), 565.0 / 576}},
		{"6488", kindDividend, adjEvent{parseDay("2024-12-01"), 0.5}},
	}
	if err := writeEvents(st, d, events, today); err != nil {
		t.Fatal(err)
	}
	if n, err := rebuild(st, d); err != nil || n != 1 {
		t.Fatalf("rebuild: %d %v", n, err)
	}
	adj := func(day string) float64 {
		var v float64
		st.DB.QueryRow(`SELECT adj_close FROM prices WHERE ticker = '6488' AND date = ?`, day).Scan(&v)
		return v
	}
	if math.Abs(adj("2024-07-17")-565) > 1e-9 || adj("2024-07-18") != 565 {
		t.Errorf("adj = %v, %v", adj("2024-07-17"), adj("2024-07-18"))
	}

	// Backfilling an older day resets nothing else and gets adjusted too;
	// the same event again from another source changes nothing.
	d = newDirty()
	writeRaw(st, d, []model.Price{p("2024-07-16", 580)})
	writeEvents(st, d, events[:1], today)
	if n, err := rebuild(st, d); err != nil || n != 1 {
		t.Fatalf("rebuild after backfill: %d %v", n, err)
	}
	if want := 580 * 565.0 / 576; math.Abs(adj("2024-07-16")-want) > 1e-9 {
		t.Errorf("backfilled adj = %v, want %v", adj("2024-07-16"), want)
	}
	// A new day after the last event needs no rebuild.
	d = newDirty()
	writeRaw(st, d, []model.Price{p("2024-07-19", 570)})
	if n, _ := rebuild(st, d); n != 0 || adj("2024-07-19") != 570 {
		t.Errorf("rebuilt %d tickers for a plain new day", n)
	}
}

func TestROCDate(t *testing.T) {
	for in, want := range map[string]string{"113/06/04": "2024-06-04", "1130205": "2024-02-05", "99/01/04": "2010-01-04", "0990104": "2010-01-04"} {
		if got := rocDate(in).Format("2006-01-02"); got != want {
			t.Errorf("rocDate(%q) = %s, want %s", in, got, want)
		}
	}
	if !rocDate("").IsZero() || !rocDate("abc").IsZero() {
		t.Error("bad dates should be zero")
	}
}

// fakeTPEx serves the saved TPEx files: daily quotes for the dates there are
// files for (an empty table otherwise), and the June 2024 ex-rights table
// for 2024.
func fakeTPEx(t *testing.T) (*TPEx, *[]string) {
	var mu sync.Mutex
	var calls []string
	empty := readTestdata(t, "tpex_otc_20241002.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Header.Get("User-Agent") == "" || q.Get("response") != "json" {
			t.Errorf("bad request: %v %v", r.URL, r.Header)
		}
		mu.Lock()
		calls = append(calls, r.URL.Path+"?"+q.Get("date")+q.Get("startDate"))
		mu.Unlock()
		switch r.URL.Path {
		case "/www/zh-tw/afterTrading/otc":
			b, err := os.ReadFile(filepath.Join("testdata", "tpex_otc_"+strings.ReplaceAll(q.Get("date"), "/", "")+".json"))
			if err != nil {
				b = empty
			}
			w.Write(b)
		case "/www/zh-tw/bulletin/exDailyQ":
			if strings.HasPrefix(q.Get("startDate"), "2024") {
				w.Write(readTestdata(t, "tpex_exdailyq_202406.json"))
				return
			}
			w.Write([]byte(`{"tables":[{"fields":[],"data":[]}],"stat":"ok"}`))
		case "/www/zh-tw/bulletin/revivt":
			w.Write([]byte(`{"tables":[{"fields":[],"data":[]}],"stat":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &TPEx{BaseURL: srv.URL, RunTime: time.Minute, HTTP: srv.Client()}, &calls
}

func TestCollect(t *testing.T) {
	cfg := config.Default()
	cfg.Run.HistoryStart = "2024-06-10"
	st := openStore(t)
	ctx := context.Background()
	now := time.Date(2024, 6, 14, 18, 0, 0, 0, taipei)

	fm, fmCalls := fakeFinMind(t, 1000)
	tp, tpCalls := fakeTPEx(t)
	if err := collect(ctx, cfg, st, fm, tp, now); err != nil {
		t.Fatal(err)
	}
	// OTC stocks come from the TPEx files, not FinMind.
	for _, c := range *fmCalls {
		if strings.Contains(c, "_6488") || strings.Contains(c, "_006201") {
			t.Errorf("FinMind asked for an OTC stock: %s", c)
		}
	}
	var close float64
	st.DB.QueryRow(`SELECT close FROM prices WHERE ticker = '6488' AND date = '2024-06-13'`).Scan(&close)
	if close != 543 {
		t.Errorf("6488 close from TPEx = %v", close)
	}
	// TWSE stocks from FinMind, adjusted for the 2024-06-13 dividend.
	var adj float64
	st.DB.QueryRow(`SELECT adj_close FROM prices WHERE ticker = '2330' AND date = '2024-06-11'`).Scan(&adj)
	if want := 883 * 905.5 / 909; math.Abs(adj-want) > 1e-9 {
		t.Errorf("2330 adj close = %v, want %v", adj, want)
	}
	// Five weekdays 06-10..06-14; the empty ones count as done, except today
	// (it may not be published yet).
	var days int
	st.DB.QueryRow(`SELECT COUNT(*) FROM tw_price_days WHERE exchange = 'TPEx'`).Scan(&days)
	if days != 4 {
		t.Errorf("recorded %d TPEx days, want 4", days)
	}
	var events int
	st.DB.QueryRow(`SELECT COUNT(*) FROM tw_adj_events WHERE kind = 'dividend' AND ticker = '6788'`).Scan(&events)
	if events != 1 {
		t.Errorf("TPEx dividend events stored: %d", events)
	}

	// Next run, same evening: settled TPEx days and event years are not
	// asked again (2024 is still open, so it is).
	*tpCalls = nil
	if err := collect(ctx, cfg, st, fm, tp, now); err != nil {
		t.Fatal(err)
	}
	for _, c := range *tpCalls {
		if strings.Contains(c, "otc?") && !strings.HasSuffix(c, "2024/06/14") {
			t.Errorf("refetched a settled day: %s", c)
		}
	}
}

// A ticker priced by the older collector has no stored events; its first
// update fetches them from history_start while prices stay incremental.
func TestOldTickerRefetchesEvents(t *testing.T) {
	cfg := config.Default()
	cfg.Run.HistoryStart = "2024-01-01"
	st := openStore(t)
	for _, q := range []string{checksTable, eventsTable, eventsFullTable} {
		st.DB.Exec(q)
	}
	st.UpsertPrices([]model.Price{{Market: model.MarketTW, Ticker: "2330", Date: parseDay("2024-06-12"), Close: 909, AdjClose: 905.5}})
	fm, calls := fakeFinMind(t, 1000)
	if err := updateTicker(context.Background(), cfg, st, fm, newDirty(), "2330", time.Date(2024, 6, 14, 18, 0, 0, 0, taipei)); err != nil {
		t.Fatal(err)
	}
	want := []string{"TaiwanStockPrice_2330@2024-06-12", "TaiwanStockDividendResult_2330@2024-01-01",
		"TaiwanStockCapitalReductionReferencePrice_2330@2024-01-01"}
	if strings.Join(*calls, " ") != strings.Join(want, " ") {
		t.Errorf("calls = %v, want %v", *calls, want)
	}
	*calls = nil
	updateTicker(context.Background(), cfg, st, fm, newDirty(), "2330", time.Date(2024, 6, 15, 18, 0, 0, 0, taipei))
	if (*calls)[1] != "TaiwanStockDividendResult_2330@2024-06-14" {
		t.Errorf("second update fetched events from %v", (*calls)[1])
	}
}
