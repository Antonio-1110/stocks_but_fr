// Package tw collects the Taiwan universe (TWSE + TPEx, including delisted
// stocks) and daily prices from FinMind.
package tw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

const (
	finmindURL = "https://api.finmindtrade.com/api/v4/data"
	userAgent  = "stocks_but_fr-radar/0.1 (+https://github.com/Antonio-1110/stocks_but_fr)"

	// FinMind allows 600 requests an hour with a token and 300 without, and
	// the limit is per token (or address), not per dataset. One request every
	// 6.5s (13s anonymous) stays under it; the run budget keeps one run near
	// an hour. Backfills resume on the next run.
	intervalToken  = 6500 * time.Millisecond
	intervalAnon   = 13 * time.Second
	runBudgetToken = 540
	runBudgetAnon  = 270
)

// ErrStop means the run should stop calling FinMind: the hourly limit was
// hit or this run's request budget is spent. Work so far is kept.
var ErrStop = errors.New("finmind: request budget spent or rate limited")

// FinMind is a small, polite client for the FinMind v4 data API. A client
// made by Allot draws on its parent's pacing and budget, so several steps
// can share one hourly limit.
type FinMind struct {
	BaseURL  string
	Token    string
	Interval time.Duration // minimum gap between requests
	Budget   int           // requests this client may still make
	HTTP     *http.Client

	parent *FinMind
	last   time.Time
}

// NewFinMind reads the token from FINMIND_TOKEN and sets the pacing and run
// budget for the limit that applies.
func NewFinMind() *FinMind {
	c := &FinMind{
		BaseURL:  finmindURL,
		Token:    os.Getenv("FINMIND_TOKEN"),
		Interval: intervalToken,
		Budget:   runBudgetToken,
		HTTP:     &http.Client{Timeout: 60 * time.Second},
	}
	if c.Token == "" {
		c.Interval, c.Budget = intervalAnon, runBudgetAnon
	}
	return c
}

var (
	sharedOnce sync.Once
	shared     *FinMind
)

// ForStep returns the FinMind client for one `radar collect` step. Every
// Taiwan step draws on one client per process, because the hourly limit is
// per token. The budget left is split evenly between this step and the
// `later` FinMind steps still to run after it, so the first step can't spend
// the whole hour; what a step leaves unused passes to the steps after it.
func ForStep(name string, later int) *FinMind {
	sharedOnce.Do(func() {
		shared = NewFinMind()
		if shared.Token == "" {
			log.Printf("tw: FINMIND_TOKEN not set, using FinMind's lower anonymous limit")
		}
	})
	c := shared.Allot(shared.Left() / (later + 1))
	log.Printf("%s: FinMind budget %d of %d requests left this run", name, c.Budget, shared.Left())
	return c
}

// Allot returns a client that may make at most n requests, counted against
// c's budget and paced together with every other client sharing it.
func (c *FinMind) Allot(n int) *FinMind {
	return &FinMind{BaseURL: c.BaseURL, Token: c.Token, HTTP: c.HTTP, Budget: n, parent: c}
}

// Left is how many requests this client may still make.
func (c *FinMind) Left() int {
	n := c.Budget
	for p := c.parent; p != nil; p = p.parent {
		n = min(n, p.Budget)
	}
	return max(n, 0)
}

func (c *FinMind) root() *FinMind {
	for c.parent != nil {
		c = c.parent
	}
	return c
}

type envelope[T any] struct {
	Msg    string `json:"msg"`
	Status int    `json:"status"`
	Data   []T    `json:"data"`
}

// Fetch gets one dataset. params are FinMind query parameters such as
// data_id and start_date.
func Fetch[T any](ctx context.Context, c *FinMind, dataset string, params map[string]string) ([]T, error) {
	body, err := c.Get(ctx, dataset, params)
	if err != nil {
		return nil, err
	}
	return parseEnvelope[T](dataset, body)
}

// Get returns the raw response body for one dataset, for callers that parse
// or cache it themselves. It waits out the shared request gap, spends one
// request of the budget, and returns ErrStop when the budget is spent or
// FinMind says the hourly limit is reached. Hitting the limit empties the
// shared budget, so later steps in the run don't keep knocking.
func (c *FinMind) Get(ctx context.Context, dataset string, params map[string]string) ([]byte, error) {
	if c.Left() <= 0 {
		return nil, ErrStop
	}
	r := c.root()
	if wait := r.Interval - time.Since(r.last); wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
	for p := c; p != nil; p = p.parent {
		p.Budget--
	}
	r.last = time.Now()

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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode == http.StatusTooManyRequests ||
		quotaStatus(body) {
		r.Budget = 0
		return nil, fmt.Errorf("%s: %w (HTTP %d)", dataset, ErrStop, resp.StatusCode)
	}
	return body, nil
}

// quotaStatus reports whether a response carries FinMind's in-body 402,
// which it may send instead of the HTTP status. Such a reply is a short
// message, so large bodies (data) are not decoded twice.
func quotaStatus(body []byte) bool {
	if len(body) > 4096 {
		return false
	}
	var env struct {
		Status int `json:"status"`
	}
	return json.Unmarshal(body, &env) == nil && env.Status == http.StatusPaymentRequired
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
