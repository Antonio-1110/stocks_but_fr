package narratives

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
)

// The weekly news scan: Claude, with web search, reads the week's news next
// to the busiest prediction markets and proposes narrative cards. It runs
// only when the ANTHROPIC_API_KEY secret is set; without it the hand-written
// cards still trade.

// scanner sends one prompt and returns the model's final text.
type scanner interface {
	scan(ctx context.Context, prompt string) (string, error)
	model() string
}

func newScanner(p config.Narratives) scanner {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		return nil
	}
	return &claude{client: anthropic.NewClient(option.WithRequestTimeout(15 * time.Minute)), p: p}
}

type claude struct {
	client anthropic.Client
	p      config.Narratives
}

func (c *claude) model() string { return c.p.NewsModel }

func (c *claude) scan(ctx context.Context, prompt string) (string, error) {
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(c.p.NewsModel),
		MaxTokens: 32000,
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(prompt))},
		Tools: []anthropic.BetaToolUnionParam{{OfWebSearchTool20260209: &anthropic.BetaWebSearchTool20260209Param{
			MaxUses: anthropic.Int(int64(c.p.NewsSearches)),
		}}},
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffortHigh},
		// A request the safety classifiers decline is re-served by a fallback model.
		Fallbacks: anthropic.BetaFallbacksParamOfDefault(),
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
	}
	// Web search can pause a long turn; send it back to continue.
	for range 5 {
		stream := c.client.Beta.Messages.NewStreaming(ctx, params)
		msg := anthropic.BetaMessage{}
		for stream.Next() {
			if err := msg.Accumulate(stream.Current()); err != nil {
				return "", err
			}
		}
		if err := stream.Err(); err != nil {
			return "", err
		}
		switch msg.StopReason {
		case anthropic.BetaStopReasonPauseTurn:
			params.Messages = append(params.Messages, msg.ToParam())
			continue
		case anthropic.BetaStopReasonRefusal:
			return "", fmt.Errorf("the model declined the request")
		case anthropic.BetaStopReasonMaxTokens:
			return "", fmt.Errorf("the answer ran past max_tokens")
		}
		var text strings.Builder
		for _, b := range msg.Content {
			if t, ok := b.AsAny().(anthropic.BetaTextBlock); ok {
				text.WriteString(t.Text)
			}
		}
		log.Printf("narratives: news scan used %d input and %d output tokens", msg.Usage.InputTokens, msg.Usage.OutputTokens)
		return text.String(), nil
	}
	return "", fmt.Errorf("still paused after 5 continuations")
}

// scanIfDue runs the scan once news_every days have passed since the last
// good one (a failed one is retried after a day).
func scanIfDue(ctx context.Context, p config.Narratives, db *sql.DB, sc scanner, hand []Card, now time.Time) error {
	if sc == nil || p.NewsEvery <= 0 {
		return nil
	}
	var lastOK, lastTry string
	db.QueryRow(`SELECT COALESCE(MAX(created_at), '') FROM narrative_scans WHERE error = ''`).Scan(&lastOK)
	db.QueryRow(`SELECT COALESCE(MAX(created_at), '') FROM narrative_scans`).Scan(&lastTry)
	if t, err := time.Parse(time.RFC3339, lastOK); err == nil && now.Sub(t) < time.Duration(p.NewsEvery)*24*time.Hour-time.Hour {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, lastTry); err == nil && lastTry != lastOK && now.Sub(t) < 20*time.Hour {
		return nil
	}
	prompt, known, err := buildPrompt(db, p, hand, now)
	if err != nil {
		return err
	}
	if prompt == "" {
		log.Printf("narratives: news scan waits for prediction-market data")
		return nil
	}
	log.Printf("narratives: news scan starting (%s)", sc.model())
	text, err := sc.scan(ctx, prompt)
	var summary string
	var cards []Card
	if err == nil {
		summary, cards, err = parseScan(text, known, p.NewsCards)
	}
	row := struct{ cards, errText string }{"[]", ""}
	if err != nil {
		row.errText = err.Error()
	} else {
		b, _ := json.Marshal(cards)
		row.cards = string(b)
		log.Printf("narratives: news scan proposed %d cards", len(cards))
	}
	if _, xerr := db.Exec(`INSERT INTO narrative_scans (created_at, model, summary, cards, error) VALUES (?,?,?,?,?)`,
		now.UTC().Format(time.RFC3339), sc.model(), summary, row.cards, row.errText); xerr != nil {
		return xerr
	}
	return err
}

