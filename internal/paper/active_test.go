package paper

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
)

// synthetic builds a market of business days from 2026-01-01 with the given
// closes (opens equal closes) and traded values; listed in companies.
func synthetic(t *testing.T, closes map[string][]float64, value map[string][]float64) (*market, *book) {
	t.Helper()
	st := openStore(t)
	m := &market{db: st.DB, bars: map[string]map[string]bar{}, names: map[string]string{}, ready: true}
	n := 0
	for _, c := range closes {
		n = max(n, len(c))
	}
	d := day("2026-01-01")
	for len(m.dates) < n {
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday {
			m.dates = append(m.dates, d.Format(dateLayout))
		}
		d = d.AddDate(0, 0, 1)
	}
	for tk, cs := range closes {
		m.bars[tk] = map[string]bar{}
		for i, c := range cs {
			v := 1e9
			if vs := value[tk]; len(vs) > 0 {
				v = vs[min(i, len(vs)-1)]
			}
			m.bars[tk][m.dates[i]] = bar{date: m.dates[i], open: c, close: c, adj: c, turnout: v}
		}
		_, err := st.DB.Exec(`INSERT INTO companies (market, ticker, name) VALUES ('TW', ?, ?)`, tk, tk)
		must(t, err)
	}
	return m, &book{Name: "test", Cash: 5e6, pos: map[string]*position{}, blocked: map[string]string{}}
}

