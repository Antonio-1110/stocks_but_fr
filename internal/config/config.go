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
