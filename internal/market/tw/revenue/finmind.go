package revenue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// finmindURL is a var so tests can point it at a local server.
var finmindURL = "https://api.finmindtrade.com/api/v4/data"

const userAgent = "stocks_but_fr-radar/0.1 (+https://github.com/Antonio-1110/stocks_but_fr)"

// errQuota means FinMind refused the request for hitting the hourly limit.
// The run stops collecting and resumes from the database next time.
var errQuota = errors.New("finmind: request quota exhausted")

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

type client struct {
	http  *http.Client
	token string // FINMIND_TOKEN; empty works with a lower rate limit
}

// fetch returns monthly revenue rows for one stock whose FinMind date falls
// on or after start.
func (c *client) fetch(ctx context.Context, ticker string, start time.Time) ([]byte, error) {
	q := url.Values{
		"dataset":    {"TaiwanStockMonthRevenue"},
		"data_id":    {ticker},
		"start_date": {start.Format("2006-01-02")},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, finmindURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode == http.StatusTooManyRequests {
		return nil, errQuota
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("finmind %s: HTTP %d", ticker, resp.StatusCode)
	}
	return body, nil
}