func flat(v float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func tickers(os []*order) string {
	var s []string
	for _, o := range os {
		s = append(s, o.Side+":"+o.Ticker)
	}
	return strings.Join(s, " ")
}

func until(m *market, i int) *market {
	c := *m
	c.dates = m.dates[:i+1]
	return &c
}

func fresh(m *market, b *book) holdings { return mark(m, b, m.last()) }

func TestRSIMatchesPandas(t *testing.T) {
	m, _ := synthetic(t, map[string][]float64{"2001": {100, 102, 101, 105, 104, 100, 97, 99}}, nil)
	// strategies/pullback.py's rsi() on the same closes.
	want := []float64{math.NaN(), math.NaN(), 66.666667, 90.909091, 66.666667, 21.276596, 10.526316, 46.540881}
	for i, w := range want {
		got, ok := rsi(m, "2001", m.dates[i], 2)
		if math.IsNaN(w) != !ok || (ok && math.Abs(got-w) > 1e-5) {
			t.Errorf("day %d: rsi = %v (%v), want %v", i, got, ok, w)
		}
	}
}

func TestTopShareAveragesTies(t *testing.T) {
	got := topShare([]ranked{{"a", 1}, {"b", 2}, {"c", 3}, {"d", 3}, {"e", 0}}, 0.3)
	if fmt.Sprint(got) != "[c d]" {
		t.Errorf("top 30%% = %v, want [c d]", got)
	}
}

func TestQuarterEnd(t *testing.T) {
	for in, want := range map[string]string{"2026-01-05": "2026-03-31", "2026-06-30": "2026-06-30",
		"2026-08-14": "2026-09-30", "2026-12-01": "2026-12-31"} {
		if got := quarterEnd(day(in)).Format(dateLayout); got != want {
			t.Errorf("quarterEnd(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestBreakoutBuysAVolumeHighAndTrails(t *testing.T) {
	closes := append(flat(100, 70), 110, 110, 110, 90)
	value := append(flat(1e8, 70), 5e8, 1e8)
	up := make([]float64, 74)
	for i := range up {
		up[i] = 50 + float64(i)*0.1
	}
	m, b := synthetic(t, map[string][]float64{"2001": closes, "2002": append(flat(100, 70), 110, 110, 110, 110), "0050": up},
		map[string][]float64{"2001": value})
	s := breakout{config.Breakout{MaxPositions: 8, BreakoutDays: 20, VolumeDays: 20, VolumeMult: 2, MinADVNTD: 1e6,
		ADVDays: 5, RegimeTicker: "0050", RegimeDays: 5, ATRDays: 10, ATRMult: 3}, m.db}
	day70 := until(m, 70)
	out, err := s.decide(day70, b, fresh(day70, b))
	must(t, err)
	if got := tickers(out); got != "buy:2001" {
		t.Fatalf("breakout day: %q, want buy:2001 (2002 has no volume)", got)
	}
	b.pos["2001"] = &position{Ticker: "2001", Opened: m.dates[71], Shares: 1000, Cost: 110_000, Gross: 110_000}
	for i := 71; i < 73; i++ {
		mi := until(m, i)
		if out, _ := s.decide(mi, b, fresh(mi, b)); len(out) != 0 {
			t.Fatalf("day %d: %s, want nothing", i, tickers(out))
		}
	}
	last := until(m, 73)
	out, err = s.decide(last, b, fresh(last, b))
	must(t, err)
	if got := tickers(out); got != "sell:2001" || !strings.Contains(out[0].Reason, "trailing stop") {
		t.Errorf("after the drop: %q (%v), want the trailing stop", got, out)
	}
}

func TestBreakoutWaitsBelowTrend(t *testing.T) {
	down := make([]float64, 71)
	for i := range down {
		down[i] = 80 - float64(i)*0.1
	}
	m, b := synthetic(t, map[string][]float64{"2001": append(flat(100, 70), 110), "0050": down},
		map[string][]float64{"2001": append(flat(1e8, 70), 5e8)})
	s := breakout{config.Breakout{MaxPositions: 8, BreakoutDays: 20, VolumeDays: 20, VolumeMult: 2, MinADVNTD: 1e6,
		ADVDays: 5, RegimeTicker: "0050", RegimeDays: 5, ATRDays: 10, ATRMult: 3}, m.db}
	out, err := s.decide(m, b, fresh(m, b))
	must(t, err)
	if len(out) != 0 {
		t.Errorf("0050 below trend: %s, want nothing", tickers(out))
	}
}

func TestPullbackBuysADipAndSellsTheBounce(t *testing.T) {
	var up []float64
	for i := 0; i < 215; i++ {
		up = append(up, 100*math.Pow(1.01, float64(i)))
	}
	top := up[len(up)-1]
	dip := append(append([]float64{}, up...), top*0.97, top*0.94, top*0.91, top*1.0)
	weak := make([]float64, len(dip))
	for i := range weak {
		weak[i] = 100 + 0.01*float64(i)
	}
	m, b := synthetic(t, map[string][]float64{"2001": dip, "2002": weak}, nil)
	s := pullback{config.Pullback{MaxPositions: 10, MinADVNTD: 1e6, ADVDays: 5, TrendDays: 200, StrengthDays: 120,
		StrengthPct: 0.5, RSIDays: 2, EntryRSI: 10, ExitMADays: 5, MaxHoldDays: 10, StopLoss: 0.10}, m.db}
	var bought int
	for i := 214; i < 218; i++ {
		mi := until(m, i)
		out, err := s.decide(mi, b, fresh(mi, b))
		must(t, err)
		if len(out) > 0 {
			if got := tickers(out); got != "buy:2001" {
				t.Fatalf("day %d: %q, want buy:2001", i, got)
			}
			bought = i
			break
		}
	}
	if bought == 0 {
		t.Fatal("no buy on the dip")
	}
	px := m.bars["2001"][m.dates[bought+1]].close
	b.pos["2001"] = &position{Ticker: "2001", Opened: m.dates[bought+1], Shares: 1000, Cost: px * 1000, Gross: px * 1000}
	last := until(m, 218)
	out, err := s.decide(last, b, fresh(last, b))
	must(t, err)
	if got := tickers(out); got != "sell:2001" || !strings.Contains(out[0].Reason, "bounced") {
		t.Errorf("after the bounce: %q, want the bounce sell", got)
	}
}

func TestWindowDressingEntersOnceAndExitsBeforeQuarterEnd(t *testing.T) {
	m, b := synthetic(t, map[string][]float64{"2001": flat(100, 70), "2002": flat(100, 70)}, nil)
	for _, d := range m.dates[:45] {
		_, err := m.db.Exec(`INSERT INTO institutional_flows (market, ticker, date, foreign_net, trust_net, dealer_net)
			VALUES ('TW', '2001', ?, 0, 100000, 0), ('TW', '2002', ?, 0, -100000, 0)`, d, d)
		must(t, err)
	}
	s := windowDressing{config.WindowDressing{Quarters: []int{3}, EntryDays: 28, ExitDays: 3, FlowDays: 20,
		MaxPositions: 10, MinADVNTD: 1e6, ADVDays: 5, StopLoss: 0.10}, m.db}
	at := func(d string) *market {
		for i, x := range m.dates {
			if x == d {
				return until(m, i)
			}
		}
		t.Fatalf("%s not a trading day", d)
		return nil
	}
	early := at("2026-03-02") // 29 days before the quarter end
	if out, _ := s.decide(early, b, fresh(early, b)); len(out) != 0 {
		t.Fatalf("29 days out: %s, want nothing", tickers(out))
	}
	entry := at("2026-03-03")
	out, err := s.decide(entry, b, fresh(entry, b))
	must(t, err)
	if got := tickers(out); got != "buy:2001" {
		t.Fatalf("28 days out: %q, want buy:2001 (2002 was sold by 投信)", got)
	}
	again := at("2026-03-04")
	if out, _ := s.decide(again, b, fresh(again, b)); len(out) != 0 {
		t.Fatalf("buys only once a quarter, got %s", tickers(out))
	}
	b.pos["2001"] = &position{Ticker: "2001", Opened: "2026-03-04", Shares: 1000, Cost: 100_000, Gross: 100_000}
	exit := at("2026-03-27") // 4 days out: still held
	if out, _ := s.decide(exit, b, fresh(exit, b)); len(out) != 0 {
		t.Fatalf("4 days out: %s, want nothing", tickers(out))
	}
	exit = at("2026-03-30")
	out, err = s.decide(exit, b, fresh(exit, b))
	must(t, err)
	if got := tickers(out); got != "sell:2001" {
		t.Errorf("1 day out: %q, want sell:2001", got)
	}
}

func TestRecordRevenueBuysRecordsAndSellsOnTime(t *testing.T) {
	m, b := synthetic(t, map[string][]float64{"2001": flat(100, 60), "2002": flat(100, 60)}, nil)
	for i := 0; i < 36; i++ {
		mo := day("2023-01-01").AddDate(0, i, 0)
		ann := mo.AddDate(0, 1, 9).Format(dateLayout)
		r1 := 1e8
		if i == 35 {
			r1 = 3e8 // 2025-12: a record, YoY +200%
		}
		_, err := m.db.Exec(`INSERT INTO monthly_revenue (market, ticker, month, revenue, yoy_pct, announced_on)
			VALUES ('TW', '2001', ?, ?, ?, ?), ('TW', '2002', ?, 1e8, 0, ?)`,
			mo.Format(dateLayout), r1, (r1/1e8-1)*100, ann, mo.Format(dateLayout), ann)
		must(t, err)
	}
	s := recordRevenue{config.RecordRevenue{MaxPositions: 8, RecordMonths: 36, MinYoYPct: 20, MinBaseNTD: 1e7,
		MinADVNTD: 1e6, ADVDays: 5, HoldDays: 20, TrailStop: 0.10, RevenueDeadlineDay: 10}, m.db, 0}
	i := 0
	for m.dates[i] != "2026-01-13" { // deadline Sat the 10th, so Mon the 12th, then this
		i++
	}
	mi := until(m, i)
	out, err := s.decide(mi, b, fresh(mi, b))
	must(t, err)
	if got := tickers(out); got != "buy:2001" {
		t.Fatalf("rebalance day: %q, want buy:2001", got)
	}
	b.pos["2001"] = &position{Ticker: "2001", Opened: m.dates[i+1], Shares: 1000, Cost: 100_000, Gross: 100_000}
	sold := ""
	for j := i + 1; j < len(m.dates) && sold == ""; j++ {
		mj := until(m, j)
		out, err := s.decide(mj, b, fresh(mj, b))
		must(t, err)
		if len(out) > 0 {
			sold = mj.last() + " " + tickers(out)
		}
	}
	if want := m.dates[i+21] + " sell:2001"; sold != want {
		t.Errorf("sold %q, want %q (20 trading days after the fill)", sold, want)
	}
}
