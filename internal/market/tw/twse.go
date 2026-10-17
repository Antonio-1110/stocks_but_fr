package tw

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

// TWSE's open data API publishes the latest trading day for every listed
// stock in one file (issue #24). www.twse.com.tw blocks cloud addresses, but
// openapi.twse.com.tw answers. The file has no history, so FinMind still
// backfills, and fills in any stock this file can't extend cleanly.
const twseDayAllURL = "https://openapi.twse.com.tw/v1/exchangeReport/STOCK_DAY_ALL"

type twseDayRow struct {
	Date    string `json:"Date"` // ROC, e.g. "1151016"
	Code    string `json:"Code"`
	Volume  string `json:"TradeVolume"`
	Value   string `json:"TradeValue"`
	Open    string `json:"OpeningPrice"`
	High    string `json:"HighestPrice"`
	Low     string `json:"LowestPrice"`
	Close   string `json:"ClosingPrice"`
	Change  string `json:"Change"` // against the day's reference price
	Trading string `json:"Transaction"`
}

// twseQuote is one stock's day from the file. Ref is the reference price the
// exchange set for the day (close minus change): the previous close, or the
// ex-rights price on an ex-dividend day.
type twseQuote struct {
	model.Price
	Ref float64
}

func parseTWSEDayAll(body []byte) (time.Time, []twseQuote, error) {
	var rows []twseDayRow
	if err := json.Unmarshal(body, &rows); err != nil {
		return time.Time{}, nil, fmt.Errorf("TWSE STOCK_DAY_ALL: %w", err)
	}
	var day time.Time
	var out []twseQuote
	for _, r := range rows {
		d := rocDate(r.Date)
		if d.IsZero() {
			continue
		}
		if day.IsZero() {
			day = d
		} else if !d.Equal(day) {
			return time.Time{}, nil, fmt.Errorf("TWSE STOCK_DAY_ALL: mixed dates %s and %s", day.Format("2006-01-02"), d.Format("2006-01-02"))
		}
		o, ok1 := number(r.Open)
		h, ok2 := number(r.High)
		l, ok3 := number(r.Low)
		c, ok4 := number(r.Close)
		chg, ok5 := number(r.Change)
		if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || c <= 0 {
			continue // no trade that day
		}
		vol, _ := number(r.Volume)
		val, _ := number(r.Value)
		out = append(out, twseQuote{
			Price: model.Price{Market: model.MarketTW, Ticker: strings.TrimSpace(r.Code), Date: d,
				Open: o, High: h, Low: l, Close: c, Volume: int64(vol), Turnover: val},
			Ref: math.Round((c-chg)*10000) / 10000,
		})
	}
	return day, out, nil
}

func fetchTWSEDayAll(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %.120q", resp.StatusCode, body)
	}
	return body, nil
}

