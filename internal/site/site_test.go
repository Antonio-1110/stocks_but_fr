package site

import (
	stdhtml "html"
	"os"
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
	return st
}

func day(s string) time.Time {
	d, err := time.Parse(dateLayout, s)
	if err != nil {
		panic(err)
	}
	return d
}

// seed writes a small fake market: two live companies, one delisted, one
// that stopped trading, 30 trading days of prices, revenue and flows.
func seed(t *testing.T, st *store.Store) {
	t.Helper()
	tw := model.MarketTW
	must(t, st.UpsertCompanies([]model.Company{
		{Market: tw, Ticker: "2330", Name: "台積電", Industry: "半導體業", Exchange: "TWSE"},
		{Market: tw, Ticker: "2603", Name: "長榮", Industry: "航運業", Exchange: "TWSE"},
		{Market: tw, Ticker: "9999", Name: "已下市", Industry: "其他", DelistedOn: day("2020-01-01")},
		{Market: tw, Ticker: "8888", Name: "停牌<b>", Industry: "其他"},
	}))

	var prices []model.Price
	start := day("2026-08-01")
	for i := 0; i < 30; i++ {
		d := start.AddDate(0, 0, i)
		prices = append(prices,
			model.Price{Market: tw, Ticker: "2330", Date: d, Close: 1000 + float64(i)*10, AdjClose: 1000 + float64(i)*10, Turnover: 2e10},
			model.Price{Market: tw, Ticker: "2603", Date: d, Close: 200 - float64(i), AdjClose: 200 - float64(i), Turnover: 5e9},
		)
		if i < 5 {
			prices = append(prices, model.Price{Market: tw, Ticker: "8888", Date: d, Close: 10, AdjClose: 10})
		}
	}
	must(t, st.UpsertPrices(prices))

	rev := func(tk, month string, yoy float64, ann string) model.MonthlyRevenue {
		return model.MonthlyRevenue{Market: tw, Ticker: tk, Month: day(month), Revenue: 1e9, YoYPct: yoy, AnnouncedOn: day(ann)}
	}
	must(t, st.UpsertMonthlyRevenue([]model.MonthlyRevenue{
		rev("2330", "2026-06-01", 30, "2026-07-10"),
		rev("2330", "2026-07-01", 40, "2026-08-10"),
		rev("2330", "2026-08-01", 50, "2026-09-10"),
		rev("2330", "2026-09-01", 999, "2026-10-10"), // not announced yet on render day
		rev("2603", "2026-06-01", -10, "2026-07-10"),
		rev("2603", "2026-07-01", -20, "2026-08-10"),
		rev("2603", "2026-08-01", 60, "2026-09-10"),
	}))

	var flows []model.InstitutionalFlow
	for i := 0; i < 15; i++ {
		flows = append(flows, model.InstitutionalFlow{Market: tw, Ticker: "2330", Date: start.AddDate(0, 0, i+15), TrustNet: 100_000, ForeignNet: -1_000_000})
	}
	must(t, st.UpsertFlows(flows))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

var renderDay = day("2026-10-09")

var testLowBase = config.Default().Revenue.LowBase

var testUnpriced = config.Default().Site.Unpriced

func TestLoadRows(t *testing.T) {
	st := openStore(t)
	seed(t, st)
	rows, latest, err := loadRows(st, model.MarketTW, renderDay, testLowBase, testUnpriced)
	must(t, err)
	if latest != "2026-08-30" {
		t.Errorf("latest = %q", latest)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (delisted and stale dropped): %+v", len(rows), rows)
	}
	tsmc, evr := rows[0], rows[1]
	if tsmc.Ticker != "2330" || tsmc.Rank != 1 || evr.Rank != 2 {
		t.Errorf("order: %s #%d, %s #%d", tsmc.Ticker, tsmc.Rank, evr.Ticker, evr.Rank)
	}
	if got := *tsmc.RevYoY; got != 50 {
		t.Errorf("2330 latest YoY = %v, want 50 (September not announced yet)", got)
	}
	if got := *tsmc.RevYoYAvg; got != 40 {
		t.Errorf("2330 3M avg YoY = %v, want 40", got)
	}
	if tsmc.RevMonth != "2026-08" {
		t.Errorf("RevMonth = %q", tsmc.RevMonth)
	}
	if got := *tsmc.LastClose; got != 1290 {
		t.Errorf("last close = %v", got)
	}
	// 21 trading days before the last bar (index 29) is index 8: close 1080.
	if got, want := *tsmc.Change1M, (1290.0/1080-1)*100; abs(got-want) > 1e-9 {
		t.Errorf("1M change = %v, want %v", got, want)
	}
	if *tsmc.FromHigh != 0 {
		t.Errorf("2330 is at its high, FromHigh = %v", *tsmc.FromHigh)
	}
	if got, want := *evr.FromHigh, (171.0/200-1)*100; abs(got-want) > 1e-9 {
		t.Errorf("2603 FromHigh = %v, want %v", got, want)
	}
	if *tsmc.TrustNet10 != 1_000_000 || *tsmc.ForeignNet10 != -10_000_000 {
		t.Errorf("flows: trust %d foreign %d, want last 10 days only", *tsmc.TrustNet10, *tsmc.ForeignNet10)
	}
	if evr.TrustNet10 != nil {
		t.Errorf("2603 has no flows, got %d", *evr.TrustNet10)
	}
	if *tsmc.Turnover20 != 2e10 {
		t.Errorf("turnover = %v", *tsmc.Turnover20)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func TestRender(t *testing.T) {
	st := openStore(t)
	seed(t, st)
	_, err := st.DB.Exec(`CREATE TABLE backtest_runs (strategy TEXT, cagr REAL);
		INSERT INTO backtest_runs VALUES ('old', 0.01), ('revenue_momentum', 0.183)`)
	must(t, err)

	dir := t.TempDir()
	must(t, render(st, dir, renderDay, testLowBase, testUnpriced, nil))
	b, err := os.ReadFile(filepath.Join(dir, "screener.html"))
	must(t, err)
	html := stdhtml.UnescapeString(string(b))
	for _, want := range []string{
		"https://goodinfo.tw/tw/StockDetail.asp?STOCK_ID=2330",
		"https://tw.stock.yahoo.com/quote/2603",
		"mops.twse.com.tw",
		"台積電",
		"+40.0%",
		"+1,000", // 投信 10d in 張
		"200.0億", // 20d traded value
		"prices as of 2026-08-30",
		`href="screener.html" aria-current="page"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("screener.html missing %q", want)
		}
	}
	for _, bad := range []string{"已下市", "停牌"} {
		if strings.Contains(html, bad) {
			t.Errorf("screener.html should not contain %q", bad)
		}
	}
	if strings.Index(html, "台積電") > strings.Index(html, "長榮") {
		t.Error("2330 should be ranked above 2603")
	}

	b, err = os.ReadFile(filepath.Join(dir, "index.html"))
	must(t, err)
	overview := stdhtml.UnescapeString(string(b))
	for _, want := range []string{
		`href="index.html" aria-current="page"`,
		"台積電", "+40.0%", // top of the growth ranking, 3M average
		"revenue_momentum", // newest backtest row
		"0.183",
		"No paper portfolio has started yet.",
		"No validated snapshot yet.",
	} {
		if !strings.Contains(overview, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	for _, bad := range []string{"已下市", ">old<", "Yahoo"} {
		if strings.Contains(overview, bad) {
			t.Errorf("index.html should not contain %q", bad)
		}
	}

	for _, f := range []string{"static/style.css", "static/app.js"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Error(err)
		}
	}
}

func TestRenderEmptyStore(t *testing.T) {
	dir := t.TempDir()
	must(t, render(openStore(t), dir, renderDay, testLowBase, testUnpriced, nil))
	for _, name := range []string{"index.html", "screener.html"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		must(t, err)
		if !strings.Contains(string(b), "No companies in the store yet") {
			t.Errorf("%s: empty store should render the empty state", name)
		}
	}
}

func TestOverviewCards(t *testing.T) {
	st := openStore(t)
	_, err := st.DB.Exec(`
		CREATE TABLE state.paper_portfolios (name TEXT, initial_capital REAL);
		CREATE TABLE state.paper_equity (portfolio TEXT, date TEXT, liquidation REAL, bench REAL);
		CREATE TABLE state.paper_positions (portfolio TEXT, ticker TEXT);
		CREATE TABLE state.paper_orders (portfolio TEXT, status TEXT);
		INSERT INTO state.paper_portfolios VALUES ('cycle_bottom', 1000), ('new_thing', 1000);
		INSERT INTO state.paper_equity VALUES ('cycle_bottom', '2026-10-01', 1000, NULL),
			('cycle_bottom', '2026-10-02', 1100, 1050), ('new_thing', '2026-10-02', 900, NULL);
		INSERT INTO state.paper_positions VALUES ('cycle_bottom', '2330'), ('cycle_bottom', '2603');
		INSERT INTO state.paper_orders VALUES ('cycle_bottom', 'pending'), ('cycle_bottom', 'filled');
		CREATE TABLE corporate_events (market TEXT, ticker TEXT, date TEXT, kind TEXT, title TEXT, name TEXT);
		INSERT INTO corporate_events VALUES ('TW', '2330', '2026-10-09', 'conference', '法說會', '台積電'),
			('TW', '0050', '2026-10-10', 'exdiv', '除息', '元大台灣50'),
			('TW', '2603', '2026-10-30', 'meeting', '股東會', '長榮');
		CREATE TABLE snapshots (id INTEGER PRIMARY KEY, manifest TEXT);
		INSERT INTO snapshots (manifest) VALUES
			('{"id":"2026-10-09T01:00:00Z","latest":{"prices":"2026-10-08"},"hard_checks":[{"ok":true}],"soft_checks":[{"ok":false},{"ok":true}]}');`)
	must(t, err)

	cards, err := loadPaper(st.DB, []string{"cycle_bottom", "missing", "new_thing"})
	must(t, err)
	if len(cards) != 2 {
		t.Fatalf("paper cards = %+v, want 2", cards)
	}
	c := cards[0]
	if c.Title != "Cycle bottom" || c.Date != "2026-10-02" || c.Holdings != 2 || c.PendingOrders != 1 ||
		!c.HasBench || c.Return < 0.0999 || c.Return > 0.1001 || c.BenchReturn < 0.0499 || c.BenchReturn > 0.0501 {
		t.Errorf("cycle_bottom card = %+v", c)
	}
	if cards[1].Title != "New thing" || cards[1].HasBench {
		t.Errorf("new_thing card = %+v", cards[1])
	}

	evs, total, err := loadUpcoming(st.DB, model.MarketTW, renderDay)
	must(t, err)
	if total != 1 || len(evs) != 1 || evs[0].Ticker != "2330" || evs[0].Kind != "法說會" || evs[0].Date != "10-09" {
		t.Errorf("upcoming = %+v (total %d), want only 2330 (ETF and later events left out)", evs, total)
	}

	h, err := loadHealth(st.DB)
	must(t, err)
	if h == nil || h.HardFailed != 0 || h.SoftFailed != 1 || h.Prices != "2026-10-08" || h.Stamp != "2026-10-09 09:00" {
		t.Errorf("health = %+v", h)
	}
}

func TestWithCommas(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "+999", 1000: "+1,000", -1234567: "-1,234,567"} {
		if got := withCommas(n, true); got != want {
			t.Errorf("withCommas(%d) = %q, want %q", n, got, want)
		}
	}
}

// monthly revenue for one ticker from first, one value per month.
func revSeries(tk, first string, revs ...float64) []model.MonthlyRevenue {
	var out []model.MonthlyRevenue
	m := day(first)
	for i, v := range revs {
		r := model.MonthlyRevenue{Market: model.MarketTW, Ticker: tk, Month: m.AddDate(0, i, 0), Revenue: v,
			AnnouncedOn: m.AddDate(0, i+1, 9)}
		if i >= 12 && revs[i-12] != 0 {
			r.YoYPct = (v/revs[i-12] - 1) * 100
		}
		out = append(out, r)
	}
	return out
}

func TestLowBase(t *testing.T) {
	st := openStore(t)
	tw := model.MarketTW
	must(t, st.UpsertCompanies([]model.Company{
		{Market: tw, Ticker: "1111", Name: "Steady"},
		{Market: tw, Ticker: "2222", Name: "Dip"},
		{Market: tw, Ticker: "3333", Name: "Tiny"},
	}))
	// 24 months from 2024-09 to 2026-08; the YoY window is 2026-06..08.
	steady, dip, tiny := make([]float64, 24), make([]float64, 24), make([]float64, 24)
	for i := range steady {
		steady[i] = 100e6 * (1 + float64(i)/50) // ~+25% a year
		dip[i] = 100e6
		tiny[i] = 5e6
	}
	dip[9] = 10e6   // 2025-06: one awful month, 10% of usual...
	dip[21] = 100e6 // ...so 2026-06 shows +900%
	tiny[22] = 20e6 // 2026-07 +300%, but every base is under the floor
	must(t, st.UpsertMonthlyRevenue(revSeries("1111", "2024-09-01", steady...)))
	must(t, st.UpsertMonthlyRevenue(revSeries("2222", "2024-09-01", dip...)))
	must(t, st.UpsertMonthlyRevenue(revSeries("3333", "2024-09-01", tiny...)))

	rows, _, err := loadRows(st, tw, renderDay, testLowBase, testUnpriced)
	must(t, err)
	got := map[string]Row{}
	for _, r := range rows {
		got[r.Ticker] = r
	}
	if rows[0].Ticker != "1111" || rows[0].Rank != 1 {
		t.Errorf("clean stock should rank first, got %s #%d", rows[0].Ticker, rows[0].Rank)
	}
	dipRow := got["2222"]
	if !dipRow.LowBase || dipRow.Rank != 0 || dipRow.RevYoYLowBase {
		t.Errorf("2222: LowBase %v Rank %d latest-flag %v; want flagged, unranked, latest month clean",
			dipRow.LowBase, dipRow.Rank, dipRow.RevYoYLowBase)
	}
	if len(dipRow.LowBaseNotes) != 1 || !strings.Contains(dipRow.LowBaseNotes[0], "2025-06") {
		t.Errorf("2222 notes = %q", dipRow.LowBaseNotes)
	}
	if r := got["3333"]; !r.LowBase || !r.RevYoYLowBase || len(r.LowBaseNotes) != 3 {
		t.Errorf("3333 should be flagged on all 3 months (floor): %+v", r)
	}
	if *dipRow.RevYoYAvg < 300 {
		t.Errorf("flagged rows keep their YoY: avg = %v", *dipRow.RevYoYAvg)
	}
}

func TestPercentiles(t *testing.T) {
	got := percentiles([]float64{30, 10, 20, 20})
	want := []float64{1, 0, 0.5, 0.5}
	for i := range want {
		if abs(got[i]-want[i]) > 1e-9 {
			t.Fatalf("percentiles = %v, want %v", got, want)
		}
	}
	if p := percentiles([]float64{5}); p[0] != 1 {
		t.Errorf("single value = %v", p)
	}
}

func TestRankUnpriced(t *testing.T) {
	f, i := ptr[float64], ptr[int64]
	row := func(tk string, yoy, avg, ch1m, fromHigh float64, net int64) Row {
		return Row{Ticker: tk, LastClose: f(100), Turnover20: f(1e9), RevYoY: f(yoy), RevYoYAvg: f(avg),
			Change1M: f(ch1m), FromHigh: f(fromHigh), TrustNet10: i(net)}
	}
	rows := []Row{
		row("RAN", 60, 50, 40, 0, 5_000_000),             // fast growth, but the price already ran and funds piled in
		row("LAG", 70, 50, -5, -30, 0),                   // same growth, speeding up, price flat and far off its high
		row("MID", 40, 40, 5, -10, 1_000_000),            //
		row("SLW", 15, 15, -20, -40, 0),                  // under min_growth_pct
		{Ticker: "NOP", RevYoY: f(80), RevYoYAvg: f(80)}, // no price yet
	}
	lb := row("LBS", 900, 900, -10, -50, 0)
	lb.LowBase = true
	rows = append(rows, lb)

	rankUnpriced(rows, testUnpriced)
	ranks := map[string]int{}
	for _, r := range rows {
		ranks[r.Ticker] = r.UnpricedRank
	}
	// LAG and RAN grow alike; only LAG's price hasn't moved yet.
	if ranks["LAG"] != 1 || ranks["RAN"] == 0 || ranks["MID"] == 0 {
		t.Errorf("ranks = %v, want LAG first and RAN, MID ranked", ranks)
	}
	for _, tk := range []string{"SLW", "NOP", "LBS"} {
		if ranks[tk] != 0 {
			t.Errorf("%s should not be ranked, got %d", tk, ranks[tk])
		}
	}
	if s := *rows[1].UnpricedScore; s <= 0 || s > 1 {
		t.Errorf("score out of 0..1: %v", s)
	}
}
