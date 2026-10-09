package tw

import (
	"regexp"
	"sort"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/model"
)

// stockInfo is one row of FinMind TaiwanStockInfo. A stock can appear on
// several rows, one per industry category.
type stockInfo struct {
	Industry string `json:"industry_category"`
	StockID  string `json:"stock_id"`
	Name     string `json:"stock_name"`
	Type     string `json:"type"` // "twse", "tpex", "emerging"
	Date     string `json:"date"` // when FinMind last saw the row
}

// delisting is one row of FinMind TaiwanStockDelisting.
type delisting struct {
	Date    string `json:"date"`
	StockID string `json:"stock_id"`
	Name    string `json:"stock_name"`
}

// Common stocks have 4-digit codes; ETFs start with "00" (0050, 00878, 00632R).
var tickerRe = regexp.MustCompile(`^(\d{4}|00\d{2,4}[A-Z]?)$`)

// broadIndustries are group labels FinMind lists alongside the real industry.
var broadIndustries = map[string]bool{"電子工業": true, "": true}

var exchanges = map[string]string{"twse": "TWSE", "tpex": "TPEx"}

func parseDay(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t
}

// buildUniverse merges current listings and the delisting table. Emerging
// board (興櫃) stocks, indices and warrants are left out.
func buildUniverse(info []stockInfo, delisted []delisting) []model.Company {
	byTicker := map[string]*model.Company{}
	seen := map[string]time.Time{} // newest info date per ticker
	for _, r := range info {
		ex, ok := exchanges[r.Type]
		if !ok || !tickerRe.MatchString(r.StockID) {
			continue
		}
		c := byTicker[r.StockID]
		if c == nil {
			c = &model.Company{Market: model.MarketTW, Ticker: r.StockID, Name: r.Name, Industry: r.Industry, Exchange: ex}
			byTicker[r.StockID] = c
		} else if broadIndustries[c.Industry] && !broadIndustries[r.Industry] {
			c.Industry = r.Industry
		}
		if d := parseDay(r.Date); d.After(seen[r.StockID]) {
			seen[r.StockID] = d
		}
	}
	for _, r := range delisted {
		if !tickerRe.MatchString(r.StockID) {
			continue
		}
		d := parseDay(r.Date)
		c := byTicker[r.StockID]
		if c == nil {
			c = &model.Company{Market: model.MarketTW, Ticker: r.StockID, Name: r.Name}
			byTicker[r.StockID] = c
		} else if seen[r.StockID].After(d) {
			// Listed again after this delisting (or the code was reused).
			continue
		}
		if d.After(c.DelistedOn) {
			c.DelistedOn = d
		}
	}
	out := make([]model.Company, 0, len(byTicker))
	for _, c := range byTicker {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ticker < out[j].Ticker })
	return out
}
