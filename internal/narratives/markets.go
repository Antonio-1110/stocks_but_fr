// Package narratives tracks world narratives through prediction markets
// (issue #95): "will the Fed hike in December?", "will Jensen Huang return as
// Nvidia's CEO?". Each run stores the odds of open Polymarket and Kalshi
// markets; narrative cards (narratives.toml, plus a weekly news scan by
// Claude) link a claim to a market and to the Taiwan stocks that win if it
// happens or if it doesn't. Signals turns cards and odds into what the
// `narrative` paper portfolio holds, and Render writes narratives.html.
package narratives

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

const userAgent = "stocks_but_fr-radar/0.1 (+https://github.com/Antonio-1110/stocks_but_fr)"

var taipei = time.FixedZone("Asia/Taipei", 8*60*60)

// Market data lives in radar.db; the news scans, which can't be fetched
// again, live in the state database.
const schema = `
CREATE TABLE IF NOT EXISTS pm_markets (
	source     TEXT NOT NULL,              -- polymarket | kalshi
	id         TEXT NOT NULL,              -- Polymarket slug, Kalshi ticker
	question   TEXT NOT NULL,
	url        TEXT NOT NULL DEFAULT '',
	end_date   TEXT NOT NULL DEFAULT '',   -- YYYY-MM-DD, when it is meant to resolve
	prob       REAL NOT NULL,              -- market's chance of yes, 0..1
	volume_24h REAL NOT NULL DEFAULT 0,    -- USD
	closed     INTEGER NOT NULL DEFAULT 0,
	seen_at    TEXT NOT NULL,              -- RFC 3339 UTC, last fetch that returned it
	PRIMARY KEY (source, id)
);
CREATE TABLE IF NOT EXISTS pm_odds (
	source TEXT NOT NULL,
	id     TEXT NOT NULL,
	date   TEXT NOT NULL,                  -- Taipei date of the fetch; the day's last fetch wins
	prob   REAL NOT NULL,
	PRIMARY KEY (source, id, date)
);
CREATE TABLE IF NOT EXISTS pm_fetches (
	source     TEXT PRIMARY KEY,
	fetched_at TEXT NOT NULL               -- RFC 3339 UTC
);
CREATE TABLE IF NOT EXISTS state.narrative_scans (
	id         INTEGER PRIMARY KEY,
	created_at TEXT NOT NULL,              -- RFC 3339 UTC
	model      TEXT NOT NULL,
	summary    TEXT NOT NULL DEFAULT '',
	cards      TEXT NOT NULL DEFAULT '[]', -- JSON: []Card
	error      TEXT NOT NULL DEFAULT ''    -- set when the scan failed; cards is then empty
);
`

func migrate(db *sql.DB) error {
	_, err := db.Exec(schema)
	return err
}

// Market is one yes/no prediction market as stored.
type Market struct {
	Source, ID, Question, URL, EndDate string
	Prob, Volume24h                    float64
	Closed                             bool
	SeenAt                             string
}

// Ref is how cards name a market: "polymarket:<slug>" or "kalshi:<ticker>".
func (m Market) Ref() string { return m.Source + ":" + m.ID }

// Collect is the `narratives` collect step: prediction-market odds, then
// (weekly, with ANTHROPIC_API_KEY set) the news scan.
func Collect(ctx context.Context, cfg config.Config, st *store.Store) error {
	return collect(ctx, cfg.Narratives, st.DB, sources(cfg.Narratives), newScanner(cfg.Narratives), time.Now())
}

// source fetches open markets. wanted are ids (no source prefix) that cards
// name explicitly: they are kept whatever their volume.
type source interface {
	name() string
	fetch(ctx context.Context, wanted []string) ([]Market, error)
}

func sources(p config.Narratives) []source {
	var out []source
	for _, s := range p.Sources {
		switch s {
		case "polymarket":
			out = append(out, &polymarket{base: "https://gamma-api.polymarket.com", client: client(), limit: p.Polymarket})
		case "kalshi":
			out = append(out, &kalshi{base: "https://api.elections.kalshi.com/trade-api/v2", client: client(), pages: p.KalshiPages})
		default:
			log.Printf("narratives: unknown source %q in [narratives] sources", s)
		}
	}
	return out
}

func client() *http.Client { return &http.Client{Timeout: 60 * time.Second} }