// known is what a scan's answer is checked against.
type known struct {
	markets map[string]bool   // refs stored in pm_markets
	tw      map[string]string // Taiwan ticker -> name
}

func buildPrompt(db *sql.DB, p config.Narratives, hand []Card, now time.Time) (string, known, error) {
	k := known{markets: map[string]bool{}, tw: map[string]string{}}
	ms, err := Markets(db)
	if err != nil {
		return "", k, err
	}
	day := now.In(taipei).Format("2006-01-02")
	weekAgo := now.In(taipei).AddDate(0, 0, -7).Format("2006-01-02")
	type row struct {
		m    Market
		move float64
	}
	var open []row
	for _, m := range ms {
		k.markets[m.Ref()] = true
		if m.Closed || (m.EndDate != "" && m.EndDate < day) {
			continue
		}
		r := row{m: m, move: math.NaN()}
		if then, _, ok := oddsOn(db, m.Source, m.ID, weekAgo); ok {
			r.move = m.Prob - then
		}
		open = append(open, r)
	}
	if len(open) == 0 {
		return "", k, nil
	}
	line := func(r row) string {
		mv := "new"
		if !math.IsNaN(r.move) {
			mv = fmt.Sprintf("%+.0f", r.move*100)
		}
		return fmt.Sprintf("%s | %s | %.0f%% | 7d %s | ends %s | $%.0fk/24h", r.m.Ref(), r.m.Question,
			r.m.Prob*100, mv, r.m.EndDate, r.m.Volume24h/1000)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `Today is %s (Taipei). You maintain the narrative cards of a paper portfolio of Taiwan stocks
that bets on world narratives priced by prediction markets: Fed decisions, tariffs, AI capex, chip export rules,
cross-strait politics, big-company leadership changes, elections, wars, commodities.

Use web search to read this week's market-moving news (Reuters, Bloomberg, CNBC, FT, Nikkei Asia, 鉅亨網 cnyes,
經濟日報, 工商時報, Focus Taiwan). Then pick the %d narratives that matter most for Taiwan stocks over the next
weeks to months, each priced by one of the prediction markets listed below.

For each narrative give your own probability that the market resolves yes ("view"), from the news and base
rates. Be calibrated: where you have no edge, put the view at the market's odds; the portfolio only trades
narratives where your view and the odds differ by at least %.0f points, and sizes by the gap.

Map each narrative to Taiwan stocks: those that gain if it resolves yes, and those that gain if it resolves no.
Prefer second-order winners in Taiwan (suppliers, customers, competitors that pick up share) over the obvious
name. 2 to 6 per side, from the Taiwan list below only; leave a side empty if nothing in Taiwan clearly gains.
US tickers are shown for reference only.

Answer with one JSON object and nothing after it:
{"summary": "<3-5 sentences: what changed this week>",
 "cards": [{"id": "<lowercase-dashed, stable week to week for the same narrative>",
   "title": "<short>", "claim": "<what yes means>", "market": "<a ref from the list, e.g. polymarket:slug>",
   "view": <0..1>, "strength": "strengthening|weakening|new", "why": "<2-3 sentences with this week's evidence>",
   "wrong_if": "<what would prove the view wrong>", "catalyst": "<YYYY-MM-DD or empty>",
   "yes_tw": ["2330"], "no_tw": [], "yes_us": ["NVDA"], "no_us": []}]}
`, day, p.NewsCards, p.MinEdge*100)

	sort.SliceStable(open, func(i, j int) bool { return open[i].m.Volume24h > open[j].m.Volume24h })
	fmt.Fprintf(&b, "\nBusiest open prediction markets (ref | question | chance of yes | 7-day move in points | end | volume):\n")
	shown := map[string]bool{}
	for _, r := range open[:min(len(open), p.NewsMarkets)] {
		b.WriteString(line(r) + "\n")
		shown[r.m.Ref()] = true
	}
	movers := append([]row{}, open...)
	sort.SliceStable(movers, func(i, j int) bool { return abs(movers[i].move) > abs(movers[j].move) })
	fmt.Fprintf(&b, "\nBiggest 7-day moves among the rest:\n")
	n := 0
	for _, r := range movers {
		if n >= 40 || math.IsNaN(r.move) || abs(r.move) < 0.05 {
			break
		}
		if !shown[r.m.Ref()] && r.m.Volume24h >= 1000 {
			b.WriteString(line(r) + "\n")
			n++
		}
	}
	if len(hand) > 0 {
		b.WriteString("\nThe owner's hand-written cards (they trade already; don't repeat them, but you may cover the same market with a different angle):\n")
		for _, c := range hand {
			fmt.Fprintf(&b, "- %s: %s\n", c.ID, c.Title)
		}
	}
	if prev, at, err := latestScan(db); err == nil && len(prev) > 0 {
		fmt.Fprintf(&b, "\nYour cards from the scan of %s (keep an id when you keep the narrative):\n", at[:10])
		for _, c := range prev {
			v := "none"
			if c.View != nil {
				v = fmt.Sprintf("%.0f%%", *c.View*100)
			}
			fmt.Fprintf(&b, "- %s: %s (%s, view %s)\n", c.ID, c.Title, c.Market, v)
		}
	}
	tw, err := liquidTW(db, 300)
	if err != nil {
		return "", k, err
	}
	b.WriteString("\nTaiwan stocks and ETFs you may use (ticker name, most traded first):\n")
	for i, t := range tw {
		k.tw[t[0]] = t[1]
		fmt.Fprintf(&b, "%s %s", t[0], t[1])
		if i%8 == 7 {
			b.WriteString("\n")
		} else {
			b.WriteString("; ")
		}
	}
	b.WriteString("\n")
	return b.String(), k, nil
}

