package site

import (
	stdhtml "html"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		return model.MonthlyRevenue{Market: tw, Ticker: tk, Month: day(month), YoYPct: yoy, AnnouncedOn: day(ann)}
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

func TestLoadRows(t *testing.T) {
	st := openStore(t)
	seed(t, st)
	rows, latest, err := loadRows(st, model.MarketTW, renderDay)
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
	must(t, render(st, dir, renderDay))
	b, err := os.ReadFile(filepath.Join(dir, "index.html"))
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
		"revenue_momentum", // newest backtest row
		"0.183",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	for _, bad := range []string{"已下市", "停牌", ">old<"} {
		if strings.Contains(html, bad) {
			t.Errorf("index.html should not contain %q", bad)
		}
	}
	if strings.Index(html, "台積電") > strings.Index(html, "長榮") {
		t.Error("2330 should be ranked above 2603")
	}
	for _, f := range []string{"static/style.css", "static/app.js"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Error(err)
		}
	}
}

func TestRenderEmptyStore(t *testing.T) {
	dir := t.TempDir()
	must(t, render(openStore(t), dir, renderDay))
	b, err := os.ReadFile(filepath.Join(dir, "index.html"))
	must(t, err)
	if !strings.Contains(string(b), "No companies in the store yet") {
		t.Error("empty store should render the empty state")
	}
}

func TestWithCommas(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "+999", 1000: "+1,000", -1234567: "-1,234,567"} {
		if got := withCommas(n, true); got != want {
			t.Errorf("withCommas(%d) = %q, want %q", n, got, want)
		}
	}
}
