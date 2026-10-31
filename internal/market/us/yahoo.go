package us

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
	_ "time/tzdata" // America/New_York on machines without a zoneinfo database
)

const (
	yahooURL  = "https://query1.finance.yahoo.com/v8/finance/chart/"
	userAgent = "stocks_but_fr-radar/0.1 (+https://github.com/Antonio-1110/stocks_but_fr)"
)

// newYork is the US exchanges' time zone: bar dates and session ends use it.
var newYork = mustZone("America/New_York")

func mustZone(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// errRateLimited means Yahoo answered 429: stop this run's US fetches.
var errRateLimited = errors.New("yahoo: rate limited")

// Bar is one daily bar as Yahoo serves it. Close is split-adjusted only;
// AdjClose also adjusts for dividends.
type Bar struct {
	Date                   string // YYYY-MM-DD, New York
	Open, High, Low, Close float64
	AdjClose               float64
	Volume                 int64
}

// Chart is a parsed chart response.
type Chart struct {
	Symbol, Name, Exchange, Currency, Kind string
	Bars                                   []Bar
	Events                                 int // dividends + splits in the range
}

type chartJSON struct {
	Chart struct {
		Result []struct {
			Meta struct {
				Symbol           string `json:"symbol"`
				LongName         string `json:"longName"`
				ShortName        string `json:"shortName"`
				FullExchangeName string `json:"fullExchangeName"`
				Currency         string `json:"currency"`
				InstrumentType   string `json:"instrumentType"`
			} `json:"meta"`
			Timestamp []int64 `json:"timestamp"`
			Events    struct {
				Dividends map[string]json.RawMessage `json:"dividends"`
				Splits    map[string]json.RawMessage `json:"splits"`
			} `json:"events"`
			Indicators struct {
				Quote []struct {
					Open   []*float64 `json:"open"`
					High   []*float64 `json:"high"`
					Low    []*float64 `json:"low"`
					Close  []*float64 `json:"close"`
					Volume []*int64   `json:"volume"`
				} `json:"quote"`
				AdjClose []struct {
					AdjClose []*float64 `json:"adjclose"`
				} `json:"adjclose"`
			} `json:"indicators"`
		} `json:"result"`
		Error *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}

// parseChart reads a chart response. Days with no close are left out; a
// missing open, high or low falls back to the close.
func parseChart(b []byte) (Chart, error) {
	var j chartJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return Chart{}, fmt.Errorf("yahoo: %w", err)
	}
	if e := j.Chart.Error; e != nil {
		return Chart{}, fmt.Errorf("yahoo: %s: %s", e.Code, e.Description)
	}
	if len(j.Chart.Result) == 0 {
		return Chart{}, errors.New("yahoo: empty result")
	}
	r := j.Chart.Result[0]
	c := Chart{
		Symbol:   r.Meta.Symbol,
		Name:     r.Meta.LongName,
		Exchange: r.Meta.FullExchangeName,
		Currency: r.Meta.Currency,
		Kind:     r.Meta.InstrumentType,
		Events:   len(r.Events.Dividends) + len(r.Events.Splits),
	}
	if c.Name == "" {
		c.Name = r.Meta.ShortName
	}
	if len(r.Indicators.Quote) == 0 {
		return c, nil
	}
	q := r.Indicators.Quote[0]
	var adj []*float64
	if len(r.Indicators.AdjClose) > 0 {
		adj = r.Indicators.AdjClose[0].AdjClose
	}
	at := func(s []*float64, i int) float64 {
		if i < len(s) && s[i] != nil {
			return *s[i]
		}
		return 0
	}
	for i, ts := range r.Timestamp {
		b := Bar{
			Date:     time.Unix(ts, 0).In(newYork).Format("2006-01-02"),
			Open:     at(q.Open, i),
			High:     at(q.High, i),
			Low:      at(q.Low, i),
			Close:    at(q.Close, i),
			AdjClose: at(adj, i),
		}
		if b.Close <= 0 {
			continue
		}
		if i < len(q.Volume) && q.Volume[i] != nil {
			b.Volume = *q.Volume[i]
		}
		for _, p := range []*float64{&b.Open, &b.High, &b.Low, &b.AdjClose} {
			if *p <= 0 {
				*p = b.Close
			}
		}
		// The same session can come twice (a live bar next to the day's); keep the last.
		if n := len(c.Bars); n > 0 && c.Bars[n-1].Date == b.Date {
			c.Bars[n-1] = b
			continue
		}
		c.Bars = append(c.Bars, b)
	}
	return c, nil
}

// yahoo is a small, polite chart client: one request at a time, spaced out.
type yahoo struct {
	base     string
	http     *http.Client
	interval time.Duration
	last     time.Time
}

func newYahoo() *yahoo {
	return &yahoo{base: yahooURL, http: &http.Client{Timeout: 30 * time.Second}, interval: 1500 * time.Millisecond}
}

// chart fetches daily bars for symbol from `from` (inclusive) to now, with
// dividend and split events.
func (y *yahoo) chart(ctx context.Context, symbol string, from time.Time) (Chart, error) {
	if wait := y.interval - time.Since(y.last); wait > 0 {
		select {
		case <-ctx.Done():
			return Chart{}, ctx.Err()
		case <-time.After(wait):
		}
	}
	y.last = time.Now()
	v := url.Values{}
	v.Set("period1", fmt.Sprint(from.Unix()))
	v.Set("period2", fmt.Sprint(time.Now().Add(24*time.Hour).Unix()))
	v.Set("interval", "1d")
	v.Set("events", "div,split")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, y.base+url.PathEscape(symbol)+"?"+v.Encode(), nil)
	if err != nil {
		return Chart{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := y.http.Do(req)
	if err != nil {
		return Chart{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return Chart{}, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return Chart{}, errRateLimited
	}
	// A 404 still carries Yahoo's JSON error ("symbol may be delisted").
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return Chart{}, fmt.Errorf("yahoo %s: HTTP %d", symbol, resp.StatusCode)
	}
	return parseChart(body)
}
