package paper

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
)

// The ledger is an append-only record of what happened to each portfolio
// (issue #48): its start, every order, fill and skip, and the benchmark
// purchase, with the cash change and the cash after each fill. The other
// paper tables are the working state and get rewritten; the ledger is never
// changed (triggers reject UPDATE and DELETE), so the history can be checked
// against them.
const ledgerSchema = `
CREATE TABLE IF NOT EXISTS state.paper_ledger (
	id          INTEGER PRIMARY KEY,
	portfolio   TEXT NOT NULL,
	kind        TEXT NOT NULL,              -- start | order | fill | skip | bench
	date        TEXT NOT NULL,              -- price date it applies to
	order_id    INTEGER NOT NULL DEFAULT 0,
	ticker      TEXT NOT NULL DEFAULT '',
	side        TEXT NOT NULL DEFAULT '',   -- buy | sell
	shares      REAL NOT NULL DEFAULT 0,
	price       REAL NOT NULL DEFAULT 0,
	value       REAL NOT NULL DEFAULT 0,
	commission  REAL NOT NULL DEFAULT 0,
	tax         REAL NOT NULL DEFAULT 0,
	cash_change REAL NOT NULL DEFAULT 0,
	cash_after  REAL NOT NULL DEFAULT 0,
	note        TEXT NOT NULL DEFAULT ''
);
CREATE TRIGGER IF NOT EXISTS state.paper_ledger_no_update BEFORE UPDATE ON paper_ledger
BEGIN SELECT RAISE(ABORT, 'paper_ledger is append-only'); END;
CREATE TRIGGER IF NOT EXISTS state.paper_ledger_no_delete BEFORE DELETE ON paper_ledger
BEGIN SELECT RAISE(ABORT, 'paper_ledger is append-only'); END;
`

type entry struct {
	kind, date   string
	orderID      int64
	ticker, side string
	shares       float64
	price, value float64
	comm, tax    float64
	cashChange   float64
	note         string
}

