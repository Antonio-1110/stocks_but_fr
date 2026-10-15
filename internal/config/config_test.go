package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadRepoConfig(t *testing.T) {
	cfg, err := Load("../../radar.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Errorf("radar.toml and Default() disagree:\nfile:    %+v\ndefault: %+v", cfg, Default())
	}
}

func TestLoadKeepsDefaultsAndRejectsBadRates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "radar.toml")

	os.WriteFile(path, []byte("[costs.tw]\nbroker_discount = 0.28\n"), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Costs.TW.BrokerDiscount != 0.28 || cfg.Costs.TW.SellTax != 0.003 {
		t.Errorf("got %+v", cfg.Costs.TW)
	}

	os.WriteFile(path, []byte("[costs.tw]\nsell_tax = 3\n"), 0o644)
	if _, err := Load(path); err == nil {
		t.Error("sell_tax = 3 should be rejected")
	}
}

func TestCommission(t *testing.T) {
	c := Default().Costs.TW
	if got := c.Commission(1000); got != 20 {
		t.Errorf("small order: got %v, want minimum 20", got)
	}
	// 1,000,000 * 0.001425 * 0.6 = 855
	if got := c.Commission(1_000_000); got != 855 {
		t.Errorf("1M order: got %v, want 855", got)
	}
}

func TestLimitsLocked(t *testing.T) {
	l := Default().Limits.TW
	for _, c := range []struct {
		day        string
		open, prev float64
		want       int
		what       string
	}{
		{"2020-01-02", 11, 10, 1, "+10%"},
		{"2020-01-02", 10.95, 9.99, 1, "limit price rounded down to the tick (+9.6%)"},
		{"2020-01-02", 10.9, 10, 0, "+9%"},
		{"2020-01-02", 9, 10, -1, "-10%"},
		{"2020-01-02", 9.35, 10, 0, "-6.5%"},
		{"2015-05-29", 10.7, 10, 1, "+7% before 2015-06-01"},
		{"2015-05-29", 9.3, 10, -1, "-7% before 2015-06-01"},
		{"2015-06-01", 10.7, 10, 0, "+7% from 2015-06-01"},
		{"2020-01-02", 11, 0, 0, "no previous close"},
	} {
		if got := l.Locked(c.day, c.open, c.prev); got != c.want {
			t.Errorf("%s: got %d, want %d", c.what, got, c.want)
		}
	}
	if (TWLimits{}).Locked("2020-01-02", 20, 10) != 0 {
		t.Error("zero limits should never lock")
	}
}
