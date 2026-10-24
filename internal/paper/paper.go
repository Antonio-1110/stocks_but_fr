// Package paper runs paper portfolios (issue #35): each strategy in radar.toml
// [paper] trades forward on live data, after real fees and tax, next to the
// same money held in 0050. Nothing here places a real order.
//
// It runs as a `radar collect` step after the collectors. Each run:
//
//  1. fills the orders signalled earlier at the open of the next trading day
//     (sells before buys, so the cash is there). As in the backtest, a buy
//     whose open is locked at limit-up is skipped, and a sell waits while the
//     open is locked at limit-down ([limits.tw], issue #53);
//  2. once a new trading day is fully collected, asks the strategy what to do
//     on its close and records the orders;
//  3. marks the portfolio and the benchmark at the latest close.
//
// Everything lives in the paper_* tables, so a run picks up where the last
// one stopped and the picks, once made, never change.
package paper

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

var taipei = time.FixedZone("Asia/Taipei", 8*60*60)

const dateLayout = "2006-01-02"

// staleDays: a held stock with no trades for this many trading days is sold
// at its last close (delisted, or suspended for good).
const staleDays = 20

// Owned by this package, not internal/store: nothing else writes them. They
// live in the state database (store.StateFile, issue #46).
const schema = `
CREATE TABLE IF NOT EXISTS state.paper_portfolios (
	name            TEXT PRIMARY KEY,
	started_on      TEXT NOT NULL,              -- price date of its first signal
	initial_capital REAL NOT NULL,
	cash            REAL NOT NULL,
	last_signal     TEXT NOT NULL DEFAULT '',   -- price date last signalled on
	last_rebalance  TEXT NOT NULL DEFAULT '',   -- YYYY-MM, for monthly strategies
	bench_ticker    TEXT NOT NULL,
	bench_date      TEXT NOT NULL DEFAULT '',   -- the benchmark's fill day
	bench_shares    REAL NOT NULL DEFAULT 0,    -- as of bench_date
	bench_cost      REAL NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS state.paper_positions (
	portfolio TEXT NOT NULL,
	ticker    TEXT NOT NULL,
	opened    TEXT NOT NULL,   -- fill day; shares are as of this day
	shares    REAL NOT NULL,
	cost      REAL NOT NULL,   -- NTD paid, commission included
	gross     REAL NOT NULL,   -- NTD bought before commission (stop-loss reference)
	PRIMARY KEY (portfolio, ticker)
);
CREATE TABLE IF NOT EXISTS state.paper_orders (
	id          INTEGER PRIMARY KEY,
	portfolio   TEXT NOT NULL,
	ticker      TEXT NOT NULL,
	side        TEXT NOT NULL,              -- buy | sell
	reason      TEXT NOT NULL,
	signal_date TEXT NOT NULL,
	target      REAL NOT NULL DEFAULT 0,    -- buys: NTD to put in
	status      TEXT NOT NULL,              -- pending | filled | skipped
	fill_date   TEXT NOT NULL DEFAULT '',
	price       REAL NOT NULL DEFAULT 0,    -- raw price traded at
	shares      REAL NOT NULL DEFAULT 0,
	value       REAL NOT NULL DEFAULT 0,    -- NTD traded, before costs
	commission  REAL NOT NULL DEFAULT 0,
	tax         REAL NOT NULL DEFAULT 0,
	pnl         REAL NOT NULL DEFAULT 0,    -- sells: after-cost proceeds minus cost
	note        TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS state.paper_equity (
	portfolio   TEXT NOT NULL,
	date        TEXT NOT NULL,
	cash        REAL NOT NULL,
	holdings    REAL NOT NULL,   -- market value at the close
	liquidation REAL NOT NULL,   -- cash + holdings after sell commission and tax
	bench       REAL,            -- benchmark the same way; NULL before it is bought
	PRIMARY KEY (portfolio, date)
);
CREATE TABLE IF NOT EXISTS state.paper_blocked (
	portfolio TEXT NOT NULL,
	ticker    TEXT NOT NULL,
	month     TEXT NOT NULL,     -- may not be bought again until revenue newer than this is out
	PRIMARY KEY (portfolio, ticker)
);
`