func appendLedger(tx *sql.Tx, portfolio string, e entry, cashAfter float64) error {
	_, err := tx.Exec(`INSERT INTO paper_ledger (portfolio, kind, date, order_id, ticker, side, shares,
		price, value, commission, tax, cash_change, cash_after, note)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		portfolio, e.kind, e.date, e.orderID, e.ticker, e.side, e.shares, e.price, e.value,
		e.comm, e.tax, e.cashChange, cashAfter, e.note)
	return err
}

// orderEntry is the ledger row for an order's current outcome; ok is false
// while it is still pending.
func orderEntry(o *order) (entry, bool) {
	e := entry{orderID: o.ID, ticker: o.Ticker, side: o.Side, date: o.FillDate, note: o.Note}
	switch o.Status {
	case "filled":
		e.kind, e.shares, e.price, e.value, e.comm, e.tax = "fill", o.Shares, o.Price, o.Value, o.Commission, o.Tax
		if o.Side == "buy" {
			e.cashChange = -(o.Value + o.Commission)
		} else {
			e.cashChange = o.Value - o.Commission - o.Tax
		}
	case "skipped":
		e.kind, e.date = "skip", o.SignalDate
	default:
		return e, false
	}
	return e, true
}

// backfillLedger gives a portfolio started before the ledger existed its
// start row and its past fills, once. The cash after each is replayed. It
// must run before save() writes this run's order changes.
func backfillLedger(tx *sql.Tx, b *book) error {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM paper_ledger WHERE portfolio = ?`, b.Name).Scan(&n); err != nil {
		return err
	}
	if n > 0 || b.isNew {
		return nil
	}
	cash := b.Capital
	if err := appendLedger(tx, b.Name, entry{kind: "start", date: b.StartedOn, cashChange: b.Capital,
		note: "backfilled"}, cash); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT id, ticker, side, signal_date, status, fill_date, price, shares, value,
		commission, tax, note FROM paper_orders WHERE portfolio = ? AND status != 'pending'
		ORDER BY CASE status WHEN 'filled' THEN fill_date ELSE signal_date END, side = 'buy', id`, b.Name)
	if err != nil {
		return err
	}
	var past []*order
	for rows.Next() {
		o := &order{}
		if err := rows.Scan(&o.ID, &o.Ticker, &o.Side, &o.SignalDate, &o.Status, &o.FillDate, &o.Price,
			&o.Shares, &o.Value, &o.Commission, &o.Tax, &o.Note); err != nil {
			rows.Close()
			return err
		}
		past = append(past, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range past {
		e, _ := orderEntry(o)
		e.note = strings.TrimSpace("backfilled " + e.note)
		cash += e.cashChange
		if err := appendLedger(tx, b.Name, e, cash); err != nil {
			return err
		}
	}
	return nil
}

// writeLedger appends this save's events: the start of a new portfolio, the
// benchmark purchase, new orders, and orders that filled or were skipped, in
// the order fill() executed them, with the cash after each.
func writeLedger(tx *sql.Tx, b *book) error {
	var outcomes []entry
	for _, o := range b.orders {
		if !o.changed {
			continue
		}
		if e, ok := orderEntry(o); ok {
			outcomes = append(outcomes, e)
		}
	}
	sort.SliceStable(outcomes, func(i, j int) bool {
		if outcomes[i].date != outcomes[j].date {
			return outcomes[i].date < outcomes[j].date
		}
		return outcomes[i].side == "sell" && outcomes[j].side == "buy"
	})
	cash := b.Cash
	for _, e := range outcomes {
		cash -= e.cashChange
	}
	if b.isNew {
		if err := appendLedger(tx, b.Name, entry{kind: "start", date: b.StartedOn, cashChange: b.Capital}, b.Capital); err != nil {
			return err
		}
	}
	if b.benchNew {
		if err := appendLedger(tx, b.Name, entry{kind: "bench", date: b.BenchDate, ticker: b.BenchTicker, side: "buy",
			shares: b.BenchShares, value: b.BenchCost, note: "benchmark, held apart from the portfolio's cash"}, cash); err != nil {
			return err
		}
	}
	for _, e := range outcomes {
		cash += e.cashChange
		if err := appendLedger(tx, b.Name, e, cash); err != nil {
			return err
		}
	}
	for _, o := range b.orders {
		if o.changed && o.Status == "pending" {
			if err := appendLedger(tx, b.Name, entry{kind: "order", date: o.SignalDate, orderID: o.ID, ticker: o.Ticker,
				side: o.Side, value: o.Target, note: o.Reason}, b.Cash); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcile replays the ledger and compares it with the saved book: the cash
// must match, and the tickers bought and not yet sold must be the positions.
func reconcile(db *sql.DB, name string) error {
	var capital, cash float64
	if err := db.QueryRow(`SELECT initial_capital, cash FROM paper_portfolios WHERE name = ?`, name).
		Scan(&capital, &cash); err != nil {
		return err
	}
	var changes float64
	if err := db.QueryRow(`SELECT COALESCE(SUM(cash_change), 0) FROM paper_ledger WHERE portfolio = ?`, name).
		Scan(&changes); err != nil {
		return err
	}
	var problems []string
	if math.Abs(changes-cash) > 0.01 {
		problems = append(problems, fmt.Sprintf("ledger cash %.2f, book cash %.2f", changes, cash))
	}
	held := func(q string) ([]string, error) {
		rows, err := db.Query(q, name)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				return nil, err
			}
			out = append(out, t)
		}
		sort.Strings(out)
		return out, rows.Err()
	}
	fromLedger, err := held(`SELECT ticker FROM paper_ledger WHERE portfolio = ? AND kind = 'fill'
		GROUP BY ticker HAVING SUM(CASE side WHEN 'buy' THEN 1 ELSE -1 END) > 0`)
	if err != nil {
		return err
	}
	positions, err := held(`SELECT ticker FROM paper_positions WHERE portfolio = ?`)
	if err != nil {
		return err
	}
	if strings.Join(fromLedger, ",") != strings.Join(positions, ",") {
		problems = append(problems, fmt.Sprintf("ledger holds %v, positions %v", fromLedger, positions))
	}
	if len(problems) > 0 {
		return fmt.Errorf("ledger does not reconcile: %s", strings.Join(problems, "; "))
	}
	return nil
}
