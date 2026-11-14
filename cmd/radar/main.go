// Command radar runs the pipeline: collect data into SQLite, then render the
// static dashboard. The Python backtester in backtest/ reads the same database.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/alerts"
	"github.com/Antonio-1110/stocks_but_fr/internal/alpha"
	"github.com/Antonio-1110/stocks_but_fr/internal/calendar"
	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/health"
	"github.com/Antonio-1110/stocks_but_fr/internal/market/tw"
	"github.com/Antonio-1110/stocks_but_fr/internal/market/tw/flows"
	"github.com/Antonio-1110/stocks_but_fr/internal/market/tw/margin"
	"github.com/Antonio-1110/stocks_but_fr/internal/market/tw/revenue"
	"github.com/Antonio-1110/stocks_but_fr/internal/market/tw/valuation"
	"github.com/Antonio-1110/stocks_but_fr/internal/market/us"
	"github.com/Antonio-1110/stocks_but_fr/internal/paper"
	"github.com/Antonio-1110/stocks_but_fr/internal/ratings"
	"github.com/Antonio-1110/stocks_but_fr/internal/site"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
	"github.com/Antonio-1110/stocks_but_fr/internal/validate"
)

// A step is one unit of work. A failing collector is logged and skipped so the
// rest of the run still happens.
type step struct {
	name string
	run  func(context.Context, config.Config, *store.Store) error
}

// collectors run in order on `radar collect`. Each data issue adds its line here.
var collectors = []step{
	{"tw-prices", tw.Collect},       // fills the TW universe first
	{"tw-master", tw.CollectMaster}, // listing dates and industry history; after prices (fallback dates)
	{"tw-revenue", revenue.Collect}, // needs the TW universe in companies
	{"tw-flows", flows.Collect},     // history needs the TW universe; daily reports don't
	{"tw-margin", margin.Collect},   // issue #84: 融資融券, needs the TW universe and price days
	// issue #85: P/E, P/B, yield, foreign holdings; the last FinMind step
	{"tw-valuation", valuation.Collect},
	{"tw-calendar", calendar.Collect},
	{"us-prices", us.Collect}, // issue #2: US large caps, own tables
	{"ratings", ratings.Run},  // issue #78: rate stocks, score past ratings; after both markets' prices
	{"alerts", alerts.Run},    // last: Telegram on stocks newly passing a rule, from the fresh data
	{"paper", paper.Run},      // paper portfolios trade on the same fresh data
}

// renderers run in order on `radar render`.
var renderers = []step{
	{"site", site.Render},
	{"calendar", calendar.Render}, // after site: reuses its static/style.css
	{"paper", paper.Render},       // after site: reuses its static/style.css
	{"health", validate.Render},   // after site: reuses its static/style.css
	{"alpha", alpha.Render},       // after site: reuses its static/style.css
	{"us", us.Render},             // after site: reuses its static/style.css
	{"ratings", ratings.Render},   // after site: reuses its static/style.css
}

func main() {
	configPath := flag.String("config", "radar.toml", "path to the config file")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: radar [-config radar.toml] collect|validate|render|notify|review|health")
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	var steps []step
	switch flag.Arg(0) {
	case "collect":
		steps = collectors
	case "render":
		steps = renderers
	case "validate":
		// Unlike collect and render, a failure here must stop the run so
		// nothing is published (issue #44).
		steps = []step{{"validate", validate.Run}}
	case "notify":
		// Sends the alerts collect queued; run it after the state is saved.
		steps = []step{{"notify", alerts.Notify}}
	case "review":
		// Prints the monthly paper review (issue #75) as markdown on stdout.
		steps = []step{{"review", paper.Review}}
	case "health":
		// Prints each collect step's latest run and each data table's newest date (issue #14).
		steps = []step{{"health", health.Print}}
	default:
		flag.Usage()
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(cfg.Run.DBPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	runStarted := time.Now() // issue #14: one id for this run's source_runs rows
	failed := 0
	for _, s := range steps {
		log.Printf("%s: starting", s.name)
		started := time.Now()
		err := s.run(ctx, cfg, st)
		if flag.Arg(0) == "collect" { // issue #14: record each source's outcome
			if rerr := health.Record(st.DB, runStarted, s.name, started, err); rerr != nil {
				log.Printf("%s: recording status: %v", s.name, rerr)
			}
		}
		if err != nil {
			log.Printf("%s: FAILED: %v", s.name, err)
			failed++
			continue
		}
		log.Printf("%s: done", s.name)
	}
	log.Printf("%s: %d steps, %d failed", flag.Arg(0), len(steps), failed)
	if flag.Arg(0) == "validate" && failed > 0 {
		os.Exit(1)
	}
}
