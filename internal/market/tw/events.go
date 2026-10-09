package tw

import (
	"sort"
	"time"
)

// FinMind's adjusted price dataset (TaiwanStockPriceAdj) needs a paid plan,
// so adjusted closes are built here from the free event datasets: ex-dividend
// and ex-rights days, capital reductions, and splits or par value changes.
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
