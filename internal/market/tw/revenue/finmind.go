package revenue

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Antonio-1110/stocks_but_fr/internal/market/tw"
)

// errQuota means FinMind refused the request for hitting the hourly limit,
// or the shared run budget is spent. The run stops collecting and resumes
// from the database next time.
var errQuota = tw.ErrStop

// finmindRow is one row of the TaiwanStockMonthRevenue dataset.
// Note: Date is the month *after* the revenue month (Jan revenue has
// date 2024-02-01); RevenueYear/RevenueMonth name the period itself.
type finmindRow struct {
	Date         string  `json:"date"`
	StockID      string  `json:"stock_id"`
	Revenue      float64 `json:"revenue"` // NTD
	RevenueMonth int     `json:"revenue_month"`
	RevenueYear  int     `json:"revenue_year"`
}

type finmindResponse struct {
	Msg    string       `json:"msg"`
	Status int          `json:"status"`
	Data   []finmindRow `json:"data"`
}

// parseFinMind decodes a TaiwanStockMonthRevenue response body.
func parseFinMind(body []byte) ([]finmindRow, error) {
	var r finmindResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("finmind: decode: %w", err)
	}
	if r.Status == http.StatusPaymentRequired {
		return nil, errQuota
	}
	if r.Status != http.StatusOK {
		return nil, fmt.Errorf("finmind: status %d: %s", r.Status, r.Msg)
	}
	return r.Data, nil
}
