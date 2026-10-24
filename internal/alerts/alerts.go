// Package alerts sends a Telegram message when a stock newly passes a rule
// (issue #34). It runs as the last `radar collect` step, so it sees the data
// that run just collected. Which rules run is set in radar.toml [alerts].
//
// A stock alerts when it enters a rule's top `enter` names, then stays "on"
// (and silent) until it falls out of the top `exit`; only then can it alert
// again. That keeps an hourly run from re-sending a stock that wobbles around
// the cut-off. What is "on" is kept in the alert_state table.
package alerts

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"log"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/site"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

var taipei = time.FixedZone("Asia/Taipei", 8*60*60)

const dateLayout = "2006-01-02"

// Owned by this package, not internal/store: nothing else reads it. It lives
// in the state database (store.StateFile, issue #46).
const schema = `
CREATE TABLE IF NOT EXISTS state.alert_state (
	rule   TEXT NOT NULL,
	ticker TEXT NOT NULL,
	since  TEXT NOT NULL,
	PRIMARY KEY (rule, ticker)
);
CREATE TABLE IF NOT EXISTS state.alert_rules (
	rule       TEXT PRIMARY KEY,
	seeded_on  TEXT NOT NULL
);
-- Append-only: rows are added by collect and only ever get sent_at/sent_via
-- set, once, by notify (issue #47).
CREATE TABLE IF NOT EXISTS state.alert_outbox (
	id         INTEGER PRIMARY KEY,
	rule       TEXT NOT NULL,
	created_at TEXT NOT NULL,             -- RFC 3339
	body       TEXT NOT NULL,             -- Telegram HTML
	sent_at    TEXT NOT NULL DEFAULT '',  -- '' until handled
	sent_via   TEXT NOT NULL DEFAULT ''   -- telegram | log | expired
);
`

// Hit is one stock passing a rule, in rank order.
type Hit struct {
	Ticker, Name, Industry string
	Detail                 string // one line on why it passes
}

// rule ranks today's stocks. asOf is a short note on the data it used.
type rule struct {
	name, title string
	enter, exit int
	rank        func(now time.Time) (hits []Hit, asOf string, err error)
}

// sender delivers one message; nil means Telegram isn't configured.
type sender interface {
	Send(ctx context.Context, html string) error
}

// Run is the `alerts` collect step. It queues messages in alert_outbox;
// `radar notify` sends them once the state is saved (issue #47).
func Run(ctx context.Context, cfg config.Config, st *store.Store) error {
	return run(ctx, cfg, st, time.Now().In(taipei))
}

