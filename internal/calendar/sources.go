package calendar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

const (
	userAgent = "stocks_but_fr-radar/0.1 (+https://github.com/Antonio-1110/stocks_but_fr)"

	// These pages change a few times a day at most; the workflow runs hourly.
	refetchAfter = 6 * time.Hour
	// The exchanges block clients that hit them faster than a few per 5s.
	requestGap = 3 * time.Second
	// MOPS lists conferences by year; ask for next year too once it is this close.
	lookahead = 90 * 24 * time.Hour
)

// errBlocked means TWSE answered with its "FOR SECURITY REASONS" page.
var errBlocked = errors.New("blocked by the exchange's firewall")

// Endpoints, exported as fields so tests can point them at a fake server.
type Collector struct {
	Client *http.Client
	Gap    time.Duration
	Now    func() time.Time

	TWSEExDivURL    string // 除權除息預告表
	TPExExDivURL    string
	TWSEMeetingsURL string // 股東常(臨時)會 彙總表
	TPExMeetingsURL string
	MOPSConfURL     string // 法人說明會一覽表 (POST)
}

func NewCollector() *Collector {
	return &Collector{
		Client:          &http.Client{Timeout: 60 * time.Second},
		Gap:             requestGap,
		Now:             time.Now,
		TWSEExDivURL:    "https://www.twse.com.tw/rwd/zh/exRight/TWT48U?response=json",
		TPExExDivURL:    "https://www.tpex.org.tw/openapi/v1/tpex_exright_prepost",
		TWSEMeetingsURL: "https://openapi.twse.com.tw/v1/opendata/t187ap41_L",
		TPExMeetingsURL: "https://www.tpex.org.tw/openapi/v1/t187ap41_O",
		MOPSConfURL:     "https://mopsov.twse.com.tw/mops/web/ajax_t100sb02_1",
	}
}

// Collect is the `radar collect` step.
func Collect(ctx context.Context, _ config.Config, st *store.Store) error {
	return NewCollector().Run(ctx, st)
}

type source struct {
	name  string
	fetch func(context.Context) ([]Event, error)
}

func (c *Collector) sources() []source {
	now := c.Now().In(taipei)
	srcs := []source{
		{"twse-exdiv", func(ctx context.Context) ([]Event, error) {
			b, err := c.get(ctx, c.TWSEExDivURL)
			if err != nil {
				return nil, err
			}
			return parseTWSEExDiv(b)
		}},
		{"tpex-exdiv", func(ctx context.Context) ([]Event, error) {
			b, err := c.get(ctx, c.TPExExDivURL)
			if err != nil {
				return nil, err
			}
			return parseTPExExDiv(b)
		}},
		{"twse-meetings", func(ctx context.Context) ([]Event, error) {
			b, err := c.get(ctx, c.TWSEMeetingsURL)
			if err != nil {
				return nil, err
			}
			return parseMeetings(b)
		}},
		{"tpex-meetings", func(ctx context.Context) ([]Event, error) {
			b, err := c.get(ctx, c.TPExMeetingsURL)
			if err != nil {
				return nil, err
			}
			return parseMeetings(b)
		}},
	}
	years := []int{now.Year()}
	if y := now.Add(lookahead).Year(); y != now.Year() {
		years = append(years, y)
	}
	for _, board := range []string{"sii", "otc"} { // listed, OTC
		srcs = append(srcs, source{"mops-conferences-" + board, func(ctx context.Context) ([]Event, error) {
			var all []Event
			for i, y := range years {
				if i > 0 {
					c.pause(ctx)
				}
				b, err := c.post(ctx, c.MOPSConfURL, url.Values{
					"encodeURIComponent": {"1"}, "step": {"1"}, "firstin": {"true"}, "off": {"1"},
					"TYPEK": {board}, "year": {strconv.Itoa(y - 1911)}, "month": {""}, "co_id": {""},
				})
				if err != nil {
					return nil, err
				}
				evs := parseConferences(b)
				if len(evs) == 0 {
					log.Printf("calendar: MOPS %s %d returned no conferences; start of answer: %.300q", board, y, b)
				}
				all = append(all, evs...)
			}
			return all, nil
		}})
	}
	return srcs
}

