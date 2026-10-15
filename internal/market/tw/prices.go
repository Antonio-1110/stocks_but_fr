package tw

import (
	"math"
	"sort"

	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

// outsideRange reports an open that can't have traded: outside the day's
// low-high range. FinMind gives these for a stock's emerging-board (興櫃)
// days before it listed, where its "open" is the previous day's average
// price, the emerging board's reference price (issue #57). Such an open is
// stored as 0, the schema's "no open", like a day without an opening trade.
func outsideRange(open, high, low float64) bool {
	return open > 0 && high > 0 && low > 0 && (open > high || open < low)
}

// repairOpens applies the same rule to stored rows, so prices collected
// before the rule are fixed without fetching them again. It returns how
// many rows changed (none once they are fixed).
func repairOpens(st *store.Store) (int64, error) {
	res, err := st.DB.Exec(`UPDATE prices SET open = 0
		WHERE market = ? AND open > 0 AND high > 0 AND low > 0 AND (open > high OR open < low)`, model.MarketTW)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// priceRow is one row of FinMind TaiwanStockPrice.
type priceRow struct {
	Date     string  `json:"date"`
	StockID  string  `json:"stock_id"`
	Volume   float64 `json:"Trading_Volume"`
	Money    float64 `json:"Trading_money"`
	Open     float64 `json:"open"`
	High     float64 `json:"max"`
	Low      float64 `json:"min"`
	Close    float64 `json:"close"`
	Spread   float64 `json:"spread"`
	Turnover float64 `json:"Trading_turnover"` // number of trades, not value
}

// mergePrices turns raw prices into back-adjusted ones: the newest adjusted
// close is the real close, and each older close is scaled by the ratio of
// every event after it. Days with no close (suspended trading) are dropped.
func mergePrices(ticker string, raw []priceRow, events []adjEvent) []model.Price {
	var out []model.Price
	for _, r := range raw {
		if r.Close <= 0 {
			continue
		}
		open := r.Open
		if outsideRange(open, r.High, r.Low) {
			open = 0
		}
		out = append(out, model.Price{
			Market:   model.MarketTW,
			Ticker:   ticker,
			Date:     parseDay(r.Date),
			Open:     open,
			High:     r.High,
			Low:      r.Low,
			Close:    r.Close,
			Volume:   int64(math.Round(r.Volume)),
			Turnover: r.Money,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	factor, next := 1.0, len(events)-1
	for i := len(out) - 1; i >= 0; i-- {
		p := &out[i]
		for ; next >= 0 && events[next].Date.After(p.Date); next-- {
			factor *= events[next].Ratio
		}
		p.AdjClose = p.Close * factor
	}
	return out
}
