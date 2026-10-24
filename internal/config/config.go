// Package config loads radar.toml, the single source of costs and settings.
package config

import (
	"fmt"
	"math"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Run      Run      `toml:"run"`
	Costs    Costs    `toml:"costs"`
	Limits   Limits   `toml:"limits"`
	Backtest Backtest `toml:"backtest"`
	Revenue  Revenue  `toml:"revenue"`
	Site     Site     `toml:"site"`
	Strategy Strategy `toml:"strategy"`
	Alerts   Alerts   `toml:"alerts"`
	Paper    Paper    `toml:"paper"`
}

type Run struct {
	DBPath       string `toml:"db_path"`
	PublicDir    string `toml:"public_dir"`
	HistoryStart string `toml:"history_start"`
}

type Costs struct {
	TW TWCosts `toml:"tw"`
}

type TWCosts struct {
	CommissionRate  float64 `toml:"commission_rate"`
	BrokerDiscount  float64 `toml:"broker_discount"`
	MinCommission   float64 `toml:"min_commission"`
	SellTax         float64 `toml:"sell_tax"`
	ETFSellTax      float64 `toml:"etf_sell_tax"`
	DayTradeSellTax float64 `toml:"day_trade_sell_tax"`
}

type Limits struct {
	TW TWLimits `toml:"tw"`
}

// TWLimits is [limits.tw]: Taiwan's daily price limit (issue #53). A buy whose
// open is locked at limit-up is skipped; a sell waits while the open is locked
// at limit-down.
type TWLimits struct {
	Pct       float64 `toml:"pct"`       // limit from Since on...
	OldPct    float64 `toml:"old_pct"`   // ...and before it
	Since     string  `toml:"since"`     // YYYY-MM-DD
	Tolerance float64 `toml:"tolerance"` // a move this close to the limit counts as locked
}

// At is the limit on trading day d (YYYY-MM-DD).
func (l TWLimits) At(d string) float64 {
	if d >= l.Since {
		return l.Pct
	}
	return l.OldPct
}

// Locked is +1 when an open of open against a previous close of prev (both
// on the same adjusted scale) is at limit-up on day d, -1 at limit-down, and
// 0 otherwise or when the limits are off (a limit of 0).
func (l TWLimits) Locked(d string, open, prev float64) int {
	limit := l.At(d)
	if limit <= 0 || open <= 0 || prev <= 0 {
		return 0
	}
	move := open/prev - 1
	switch {
	case move >= limit-l.Tolerance:
		return 1
	case move <= -(limit - l.Tolerance):
		return -1
	}
	return 0
}

type Backtest struct {
	InitialCapital float64 `toml:"initial_capital"`
	Benchmark      string  `toml:"benchmark"`
	DesignStart    string  `toml:"design_start"`
	DesignEnd      string  `toml:"design_end"`
	TestStart      string  `toml:"test_start"`
}

type Revenue struct {
	LowBase LowBase `toml:"low_base"`
	// MinCoverage: paper portfolios and alerts use a revenue month only once
	// this share of listed stocks has it (issue #37). 0 turns the check off.
	MinCoverage float64 `toml:"min_coverage"`
}

// LowBase decides when a revenue YoY is off a base too small to mean anything.
type LowBase struct {
	MinBaseNTD    float64 `toml:"min_base_ntd"`
	MinBaseRatio  float64 `toml:"min_base_ratio"`
	TypicalMonths int     `toml:"typical_months"`
}

type Site struct {
	Unpriced Unpriced `toml:"unpriced"`
}

// Unpriced weights the dashboard's "growth not yet priced in" ranking. Each
// weight multiplies a 0..1 percentile rank among the eligible stocks.
type Unpriced struct {
	MinGrowthPct   float64 `toml:"min_growth_pct"`
	MinTurnoverNTD float64 `toml:"min_turnover_ntd"`
	GrowthWeight   float64 `toml:"growth_weight"`
	AccelWeight    float64 `toml:"accel_weight"`
	PriceWeight    float64 `toml:"price_weight"`
	HighWeight     float64 `toml:"high_weight"`
	FlowWeight     float64 `toml:"flow_weight"`
}

