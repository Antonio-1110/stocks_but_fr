package tw

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FinMind's adjusted price dataset (TaiwanStockPriceAdj) needs a paid plan,
// so adjusted closes are built here from event data: ex-dividend and
// ex-rights days, capital reductions (from TPEx for the whole OTC market and
// from FinMind per stock), and splits or par value changes (FinMind).
// Each event says what the exchange set as the reference price after the
// event against the last close before it.

// adjEvent scales every price before Date by Ratio (reference price after
// the event divided by the last close before it).
type adjEvent struct {
	Date  time.Time
	Ratio float64
}

// dividendRow is one row of FinMind TaiwanStockDividendResult (除權息).
type dividendRow struct {
	Date        string  `json:"date"`
	StockID     string  `json:"stock_id"`
	BeforePrice float64 `json:"before_price"`
	AfterPrice  float64 `json:"after_price"`
}

// reductionRow is one row of FinMind TaiwanStockCapitalReductionReferencePrice (減資).
type reductionRow struct {
	Date        string  `json:"date"`
	StockID     string  `json:"stock_id"`
	LastClose   float64 `json:"ClosingPriceonTheLastTradingDay"`
	ResumePrice float64 `json:"PostReductionReferencePrice"`
}

// splitRow is one row of FinMind TaiwanStockSplitPrice (分割 and 面額變更).
// This dataset can be fetched for the whole market in one request.
type splitRow struct {
	Date        string  `json:"date"`
	StockID     string  `json:"stock_id"`
	BeforePrice float64 `json:"before_price"`
	AfterPrice  float64 `json:"after_price"`
}

func newEvent(date string, before, after float64) (adjEvent, bool) {
	d := parseDay(date)
	if d.IsZero() || before <= 0 || after <= 0 {
		return adjEvent{}, false
	}
	return adjEvent{Date: d, Ratio: after / before}, true
}

// buildEvents collects one ticker's events, oldest first.
func buildEvents(divs []dividendRow, reds []reductionRow, splits []splitRow) []adjEvent {
	var out []adjEvent
	add := func(e adjEvent, ok bool) {
		if ok {
			out = append(out, e)
		}
	}
	for _, r := range divs {
		add(newEvent(r.Date, r.BeforePrice, r.AfterPrice))
	}
	for _, r := range reds {
		add(newEvent(r.Date, r.LastClose, r.ResumePrice))
	}
	for _, r := range splits {
		add(newEvent(r.Date, r.BeforePrice, r.AfterPrice))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out
}

// eventsTable keeps every adjustment event from every source, so adjusted
// closes can be rebuilt from stored raw prices at any time. A dividend that
// FinMind and TPEx both report lands on one row (same ticker, day and kind).
const eventsTable = `CREATE TABLE IF NOT EXISTS tw_adj_events (
	ticker TEXT NOT NULL,
	date   TEXT NOT NULL,
	kind   TEXT NOT NULL,
	ratio  REAL NOT NULL,
	PRIMARY KEY (ticker, date, kind)
)`

const (
	kindDividend  = "dividend"  // 除權息
	kindReduction = "reduction" // 減資
	kindSplit     = "split"     // 分割, 面額變更
)

type tickerEvent struct {
	Ticker string
	Kind   string
	adjEvent
}

// rocDate parses Taiwan (ROC) dates such as "113/06/04" and "1130205".
func rocDate(s string) time.Time {
	s = strings.ReplaceAll(strings.TrimSpace(s), "/", "")
	if len(s) < 6 {
		return time.Time{}
	}
	y, err := strconv.Atoi(s[:len(s)-4])
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse("20060102", fmt.Sprintf("%04d%s", y+1911, s[len(s)-4:]))
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseEventTable reads TPEx's ex-rights (exDailyQ) or capital reduction
// (revivt) report: one row per event, the date, the code, the close before
// the event and the reference price after it, found by column name.
func parseEventTable(body []byte, kind, dateCol, codeCol, beforeCol, afterCol string) ([]tickerEvent, error) {
	var doc struct {
		Tables []exTable `json:"tables"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode: %w (%.120q)", err, body)
	}
	var out []tickerEvent
	for _, t := range doc.Tables {
		d, c, b, a := t.column(dateCol), t.column(codeCol), t.column(beforeCol), t.column(afterCol)
		if d < 0 || c < 0 || b < 0 || a < 0 {
			if len(t.Data) > 0 {
				return nil, fmt.Errorf("%s table is missing a column: %v", kind, t.Fields)
			}
			continue
		}
		for _, row := range t.Data {
			before, ok1 := number(cell(row, b))
			after, ok2 := number(cell(row, a))
			ticker := cell(row, c)
			date := rocDate(cell(row, d))
			if !ok1 || !ok2 || before <= 0 || after <= 0 || date.IsZero() || !tickerRe.MatchString(ticker) {
				continue
			}
			out = append(out, tickerEvent{Ticker: ticker, Kind: kind, adjEvent: adjEvent{Date: date, Ratio: after / before}})
		}
	}
	return out, nil
}

func parseTPExDividends(body []byte) ([]tickerEvent, error) {
	return parseEventTable(body, kindDividend, "除權息日期", "代號", "除權息前收盤價", "除權息參考價")
}

func parseTPExReductions(body []byte) ([]tickerEvent, error) {
	return parseEventTable(body, kindReduction, "恢復買賣日期", "股票代號", "最後交易日之收盤價格", "減資恢復買賣開始日參考價格")
}

// finmindEvents turns one ticker's FinMind event rows into stored events.
func finmindEvents(ticker string, divs []dividendRow, reds []reductionRow) []tickerEvent {
	var out []tickerEvent
	for _, r := range divs {
		if e, ok := newEvent(r.Date, r.BeforePrice, r.AfterPrice); ok {
			out = append(out, tickerEvent{ticker, kindDividend, e})
		}
	}
	for _, r := range reds {
		if e, ok := newEvent(r.Date, r.LastClose, r.ResumePrice); ok {
			out = append(out, tickerEvent{ticker, kindReduction, e})
		}
	}
	return out
}

func splitEvents(rows []splitRow) []tickerEvent {
	var out []tickerEvent
	for _, r := range rows {
		if e, ok := newEvent(r.Date, r.BeforePrice, r.AfterPrice); ok {
			out = append(out, tickerEvent{r.StockID, kindSplit, e})
		}
	}
	return out
}
