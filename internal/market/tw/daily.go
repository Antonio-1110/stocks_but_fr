package tw

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/model"
)

// TPEx publishes one file per trading day with every OTC stock's OHLCV, so
// a backfill from 2010 is about 4,100 requests however many stocks there
// are, instead of three per stock on FinMind. (TWSE has the same kind of
// file, MI_INDEX, but blocks cloud addresses after a request or two.)

// errBlocked means the exchange answered with its "FOR SECURITY REASONS"
// page. TWSE blocks an address for a while after a burst of requests, so the
// rest of the run leaves that exchange alone.
var errBlocked = errors.New("blocked by the exchange's firewall")

// exTable is one table of a TWSE or TPEx JSON report. Cells are strings in
// practice; numbers are accepted too.
type exTable struct {
	Title  string   `json:"title"`
	Fields []string `json:"fields"`
	Data   [][]any  `json:"data"`
}

// column returns the index of the first field named one of names.
func (t exTable) column(names ...string) int {
	for _, n := range names {
		for i, f := range t.Fields {
			if strings.TrimSpace(f) == n {
				return i
			}
		}
	}
	return -1
}

func cell(row []any, i int) string {
	if i < 0 || i >= len(row) {
		return ""
	}
	switch v := row[i].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// number parses "1,234.50". Empty, "--", "---" and similar mean no value.
func number(s string) (float64, bool) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if s == "" || strings.Trim(s, "-") == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

// dailyColumns names one exchange's columns, most likely name first.
type dailyColumns struct {
	code, open, high, low, close, volume, value []string
}

// tpexColumns are the TPEx daily quote columns. Names carry stray spaces
// and changed between 2010 and 2024, so columns are found by trimmed name.
var tpexColumns = dailyColumns{
	code: []string{"代號"}, open: []string{"開盤"}, high: []string{"最高"}, low: []string{"最低"},
	close: []string{"收盤"}, volume: []string{"成交股數"}, value: []string{"成交金額(元)"},
}

// parseDaily reads one day's report. A day without trading comes back
// without the quotes table, which is no rows and no error. Rows with no
// trade that day, and codes that are not stocks or ETFs, are skipped.
func parseDaily(body []byte, day time.Time, cols dailyColumns) ([]model.Price, error) {
	if bytes.Contains(body, []byte("FOR SECURITY REASONS")) {
		return nil, errBlocked
	}
	var doc struct {
		Tables []exTable `json:"tables"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode: %w (%.120q)", err, body)
	}
	for _, t := range doc.Tables {
		code, cl := t.column(cols.code...), t.column(cols.close...)
		if code < 0 || cl < 0 {
			continue
		}
		op, hi, lo := t.column(cols.open...), t.column(cols.high...), t.column(cols.low...)
		vol, val := t.column(cols.volume...), t.column(cols.value...)
		if op < 0 || hi < 0 || lo < 0 || vol < 0 || val < 0 {
			return nil, fmt.Errorf("quotes table %q is missing a column: %v", t.Title, t.Fields)
		}
		var out []model.Price
		for _, row := range t.Data {
			ticker := cell(row, code)
			c, ok := number(cell(row, cl))
			if !ok || c <= 0 || !tickerRe.MatchString(ticker) {
				continue
			}
			o, _ := number(cell(row, op))
			h, _ := number(cell(row, hi))
			l, _ := number(cell(row, lo))
			v, _ := number(cell(row, vol))
			m, _ := number(cell(row, val))
			out = append(out, model.Price{Market: model.MarketTW, Ticker: ticker, Date: day,
				Open: o, High: h, Low: l, Close: c, AdjClose: c, Volume: int64(v), Turnover: m})
		}
		return out, nil
	}
	return nil, nil
}