func collect(ctx context.Context, p config.Narratives, db *sql.DB, srcs []source, sc scanner, now time.Time) error {
	if err := migrate(db); err != nil {
		return err
	}
	cards, err := LoadCards(p.CardsFile)
	if err != nil {
		return err // a broken cards file must be fixed, not skipped quietly
	}
	news, _, _ := latestScan(db)
	wanted := map[string][]string{}
	for _, c := range append(cards, news...) {
		if src, id, ok := strings.Cut(c.Market, ":"); ok {
			wanted[src] = append(wanted[src], id)
		}
	}
	var failed []string
	for _, s := range srcs {
		var last string
		db.QueryRow(`SELECT fetched_at FROM pm_fetches WHERE source = ?`, s.name()).Scan(&last)
		if t, err := time.Parse(time.RFC3339, last); err == nil && now.Sub(t) < time.Duration(p.FetchHours*float64(time.Hour)) {
			continue
		}
		ms, err := s.fetch(ctx, wanted[s.name()])
		if err != nil {
			log.Printf("narratives: %s: %v", s.name(), err)
			failed = append(failed, s.name())
			continue
		}
		keep := map[string]bool{}
		for _, id := range wanted[s.name()] {
			keep[id] = true
		}
		n, err := save(db, ms, keep, p.MinVolume24h, now)
		if err != nil {
			return err
		}
		log.Printf("narratives: %s: %d markets fetched, %d stored", s.name(), len(ms), n)
	}
	if err := scanIfDue(ctx, p, db, sc, cards, now); err != nil {
		log.Printf("narratives: news scan: %v", err)
		failed = append(failed, "news scan")
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed: %s", strings.Join(failed, ", "))
	}
	return nil
}

func save(db *sql.DB, ms []Market, keep map[string]bool, minVol float64, now time.Time) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	seen := now.UTC().Format(time.RFC3339)
	day := now.In(taipei).Format("2006-01-02")
	n := 0
	src := ""
	for _, m := range ms {
		src = m.Source
		if m.Volume24h < minVol && !keep[m.ID] {
			continue
		}
		if math.IsNaN(m.Prob) || m.Prob < 0 || m.Prob > 1 {
			continue
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO pm_markets
			(source, id, question, url, end_date, prob, volume_24h, closed, seen_at) VALUES (?,?,?,?,?,?,?,?,?)`,
			m.Source, m.ID, m.Question, m.URL, m.EndDate, m.Prob, m.Volume24h, m.Closed, seen); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO pm_odds (source, id, date, prob) VALUES (?,?,?,?)`,
			m.Source, m.ID, day, m.Prob); err != nil {
			return 0, err
		}
		n++
	}
	if src != "" {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO pm_fetches (source, fetched_at) VALUES (?, ?)`, src, seen); err != nil {
			return 0, err
		}
	}
	return n, tx.Commit()
}

func get(ctx context.Context, c *http.Client, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d: %.200s", u, resp.StatusCode, body)
	}
	return json.Unmarshal(body, v)
}

// num reads a JSON number or a number in a string ("0.53"); ok is false
// when the field is missing, null or not a number.
type num struct {
	v  float64
	ok bool
}

func (n *num) UnmarshalJSON(b []byte) error {
	f, err := strconv.ParseFloat(strings.Trim(string(b), `"`), 64)
	*n = num{f, err == nil}
	return nil
}

