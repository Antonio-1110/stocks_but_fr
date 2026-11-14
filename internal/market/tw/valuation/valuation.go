// Package valuation collects two daily Taiwan datasets (issue #85):
//
//   - tw_valuations: P/E, P/B and dividend yield as the exchanges publish
//     them (history: FinMind TaiwanStockPER; recent days: TWSE BWIBBU_d and
//     the TPEx P/E report).
//   - tw_foreign_holdings: shares issued and shares held by foreign
//     investors (外資持股), so foreign ownership % and market cap (shares
//     issued x close) are known per day (history: FinMind
//     TaiwanStockShareholding; recent days: TWSE MI_QFIIS and the TPEx
//     foreign holdings report).
//
// Both go through the shared engine in package dataset.
package valuation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/market/tw"
	"github.com/Antonio-1110/stocks_but_fr/internal/market/tw/dataset"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

// Collect is the `radar collect` step. The step's FinMind budget is split
// between the two backfills; what the first leaves passes to the second.
func Collect(ctx context.Context, cfg config.Config, st *store.Store) error {
	start, err := time.Parse("2006-01-02", cfg.Run.HistoryStart)
	if err != nil {
		return fmt.Errorf("run.history_start: %w", err)
	}
	fm := tw.ForStep("tw-valuation", 0) // the last FinMind step: whatever is left
	errVal := dataset.New(Valuations, fm.Allot(fm.Left()/2)).Run(ctx, st, start)
	errHold := dataset.New(ForeignHoldings, fm).Run(ctx, st, start)
	return errors.Join(errVal, errHold)
}

// ---- P/E, P/B, dividend yield ----

// Valuation is one stock's ratios on one day. A ratio the exchange leaves
// blank (P/E of a loss-making company) is nil.
type Valuation struct {
	Ticker        string
	Date          time.Time
	PER           *float64 // price / trailing four quarters' EPS
	PBR           *float64 // price / book value per share
	DividendYield *float64 // percent, last year's dividends / price
}

const valuationSchema = `CREATE TABLE IF NOT EXISTS tw_valuations (
	market         TEXT NOT NULL,
	ticker         TEXT NOT NULL,
	date           TEXT NOT NULL,
	per            REAL,  -- P/E (本益比); NULL when not meaningful (losses)
	pbr            REAL,  -- P/B (股價淨值比)
	dividend_yield REAL,  -- percent (殖利率)
	PRIMARY KEY (market, ticker, date)
)`

var Valuations = dataset.Dataset[Valuation]{
	Name:         "valuation",
	Schema:       valuationSchema,
	FinMind:      "TaiwanStockPER",
	ParseFinMind: parseValuationFinMind,
	Exchanges: []dataset.Exchange[Valuation]{
		{
			Name: "twse",
			URL:  "https://www.twse.com.tw/rwd/zh/afterTrading/BWIBBU_d",
			Query: func(d time.Time) url.Values {
				return url.Values{"date": {d.Format("20060102")}, "selectType": {"ALL"}, "response": {"json"}}
			},
			Parse: func(b []byte, d time.Time) ([]Valuation, error) {
				return parseValuationReport(b, d, "證券代號", "本益比", "股價淨值比", "殖利率(%)")
			},
		},
		{
			Name: "tpex",
			URL:  "https://www.tpex.org.tw/web/stock/aftertrading/peratio_analysis/pera_result.php",
			Query: func(d time.Time) url.Values {
				return url.Values{"l": {"zh-tw"}, "o": {"json"}, "d": {dataset.ROCDate(d)}}
			},
			Parse: func(b []byte, d time.Time) ([]Valuation, error) {
				return parseValuationReport(b, d, "股票代號", "本益比", "股價淨值比", "殖利率(%)")
			},
		},
	},
	Save: func(db *sql.DB, rows []Valuation) error {
		return dataset.Upsert(db, `INSERT OR REPLACE INTO tw_valuations
			(market, ticker, date, per, pbr, dividend_yield) VALUES (?, ?, ?, ?, ?, ?)`,
			rows, func(v Valuation) []any {
				return []any{model.MarketTW, v.Ticker, v.Date.Format("2006-01-02"), v.PER, v.PBR, v.DividendYield}
			})
	},
}