func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema + ledgerSchema); err != nil {
		return err
	}
	return store.MoveToState(db, "paper_portfolios", "paper_positions", "paper_orders", "paper_equity", "paper_blocked")
}

type position struct {
	Ticker string
	Opened string
	Shares float64
	Cost   float64
	Gross  float64
}

type order struct {
	ID                   int64
	Ticker, Side, Reason string
	SignalDate           string
	Target               float64
	Status, FillDate     string
	Price, Shares, Value float64
	Commission, Tax, PnL float64
	Note                 string
	changed              bool
	waited               int // opens it waited out at limit-down (sells, this run)
	halted               int // trading days it waited out with no trade (sells, this run)
}

// book is one portfolio's state, loaded, changed in memory, then saved.
type book struct {
	Name, StartedOn        string
	Capital, Cash          float64
	LastSignal             string
	LastRebalance          string
	BenchTicker, BenchDate string
	BenchShares, BenchCost float64
	pos                    map[string]*position
	orders                 []*order // pending ones, then new ones
	blocked                map[string]string
	isNew                  bool
	benchNew               bool // the benchmark was bought this run
}

// strategy turns the close of m.last() into orders.
type strategy interface {
	decide(m *market, b *book, h holdings) ([]*order, error)
}

// holdings is the book marked at the signal day's close.
type holdings struct {
	day    string
	equity float64            // cash + market value
	value  map[string]float64 // ticker -> market value
	leave  map[string]bool    // already has a pending sell
	buys   int                // pending buys
}

// Run is the `paper` collect step.
func Run(_ context.Context, cfg config.Config, st *store.Store) error {
	return run(st.DB, cfg, time.Now().In(taipei))
}