// Strategy holds the strategy sections the Go side reads (the backtester reads
// all of them itself). Alerts reuse revenue_momentum's entry filters.
type Strategy struct {
	RevenueMomentum RevenueMomentum `toml:"revenue_momentum"`
	CycleBottom     CycleBottom     `toml:"cycle_bottom"`
	ForeignFlow     ForeignFlow     `toml:"foreign_flow"`
	RecordRevenue   RecordRevenue   `toml:"record_revenue"`
	Breakout        Breakout        `toml:"breakout"`
	WindowDressing  WindowDressing  `toml:"window_dressing"`
	Pullback        Pullback        `toml:"pullback"`
}

// RevenueMomentum is [strategy.revenue_momentum]; see backtest/strategies/revenue_momentum.py.
type RevenueMomentum struct {
	TopN               int     `toml:"top_n"`
	ExitRank           int     `toml:"exit_rank"`
	YoYMonths          int     `toml:"yoy_months"`
	MinYoYPct          float64 `toml:"min_yoy_pct"`
	HighDays           int     `toml:"high_days"`
	MaxBelowHigh       float64 `toml:"max_below_high"`
	HighWeight         float64 `toml:"high_weight"`
	MinADVNTD          float64 `toml:"min_adv_ntd"`
	ADVDays            int     `toml:"adv_days"`
	RevenueDeadlineDay int     `toml:"revenue_deadline_day"`
	TrustFilter        bool    `toml:"trust_filter"`
	TrustDays          int     `toml:"trust_days"`
	StopLoss           float64 `toml:"stop_loss"`
	StopMADays         int     `toml:"stop_ma_days"`
	// Set in code, not read from this section (the backtester rejects unknown
	// keys there): the paper portfolio revenue_momentum_foreign copies them
	// from [strategy.foreign_flow.rm_score] (issue #67).
	ForeignWeight float64 `toml:"-"` // weight of the foreign rank in the score; 0 = off
	ForeignDays   int     `toml:"-"` // flow dates in the foreign intensity window
}

// ForeignFlow is the part of [strategy.foreign_flow] the Go side reads; see
// backtest/strategies/foreign_flow.py. Only the rm_score variant runs live.
type ForeignFlow struct {
	FlowDays int           `toml:"flow_days"`
	RMScore  ForeignScored `toml:"rm_score"`
}

// ForeignScored is one revenue-variant table under [strategy.foreign_flow].
type ForeignScored struct {
	FlowDays      int     `toml:"flow_days"` // 0 = the section's flow_days
	ForeignWeight float64 `toml:"foreign_weight"`
}

// CycleBottom is [strategy.cycle_bottom]; see backtest/strategies/cycle_bottom.py.
type CycleBottom struct {
	Industries        []string `toml:"industries"`
	Tickers           []string `toml:"tickers"`
	YoYWindow         int      `toml:"yoy_window"`
	TroughMonths      int      `toml:"trough_months"`
	MinNegativeMonths int      `toml:"min_negative_months"`
	ImproveMonths     int      `toml:"improve_months"`
	MaxEntryYoY       float64  `toml:"max_entry_yoy"`
	RolloverMonths    int      `toml:"rollover_months"`
	StopLoss          float64  `toml:"stop_loss"`
	PriceMADays       int      `toml:"price_ma_days"`
	MaxPositions      int      `toml:"max_positions"`
	MinAvgTurnover    float64  `toml:"min_avg_turnover"`
	TurnoverDays      int      `toml:"turnover_days"`
}

// RecordRevenue is [strategy.record_revenue]; see backtest/strategies/record_revenue.py (issue #70).
type RecordRevenue struct {
	MaxPositions       int     `toml:"max_positions"`
	RecordMonths       int     `toml:"record_months"`
	MinYoYPct          float64 `toml:"min_yoy_pct"`
	MinBaseNTD         float64 `toml:"min_base_ntd"`
	MinADVNTD          float64 `toml:"min_adv_ntd"`
	ADVDays            int     `toml:"adv_days"`
	HoldDays           int     `toml:"hold_days"`
	TrailStop          float64 `toml:"trail_stop"`
	RevenueDeadlineDay int     `toml:"revenue_deadline_day"`
}