func parseValuationFinMind(body []byte) ([]Valuation, error) {
	data, err := dataset.FinMindData[struct {
		Date          string  `json:"date"`
		StockID       string  `json:"stock_id"`
		PER           float64 `json:"PER"`
		PBR           float64 `json:"PBR"`
		DividendYield float64 `json:"dividend_yield"`
	}](body)
	if err != nil {
		return nil, err
	}
	out := make([]Valuation, 0, len(data))
	for _, d := range data {
		date, err := time.Parse("2006-01-02", d.Date)
		if err != nil {
			return nil, fmt.Errorf("bad date %q: %w", d.Date, err)
		}
		// FinMind writes a blank P/E or P/B as 0; the exchanges leave it empty.
		out = append(out, Valuation{Ticker: strings.TrimSpace(d.StockID), Date: date,
			PER: positive(d.PER), PBR: positive(d.PBR), DividendYield: &d.DividendYield})
	}
	return out, nil
}

func parseValuationReport(body []byte, day time.Time, tickerCol, perCol, pbrCol, yieldCol string) ([]Valuation, error) {
	t, err := reportTable(body, day, tickerCol)
	if t == nil || err != nil {
		return nil, err
	}
	cols, err := dataset.Columns(t.Fields, tickerCol, perCol, pbrCol, yieldCol)
	if err != nil {
		return nil, err
	}
	date := dataset.Date(day)
	out := make([]Valuation, 0, len(t.Data))
	for n, cells := range t.Data {
		if len(cells) <= maxOf(cols) {
			return nil, fmt.Errorf("row %d has %d cells", n, len(cells))
		}
		v := Valuation{Ticker: strings.TrimSpace(string(cells[cols[0]])), Date: date}
		if v.Ticker == "" {
			continue
		}
		for i, dst := range []**float64{&v.PER, &v.PBR, &v.DividendYield} {
			f, ok, err := dataset.Float(string(cells[cols[i+1]]))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", v.Ticker, err)
			}
			if ok {
				*dst = &f
			}
		}
		if v.PER != nil && *v.PER <= 0 {
			v.PER = nil
		}
		out = append(out, v)
	}
	return out, nil
}

// ---- foreign holdings and shares issued ----

// Holding is one stock's shares issued and foreign-held shares on one day.
type Holding struct {
	Ticker        string
	Date          time.Time
	SharesIssued  int64    // 發行股數
	ForeignShares int64    // 外資及陸資持有股數
	ForeignPct    *float64 // percent of shares issued, as published
	LimitPct      *float64 // legal cap on foreign ownership, percent
}

const holdingSchema = `CREATE TABLE IF NOT EXISTS tw_foreign_holdings (
	market         TEXT NOT NULL,
	ticker         TEXT NOT NULL,
	date           TEXT NOT NULL,
	shares_issued  INTEGER,  -- shares (股), not lots; x close = market cap
	foreign_shares INTEGER,  -- shares held by foreign and mainland investors
	foreign_pct    REAL,     -- percent of shares issued (外資持股比率)
	limit_pct      REAL,     -- legal foreign ownership cap, percent
	PRIMARY KEY (market, ticker, date)
)`

