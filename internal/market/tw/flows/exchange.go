package flows

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/model"
)

func (c *Collector) fetchExchange(ctx context.Context, src string, day time.Time) ([]model.InstitutionalFlow, error) {
	var u string
	switch src {
	case srcTWSE:
		u = c.TWSEURL + "?" + url.Values{
			"date":       {day.Format("20060102")},
			"selectType": {"ALLBUT0999"}, // every stock and ETF, no warrants
			"response":   {"json"},
		}.Encode()
	case srcTPEx:
		u = c.TPExURL + "?" + url.Values{
			"type":     {"Daily"},
			"sect":     {"EW"}, // all securities
			"date":     {day.Format("2006/01/02")},
			"response": {"json"},
		}.Encode()
	default:
		return nil, fmt.Errorf("unknown source %q", src)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, body)
	}
	if src == srcTWSE {
		return parseTWSE(body, day)
	}
	return parseTPEx(body, day)
}

type table struct {
	Fields []string   `json:"fields"`
	Data   [][]string `json:"data"`
}

// parseTWSE reads the T86 report. A closed day comes back with a non-OK
// stat and no data, which is not an error.
func parseTWSE(body []byte, day time.Time) ([]model.InstitutionalFlow, error) {
	var r struct {
		Stat string `json:"stat"`
		table
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode TWSE T86: %w", err)
	}
	if len(r.Data) == 0 {
		return nil, nil
	}
	if !strings.EqualFold(r.Stat, "OK") {
		return nil, fmt.Errorf("TWSE stat %q", r.Stat)
	}
	return parseTable(r.table, day)
}

// parseTPEx reads TPEx's daily 三大法人 report. Its JSON holds a list of
// tables; the first one is the per-stock table.
func parseTPEx(body []byte, day time.Time) ([]model.InstitutionalFlow, error) {
	var r struct {
		Stat   string  `json:"stat"`
		Tables []table `json:"tables"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode TPEx: %w", err)
	}
	if len(r.Tables) == 0 || len(r.Tables[0].Data) == 0 {
		return nil, nil
	}
	if !strings.EqualFold(r.Stat, "OK") {
		return nil, fmt.Errorf("TPEx stat %q", r.Stat)
	}
	t := r.Tables[0]
	if _, err := findColumns(t.Fields); err != nil && len(t.Fields) == len(tpexLayout) {
		// Headers we can't classify (e.g. group names split out of the field
		// names) but the long-standing 24-column shape: go by position.
		t.Fields = tpexLayout
	}
	return parseTable(t, day)
}

// tpexLayout is the column order TPEx has used since 2018.
var tpexLayout = []string{
	"代號", "名稱",
	"外資及陸資(不含外資自營商)買進股數", "外資及陸資(不含外資自營商)賣出股數", "外資及陸資(不含外資自營商)買賣超股數",
	"外資自營商買進股數", "外資自營商賣出股數", "外資自營商買賣超股數",
	"外資及陸資買進股數", "外資及陸資賣出股數", "外資及陸資買賣超股數",
	"投信買進股數", "投信賣出股數", "投信買賣超股數",
	"自營商(自行買賣)買進股數", "自營商(自行買賣)賣出股數", "自營商(自行買賣)買賣超股數",
	"自營商(避險)買進股數", "自營商(避險)賣出股數", "自營商(避險)買賣超股數",
	"自營商買進股數", "自營商賣出股數", "自營商買賣超股數",
	"三大法人買賣超股數合計",
}

// column roles, found from header text so that the exchanges' column
// reshuffles over the years don't break parsing.
type columns struct {
	ticker        int
	foreignTotal  int   // 外資及陸資 net, dealers included
	foreignExDlr  int   // 外資 net excluding foreign-owned dealers
	foreignDealer int   // 外資自營商 net
	trust         int   // 投信 net
	dealerTotal   int   // 自營商 net
	dealerParts   []int // 自營商(自行買賣) and 自營商(避險) nets
}

func findColumns(fields []string) (columns, error) {
	c := columns{ticker: -1, foreignTotal: -1, foreignExDlr: -1, foreignDealer: -1, trust: -1, dealerTotal: -1}
	for i, f := range fields {
		f = strings.ReplaceAll(f, " ", "")
		if strings.Contains(f, "代號") {
			c.ticker = i
			continue
		}
		if !strings.Contains(f, "買賣超") {
			continue
		}
		switch {
		case strings.Contains(f, "三大法人"):
		case strings.Contains(f, "不含"): // 外(陸)資...(不含外資自營商)
			c.foreignExDlr = i
		case strings.Contains(f, "外資自營商"):
			c.foreignDealer = i
		case strings.Contains(f, "外"):
			c.foreignTotal = i
		case strings.Contains(f, "投信"):
			c.trust = i
		case strings.Contains(f, "自營商"):
			if strings.Contains(f, "自行") || strings.Contains(f, "避險") {
				c.dealerParts = append(c.dealerParts, i)
			} else {
				c.dealerTotal = i
			}
		}
	}
	switch {
	case c.ticker < 0:
		return c, fmt.Errorf("no ticker column in %q", fields)
	case c.foreignTotal < 0 && c.foreignExDlr < 0:
		return c, fmt.Errorf("no foreign net column in %q", fields)
	case c.trust < 0:
		return c, fmt.Errorf("no 投信 net column in %q", fields)
	case c.dealerTotal < 0 && len(c.dealerParts) == 0:
		return c, fmt.Errorf("no dealer net column in %q", fields)
	}
	return c, nil
}

func parseTable(t table, day time.Time) ([]model.InstitutionalFlow, error) {
	cols, err := findColumns(t.Fields)
	if err != nil {
		return nil, err
	}
	date := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	out := make([]model.InstitutionalFlow, 0, len(t.Data))
	for n, row := range t.Data {
		get := func(i int) (int64, error) {
			if i < 0 {
				return 0, nil
			}
			if i >= len(row) {
				return 0, fmt.Errorf("row %d has %d cells, want > %d", n, len(row), i)
			}
			return parseShares(row[i])
		}
		sum := func(idx ...int) (int64, error) {
			var s int64
			for _, i := range idx {
				v, err := get(i)
				if err != nil {
					return 0, err
				}
				s += v
			}
			return s, nil
		}
		if cols.ticker >= len(row) {
			return nil, fmt.Errorf("row %d has %d cells", n, len(row))
		}
		f := model.InstitutionalFlow{Market: model.MarketTW, Ticker: strings.TrimSpace(row[cols.ticker]), Date: date}
		if f.Ticker == "" {
			continue
		}
		if cols.foreignTotal >= 0 {
			f.ForeignNet, err = get(cols.foreignTotal)
		} else {
			f.ForeignNet, err = sum(cols.foreignExDlr, cols.foreignDealer)
		}
		if err != nil {
			return nil, err
		}
		if f.TrustNet, err = get(cols.trust); err != nil {
			return nil, err
		}
		if cols.dealerTotal >= 0 {
			f.DealerNet, err = get(cols.dealerTotal)
		} else {
			f.DealerNet, err = sum(cols.dealerParts...)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// parseShares reads "1,234,567" or "-1,234". Blank or "--" means zero.
func parseShares(s string) (int64, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if s == "" || s == "--" || s == "-" {
		return 0, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad share count %q", s)
	}
	return v, nil
}