// Run fetches every source that is due. A failing source is logged and
// skipped; the run fails only if every due source failed.
func (c *Collector) Run(ctx context.Context, st *store.Store) error {
	if err := migrate(st.DB); err != nil {
		return err
	}
	now := c.Now()
	today := dayOf(now.In(taipei))
	due, failed, requests := 0, 0, 0
	for _, s := range c.sources() {
		last, err := lastFetched(st.DB, s.name)
		if err != nil {
			return err
		}
		if now.Sub(last) < refetchAfter {
			continue
		}
		due++
		if requests > 0 {
			c.pause(ctx)
		}
		requests++
		evs, err := s.fetch(ctx)
		if err == nil && len(evs) == 0 {
			// An empty answer is far more often a changed or blocked page than
			// a quiet market: keep what we had and try again next run.
			log.Printf("calendar: %s: no events parsed, keeping stored rows", s.name)
			continue
		}
		if err == nil {
			err = replaceSource(st.DB, s.name, today, evs)
		}
		if err != nil {
			log.Printf("calendar: %s: %v", s.name, err)
			failed++
			continue
		}
		if err := markFetched(st.DB, s.name, now); err != nil {
			return err
		}
		log.Printf("calendar: %s: %d events", s.name, len(evs))
	}
	if due > 0 && failed == due {
		return fmt.Errorf("all %d calendar sources failed", due)
	}
	return nil
}

func (c *Collector) pause(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(c.Gap):
	}
}

func (c *Collector) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

func (c *Collector) post(ctx context.Context, u string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", strings.Replace(u, "ajax_", "", 1))
	return c.do(req)
}

func (c *Collector) do(req *http.Request) ([]byte, error) {
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
	if bytes.Contains(body, []byte("FOR SECURITY REASONS")) {
		return nil, errBlocked
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, body)
	}
	return body, nil
}

// --- parsers -----------------------------------------------------------

var digitRuns = regexp.MustCompile(`\d+`)

// rocDate reads a Republic of China calendar date in any of the forms these
// sources use: "115年10月08日", "115/10/08" or "1151008".
func rocDate(s string) (time.Time, bool) {
	parts := digitRuns.FindAllString(s, -1)
	if len(parts) == 1 && len(parts[0]) == 7 {
		p := parts[0]
		parts = []string{p[:3], p[3:5], p[5:]}
	}
	if len(parts) != 3 {
		return time.Time{}, false
	}
	y, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	d, _ := strconv.Atoi(parts[2])
	if y < 1 || m < 1 || m > 12 || d < 1 || d > 31 {
		return time.Time{}, false
	}
	t := time.Date(y+1911, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	if t.Day() != d {
		return time.Time{}, false
	}
	return t, true
}

// num reads a decimal field; anything else (blank, "尚未公告", HTML) is 0, false.
func num(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(s, ",", "")), 64)
	return f, err == nil
}

func trimNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// dividendDetail describes what a holder gets: cash per share, bonus shares
// per share, and any rights offering.
func dividendDetail(cash, stock, subRatio, subPrice string) string {
	var parts []string
	if f, ok := num(cash); ok && f > 0 {
		parts = append(parts, "現金 "+trimNum(f)+" 元")
	} else if strings.Contains(cash, "待公告") {
		parts = append(parts, "配息金額待公告")
	}
	if f, ok := num(stock); ok && f > 0 {
		parts = append(parts, "配股 "+trimNum(round(f, 4))+" 股/股")
	}
	if f, ok := num(subRatio); ok && f > 0 {
		s := "現增 " + trimNum(round(f, 4)) + " 股/股"
		if p, ok := num(subPrice); ok && p > 0 {
			s += " @" + trimNum(p)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " · ")
}

func round(f float64, places int) float64 {
	p := 1.0
	for range places {
		p *= 10
	}
	return float64(int64(f*p+0.5)) / p
}

func exDivTitle(kind string) string {
	switch strings.TrimPrefix(kind, "除") {
	case "息":
		return "除息"
	case "權":
		return "除權"
	case "權息":
		return "除權息"
	}
	return "除權息"
}

// parseTWSEExDiv reads TWSE's TWT48U preview (rwd JSON: fields + data rows).
func parseTWSEExDiv(body []byte) ([]Event, error) {
	var r struct {
		Stat   string     `json:"stat"`
		Fields []string   `json:"fields"`
		Data   [][]string `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.Stat != "OK" {
		return nil, fmt.Errorf("stat %q", r.Stat)
	}
	col := map[string]int{}
	for i, f := range r.Fields {
		col[f] = i
	}
	need := []string{"除權除息日期", "股票代號", "名稱", "除權息", "無償配股率", "現金增資配股率", "現金增資認購價", "現金股利"}
	for _, f := range need {
		if _, ok := col[f]; !ok {
			return nil, fmt.Errorf("TWT48U is missing column %q", f)
		}
	}
	var out []Event
	for _, row := range r.Data {
		if len(row) < len(r.Fields) {
			continue
		}
		d, ok := rocDate(row[col["除權除息日期"]])
		if !ok {
			continue
		}
		out = append(out, Event{
			Market: model.MarketTW, Ticker: strings.TrimSpace(row[col["股票代號"]]), Name: strings.TrimSpace(row[col["名稱"]]),
			Date: d, Kind: KindExDividend, Title: exDivTitle(row[col["除權息"]]),
			Detail: dividendDetail(row[col["現金股利"]], row[col["無償配股率"]], row[col["現金增資配股率"]], row[col["現金增資認購價"]]),
		})
	}
	return out, nil
}

// parseTPExExDiv reads TPEx OpenAPI tpex_exright_prepost.
func parseTPExExDiv(body []byte) ([]Event, error) {
	var rows []struct {
		Date     string `json:"ExRrightsExDividendDate"`
		Code     string `json:"SecuritiesCompanyCode"`
		Name     string `json:"CompanyName"`
		Kind     string `json:"ExRrightsExDividend"`
		Stock    string `json:"StockDividendRatio"`
		SubRatio string `json:"SubscriptionRatioToNewSharesIssued"`
		SubPrice string `json:"SubscriptionPricePerShare"`
		Cash     string `json:"CashDividend"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, err
	}
	var out []Event
	for _, r := range rows {
		d, ok := rocDate(r.Date)
		if !ok || r.Code == "" {
			continue
		}
		out = append(out, Event{
			Market: model.MarketTW, Ticker: strings.TrimSpace(r.Code), Name: strings.TrimSpace(r.Name),
			Date: d, Kind: KindExDividend, Title: exDivTitle(r.Kind),
			Detail: dividendDetail(r.Cash, r.Stock, r.SubRatio, r.SubPrice),
		})
	}
	return out, nil
}

// parseMeetings reads the MOPS shareholder meeting summary (t187ap41), which
// TWSE (_L) and TPEx (_O) publish with the same Chinese field names.
func parseMeetings(body []byte) ([]Event, error) {
	var rows []map[string]string
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, err
	}
	var out []Event
	for _, r := range rows {
		d, ok := rocDate(r["開會日期"])
		code := strings.TrimSpace(r["公司代號"])
		if !ok || code == "" {
			continue
		}
		title := "股東常會"
		if strings.Contains(r["股東常(臨時)會"], "臨時") {
			title = "股東臨時會"
		}
		var detail []string
		if strings.TrimSpace(r["是否改選董監"]) == "是" {
			detail = append(detail, "改選董監")
		}
		if p := strings.TrimSpace(r["開會地點"]); p != "" {
			detail = append(detail, p)
		}
		out = append(out, Event{
			Market: model.MarketTW, Ticker: code, Name: strings.TrimSpace(r["公司名稱"]),
			Date: d, Kind: KindMeeting, Title: title, Detail: strings.Join(detail, " · "),
		})
	}
	return out, nil
}

var (
	reRow    = regexp.MustCompile(`(?is)<tr\b[^>]*>(.*?)</tr>`)
	reCell   = regexp.MustCompile(`(?is)<td\b[^>]*>(.*?)</td>`)
	reBreak  = regexp.MustCompile(`(?i)<br\s*/?>`)
	reTag    = regexp.MustCompile(`(?s)<[^>]*>`)
	reTicker = regexp.MustCompile(`^[0-9]{4}[0-9A-Z]{0,2}$`)
	reClock  = regexp.MustCompile(`^\d{1,2}:\d{2}`)
)

func cellText(s string) string {
	s = reBreak.ReplaceAllString(s, " ")
	s = html.UnescapeString(reTag.ReplaceAllString(s, ""))
	return strings.Join(strings.Fields(s), " ")
}

// parseConferences reads MOPS 法人說明會一覽表 (t100sb02_1). It keys on the
// shape of a data row (ticker, name, ROC date, time, place, summary) rather
// than on exact markup, and skips anything else.
func parseConferences(body []byte) []Event {
	var out []Event
	for _, m := range reRow.FindAllSubmatch(body, -1) {
		cells := reCell.FindAllSubmatch(m[1], -1)
		if len(cells) < 6 {
			continue
		}
		txt := make([]string, 6)
		for i := range txt {
			txt[i] = cellText(string(cells[i][1]))
		}
		if !reTicker.MatchString(txt[0]) {
			continue
		}
		dates := strings.Split(txt[2], "至")
		start, ok := rocDate(dates[0])
		if !ok {
			continue
		}
		var end time.Time
		if len(dates) == 2 {
			if e, ok := rocDate(dates[1]); ok && e.After(start) {
				end = e
			}
		}
		title := "法說會"
		if clock := reClock.FindString(txt[3]); clock != "" {
			title += " " + clock
		}
		var detail []string
		for _, s := range []string{txt[4], txt[5]} {
			if s != "" {
				detail = append(detail, clip(s, 160))
			}
		}
		out = append(out, Event{
			Market: model.MarketTW, Ticker: txt[0], Name: txt[1],
			Date: start, EndDate: end, Kind: KindConference, Title: title,
			Detail: strings.Join(detail, " · "),
		})
	}
	return out
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func dayOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