var ForeignHoldings = dataset.Dataset[Holding]{
	Name:         "foreign_holdings",
	Schema:       holdingSchema,
	FinMind:      "TaiwanStockShareholding",
	ParseFinMind: parseHoldingFinMind,
	Exchanges: []dataset.Exchange[Holding]{
		{
			Name: "twse",
			URL:  "https://www.twse.com.tw/rwd/zh/fund/MI_QFIIS",
			Query: func(d time.Time) url.Values {
				return url.Values{"date": {d.Format("20060102")}, "selectType": {"ALLBUT0999"}, "response": {"json"}}
			},
			Parse: func(b []byte, d time.Time) ([]Holding, error) {
				return parseHoldingReport(b, d, "證券代號", "發行股數", "全體外資及陸資持有股數",
					"全體外資及陸資持股比率", "外資及陸資共用法令投資上限比率")
			},
		},
		{
			Name: "tpex",
			URL:  "https://www.tpex.org.tw/web/stock/3insti/qfii/qfii_result.php",
			Query: func(d time.Time) url.Values {
				return url.Values{"l": {"zh-tw"}, "o": {"json"}, "d": {dataset.ROCDate(d)}}
			},
			Parse: func(b []byte, d time.Time) ([]Holding, error) {
				return parseHoldingReport(b, d, "代號", "發行股數(A)", "僑外資及陸資持有股數(C)",
					"僑外資及陸資持股比率(E=C/A)", "法令投資上限比率(F)")
			},
		},
	},
	Save: func(db *sql.DB, rows []Holding) error {
		return dataset.Upsert(db, `INSERT OR REPLACE INTO tw_foreign_holdings
			(market, ticker, date, shares_issued, foreign_shares, foreign_pct, limit_pct)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, rows, func(h Holding) []any {
			return []any{model.MarketTW, h.Ticker, h.Date.Format("2006-01-02"),
				h.SharesIssued, h.ForeignShares, h.ForeignPct, h.LimitPct}
		})
	},
}

func parseHoldingFinMind(body []byte) ([]Holding, error) {
	data, err := dataset.FinMindData[struct {
		Date          string  `json:"date"`
		StockID       string  `json:"stock_id"`
		SharesIssued  int64   `json:"NumberOfSharesIssued"`
		ForeignShares int64   `json:"ForeignInvestmentShares"`
		ForeignPct    float64 `json:"ForeignInvestmentSharesRatio"`
		LimitPct      float64 `json:"ForeignInvestmentUpperLimitRatio"`
	}](body)
	if err != nil {
		return nil, err
	}
	out := make([]Holding, 0, len(data))
	for _, d := range data {
		date, err := time.Parse("2006-01-02", d.Date)
		if err != nil {
			return nil, fmt.Errorf("bad date %q: %w", d.Date, err)
		}
		out = append(out, Holding{Ticker: strings.TrimSpace(d.StockID), Date: date,
			SharesIssued: d.SharesIssued, ForeignShares: d.ForeignShares,
			ForeignPct: &d.ForeignPct, LimitPct: positive(d.LimitPct)})
	}
	return out, nil
}

func parseHoldingReport(body []byte, day time.Time, want ...string) ([]Holding, error) {
	t, err := reportTable(body, day, want[0])
	if t == nil || err != nil {
		return nil, err
	}
	cols, err := dataset.Columns(t.Fields, want...)
	if err != nil {
		return nil, err
	}
	date := dataset.Date(day)
	out := make([]Holding, 0, len(t.Data))
	for n, cells := range t.Data {
		if len(cells) <= maxOf(cols) {
			return nil, fmt.Errorf("row %d has %d cells", n, len(cells))
		}
		h := Holding{Ticker: strings.TrimSpace(string(cells[cols[0]])), Date: date}
		if h.Ticker == "" {
			continue
		}
		if h.SharesIssued, err = dataset.Int(string(cells[cols[1]])); err != nil {
			return nil, fmt.Errorf("%s: %w", h.Ticker, err)
		}
		if h.ForeignShares, err = dataset.Int(string(cells[cols[2]])); err != nil {
			return nil, fmt.Errorf("%s: %w", h.Ticker, err)
		}
		for i, dst := range []**float64{&h.ForeignPct, &h.LimitPct} {
			f, ok, err := dataset.Float(string(cells[cols[3+i]]))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", h.Ticker, err)
			}
			if ok {
				*dst = &f
			}
		}
		out = append(out, h)
	}
	return out, nil
}

// ---- shared ----

// reportTable finds the per-stock table in an exchange report. TWSE puts
// fields and data at the top level; TPEx (and newer TWSE pages) under
// "tables". A report dated another day than asked, or with no rows, is no
// data (nil, nil).
func reportTable(body []byte, day time.Time, tickerCol string) (*dataset.Table, error) {
	var r struct {
		Date string `json:"date"`
		dataset.Table
		Tables []dataset.Table `json:"tables"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode report: %w", err)
	}
	if r.Date != "" && r.Date != day.Format("20060102") {
		return nil, nil
	}
	for _, t := range append([]dataset.Table{r.Table}, r.Tables...) {
		if len(t.Data) == 0 {
			continue
		}
		for _, f := range t.Fields {
			if strings.TrimSpace(f) == tickerCol {
				return &t, nil
			}
		}
	}
	return nil, nil
}

func positive(f float64) *float64 {
	if f <= 0 {
		return nil
	}
	return &f
}

func maxOf(xs []int) int {
	m := 0
	for _, x := range xs {
		m = max(m, x)
	}
	return m
}