func run(db *sql.DB, cfg config.Config, now time.Time) error {
	if err := migrate(db); err != nil {
		return fmt.Errorf("paper tables: %w", err)
	}
	var since sql.NullString
	db.QueryRow(`SELECT MIN(signal_date) FROM paper_orders WHERE status = 'pending'`).Scan(&since)
	rm, cb := cfg.Strategy.RevenueMomentum, cfg.Strategy.CycleBottom
	days := max(cb.PriceMADays, cb.TurnoverDays, rm.StopMADays, staleDays) + 45 // + a month for the rebalance day
	days = max(days, activeDays(cfg))                                           // issues #70-#73
	m, err := loadMarket(db, days, since.String, cfg.Paper.MinCoverage)
	if err != nil {
		return err
	}
	if len(m.dates) == 0 {
		log.Printf("paper: no prices yet")
		return nil
	}
	if !m.ready {
		log.Printf("paper: newest price day still being collected; using %s", m.last())
	}
	var failed []string
	for _, name := range cfg.Paper.Strategies {
		s, err := strategyFor(name, cfg, db)
		if err == nil {
			err = step(db, cfg, m, name, s, now)
		}
		if err != nil {
			log.Printf("paper: %s: %v", name, err)
			failed = append(failed, name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("portfolios failed: %s", strings.Join(failed, ", "))
	}
	return nil
}

func strategyFor(name string, cfg config.Config, db *sql.DB) (strategy, error) {
	switch name {
	case "revenue_momentum":
		return momentum{cfg.Strategy.RevenueMomentum, db, cfg.Revenue.MinCoverage}, nil
	case "revenue_momentum_foreign":
		// The rm_score variant of backtest/strategies/foreign_flow.py (issue #67).
		p, f := cfg.Strategy.RevenueMomentum, cfg.Strategy.ForeignFlow
		p.ForeignWeight, p.ForeignDays = f.RMScore.ForeignWeight, f.RMScore.FlowDays
		if p.ForeignDays == 0 {
			p.ForeignDays = f.FlowDays
		}
		if p.ForeignWeight <= 0 || p.ForeignWeight > 1 || p.ForeignDays <= 0 {
			return nil, fmt.Errorf("[strategy.foreign_flow.rm_score] needs foreign_weight in (0, 1] and flow_days > 0")
		}
		return momentum{p, db, cfg.Revenue.MinCoverage}, nil
	case "cycle_bottom":
		return cycle{cfg.Strategy.CycleBottom, db, cfg.Revenue.MinCoverage}, nil
	case "record_revenue":
		return recordRevenue{cfg.Strategy.RecordRevenue, db, cfg.Revenue.MinCoverage}, nil
	case "breakout":
		return breakout{cfg.Strategy.Breakout, db}, nil
	case "window_dressing":
		return windowDressing{cfg.Strategy.WindowDressing, db}, nil
	case "pullback":
		return pullback{cfg.Strategy.Pullback, db}, nil
	}
	return nil, fmt.Errorf("unknown strategy %q (want revenue_momentum, revenue_momentum_foreign, cycle_bottom, "+
		"record_revenue, breakout, window_dressing or pullback)", name)
}

func step(db *sql.DB, cfg config.Config, m *market, name string, s strategy, now time.Time) error {
	b, err := load(db, name)
	if err != nil {
		return err
	}
	if b == nil {
		if !m.ready {
			return nil // start on a fully collected day
		}
		b = &book{Name: name, StartedOn: m.last(), Capital: cfg.Paper.InitialCapital,
			Cash: cfg.Paper.InitialCapital, BenchTicker: cfg.Paper.Benchmark,
			pos: map[string]*position{}, blocked: map[string]string{}, isNew: true}
		log.Printf("paper: %s: started with NTD %.0f on the %s close", name, b.Capital, b.StartedOn)
	}
	costs := cfg.Costs.TW

	lim := cfg.Limits.TW
	fill(m, b, costs, lim)
	fillBench(m, b, costs, lim)

	if m.ready && m.last() > b.LastSignal {
		h := mark(m, b, m.last())
		orders, err := s.decide(m, b, h)
		if err != nil {
			return err
		}
		for _, o := range orders {
			o.SignalDate, o.Status, o.changed = m.last(), "pending", true
			log.Printf("paper: %s: %s %s %s (%s)", name, o.Side, o.Ticker, m.names[o.Ticker], o.Reason)
		}
		b.orders = append(b.orders, orders...)
		b.LastSignal = m.last()
	}

	if err := save(db, b, equityRow(m, b, costs)); err != nil {
		return err
	}
	// The book is saved either way; a mismatch means a bug to look at.
	if err := reconcile(db, name); err != nil {
		log.Printf("paper: %s: %v", name, err)
	}
	return nil
}

// fill executes pending orders whose next trading day is in the window, in
// day order, sells first. A sell moves past opens locked at limit-down and
// days the stock didn't trade (#58), to the first open it can be sold at.
func fill(m *market, b *book, costs config.TWCosts, lim config.TWLimits) {
	type job struct {
		o   *order
		day string
	}
	var jobs []job
	for _, o := range b.orders {
		if o.Status != "pending" {
			continue
		}
		d := m.after(o.SignalDate)
		if o.Side == "sell" {
			first := d
			for o.waited, o.halted = 0, 0; d != ""; d = m.after(d) {
				if px, ok := m.on(o.Ticker, d); !ok || px.open <= 0 {
					o.halted++
				} else if m.locked(lim, o.Ticker, d) < 0 {
					o.waited++
				} else {
					break
				}
			}
			if d == "" && first != "" {
				if m.sinceLast(o.Ticker) >= staleDays {
					d = m.last() // stopped trading: sell() takes the last close
				} else {
					log.Printf("paper: %s: sell %s waits: %d day(s) at limit-down, %d with no trade so far",
						b.Name, o.Ticker, o.waited, o.halted)
				}
			}
		}
		if d != "" {
			jobs = append(jobs, job{o, d})
		}
	}
	sort.SliceStable(jobs, func(i, j int) bool {
		if jobs[i].day != jobs[j].day {
			return jobs[i].day < jobs[j].day
		}
		return jobs[i].o.Side == "sell" && jobs[j].o.Side == "buy"
	})
	for _, j := range jobs {
		if j.o.Side == "sell" {
			sell(m, b, j.o, j.day, costs)
		} else {
			buy(m, b, j.o, j.day, costs, lim)
		}
	}
}

func sell(m *market, b *book, o *order, day string, costs config.TWCosts) {
	p := b.pos[o.Ticker]
	if p == nil {
		o.Status, o.Note, o.changed = "skipped", "not held", true
		return
	}
	px, ok := m.on(o.Ticker, day)
	price := px.open
	if !ok || price <= 0 {
		// No trade that day. Wait for it to trade again, unless it has
		// stopped for good: then take the last close, as the backtest does.
		if m.sinceLast(o.Ticker) < staleDays {
			return
		}
		if px, ok = m.lastOn(o.Ticker, day); !ok {
			return
		}
		price = px.close
		o.Note = "no trades since " + px.date + ", sold at its last close"
	} else if o.waited > 0 || o.halted > 0 {
		var why []string
		if o.waited > 0 {
			why = append(why, fmt.Sprintf("opened at limit-down %d day(s)", o.waited))
		}
		if o.halted > 0 {
			why = append(why, fmt.Sprintf("didn't trade %d day(s)", o.halted))
		}
		o.Note = strings.Join(why, ", ") + ", sold at the next open"
	}
	opened, ok := m.exact(o.Ticker, p.Opened)
	if !ok {
		opened = px
	}
	shares := p.Shares * px.factor() / opened.factor()
	value := shares * price
	comm := costs.Commission(value)
	tax := sellTax(costs, o.Ticker, value)
	b.Cash += value - comm - tax
	o.Status, o.FillDate, o.changed = "filled", day, true
	o.Price, o.Shares, o.Value, o.Commission, o.Tax = price, shares, value, comm, tax
	o.PnL = value - comm - tax - p.Cost
	delete(b.pos, o.Ticker)
}

func buy(m *market, b *book, o *order, day string, costs config.TWCosts, lim config.TWLimits) {
	o.changed = true
	if b.pos[o.Ticker] != nil {
		o.Status, o.Note = "skipped", "already held"
		return
	}
	px, ok := m.on(o.Ticker, day)
	if !ok || px.open <= 0 {
		o.Status, o.Note = "skipped", "no trade on "+day
		return
	}
	if m.locked(lim, o.Ticker, day) > 0 {
		o.Status, o.Note = "skipped", "opened at limit-up on "+day
		return
	}
	value := math.Min(o.Target, b.Cash)
	for value > 0 && value+costs.Commission(value) > b.Cash {
		value = b.Cash - costs.Commission(value)
	}
	if value <= costs.MinCommission {
		o.Status, o.Note = "skipped", "not enough cash"
		return
	}
	comm := costs.Commission(value)
	b.Cash -= value + comm
	shares := value / px.open
	b.pos[o.Ticker] = &position{Ticker: o.Ticker, Opened: day, Shares: shares, Cost: value + comm, Gross: value}
	o.Status, o.FillDate = "filled", day
	o.Price, o.Shares, o.Value, o.Commission = px.open, shares, value, comm
}

// fillBench puts the starting capital into the benchmark at the open of the
// first trading day after the portfolio started, like the portfolio's first
// fills, or of the next day whose open isn't locked at limit-up.
func fillBench(m *market, b *book, costs config.TWCosts, lim config.TWLimits) {
	if b.BenchDate != "" {
		return
	}
	for d := m.after(b.StartedOn); d != ""; d = m.after(d) {
		px, ok := m.on(b.BenchTicker, d)
		if !ok || px.open <= 0 || m.locked(lim, b.BenchTicker, d) > 0 {
			continue
		}
		value := b.Capital
		for value+costs.Commission(value) > b.Capital {
			value = b.Capital - costs.Commission(value)
		}
		b.BenchDate, b.BenchShares, b.BenchCost = d, value/px.open, b.Capital
		b.benchNew = true
		return
	}
}

// worth is a holding's shares and close on d, dividends and splits since
// `opened` included.
func worth(m *market, ticker, opened string, shares float64, d string) float64 {
	px, ok := m.lastOn(ticker, d)
	if !ok {
		return 0
	}
	start, ok := m.exact(ticker, opened)
	if !ok {
		start = px
	}
	return shares * px.factor() / start.factor() * px.close
}

func mark(m *market, b *book, d string) holdings {
	h := holdings{day: d, equity: b.Cash, value: map[string]float64{}, leave: map[string]bool{}}
	for t, p := range b.pos {
		v := worth(m, t, p.Opened, p.Shares, d)
		h.value[t] = v
		h.equity += v
	}
	for _, o := range b.orders {
		if o.Status != "pending" {
			continue
		}
		if o.Side == "sell" {
			h.leave[o.Ticker] = true
		} else {
			h.buys++
		}
	}
	return h
}

type equity struct {
	date                        string
	cash, holdings, liquidation float64
	bench                       sql.NullFloat64
}

// equityRow marks the portfolio and the benchmark at the newest usable close
// as if everything were sold there: after commission and tax.
func equityRow(m *market, b *book, costs config.TWCosts) equity {
	d := m.last()
	e := equity{date: d, cash: b.Cash, liquidation: b.Cash}
	for t, p := range b.pos {
		v := worth(m, t, p.Opened, p.Shares, d)
		e.holdings += v
		e.liquidation += v - costs.Commission(v) - sellTax(costs, t, v)
	}
	if b.BenchDate != "" && b.BenchDate <= d {
		v := worth(m, b.BenchTicker, b.BenchDate, b.BenchShares, d)
		e.bench = sql.NullFloat64{Float64: v - costs.Commission(v) - sellTax(costs, b.BenchTicker, v), Valid: true}
	}
	return e
}

func sellTax(c config.TWCosts, ticker string, value float64) float64 {
	rate := c.SellTax
	if strings.HasPrefix(ticker, "00") {
		rate = c.ETFSellTax
	}
	return math.Floor(value * rate)
}

func load(db *sql.DB, name string) (*book, error) {
	b := &book{Name: name, pos: map[string]*position{}, blocked: map[string]string{}}
	err := db.QueryRow(`SELECT started_on, initial_capital, cash, last_signal, last_rebalance,
		bench_ticker, bench_date, bench_shares, bench_cost FROM paper_portfolios WHERE name = ?`, name).
		Scan(&b.StartedOn, &b.Capital, &b.Cash, &b.LastSignal, &b.LastRebalance,
			&b.BenchTicker, &b.BenchDate, &b.BenchShares, &b.BenchCost)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pos, err := loadPositions(db, name)
	if err != nil {
		return nil, err
	}
	for _, p := range pos {
		b.pos[p.Ticker] = p
	}
	if b.orders, err = loadOrders(db, name, `status = 'pending'`, 0); err != nil {
		return nil, err
	}
	q, err := db.Query(`SELECT ticker, month FROM paper_blocked WHERE portfolio = ?`, name)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	for q.Next() {
		var t, mo string
		if err := q.Scan(&t, &mo); err != nil {
			return nil, err
		}
		b.blocked[t] = mo
	}
	return b, q.Err()
}

func loadPositions(db *sql.DB, name string) ([]*position, error) {
	q, err := db.Query(`SELECT ticker, opened, shares, cost, gross FROM paper_positions
		WHERE portfolio = ? ORDER BY opened, ticker`, name)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	var out []*position
	for q.Next() {
		p := &position{}
		if err := q.Scan(&p.Ticker, &p.Opened, &p.Shares, &p.Cost, &p.Gross); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, q.Err()
}

// loadOrders returns orders matching where, newest first (limit 0 = all).
func loadOrders(db *sql.DB, name, where string, limit int) ([]*order, error) {
	sqlText := `SELECT id, ticker, side, reason, signal_date, target, status, fill_date,
		price, shares, value, commission, tax, pnl, note FROM paper_orders
		WHERE portfolio = ? AND ` + where + ` ORDER BY signal_date DESC, id DESC`
	if limit > 0 {
		sqlText += fmt.Sprintf(" LIMIT %d", limit)
	}
	q, err := db.Query(sqlText, name)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	var out []*order
	for q.Next() {
		o := &order{}
		if err := q.Scan(&o.ID, &o.Ticker, &o.Side, &o.Reason, &o.SignalDate, &o.Target, &o.Status,
			&o.FillDate, &o.Price, &o.Shares, &o.Value, &o.Commission, &o.Tax, &o.PnL, &o.Note); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, q.Err()
}

func save(db *sql.DB, b *book, e equity) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := backfillLedger(tx, b); err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO paper_portfolios (name, started_on, initial_capital,
		cash, last_signal, last_rebalance, bench_ticker, bench_date, bench_shares, bench_cost)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, b.Name, b.StartedOn, b.Capital, b.Cash, b.LastSignal,
		b.LastRebalance, b.BenchTicker, b.BenchDate, b.BenchShares, b.BenchCost); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM paper_positions WHERE portfolio = ?`, b.Name); err != nil {
		return err
	}
	for _, p := range b.pos {
		if _, err := tx.Exec(`INSERT INTO paper_positions (portfolio, ticker, opened, shares, cost, gross)
			VALUES (?, ?, ?, ?, ?, ?)`, b.Name, p.Ticker, p.Opened, p.Shares, p.Cost, p.Gross); err != nil {
			return err
		}
	}
	for _, o := range b.orders {
		if !o.changed {
			continue
		}
		args := []any{o.Status, o.FillDate, o.Price, o.Shares, o.Value, o.Commission, o.Tax, o.PnL, o.Note}
		if o.ID == 0 {
			var res sql.Result
			res, err = tx.Exec(`INSERT INTO paper_orders (portfolio, ticker, side, reason, signal_date, target,
				status, fill_date, price, shares, value, commission, tax, pnl, note)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				append([]any{b.Name, o.Ticker, o.Side, o.Reason, o.SignalDate, o.Target}, args...)...)
			if err == nil {
				o.ID, err = res.LastInsertId()
			}
		} else {
			_, err = tx.Exec(`UPDATE paper_orders SET status = ?, fill_date = ?, price = ?, shares = ?,
				value = ?, commission = ?, tax = ?, pnl = ?, note = ? WHERE id = ?`, append(args, o.ID)...)
		}
		if err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM paper_blocked WHERE portfolio = ?`, b.Name); err != nil {
		return err
	}
	for t, mo := range b.blocked {
		if _, err := tx.Exec(`INSERT INTO paper_blocked (portfolio, ticker, month) VALUES (?, ?, ?)`,
			b.Name, t, mo); err != nil {
			return err
		}
	}
	if err := writeLedger(tx, b); err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	if e.date >= b.StartedOn {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO paper_equity (portfolio, date, cash, holdings, liquidation, bench)
			VALUES (?, ?, ?, ?, ?, ?)`, b.Name, e.date, e.cash, e.holdings, e.liquidation, e.bench); err != nil {
			return err
		}
	}
	return tx.Commit()
}