// twseDay writes the file's day for every TWSE stock whose stored prices end
// on the trading day before it, so nothing is skipped in between. A
// reference price that differs from the stored close means an ex-dividend
// (or ex-rights) day; it is stored as an event so adjusted closes are
// rebuilt. Other stocks are left to FinMind. It returns how many it wrote.
func twseDay(st *store.Store, d *dirtySet, day time.Time, quotes []twseQuote, now time.Time) (int, error) {
	if len(quotes) == 0 {
		return 0, nil
	}
	prev, ok, err := previousTradingDay(st, day)
	if err != nil || !ok {
		return 0, err
	}
	companies, err := st.Companies(model.MarketTW)
	if err != nil {
		return 0, err
	}
	twse := map[string]bool{}
	for _, co := range companies {
		if co.Exchange != "TPEx" && co.DelistedOn.IsZero() {
			twse[co.Ticker] = true
		}
	}
	last := map[string]struct {
		date  string
		close float64
	}{}
	rows, err := st.DB.Query(`SELECT p.ticker, p.date, p.close FROM prices p
		JOIN (SELECT ticker, MAX(date) AS date FROM prices WHERE market = ? AND date < ? GROUP BY ticker) m
		ON p.ticker = m.ticker AND p.date = m.date WHERE p.market = ?`,
		model.MarketTW, day.Format("2006-01-02"), model.MarketTW)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var t, dt string
		var c float64
		if err := rows.Scan(&t, &dt, &c); err != nil {
			rows.Close()
			return 0, err
		}
		last[t] = struct {
			date  string
			close float64
		}{dt, c}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var prices []model.Price
	var events []tickerEvent
	for _, q := range quotes {
		l, ok := last[q.Ticker]
		if !twse[q.Ticker] || !ok || l.date != prev.Format("2006-01-02") || l.close <= 0 {
			continue
		}
		prices = append(prices, q.Price)
		if math.Abs(q.Ref-l.close) > 0.005 && q.Ref > 0 {
			events = append(events, tickerEvent{Ticker: q.Ticker, Kind: kindDividend,
				adjEvent: adjEvent{Date: day, Ratio: q.Ref / l.close}})
		}
	}
	if err := writeEvents(st, d, events, today); err != nil {
		return 0, err
	}
	if err := writeRaw(st, d, prices); err != nil {
		return 0, err
	}
	if len(events) > 0 {
		log.Printf("tw: TWSE %s: %d ex-rights days from reference prices", day.Format("2006-01-02"), len(events))
	}
	return len(prices), nil
}

// previousTradingDay is the last trading day before day, from the TPEx days
// already fetched (both exchanges share a calendar). ok is false unless every
// weekday in between is known to have had no trading, so a gap is never
// stepped over.
func previousTradingDay(st *store.Store, day time.Time) (time.Time, bool, error) {
	rows, err := st.DB.Query(`SELECT date, rows FROM tw_price_days WHERE exchange = 'TPEx' AND date < ?
		ORDER BY date DESC LIMIT 20`, day.Format("2006-01-02"))
	if err != nil {
		return time.Time{}, false, err
	}
	defer rows.Close()
	known := map[string]int{}
	for rows.Next() {
		var dt string
		var n int
		if err := rows.Scan(&dt, &n); err != nil {
			return time.Time{}, false, err
		}
		known[dt] = n
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, false, err
	}
	for d := day.AddDate(0, 0, -1); d.After(day.AddDate(0, 0, -20)); d = d.AddDate(0, 0, -1) {
		n, ok := known[d.Format("2006-01-02")]
		switch {
		case ok && n > 0:
			return d, true, nil
		case !ok && d.Weekday() != time.Saturday && d.Weekday() != time.Sunday:
			return time.Time{}, false, nil // a weekday not fetched yet
		}
	}
	return time.Time{}, false, nil
}

// twseURL is a variable so tests can point it at a local server (or turn it
// off with "").
var twseURL = twseDayAllURL

// twseLatest fetches the file and applies it. Any failure is logged and
// leaves the TWSE stocks to FinMind for this run.
func twseLatest(ctx context.Context, st *store.Store, d *dirtySet, now time.Time) {
	if twseURL == "" {
		return
	}
	body, err := fetchTWSEDayAll(ctx, &http.Client{Timeout: 60 * time.Second}, twseURL)
	if err != nil {
		log.Printf("tw: TWSE daily file unavailable, FinMind covers TWSE stocks this run: %v", err)
		return
	}
	day, quotes, err := parseTWSEDayAll(body)
	if err != nil {
		log.Printf("tw: %v", err)
		return
	}
	if day.After(now) {
		log.Printf("tw: TWSE daily file dated %s, after today; ignored", day.Format("2006-01-02"))
		return
	}
	n, err := twseDay(st, d, day, quotes, now)
	if err != nil {
		log.Printf("tw: TWSE %s: %v", day.Format("2006-01-02"), err)
		return
	}
	log.Printf("tw: TWSE %s: %d of %d stocks from the exchange's daily file; FinMind covers the rest",
		day.Format("2006-01-02"), n, len(quotes))
}