// Breakout is [strategy.breakout]; see backtest/strategies/breakout.py (issue #71).
type Breakout struct {
	MaxPositions int     `toml:"max_positions"`
	BreakoutDays int     `toml:"breakout_days"`
	VolumeDays   int     `toml:"volume_days"`
	VolumeMult   float64 `toml:"volume_mult"`
	MinADVNTD    float64 `toml:"min_adv_ntd"`
	ADVDays      int     `toml:"adv_days"`
	RegimeTicker string  `toml:"regime_ticker"`
	RegimeDays   int     `toml:"regime_days"`
	ATRDays      int     `toml:"atr_days"`
	ATRMult      float64 `toml:"atr_mult"`
}

// WindowDressing is [strategy.window_dressing]; see backtest/strategies/window_dressing.py (issue #72).
type WindowDressing struct {
	Quarters     []int   `toml:"quarters"`
	EntryDays    int     `toml:"entry_days"`
	ExitDays     int     `toml:"exit_days"`
	FlowDays     int     `toml:"flow_days"`
	MaxPositions int     `toml:"max_positions"`
	MinADVNTD    float64 `toml:"min_adv_ntd"`
	ADVDays      int     `toml:"adv_days"`
	SkipLargest  int     `toml:"skip_largest"`
	StopLoss     float64 `toml:"stop_loss"`
}

// Pullback is [strategy.pullback]; see backtest/strategies/pullback.py (issue #73).
type Pullback struct {
	MaxPositions int     `toml:"max_positions"`
	MinADVNTD    float64 `toml:"min_adv_ntd"`
	ADVDays      int     `toml:"adv_days"`
	TrendDays    int     `toml:"trend_days"`
	StrengthDays int     `toml:"strength_days"`
	StrengthPct  float64 `toml:"strength_pct"`
	RSIDays      int     `toml:"rsi_days"`
	EntryRSI     float64 `toml:"entry_rsi"`
	ExitMADays   int     `toml:"exit_ma_days"`
	MaxHoldDays  int     `toml:"max_hold_days"`
	StopLoss     float64 `toml:"stop_loss"`
}

// Paper is [paper]: portfolios that follow a strategy forward on live data (issue #35).
type Paper struct {
	Strategies     []string `toml:"strategies"`
	InitialCapital float64  `toml:"initial_capital"`
	Benchmark      string   `toml:"benchmark"`
	MinCoverage    float64  `toml:"min_coverage"`
}

// Alerts is [alerts]: Telegram messages when a stock newly passes a rule (issue #34).
type Alerts struct {
	Rules        []string `toml:"rules"`
	UnpricedTop  int      `toml:"unpriced_top"`
	UnpricedExit int      `toml:"unpriced_exit"`
}

