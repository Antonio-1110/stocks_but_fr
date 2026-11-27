package paper

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/narratives"
)

// --- narratives (issue #95) -----------------------------------------------------

// narrative holds the Taiwan winners of each live narrative card: one whose
// view is at least min_edge from the prediction market's odds (the side we
// think the market underprices), or, for a card without a view, whose odds
// jumped at least min_jump in jump_days (the side the odds moved to). Each
// live narrative gets money in proportion to its gap (or jump x jump_weight),
// at most max_narrative_share of equity, split evenly over its stocks.
//
// Every day: sell a holding no live narrative names any more, or one
// stop_loss below its cost; buy the newly named ones. Holdings aren't resized.
// A stopped-out stock isn't bought again for 20 trading days (book.blocked
// holds the stop day here).
type narrative struct {
	p  config.Narratives
	db *sql.DB
}

const narrativeCooldown = 20

func (s narrative) decide(m *market, b *book, h holdings) ([]*order, error) {
	day := m.last()
	var out []*order
	selling := map[string]bool{}
	for _, t := range sortedKeys(b.pos) {
		if h.leave[t] {
			continue
		}
		p := b.pos[t]
		if s.p.StopLoss > 0 && h.value[t] <= p.Gross*(1-s.p.StopLoss) {
			out = append(out, &order{Ticker: t, Side: "sell",
				Reason: fmt.Sprintf("stop loss: %s since bought", pct(h.value[t]/p.Gross-1))})
			selling[t] = true
			b.blocked[t] = day
		}
	}
	held := map[string]bool{}
	for t := range b.pos {
		held[t] = !selling[t] && !h.leave[t]
	}
	sigs, err := narratives.Signals(s.db, s.p, day, held)
	if err != nil {
		return nil, err
	}

	type want struct {
		target float64
		why    []string
	}
	wants := map[string]*want{}
	var total float64
	type live struct {
		s       narratives.Signal
		tickers []string
	}
	var lives []live
	for _, sg := range sigs {
		if sg.Side == "" {
			continue
		}
		var ts []string
		for _, t := range sg.Tickers {
			if s.tradable(m, t, day) {
				ts = append(ts, t)
			}
		}
		if len(ts) == 0 {
			continue
		}
		lives = append(lives, live{sg, ts})
		total += sg.Weight
	}
	for _, l := range lives {
		share := math.Min(s.p.MaxNarrativeShare, l.s.Weight/total)
		why := fmt.Sprintf("%s: %s, %s", l.s.Title, strings.ToUpper(l.s.Side[:1])+l.s.Side[1:], l.s.Basis)
		for _, t := range l.tickers {
			w := wants[t]
			if w == nil {
				w = &want{}
				wants[t] = w
			}
			w.target += h.equity * share / float64(len(l.tickers))
			w.why = append(w.why, why)
		}
	}

	// Why a holding's narratives went quiet, for the sell reason.
	gone := func(t string) string {
		var why []string
		for _, sg := range sigs {
			for _, side := range []string{"yes", "no"} {
				for _, x := range map[string][]string{"yes": sg.YesTW, "no": sg.NoTW}[side] {
					if x == t {
						why = append(why, fmt.Sprintf("%s (%s side): %s", sg.Title, side, sg.Basis))
					}
				}
			}
		}
		if len(why) == 0 {
			return "no narrative card names it any more"
		}
		return "narrative off: " + strings.Join(why, "; ")
	}
	for _, t := range sortedKeys(b.pos) {
		if h.leave[t] || selling[t] || wants[t] != nil {
			continue
		}
		out = append(out, &order{Ticker: t, Side: "sell", Reason: gone(t)})
	}

	pending := map[string]bool{}
	for _, o := range b.orders {
		if o.Status == "pending" && o.Side == "buy" {
			pending[o.Ticker] = true
		}
	}
	var buysT []string
	for t := range wants {
		if b.pos[t] != nil || pending[t] {
			continue
		}
		if d, ok := b.blocked[t]; ok && d != "" && heldDays(m, d, day) < narrativeCooldown {
			continue
		}
		buysT = append(buysT, t)
	}
	sort.Slice(buysT, func(i, j int) bool {
		if wants[buysT[i]].target != wants[buysT[j]].target {
			return wants[buysT[i]].target > wants[buysT[j]].target
		}
		return buysT[i] < buysT[j]
	})
	for _, t := range buysT {
		out = append(out, &order{Ticker: t, Side: "buy", Target: wants[t].target, Reason: strings.Join(wants[t].why, "; ")})
	}
	return out, nil
}

// tradable: traded on day with at least min_adv_ntd average traded value.
func (s narrative) tradable(m *market, t, day string) bool {
	if _, ok := m.on(t, day); !ok {
		return false
	}
	days := m.window(day, s.p.ADVDays)
	if len(days) == 0 {
		return false
	}
	sum, _, _ := window(m, t, days, turnOf)
	return sum/float64(len(days)) >= s.p.MinADVNTD
}
