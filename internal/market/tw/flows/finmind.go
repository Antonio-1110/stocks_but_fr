package flows

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/market/tw"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
)

// errQuota means FinMind's hourly limit is reached or the shared run budget
// is spent; stop for this run.
var errQuota = tw.ErrStop

func (c *Collector) fetchFinMind(ctx context.Context, ticker, start, end string) ([]model.InstitutionalFlow, error) {
	body, err := c.FinMind.Get(ctx, "TaiwanStockInstitutionalInvestorsBuySell", map[string]string{
		"data_id":    ticker,
		"start_date": start,
		"end_date":   end,
	})
	if err != nil {
		return nil, err
	}
	return parseFinMind(body)
}

// finmindResponse is FinMind's v4 envelope. Each data row is one investor
// group on one day; a day has several rows per ticker.
type finmindResponse struct {
	Msg    string `json:"msg"`
	Status int    `json:"status"`
	Data   []struct {
		Date    string `json:"date"`
		StockID string `json:"stock_id"`
		Buy     int64  `json:"buy"`
		Sell    int64  `json:"sell"`
		Name    string `json:"name"`
	} `json:"data"`
}

// parseFinMind folds FinMind's per-group rows into one flow per ticker-day.
// Groups: Foreign_Investor and Foreign_Dealer_Self are foreign;
// Investment_Trust is 投信; Dealer_self, Dealer_Hedging (and the older
// combined Dealer) are dealers. Unknown names are an error rather than
// silently dropped, so a renamed group is noticed.
func parseFinMind(body []byte) ([]model.InstitutionalFlow, error) {
	var r finmindResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode FinMind response: %w", err)
	}
	if r.Status == 402 {
		return nil, errQuota
	}
	if r.Status != 200 {
		return nil, fmt.Errorf("FinMind status %d: %s", r.Status, r.Msg)
	}
	type key struct{ ticker, date string }
	byDay := map[key]*model.InstitutionalFlow{}
	for _, d := range r.Data {
		k := key{strings.TrimSpace(d.StockID), d.Date}
		f := byDay[k]
		if f == nil {
			date, err := time.Parse("2006-01-02", d.Date)
			if err != nil {
				return nil, fmt.Errorf("bad date %q: %w", d.Date, err)
			}
			f = &model.InstitutionalFlow{Market: model.MarketTW, Ticker: k.ticker, Date: date}
			byDay[k] = f
		}
		net := d.Buy - d.Sell
		switch {
		case strings.EqualFold(d.Name, "total"):
			// a sum of the groups below, if FinMind ever sends one
		case strings.HasPrefix(d.Name, "Foreign"):
			f.ForeignNet += net
		case d.Name == "Investment_Trust":
			f.TrustNet += net
		case strings.HasPrefix(d.Name, "Dealer"):
			f.DealerNet += net
		default:
			return nil, fmt.Errorf("unknown investor group %q", d.Name)
		}
	}
	out := make([]model.InstitutionalFlow, 0, len(byDay))
	for _, f := range byDay {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Date.Equal(out[j].Date) {
			return out[i].Date.Before(out[j].Date)
		}
		return out[i].Ticker < out[j].Ticker
	})
	return out, nil
}
