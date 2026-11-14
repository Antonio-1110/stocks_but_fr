package valuation

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func eq(p *float64, want float64) bool { return p != nil && math.Abs(*p-want) < 1e-9 }

func findV(t *testing.T, rows []Valuation, ticker string) Valuation {
	t.Helper()
	for _, r := range rows {
		if r.Ticker == ticker {
			return r
		}
	}
	t.Fatalf("no row for %s", ticker)
	return Valuation{}
}

func findH(t *testing.T, rows []Holding, ticker string) Holding {
	t.Helper()
	for _, r := range rows {
		if r.Ticker == ticker {
			return r
		}
	}
	t.Fatalf("no row for %s", ticker)
	return Holding{}
}

func TestValuationReports(t *testing.T) {
	for _, tc := range []struct {
		file, ticker  string
		n             int
		per, pbr, yld float64
	}{
		{"twse_bwibbu_20261113.json", "2330", 3, 23.92, 8.71, 0.94},
		{"tpex_pera_20261113.json", "6488", 3, 35.47, 2.28, 1.22},
	} {
		ex := Valuations.Exchanges[0]
		if tc.file[:4] == "tpex" {
			ex = Valuations.Exchanges[1]
		}
		rows, err := ex.Parse(readFile(t, tc.file), nov13)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if len(rows) != tc.n {
			t.Fatalf("%s: %d rows, want %d", tc.file, len(rows), tc.n)
		}
		v := findV(t, rows, tc.ticker)
		if !v.Date.Equal(nov13) || !eq(v.PER, tc.per) || !eq(v.PBR, tc.pbr) || !eq(v.DividendYield, tc.yld) {
			t.Errorf("%s %s = %+v, want P/E %v P/B %v yield %v", tc.file, tc.ticker, v, tc.per, tc.pbr, tc.yld)
		}
		// Another day's date in the report means no data for the day asked.
		if rows, err := ex.Parse(readFile(t, tc.file), nov13.AddDate(0, 0, -1)); err != nil || rows != nil {
			t.Errorf("%s for Nov 12: %d rows, err %v", tc.file, len(rows), err)
		}
	}
}

func TestValuationBlanks(t *testing.T) {
	body := `{"stat":"OK","date":"20261113","fields":["證券代號","證券名稱","收盤價","殖利率(%)","股利年度","本益比","股價淨值比","財報年/季"],
		"data":[["2498","宏達電","40.00","0.00",114,"-","1.10","115/3"]]}`
	rows, err := Valuations.Exchanges[0].Parse([]byte(body), nov13)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].PER != nil || !eq(rows[0].PBR, 1.1) || !eq(rows[0].DividendYield, 0) {
		t.Errorf("got %+v; want no P/E, P/B 1.1, yield 0", rows)
	}
}

func TestValuationFinMind(t *testing.T) {
	rows, err := parseValuationFinMind(readFile(t, "finmind_per_2330.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || !eq(rows[0].PER, 26.6) || !eq(rows[0].PBR, 9.25) || !eq(rows[0].DividendYield, 0.96) {
		t.Errorf("got %+v", rows)
	}
	zero := `{"msg":"success","status":200,"data":[{"date":"2026-11-03","stock_id":"2498","dividend_yield":0,"PER":0,"PBR":1.1}]}`
	rows, err = parseValuationFinMind([]byte(zero))
	if err != nil || rows[0].PER != nil || !eq(rows[0].DividendYield, 0) {
		t.Errorf("P/E 0 should be unknown: %+v %v", rows, err)
	}
}

func TestHoldingReports(t *testing.T) {
	rows, err := ForeignHoldings.Exchanges[0].Parse(readFile(t, "twse_mi_qfiis_20261113.json"), nov13)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("TWSE: %d rows, want 3", len(rows))
	}
	h := findH(t, rows, "2330")
	if h.SharesIssued != 25_932_370_067 || h.ForeignShares != 17_659_735_183 || !eq(h.ForeignPct, 68.09) || !eq(h.LimitPct, 100) {
		t.Errorf("2330 = %+v", h)
	}
	if h := findH(t, rows, "2603"); !eq(h.LimitPct, 49.99) {
		t.Errorf("2603 limit = %v, want 49.99", h.LimitPct)
	}

	rows, err = ForeignHoldings.Exchanges[1].Parse(readFile(t, "tpex_qfii_20261113.json"), nov13)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("TPEx: %d rows, want 3", len(rows))
	}
	h = findH(t, rows, "6488")
	if h.SharesIssued != 528_113_725 || h.ForeignShares != 156_927_780 || !eq(h.ForeignPct, 29.71) || !eq(h.LimitPct, 100) {
		t.Errorf("6488 = %+v", h)
	}
}

func TestHoldingFinMind(t *testing.T) {
	rows, err := parseHoldingFinMind(readFile(t, "finmind_shareholding_2330.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	h := rows[0]
	if h.Ticker != "2330" || h.SharesIssued != 25_932_370_067 || h.ForeignShares != 17_737_733_620 || !eq(h.ForeignPct, 68.39) {
		t.Errorf("got %+v", h)
	}
}
