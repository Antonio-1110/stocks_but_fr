package narratives

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
)

// Card is one narrative: a claim, the market that prices it, our view, and
// who wins either way. Hand-written cards come from narratives.toml; the
// weekly news scan writes the same shape as JSON.
type Card struct {
	ID       string   `toml:"id" json:"id"`
	Title    string   `toml:"title" json:"title"`
	Claim    string   `toml:"claim" json:"claim"`         // what "yes" means, in a sentence
	Market   string   `toml:"market" json:"market"`       // "polymarket:<slug>" or "kalshi:<ticker>"...
	Keywords []string `toml:"keywords" json:"keywords"`   // ...or words its question must all contain (busiest match wins)
	View     *float64 `toml:"view" json:"view,omitempty"` // our chance of yes, 0..1; none = follow the odds' moves
	Why      string   `toml:"why" json:"why"`             // the reasoning behind the view or the stock picks
	WrongIf  string   `toml:"wrong_if" json:"wrong_if"`   // what would prove us wrong
	Catalyst string   `toml:"catalyst" json:"catalyst"`   // YYYY-MM-DD of the deciding event, if known
	YesTW    []string `toml:"yes_tw" json:"yes_tw"`       // Taiwan tickers that gain if yes (these are traded)
	NoTW     []string `toml:"no_tw" json:"no_tw"`         // Taiwan tickers that gain if no
	YesUS    []string `toml:"yes_us" json:"yes_us"`       // US tickers, shown only
	NoUS     []string `toml:"no_us" json:"no_us"`
	Strength string   `toml:"strength" json:"strength,omitempty"` // news scan: strengthening | weakening | new
	Source   string   `toml:"-" json:"source,omitempty"`          // "hand" or "news"
}

type cardsFile struct {
	Card []Card `toml:"card"`
}

