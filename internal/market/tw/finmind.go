// Package tw collects the Taiwan universe (TWSE + TPEx, including delisted
// stocks) and daily prices from FinMind.
package tw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

const (
	finmindURL = "https://api.finmindtrade.com/api/v4/data"
	userAgent  = "stocks_but_fr-radar/0.1 (+https://github.com/Antonio-1110/stocks_but_fr)"

	// FinMind allows 600 requests an hour with a token (300 without). One
	// request every 6.5s stays under that; maxRequestsPerRun keeps one run
	// near an hour. A backfill resumes on the next run.
	requestInterval   = 6500 * time.Millisecond
	maxRequestsPerRun = 540
)

// ErrStop means the run should stop calling FinMind: the hourly limit was
// hit or this run's request budget is spent. Work so far is kept.
var ErrStop = errors.New("finmind: request budget spent or rate limited")

// FinMind is a small, polite client for the FinMind v4 data API.
type FinMind struct {
	BaseURL  string
	Token    string
	Interval time.Duration // minimum gap between requests
	Budget   int           // requests left this run
	HTTP     *http.Client

	last time.Time
}

// NewFinMind reads the token from FINMIND_TOKEN.
func NewFinMind() *FinMind {
	return &FinMind{
		BaseURL:  finmindURL,
		Token:    os.Getenv("FINMIND_TOKEN"),
		Interval: requestInterval,
		Budget:   maxRequestsPerRun,
		HTTP:     &http.Client{Timeout: 60 * time.Second},
	}
}

type envelope[T any] struct {
	Msg    string `json:"msg"`
	Status int    `json:"status"`
	Data   []T    `json:"data"`
}

// Fetch gets one dataset. params are FinMind query parameters such as
// data_id and start_date.
func Fetch[T any](ctx context.Context, c *FinMind, dataset string, params map[string]string) ([]T, error) {
	if c.Budget <= 0 {
		return nil, ErrStop
	}
	if wait := c.Interval - time.Since(c.last); wait > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	c.Budget--
	c.last = time.Now()

	q := url.Values{"dataset": {dataset}}
	for k, v := range params {
		q.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%s: %w (HTTP %d)", dataset, ErrStop, resp.StatusCode)
	}
	return parseEnvelope[T](dataset, body)
}

func parseEnvelope[T any](dataset string, body []byte) ([]T, error) {
	var env envelope[T]
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("%s: decode: %w", dataset, err)
	}
	switch {
	case env.Status == http.StatusPaymentRequired:
		return nil, fmt.Errorf("%s: %w: %s", dataset, ErrStop, env.Msg)
	case env.Status != http.StatusOK:
		return nil, fmt.Errorf("%s: status %d: %s", dataset, env.Status, env.Msg)
	}
	return env.Data, nil
}
