package paper

import (
	"strings"
	"testing"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
)

// revenue_momentum_foreign is revenue momentum with the weight and window of
// [strategy.foreign_flow.rm_score] (issue #67).
func TestForeignStrategyParams(t *testing.T) {
	cfg := config.Default()
	cfg.Strategy.ForeignFlow.RMScore.ForeignWeight = 0.33
	s, err := strategyFor("revenue_momentum_foreign", cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := s.(momentum).p
	if p.ForeignWeight != 0.33 || p.ForeignDays != 60 || p.TopN != cfg.Strategy.RevenueMomentum.TopN {
		t.Errorf("params %+v, want weight 0.33 over 60 days on revenue momentum's settings", p)
	}
	if plain, _ := strategyFor("revenue_momentum", cfg, nil); plain.(momentum).p.ForeignWeight != 0 {
		t.Error("plain revenue_momentum picked up the foreign weight")
	}

	cfg.Strategy.ForeignFlow.RMScore.ForeignWeight = 0
	if _, err := strategyFor("revenue_momentum_foreign", cfg, nil); err == nil || !strings.Contains(err.Error(), "rm_score") {
		t.Errorf("no weight: err %v, want one naming rm_score", err)
	}
}

func TestForeignStrategyFromRadarToml(t *testing.T) {
	cfg, err := config.Load("../../radar.toml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strategyFor("revenue_momentum_foreign", cfg, nil); err != nil {
		t.Fatal(err)
	}
}