var (
	idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,60}$`)
	twTicker  = regexp.MustCompile(`^[0-9]{4,6}[A-Z]?$`)
)

// LoadCards reads the hand-written cards. A missing file means no cards.
func LoadCards(path string) ([]Card, error) {
	var f cardsFile
	if _, err := toml.DecodeFile(path, &f); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	seen := map[string]bool{}
	for i := range f.Card {
		c := &f.Card[i]
		c.Source = "hand"
		if err := c.check(); err != nil {
			return nil, fmt.Errorf("%s: card %d (%s): %w", path, i+1, c.ID, err)
		}
		if seen[c.ID] {
			return nil, fmt.Errorf("%s: card id %q used twice", path, c.ID)
		}
		seen[c.ID] = true
	}
	return f.Card, nil
}

func (c *Card) check() error {
	if !idPattern.MatchString(c.ID) {
		return fmt.Errorf("id must be lowercase letters, digits and dashes")
	}
	if c.Title == "" {
		return fmt.Errorf("title is empty")
	}
	if c.Market == "" && len(c.Keywords) == 0 {
		return fmt.Errorf("needs market or keywords")
	}
	if c.Market != "" {
		src, id, ok := strings.Cut(c.Market, ":")
		if !ok || id == "" || (src != "polymarket" && src != "kalshi") {
			return fmt.Errorf("market %q: want polymarket:<slug> or kalshi:<ticker>", c.Market)
		}
	}
	if c.View != nil && (*c.View < 0 || *c.View > 1) {
		return fmt.Errorf("view %v: want a chance between 0 and 1", *c.View)
	}
	for _, t := range append(append([]string{}, c.YesTW...), c.NoTW...) {
		if !twTicker.MatchString(t) {
			return fmt.Errorf("Taiwan ticker %q looks wrong", t)
		}
	}
	return nil
}

// latestScan is the newest successful news scan's cards.
func latestScan(db *sql.DB) ([]Card, string, error) {
	var raw, at string
	err := db.QueryRow(`SELECT cards, created_at FROM narrative_scans WHERE error = ''
		ORDER BY id DESC LIMIT 1`).Scan(&raw, &at)
	if err != nil {
		return nil, "", err
	}
	var cards []Card
	if err := json.Unmarshal([]byte(raw), &cards); err != nil {
		return nil, at, err
	}
	for i := range cards {
		cards[i].Source = "news"
	}
	return cards, at, nil
}

// AllCards is the hand-written cards and, unless [narratives] trade_news is
// off, the latest news scan's, a hand card winning an id clash.
func AllCards(db *sql.DB, p config.Narratives) ([]Card, error) {
	cards, err := LoadCards(p.CardsFile)
	if err != nil {
		return nil, err
	}
	if !p.TradeNews {
		return cards, nil
	}
	news, _, _ := latestScan(db)
	ids := map[string]bool{}
	for _, c := range cards {
		ids[c.ID] = true
	}
	for _, c := range news {
		if !ids[c.ID] {
			cards = append(cards, c)
		}
	}
	return cards, nil
}

// Signal is a card on a day: its market, odds, and what it says to hold.
type Signal struct {
	Card
	Market    *Market // nil when no stored market matches
	Prob      float64 // the market's chance of yes
	Then      float64 // the same jump_days calendar days earlier (NaN if unknown)
	Edge      float64 // view - prob (0 without a view)
	Side      string  // "yes" or "no": hold that side's winners; "" = nothing
	Basis     string  // why: the view's gap, the jump, or why not
	Weight    float64 // share of the money, before normalising across signals
	Tickers   []string
	Held      bool // the portfolio already holds this side's winners
	EndPassed bool
}

// Signals evaluates every card on day. held says which Taiwan tickers the
// portfolio holds: a narrative already held stays on with a smaller gap
// (exit_edge) than it takes to enter (min_edge), so it doesn't flicker.
func Signals(db *sql.DB, p config.Narratives, day string, held map[string]bool) ([]Signal, error) {
	if err := migrate(db); err != nil {
		return nil, err
	}
	cards, err := AllCards(db, p)
	if err != nil {
		return nil, err
	}
	ms, err := Markets(db)
	if err != nil {
		return nil, err
	}
	at, err := time.ParseInLocation("2006-01-02", day, taipei)
	if err != nil {
		return nil, err
	}
	thenDay := at.AddDate(0, 0, -p.JumpDays).Format("2006-01-02")
	var out []Signal
	for _, c := range cards {
		s := Signal{Card: c, Then: math.NaN()}
		s.Market = match(c, ms, day)
		if s.Market == nil {
			s.Basis = "no open market matches"
			out = append(out, s)
			continue
		}
		mk := s.Market
		s.Prob = mk.Prob
		if prob, d, ok := oddsOn(db, mk.Source, mk.ID, day); ok && d >= thenDay {
			s.Prob = prob
		}
		if prob, _, ok := oddsOn(db, mk.Source, mk.ID, thenDay); ok {
			s.Then = prob
		}
		s.EndPassed = mk.EndDate != "" && mk.EndDate < day
		heldSide := func(side string) bool {
			for _, t := range c.side(side) {
				if held[t] {
					return true
				}
			}
			return false
		}
		switch {
		case mk.Closed || s.EndPassed:
			s.Basis = "market closed"
		case s.Prob <= p.Decided || s.Prob >= 1-p.Decided:
			s.Basis = fmt.Sprintf("settled at %.0f%%", s.Prob*100)
		case c.View != nil:
			s.Edge = *c.View - s.Prob
			side := "yes"
			if s.Edge < 0 {
				side = "no"
			}
			gap := math.Abs(s.Edge) + 1e-9 // 0.95 - 0.87 is 0.0799... in floating point
			on := heldSide(side)
			switch {
			case gap >= p.MinEdge || (on && gap >= p.ExitEdge):
				s.Side, s.Held, s.Weight = side, on, math.Min(math.Abs(s.Edge), p.MaxWeight)
				s.Basis = fmt.Sprintf("our %.0f%% vs market %.0f%%", *c.View*100, s.Prob*100)
			default:
				s.Basis = fmt.Sprintf("our %.0f%% is within %.0f points of the market's %.0f%%",
					*c.View*100, p.MinEdge*100, s.Prob*100)
			}
		case math.IsNaN(s.Then):
			s.Basis = fmt.Sprintf("no view; odds history starts after %s", thenDay)
		default:
			d := s.Prob - s.Then
			side := "yes"
			if d < 0 {
				side = "no"
			}
			on := heldSide(side)
			if jump := math.Abs(d) + 1e-9; jump >= p.MinJump || (on && jump >= p.MinJump/2) {
				s.Side, s.Held, s.Weight = side, on, math.Min(math.Abs(d), p.MaxWeight)*p.JumpWeight
				s.Basis = fmt.Sprintf("odds %+.0f points in %d days (%.0f%% → %.0f%%)", d*100, p.JumpDays, s.Then*100, s.Prob*100)
			} else {
				s.Basis = fmt.Sprintf("no view; odds moved %+.0f points in %d days (needs ±%.0f)", d*100, p.JumpDays, p.MinJump*100)
			}
		}
		if s.Side != "" {
			s.Tickers = c.side(s.Side)
			if len(s.Tickers) == 0 {
				s.Basis += fmt.Sprintf("; no Taiwan stocks listed for %q", s.Side)
				s.Side, s.Weight = "", 0
			}
		}
		out = append(out, s)
	}
	return out, nil
}

func (c Card) side(s string) []string {
	if s == "yes" {
		return c.YesTW
	}
	if s == "no" {
		return c.NoTW
	}
	return nil
}

// match finds a card's market: the named one, or the busiest open market
// whose question contains every keyword (case-insensitive) and hasn't ended.
func match(c Card, ms []Market, day string) *Market {
	if c.Market != "" {
		for i := range ms {
			if ms[i].Ref() == c.Market {
				return &ms[i]
			}
		}
		return nil
	}
	var cands []*Market
	for i := range ms {
		m := &ms[i]
		if m.Closed || (m.EndDate != "" && m.EndDate < day) {
			continue
		}
		q := strings.ToLower(m.Question)
		all := true
		for _, k := range c.Keywords {
			if !strings.Contains(q, strings.ToLower(k)) {
				all = false
				break
			}
		}
		if all {
			cands = append(cands, m)
		}
	}
	if len(cands) == 0 {
		return nil
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Volume24h > cands[j].Volume24h })
	return cands[0]
}
