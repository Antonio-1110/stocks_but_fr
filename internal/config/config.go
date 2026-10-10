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
	Backtest Backtest `toml:"backtest"`
	Revenue  Revenue  `toml:"revenue"`
	Site     Site     `toml:"site"`
	Strategy Strategy `toml:"strategy"`
	Alerts   Alerts   `toml:"alerts"`
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

type Backtest struct {
	InitialCapital float64 `toml:"initial_capital"`
	Benchmark      string  `toml:"benchmark"`
	DesignStart    string  `toml:"design_start"`
	DesignEnd      string  `toml:"design_end"`
	TestStart      string  `toml:"test_start"`
}

type Revenue struct {
	LowBase LowBase `toml:"low_base"`
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
		Backtest: Backtest{
			InitialCapital: 5_000_000,
			Benchmark:      "0050",
			DesignStart:    "2012-01-01",
			DesignEnd:      "2019-12-31",
			TestStart:      "2020-01-01",
		},
		Revenue: Revenue{LowBase: LowBase{MinBaseNTD: 10_000_000, MinBaseRatio: 0.3, TypicalMonths: 12}},
		Site: Site{Unpriced: Unpriced{
			MinGrowthPct:   20,
			MinTurnoverNTD: 10_000_000,
			GrowthWeight:   0.35,
			AccelWeight:    0.15,
			PriceWeight:    0.2,
			HighWeight:     0.15,
			FlowWeight:     0.15,
		}},
		Strategy: Strategy{RevenueMomentum: RevenueMomentum{
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
		}},
		Alerts: Alerts{Rules: []string{"revenue_momentum", "unpriced"}, UnpricedTop: 10, UnpricedExit: 25},
	}
}

func (c Config) validate() error {
	rates := map[string]float64{
		"costs.tw.commission_rate":    c.Costs.TW.CommissionRate,
		"costs.tw.broker_discount":    c.Costs.TW.BrokerDiscount,
		"costs.tw.sell_tax":           c.Costs.TW.SellTax,
		"costs.tw.etf_sell_tax":       c.Costs.TW.ETFSellTax,
		"costs.tw.day_trade_sell_tax": c.Costs.TW.DayTradeSellTax,
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
