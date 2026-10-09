package tw

import (
	"math"
	"sort"

	"github.com/Antonio-1110/stocks_but_fr/internal/model"
)

// priceRow is one row of FinMind TaiwanStockPrice or TaiwanStockPriceAdj.
// The adjusted dataset is back-adjusted: the newest close is the real close
// and older prices are scaled for every later dividend and split.
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

// mergePrices joins raw prices with adjusted closes by date. Days with no
// close (suspended trading) are dropped. A day missing from the adjusted
// series takes the adjustment factor of the next later day that has one.
func mergePrices(ticker string, raw, adj []priceRow) []model.Price {
	adjClose := make(map[string]float64, len(adj))
	for _, a := range adj {
		if a.Close > 0 {
			adjClose[a.Date] = a.Close
		}
	}
	var out []model.Price
	for _, r := range raw {
		if r.Close <= 0 {
			continue
		}
		out = append(out, model.Price{
			Market:   model.MarketTW,
			Ticker:   ticker,
			Date:     parseDay(r.Date),
			Open:     r.Open,
			High:     r.High,
			Low:      r.Low,
			Close:    r.Close,
			Volume:   int64(math.Round(r.Volume)),
			Turnover: r.Money,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	factor := 1.0
	for i := len(out) - 1; i >= 0; i-- {
		p := &out[i]
		if a, ok := adjClose[p.Date.Format("2006-01-02")]; ok {
			factor = a / p.Close
		}
		p.AdjClose = p.Close * factor
	}
	return out
}
