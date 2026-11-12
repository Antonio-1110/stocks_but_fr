package ratings

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

func TestRank(t *testing.T) {
	got := rank([]float64{3, 1, math.NaN(), 2, 2})
	want := []float64{1, 0, 0.5, 0.5, 0.5}
	for k := range want {
		if math.Abs(got[k]-want[k]) > 1e-9 {
			t.Fatalf("rank = %v, want %v", got, want)
		}
	}
}

func TestQuintile(t *testing.T) {
	scores := make([]float64, 10)
	for k := range scores {
		scores[k] = float64(10 - k)
	}
	got := quintile(scores)
	want := []int{5, 5, 4, 4, 3, 3, 2, 2, 1, 1}
	for k := range want {
		if got[k] != want[k] {
			t.Fatalf("quintile = %v, want %v", got, want)
		}
	}
}

func TestKnownMonth(t *testing.T) {
	for d, want := range map[string]string{
		"2026-11-09": "2026-09", // October's revenue isn't due until the 10th
		"2026-11-10": "2026-10",
		"2026-01-15": "2025-12",
		"2026-01-05": "2025-11",
	} {
		if got := knownMonth(d, 10); got != want {
			t.Errorf("knownMonth(%s) = %s, want %s", d, got, want)
		}
	}
}

// weekdays returns n weekdays from 2024-01-01 on.
func weekdays(n int) []string {
	var out []string
	for d := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC); len(out) < n; d = d.AddDate(0, 0, 1) {
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday {
			out = append(out, d.Format("2006-01-02"))
		}
	}
	return out
}

// seed writes 10 Taiwan stocks with steady trends from -0.4%/day to +0.5%/day,
// plus a flat 0050, for the first `days` of dates.
func seed(t *testing.T, st *store.Store, dates []string, from, to int) {
	t.Helper()
	var prices []model.Price
	var cos []model.Company
	for k := 0; k < 10; k++ {
		tk := fmt.Sprintf("%d", 1101+k)
		drift := -0.004 + 0.001*float64(k)
		cos = append(cos, model.Company{Market: "TW", Ticker: tk, Name: "Co" + tk})
		for i := from; i < to; i++ {
			// A small wobble so 1-week moves differ; the drift dominates.
			c := 100 * math.Exp(drift*float64(i)+0.01*math.Sin(float64(i+k)))
			prices = append(prices, priceRow(tk, dates[i], c))
		}
	}
	for i := from; i < to; i++ {
		prices = append(prices, priceRow("0050", dates[i], 100))
	}
	if err := st.UpsertCompanies(cos); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPrices(prices); err != nil {
		t.Fatal(err)
	}
}

func priceRow(tk, d string, c float64) model.Price {
	day, _ := time.Parse("2006-01-02", d)
	return model.Price{Market: "TW", Ticker: tk, Date: day, Open: c, High: c, Low: c, Close: c, AdjClose: c, Volume: 1000, Turnover: c * 1e6}
}

func testConfig() config.Config {
	cfg := config.Default()
	cfg.Ratings.TWTop = 8 // leaves out the two least traded (the cheapest, falling ones)
	cfg.Ratings.BackfillDays = 100
	cfg.Ratings.BackfillStep = 5
	return cfg
}