func abs(x float64) float64 {
	if math.IsNaN(x) {
		return -1
	}
	return math.Abs(x)
}

// liquidTW lists the n most traded Taiwan tickers over the last 20 trading days.
func liquidTW(db *sql.DB, n int) ([][2]string, error) {
	q, err := db.Query(`
		WITH d AS (SELECT DISTINCT date FROM prices WHERE market = 'TW' ORDER BY date DESC LIMIT 20)
		SELECT p.ticker, COALESCE(c.name, '') FROM prices p
		JOIN d ON d.date = p.date
		LEFT JOIN companies c ON c.market = 'TW' AND c.ticker = p.ticker
		WHERE p.market = 'TW'
		GROUP BY p.ticker ORDER BY SUM(COALESCE(p.turnover, 0)) DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	var out [][2]string
	for q.Next() {
		var t, name string
		if err := q.Scan(&t, &name); err != nil {
			return nil, err
		}
		out = append(out, [2]string{t, name})
	}
	return out, q.Err()
}

// parseScan reads the model's JSON answer and keeps what checks out: cards
// on a stored market, with tickers from the Taiwan list.
func parseScan(text string, k known, max int) (string, []Card, error) {
	i, j := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if i < 0 || j < i {
		return "", nil, fmt.Errorf("no JSON in the answer: %.200s", text)
	}
	var ans struct {
		Summary string `json:"summary"`
		Cards   []Card `json:"cards"`
	}
	if err := json.Unmarshal([]byte(text[i:j+1]), &ans); err != nil {
		return "", nil, fmt.Errorf("answer isn't the JSON asked for: %w", err)
	}
	var out []Card
	seen := map[string]bool{}
	for _, c := range ans.Cards {
		c.Source, c.Keywords = "news", nil
		c.ID = strings.ToLower(strings.TrimSpace(c.ID))
		if !k.markets[c.Market] {
			log.Printf("narratives: news card %s: market %q isn't one we track; dropped", c.ID, c.Market)
			continue
		}
		keep := func(ts []string) []string {
			var out []string
			for _, t := range ts {
				t = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(t, ".TW"), ".TWO"))
				if _, ok := k.tw[t]; ok {
					out = append(out, t)
				} else {
					log.Printf("narratives: news card %s: ticker %q not in the Taiwan list; dropped", c.ID, t)
				}
			}
			return out
		}
		c.YesTW, c.NoTW = keep(c.YesTW), keep(c.NoTW)
		if err := c.check(); err != nil || seen[c.ID] {
			log.Printf("narratives: news card %s dropped: %v", c.ID, err)
			continue
		}
		seen[c.ID] = true
		out = append(out, c)
		if len(out) >= max {
			break
		}
	}
	if len(out) == 0 {
		return ans.Summary, nil, fmt.Errorf("no usable cards in the answer")
	}
	return ans.Summary, out, nil
}