func run(ctx context.Context, cfg config.Config, st *store.Store, now time.Time) error {
	if _, err := st.DB.Exec(schema); err != nil {
		return fmt.Errorf("alert tables: %w", err)
	}
	if err := store.MoveToState(st.DB, "alert_state", "alert_rules"); err != nil {
		return fmt.Errorf("alert tables: %w", err)
	}
	var failed []string
	for _, name := range cfg.Alerts.Rules {
		r, err := ruleFor(name, cfg, st)
		if err == nil {
			err = check(st.DB, r, now)
		}
		if err != nil {
			log.Printf("alerts: %s: %v", name, err)
			failed = append(failed, name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("rules failed: %s", strings.Join(failed, ", "))
	}
	return nil
}

func ruleFor(name string, cfg config.Config, st *store.Store) (rule, error) {
	// Both rules rank on monthly revenue, so both wait while the month they
	// would use is collected for too little of the market (issue #37).
	gate := func(rank func(time.Time) ([]Hit, string, error)) func(time.Time) ([]Hit, string, error) {
		return func(now time.Time) ([]Hit, string, error) {
			month := revenueMonth(now, cfg.Strategy.RevenueMomentum.RevenueDeadlineDay)
			ok, note, err := RevenueReady(st.DB, month, now, cfg.Revenue.MinCoverage)
			if err != nil || !ok {
				return nil, note, err
			}
			return rank(now)
		}
	}
	switch name {
	case "revenue_momentum":
		p := cfg.Strategy.RevenueMomentum
		return rule{name, "Revenue momentum", p.TopN, p.ExitRank, gate(func(now time.Time) ([]Hit, string, error) {
			through, err := CollectedDay(st.DB, cfg.Paper.MinCoverage)
			if err != nil {
				return nil, "", err
			}
			return revenueMomentum(st.DB, now, p, through)
		})}, nil
	case "unpriced":
		a := cfg.Alerts
		return rule{name, "Growth not yet priced in", a.UnpricedTop, a.UnpricedExit, gate(func(now time.Time) ([]Hit, string, error) {
			return unpriced(st, now, cfg)
		})}, nil
	}
	return rule{}, fmt.Errorf("unknown rule %q (want revenue_momentum or unpriced)", name)
}

// check ranks one rule, queues messages for the stocks that newly entered
// it, and saves what is on, all in one transaction.
func check(db *sql.DB, r rule, now time.Time) error {
	hits, asOf, err := r.rank(now)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		// Usually missing data (a backfill, a failed collector), not an empty
		// market: keep the state so the stocks don't all re-alert later.
		log.Printf("alerts: %s: nothing ranked (%s); state kept", r.name, asOf)
		return nil
	}
	prev, err := loadOn(db, r.name)
	if err != nil {
		return err
	}
	entered, on := diff(prev, hits, r.enter, r.exit)

	var seeded sql.NullString
	db.QueryRow(`SELECT seeded_on FROM alert_rules WHERE rule = ?`, r.name).Scan(&seeded)
	var msgs []string
	switch {
	case !seeded.Valid:
		// Only a hello, so the owner sees the setup works without a flood.
		log.Printf("alerts: %s: first run, recording %d stocks without alerting", r.name, len(on))
		msgs = []string{fmt.Sprintf("<b>%s</b> alerts are on. %d stocks pass now; you'll get a message when a new one enters the top %d.",
			html.EscapeString(r.title), len(on), r.enter)}
	case len(entered) == 0:
		log.Printf("alerts: %s: no new stocks (%d on)", r.name, len(on))
	default:
		msgs = format(r, entered, asOf)
		log.Printf("alerts: %s: %d new, %d messages queued", r.name, len(entered), len(msgs))
	}
	return saveOn(db, r.name, prev, on, now, msgs)
}

// diff walks the ranking: a stock already on stays while it ranks within
// exit; a new one turns on when it ranks within enter.
func diff(prev map[string]string, hits []Hit, enter, exit int) (entered []Hit, on []string) {
	for i, h := range hits {
		pos := i + 1
		_, was := prev[h.Ticker]
		switch {
		case was && pos <= exit:
			on = append(on, h.Ticker)
		case !was && pos <= enter:
			on = append(on, h.Ticker)
			entered = append(entered, h)
		}
	}
	return entered, on
}

func loadOn(db *sql.DB, rule string) (map[string]string, error) {
	q, err := db.Query(`SELECT ticker, since FROM alert_state WHERE rule = ?`, rule)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	out := map[string]string{}
	for q.Next() {
		var t, since string
		if err := q.Scan(&t, &since); err != nil {
			return nil, err
		}
		out[t] = since
	}
	return out, q.Err()
}

func saveOn(db *sql.DB, rule string, prev map[string]string, on []string, now time.Time, msgs []string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	today := now.Format(dateLayout)
	if _, err := tx.Exec(`DELETE FROM alert_state WHERE rule = ?`, rule); err != nil {
		return err
	}
	for _, t := range on {
		since, ok := prev[t]
		if !ok {
			since = today
		}
		if _, err := tx.Exec(`INSERT INTO alert_state (rule, ticker, since) VALUES (?, ?, ?)`, rule, t, since); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO alert_rules (rule, seeded_on) VALUES (?, ?)`, rule, today); err != nil {
		return err
	}
	for _, m := range msgs {
		if _, err := tx.Exec(`INSERT INTO alert_outbox (rule, created_at, body) VALUES (?, ?, ?)`,
			rule, now.UTC().Format(time.RFC3339), m); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// unpriced is the dashboard's "growth not yet priced in" ranking.
func unpriced(st *store.Store, now time.Time, cfg config.Config) ([]Hit, string, error) {
	rows, latest, err := site.LoadTW(st, now, cfg)
	if err != nil {
		return nil, "", err
	}
	ranked := make([]site.Row, 0, len(rows))
	for _, r := range rows {
		if r.UnpricedRank > 0 {
			ranked = append(ranked, r)
		}
	}
	// LoadTW sorts by the growth ranking; put these in their own order.
	byRank := make([]Hit, len(ranked))
	for _, r := range ranked {
		byRank[r.UnpricedRank-1] = Hit{
			Ticker: r.Ticker, Name: r.Name, Industry: r.Industry,
			Detail: fmt.Sprintf("revenue YoY %+.0f%% (3-mo avg) · price %+.1f%% in a month · %.0f%% below 52-week high",
				*r.RevYoYAvg, *r.Change1M, 0-*r.FromHigh), // 0-x: no "-0%"
		}
	}
	return byRank, "prices " + latest, nil
}