func TestRunRateAndScore(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "radar.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dates := weekdays(420)
	seed(t, st, dates, 0, 400)
	cfg := testConfig()
	ctx := context.Background()
	if err := Run(ctx, cfg, st); err != nil {
		t.Fatal(err)
	}

	// Today's live ratings: the steepest uptrend is a 5 for the long term.
	last := dates[399]
	var n, long int
	st.DB.QueryRow(`SELECT COUNT(*) FROM state.ratings WHERE market = 'TW' AND made_on = ? AND backfill = 0`, last).Scan(&n)
	if n != 8 {
		t.Fatalf("live ratings on %s = %d, want 8", last, n)
	}
	st.DB.QueryRow(`SELECT long_rating FROM state.ratings WHERE ticker = '1110' AND made_on = ?`, last).Scan(&long)
	if long != 5 {
		t.Errorf("1110 (steepest uptrend) long rating = %d, want 5", long)
	}
	var bench int
	st.DB.QueryRow(`SELECT COUNT(*) FROM state.ratings WHERE ticker = '0050'`).Scan(&bench)
	if bench != 0 {
		t.Errorf("the benchmark was rated %d times", bench)
	}

	// The backfill: every 5th day back from the first live day, 100 days.
	var days, bf int
	st.DB.QueryRow(`SELECT COUNT(DISTINCT made_on), COUNT(*) FROM state.ratings WHERE backfill = 1`).Scan(&days, &bf)
	if days != 19 || bf != 19*8 {
		t.Errorf("backfill = %d days, %d ratings; want 19 days, %d", days, bf, 19*8)
	}

	// Outcomes: a rating made on i is scored over h days from i+1, so on
	// day 399 a 5-day rating needs i+6 <= 399.
	var scored, unscorable int
	st.DB.QueryRow(`SELECT COUNT(*), SUM(ret IS NULL) FROM state.rating_outcomes WHERE horizon = 5`).Scan(&scored, &unscorable)
	if scored != 18*8 || unscorable != 0 {
		t.Errorf("5-day outcomes = %d (%d without a return), want %d (the newest backfill day is too recent)", scored, unscorable, 18*8)
	}
	var start, end string
	var ret, b float64
	st.DB.QueryRow(`SELECT start_on, end_on, ret, bench FROM state.rating_outcomes
		WHERE ticker = '1110' AND made_on = ? AND horizon = 20`, dates[394-5*15]).Scan(&start, &end, &ret, &b)
	if start != dates[394-5*15+1] || end != dates[394-5*15+21] {
		t.Errorf("20-day window %s..%s, want %s..%s", start, end, dates[394-5*15+1], dates[394-5*15+21])
	}
	if ret <= 0 || b != 0 {
		t.Errorf("uptrend return %v, flat benchmark %v", ret, b)
	}

	// A second run with no new data does nothing; a new day is rated and
	// scores the ratings it matures.
	if err := Run(ctx, cfg, st); err != nil {
		t.Fatal(err)
	}
	var total int
	st.DB.QueryRow(`SELECT COUNT(*) FROM state.ratings`).Scan(&total)
	if total != 8+19*8 {
		t.Errorf("ratings after a rerun = %d", total)
	}
	seed(t, st, dates, 400, 410)
	if err := Run(ctx, cfg, st); err != nil {
		t.Fatal(err)
	}
	st.DB.QueryRow(`SELECT COUNT(*) FROM state.ratings WHERE made_on = ?`, dates[409]).Scan(&n)
	if n != 8 {
		t.Errorf("ratings on the new day = %d, want 8", n)
	}
	var liveScored int
	st.DB.QueryRow(`SELECT COUNT(*) FROM state.rating_outcomes WHERE made_on = ? AND horizon = 5`, last).Scan(&liveScored)
	if liveScored != 8 {
		t.Errorf("day %s's 5-day outcomes = %d, want 8", last, liveScored)
	}

	// The page: today's names, live accuracy and the backtest table.
	out := filepath.Join(dir, "public")
	if err := render(st.DB, cfg, out, time.Now()); err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile(filepath.Join(out, "ratings.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Stock ratings", "Rated on " + dates[409], "Co1110", "Accuracy: live", "Accuracy: backtest", "Strong up", "No ratings yet"} {
		if !strings.Contains(string(html), want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// Steady trends: up calls rose, down calls fell.
	m := Market{Code: "TW"}
	if err := load(st.DB, cfg.Ratings, &m); err != nil {
		t.Fatal(err)
	}
	for _, a := range m.Backtest {
		if a.Horizon == 120 && (a.UpRate() < 0.99 || a.DownRate() < 0.99 || a.Spread <= 0) {
			t.Errorf("120-day backtest accuracy %+v", a)
		}
	}
}

// A newest day still being collected isn't rated.
func TestSkipsHalfCollectedDay(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "radar.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dates := weekdays(300)
	seed(t, st, dates, 0, 299)
	if err := st.UpsertPrices([]model.Price{priceRow("1101", dates[299], 50)}); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), testConfig(), st); err != nil {
		t.Fatal(err)
	}
	var newest string
	st.DB.QueryRow(`SELECT MAX(made_on) FROM state.ratings`).Scan(&newest)
	if newest != dates[298] {
		t.Errorf("newest rating day %s, want %s", newest, dates[298])
	}
}