func dateOf(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// --- Polymarket (gamma API, public) ------------------------------------------

type polymarket struct {
	base   string
	client *http.Client
	limit  int
}

func (p *polymarket) name() string { return "polymarket" }

type pmMarket struct {
	Question      string `json:"question"`
	Slug          string `json:"slug"`
	EndDate       string `json:"endDate"`
	Outcomes      string `json:"outcomes"`      // JSON in a string: ["Yes", "No"]
	OutcomePrices string `json:"outcomePrices"` // JSON in a string: ["0.53", "0.47"]
	Volume24hr    num    `json:"volume24hr"`
	Closed        bool   `json:"closed"`
	Events        []struct {
		Slug string `json:"slug"`
	} `json:"events"`
}

func (p *polymarket) fetch(ctx context.Context, wanted []string) ([]Market, error) {
	var out []Market
	have := map[string]bool{}
	const page = 100 // the gamma API serves at most about this many per call
	for off := 0; off < p.limit; off += page {
		q := url.Values{"active": {"true"}, "closed": {"false"}, "order": {"volume24hr"}, "ascending": {"false"},
			"limit": {strconv.Itoa(min(page, p.limit-off))}, "offset": {strconv.Itoa(off)}}
		var rows []pmMarket
		if err := get(ctx, p.client, p.base+"/markets?"+q.Encode(), &rows); err != nil {
			return nil, err
		}
		for _, r := range parsePolymarket(rows) {
			if !have[r.ID] {
				have[r.ID] = true
				out = append(out, r)
			}
		}
		if len(rows) == 0 {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	for _, slug := range wanted {
		if have[slug] {
			continue
		}
		var rows []pmMarket
		if err := get(ctx, p.client, p.base+"/markets?"+url.Values{"slug": {slug}}.Encode(), &rows); err != nil {
			log.Printf("narratives: polymarket %s: %v", slug, err)
			continue
		}
		out = append(out, parsePolymarket(rows)...)
		have[slug] = true
	}
	return out, nil
}

// parsePolymarket keeps yes/no markets: the yes price is the chance of yes.
func parsePolymarket(rows []pmMarket) []Market {
	var out []Market
	for _, r := range rows {
		var outcomes, prices []string
		if json.Unmarshal([]byte(r.Outcomes), &outcomes) != nil || json.Unmarshal([]byte(r.OutcomePrices), &prices) != nil {
			continue
		}
		if len(outcomes) != 2 || len(prices) != 2 || !strings.EqualFold(outcomes[0], "yes") {
			continue
		}
		prob, err := strconv.ParseFloat(prices[0], 64)
		if err != nil || r.Slug == "" {
			continue
		}
		u := "https://polymarket.com/market/" + r.Slug
		if len(r.Events) > 0 && r.Events[0].Slug != "" {
			u = "https://polymarket.com/event/" + r.Events[0].Slug
		}
		vol := 0.0
		if r.Volume24hr.ok {
			vol = r.Volume24hr.v
		}
		out = append(out, Market{Source: "polymarket", ID: r.Slug, Question: strings.TrimSpace(r.Question), URL: u,
			EndDate: dateOf(r.EndDate), Prob: prob, Volume24h: vol, Closed: r.Closed})
	}
	return out
}

// --- Kalshi (trade API v2, public market data) --------------------------------

type kalshi struct {
	base   string
	client *http.Client
	pages  int
}

func (k *kalshi) name() string { return "kalshi" }

type kMarket struct {
	Ticker      string `json:"ticker"`
	EventTicker string `json:"event_ticker"`
	Title       string `json:"title"`
	YesSubTitle string `json:"yes_sub_title"`
	Status      string `json:"status"`
	CloseTime   string `json:"close_time"`
	// Prices: older responses in cents, newer ones also in dollar strings.
	YesBid     num `json:"yes_bid"`
	YesAsk     num `json:"yes_ask"`
	LastPrice  num `json:"last_price"`
	YesBidD    num `json:"yes_bid_dollars"`
	YesAskD    num `json:"yes_ask_dollars"`
	LastPriceD num `json:"last_price_dollars"`
	Volume24h  num `json:"volume_24h"`
	Volume24hF num `json:"volume_24h_fp"`
}

type kPage struct {
	Markets []kMarket `json:"markets"`
	Cursor  string    `json:"cursor"`
}

type kEvents struct {
	Events []struct {
		EventTicker string    `json:"event_ticker"`
		Markets     []kMarket `json:"markets"`
	} `json:"events"`
	Cursor string `json:"cursor"`
}

// fetch reads open events with their markets. The /markets list isn't
// ordered by activity, and its first pages are mostly untraded markets (the
// first live run, #95, kept none of 4,882); events are fewer, so a few dozen
// pages cover them.
func (k *kalshi) fetch(ctx context.Context, wanted []string) ([]Market, error) {
	var out []Market
	have := map[string]bool{}
	cursor := ""
	maxVol := 0.0
	for i := 0; i < k.pages; i++ {
		q := url.Values{"status": {"open"}, "limit": {"200"}, "with_nested_markets": {"true"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var pg kEvents
		if err := get(ctx, k.client, k.base+"/events?"+q.Encode(), &pg); err != nil {
			if i > 0 {
				log.Printf("narratives: kalshi: page %d: %v; keeping the first %d pages", i+1, err, i)
				break
			}
			return nil, err
		}
		for _, e := range pg.Events {
			for _, m := range parseKalshi(e.Markets) {
				if !have[m.ID] {
					have[m.ID] = true
					maxVol = math.Max(maxVol, m.Volume24h)
					out = append(out, m)
				}
			}
		}
		if pg.Cursor == "" || len(pg.Events) == 0 {
			break
		}
		cursor = pg.Cursor
		time.Sleep(300 * time.Millisecond)
	}
	log.Printf("narratives: kalshi: busiest market traded about $%.0f in 24h", maxVol)
	for _, t := range wanted {
		if have[t] {
			continue
		}
		var one struct {
			Market kMarket `json:"market"`
		}
		if err := get(ctx, k.client, k.base+"/markets/"+url.PathEscape(t), &one); err != nil {
			log.Printf("narratives: kalshi %s: %v", t, err)
			continue
		}
		out = append(out, parseKalshi([]kMarket{one.Market})...)
	}
	return out, nil
}

func parseKalshi(rows []kMarket) []Market {
	var out []Market
	for _, r := range rows {
		if r.Ticker == "" {
			continue
		}
		price := func(d, c num) float64 {
			if d.ok {
				return d.v
			}
			if c.ok {
				return c.v / 100
			}
			return 0
		}
		bid, ask, last := price(r.YesBidD, r.YesBid), price(r.YesAskD, r.YesAsk), price(r.LastPriceD, r.LastPrice)
		prob := last
		if bid > 0 && ask > bid && ask-bid <= 0.2 {
			prob = (bid + ask) / 2 // a tight quote says more than an old trade
		}
		if prob <= 0 && bid <= 0 && ask <= 0 {
			continue // never traded, no quotes
		}
		q := strings.TrimSpace(r.Title)
		if s := strings.TrimSpace(r.YesSubTitle); s != "" && !strings.Contains(strings.ToLower(q), strings.ToLower(s)) {
			q += " · " + s
		}
		series, _, _ := strings.Cut(r.EventTicker, "-")
		vol := 0.0
		if r.Volume24hF.ok {
			vol = r.Volume24hF.v
		} else if r.Volume24h.ok {
			vol = r.Volume24h.v
		}
		// Kalshi volume is in contracts (each pays $1); times the price is
		// roughly USD traded, comparable with Polymarket's.
		vol *= math.Max(prob, 0.01)
		out = append(out, Market{Source: "kalshi", ID: r.Ticker, Question: q,
			URL: "https://kalshi.com/markets/" + strings.ToLower(series), EndDate: dateOf(r.CloseTime),
			Prob: prob, Volume24h: vol, Closed: r.Status != "" && r.Status != "active" && r.Status != "open"})
	}
	return out
}

// --- reading ------------------------------------------------------------------

// Markets returns the stored markets, busiest first.
func Markets(db *sql.DB) ([]Market, error) {
	q, err := db.Query(`SELECT source, id, question, url, end_date, prob, volume_24h, closed, seen_at
		FROM pm_markets ORDER BY volume_24h DESC, source, id`)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	var out []Market
	for q.Next() {
		var m Market
		if err := q.Scan(&m.Source, &m.ID, &m.Question, &m.URL, &m.EndDate, &m.Prob, &m.Volume24h, &m.Closed, &m.SeenAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, q.Err()
}

// oddsOn is a market's odds on the last stored day at or before day.
func oddsOn(db *sql.DB, src, id, day string) (float64, string, bool) {
	var p float64
	var d string
	err := db.QueryRow(`SELECT prob, date FROM pm_odds WHERE source = ? AND id = ? AND date <= ?
		ORDER BY date DESC LIMIT 1`, src, id, day).Scan(&p, &d)
	return p, d, err == nil
}

// history is a market's daily odds from day `from` on.
func history(db *sql.DB, src, id, from string) ([]float64, error) {
	q, err := db.Query(`SELECT prob FROM pm_odds WHERE source = ? AND id = ? AND date >= ? ORDER BY date`, src, id, from)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	var out []float64
	for q.Next() {
		var p float64
		if err := q.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, q.Err()
}