// Load reads the file at path. Keys missing from the file keep their defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Default mirrors the values shipped in radar.toml.
func Default() Config {
	return Config{
		Run: Run{DBPath: "data/radar.db", PublicDir: "public", HistoryStart: "2010-01-01"},
		Costs: Costs{TW: TWCosts{
			CommissionRate:  0.001425,
			BrokerDiscount:  0.6,
			MinCommission:   20,
			SellTax:         0.003,
			ETFSellTax:      0.001,
			DayTradeSellTax: 0.0015,
		}},
		Limits: Limits{TW: TWLimits{Pct: 0.10, OldPct: 0.07, Since: "2015-06-01", Tolerance: 0.005}},
		Backtest: Backtest{
			InitialCapital: 5_000_000,
			Benchmark:      "0050",
			DesignStart:    "2012-01-01",
			DesignEnd:      "2019-12-31",
			TestStart:      "2020-01-01",
		},
		Revenue: Revenue{LowBase: LowBase{MinBaseNTD: 10_000_000, MinBaseRatio: 0.3, TypicalMonths: 12}, MinCoverage: 0.9},
		Site: Site{Unpriced: Unpriced{
			MinGrowthPct:   20,
			MinTurnoverNTD: 10_000_000,
			GrowthWeight:   0.35,
			AccelWeight:    0.15,
			PriceWeight:    0.2,
			HighWeight:     0.15,
			FlowWeight:     0.15,
		}},
		Strategy: Strategy{
			RevenueMomentum: RevenueMomentum{
				TopN:               12,
				ExitRank:           30,
				YoYMonths:          3,
				MinYoYPct:          10,
				HighDays:           252,
				MaxBelowHigh:       0.10,
				HighWeight:         0.5,
				MinADVNTD:          10_000_000,
				ADVDays:            20,
				RevenueDeadlineDay: 10,
				TrustDays:          10,
				StopLoss:           0.15,
			},
			CycleBottom: CycleBottom{
				Industries:        []string{"半導體業", "光電業", "航運業", "鋼鐵工業", "塑膠工業"},
				Tickers:           []string{},
				YoYWindow:         3,
				TroughMonths:      12,
				MinNegativeMonths: 6,
				ImproveMonths:     2,
				MaxEntryYoY:       10,
				RolloverMonths:    2,
				StopLoss:          0.25,
				PriceMADays:       60,
				MaxPositions:      10,
				MinAvgTurnover:    20_000_000,
				TurnoverDays:      20,
			},
			ForeignFlow: ForeignFlow{FlowDays: 60, RMScore: ForeignScored{ForeignWeight: 0.33}},
			RecordRevenue: RecordRevenue{MaxPositions: 8, RecordMonths: 36, MinYoYPct: 20, MinBaseNTD: 10_000_000,
				MinADVNTD: 20_000_000, ADVDays: 20, HoldDays: 20, TrailStop: 0.10, RevenueDeadlineDay: 10},
			Breakout: Breakout{MaxPositions: 8, BreakoutDays: 55, VolumeDays: 50, VolumeMult: 2, MinADVNTD: 30_000_000,
				ADVDays: 20, RegimeTicker: "0050", RegimeDays: 200, ATRDays: 20, ATRMult: 3},
			WindowDressing: WindowDressing{Quarters: []int{3, 6, 9, 12}, EntryDays: 28, ExitDays: 3, FlowDays: 20,
				MaxPositions: 10, MinADVNTD: 20_000_000, ADVDays: 20, SkipLargest: 100, StopLoss: 0.10},
			Pullback: Pullback{MaxPositions: 10, MinADVNTD: 30_000_000, ADVDays: 20, TrendDays: 200, StrengthDays: 120,
				StrengthPct: 0.2, RSIDays: 2, EntryRSI: 10, ExitMADays: 5, MaxHoldDays: 10, StopLoss: 0.10},
		},
		Alerts: Alerts{Rules: []string{"revenue_momentum", "unpriced"}, UnpricedTop: 10, UnpricedExit: 25},
		Paper: Paper{
			Strategies: []string{"revenue_momentum", "cycle_bottom", "revenue_momentum_foreign",
				"record_revenue", "breakout", "window_dressing", "pullback"},
			InitialCapital: 5_000_000,
			Benchmark:      "0050",
			MinCoverage:    0.9,
		},
	}
}

func (c Config) validate() error {
	rates := map[string]float64{
		"costs.tw.commission_rate":    c.Costs.TW.CommissionRate,
		"costs.tw.broker_discount":    c.Costs.TW.BrokerDiscount,
		"costs.tw.sell_tax":           c.Costs.TW.SellTax,
		"costs.tw.etf_sell_tax":       c.Costs.TW.ETFSellTax,
		"costs.tw.day_trade_sell_tax": c.Costs.TW.DayTradeSellTax,
		"revenue.min_coverage":        c.Revenue.MinCoverage,
		"limits.tw.pct":               c.Limits.TW.Pct,
		"limits.tw.old_pct":           c.Limits.TW.OldPct,
		"limits.tw.tolerance":         c.Limits.TW.Tolerance,
	}
	for name, v := range rates {
		if v < 0 || v > 1 {
			return fmt.Errorf("%s = %v, want a fraction between 0 and 1", name, v)
		}
	}
	if c.Costs.TW.MinCommission < 0 {
		return fmt.Errorf("costs.tw.min_commission must not be negative")
	}
	if c.Run.DBPath == "" {
		return fmt.Errorf("run.db_path is empty")
	}
	return nil
}

// Commission is the broker fee for one order of the given NTD value.
func (t TWCosts) Commission(value float64) float64 {
	return math.Max(math.Floor(value*t.CommissionRate*t.BrokerDiscount), t.MinCommission)
}
