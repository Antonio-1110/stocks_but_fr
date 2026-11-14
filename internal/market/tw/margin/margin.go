// Package margin collects Taiwan margin trading (融資) and short selling (融券)
// per stock per day (issue #84): how many lots were bought on margin, sold
// short, and the balances left open after the day. Counts are in lots (張).
//
// History comes from FinMind's TaiwanStockMarginPurchaseShortSale; recent
// days from TWSE's MI_MARGN and TPEx's margin balance report, through the
// shared engine in package dataset.
package margin

import (
	"context"
	"database/sql"
	"encoding/json"
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

// Row is one stock's margin and short activity on one day, in lots (張).
type Row struct {
	Ticker        string
	Date          time.Time
	MarginBuy     int64 // 融資買進
	MarginSell    int64 // 融資賣出
	MarginBalance int64 // 融資今日餘額
	ShortSell     int64 // 融券賣出 (new shorts)
	ShortBuy      int64 // 融券買進 (covers)
	ShortBalance  int64 // 融券今日餘額
	Offset        int64 // 資券互抵: margin buys and shorts netted the same day (day trades)
	Note          string
}

// Schema is the table the backtester reads.
const Schema = `CREATE TABLE IF NOT EXISTS tw_margin (
	market         TEXT NOT NULL,
	ticker         TEXT NOT NULL,
	date           TEXT NOT NULL,
	margin_buy     INTEGER,  -- lots (張) bought on margin (融資買進)
	margin_sell    INTEGER,  -- lots of margin positions sold (融資賣出)
	margin_balance INTEGER,  -- margin lots open after the day (融資餘額)
	short_sell     INTEGER,  -- lots sold short (融券賣出)
	short_buy      INTEGER,  -- lots bought back to cover (融券買進)
	short_balance  INTEGER,  -- short lots open after the day (融券餘額)
	offset_lots    INTEGER,  -- 資券互抵 (same-day margin/short offset)
	note           TEXT NOT NULL DEFAULT '',  -- exchange flags, e.g. O stop margin, X stop short
	PRIMARY KEY (market, ticker, date)
)`

// Dataset describes margin data for the engine.
var Dataset = dataset.Dataset[Row]{
	Name:         "margin",
	Schema:       Schema,
	FinMind:      "TaiwanStockMarginPurchaseShortSale",
	ParseFinMind: parseFinMind,
	Exchanges: []dataset.Exchange[Row]{
		{
			Name: "twse",
			URL:  "https://www.twse.com.tw/rwd/zh/marginTrading/MI_MARGN",
			Query: func(d time.Time) url.Values {
				return url.Values{"date": {d.Format("20060102")}, "selectType": {"ALL"}, "response": {"json"}}
			},
			Parse: parseTWSE,
		},
		{
			Name: "tpex",
			URL:  "https://www.tpex.org.tw/web/stock/margin_trading/margin_balance/margin_bal_result.php",
			Query: func(d time.Time) url.Values {
				return url.Values{"l": {"zh-tw"}, "o": {"json"}, "d": {dataset.ROCDate(d)}}
			},
			Parse: parseTPEx,
		},
	},
	Save: save,
}

// Collect is the `radar collect` step.
func Collect(ctx context.Context, cfg config.Config, st *store.Store) error {
	start, err := time.Parse("2006-01-02", cfg.Run.HistoryStart)
	if err != nil {
		return fmt.Errorf("run.history_start: %w", err)
	}
	fm := tw.ForStep("tw-margin", 1) // tw-valuation runs after this step
	return dataset.New(Dataset, fm).Run(ctx, st, start)
}

func save(db *sql.DB, rows []Row) error {
	return dataset.Upsert(db, `INSERT OR REPLACE INTO tw_margin
		(market, ticker, date, margin_buy, margin_sell, margin_balance,
		 short_sell, short_buy, short_balance, offset_lots, note)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, rows, func(r Row) []any {
		return []any{model.MarketTW, r.Ticker, r.Date.Format("2006-01-02"),
			r.MarginBuy, r.MarginSell, r.MarginBalance,
			r.ShortSell, r.ShortBuy, r.ShortBalance, r.Offset, r.Note}
	})
}

type finmindRow struct {
	Date          string `json:"date"`
	StockID       string `json:"stock_id"`
	MarginBuy     int64  `json:"MarginPurchaseBuy"`
	MarginSell    int64  `json:"MarginPurchaseSell"`
	MarginBalance int64  `json:"MarginPurchaseTodayBalance"`
	ShortSell     int64  `json:"ShortSaleSell"`
	ShortBuy      int64  `json:"ShortSaleBuy"`
	ShortBalance  int64  `json:"ShortSaleTodayBalance"`
	Offset        int64  `json:"OffsetLoanAndShort"`
	Note          string `json:"Note"`
}

func parseFinMind(body []byte) ([]Row, error) {
	data, err := dataset.FinMindData[finmindRow](body)
	if err != nil {
		return nil, err
	}
	out := make([]Row, 0, len(data))
	for _, d := range data {
		date, err := time.Parse("2006-01-02", d.Date)
		if err != nil {
			return nil, fmt.Errorf("bad date %q: %w", d.Date, err)
		}
		out = append(out, Row{
			Ticker: strings.TrimSpace(d.StockID), Date: date,
			MarginBuy: d.MarginBuy, MarginSell: d.MarginSell, MarginBalance: d.MarginBalance,
			ShortSell: d.ShortSell, ShortBuy: d.ShortBuy, ShortBalance: d.ShortBalance,
			Offset: d.Offset, Note: strings.TrimSpace(d.Note),
		})
	}
	return out, nil
}

// report is both exchanges' envelope: a date and a list of tables.
type report struct {
	Stat   string          `json:"stat"`
	Date   string          `json:"date"`
	Tables []dataset.Table `json:"tables"`
}

func decode(body []byte, day time.Time, name string) (*report, error) {
	var r report
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	// A report for another day than asked (or none) means no data yet.
	if r.Date != "" && r.Date != day.Format("20060102") {
		return nil, nil
	}
	return &r, nil
}

// parseTWSE reads MI_MARGN. Its first table is a market summary; the
// per-stock table is the one with a 代號 column. Margin and short columns
// share names (買進, 賣出, 今日餘額), margin first.
func parseTWSE(body []byte, day time.Time) ([]Row, error) {
	r, err := decode(body, day, "TWSE MI_MARGN")
	if r == nil || err != nil {
		return nil, err
	}
	for _, t := range r.Tables {
		if len(t.Data) == 0 || !hasField(t.Fields, "代號") {
			continue
		}
		cols, err := dataset.Columns(t.Fields, "代號",
			"買進", "賣出", "今日餘額", // 融資
			"買進", "賣出", "今日餘額", // 融券
			"資券互抵", "註記")
		if err != nil {
			return nil, err
		}
		// col order: ticker, marginBuy, marginSell, marginBal, shortBuy, shortSell, shortBal, offset, note
		return parseRows(t, day, [9]int{cols[0], cols[1], cols[2], cols[3], cols[5], cols[4], cols[6], cols[7], cols[8]})
	}
	return nil, nil
}

// parseTPEx reads TPEx's margin balance report.
func parseTPEx(body []byte, day time.Time) ([]Row, error) {
	r, err := decode(body, day, "TPEx margin")
	if r == nil || err != nil {
		return nil, err
	}
	if len(r.Tables) == 0 || len(r.Tables[0].Data) == 0 {
		return nil, nil
	}
	t := r.Tables[0]
	cols, err := dataset.Columns(t.Fields, "代號", "資買", "資賣", "資餘額", "券賣", "券買", "券餘額", "資券相抵(張)", "備註")
	if err != nil {
		return nil, err
	}
	return parseRows(t, day, [9]int(cols))
}

// parseRows reads a table given column indexes in Row's order: ticker,
// margin buy, sell, balance, short sell, buy, balance, offset, note.
func parseRows(t dataset.Table, day time.Time, c [9]int) ([]Row, error) {
	date := dataset.Date(day)
	out := make([]Row, 0, len(t.Data))
	for n, cells := range t.Data {
		if len(cells) <= maxOf(c[:]) {
			return nil, fmt.Errorf("row %d has %d cells", n, len(cells))
		}
		r := Row{Ticker: strings.TrimSpace(string(cells[c[0]])), Date: date, Note: strings.TrimSpace(string(cells[c[8]]))}
		if r.Ticker == "" {
			continue
		}
		for i, dst := range []*int64{&r.MarginBuy, &r.MarginSell, &r.MarginBalance, &r.ShortSell, &r.ShortBuy, &r.ShortBalance, &r.Offset} {
			v, err := dataset.Int(string(cells[c[i+1]]))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", r.Ticker, err)
			}
			*dst = v
		}
		out = append(out, r)
	}
	return out, nil
}

func hasField(fields []string, name string) bool {
	for _, f := range fields {
		if strings.TrimSpace(f) == name {
			return true
		}
	}
	return false
}

func maxOf(xs []int) int {
	m := 0
	for _, x := range xs {
		m = max(m, x)
	}
	return m
}
