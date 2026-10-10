// Command radar runs the pipeline: collect data into SQLite, then render the
// static dashboard. The Python backtester in backtest/ reads the same database.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/Antonio-1110/stocks_but_fr/internal/alerts"
	"github.com/Antonio-1110/stocks_but_fr/internal/calendar"
	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/market/tw"
	"github.com/Antonio-1110/stocks_but_fr/internal/market/tw/flows"
	"github.com/Antonio-1110/stocks_but_fr/internal/market/tw/revenue"
	"github.com/Antonio-1110/stocks_but_fr/internal/site"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
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
	{"tw-revenue", revenue.Collect}, // needs the TW universe in companies
	{"tw-flows", flows.Collect},     // history needs the TW universe; daily reports don't
	{"tw-calendar", calendar.Collect},
	{"alerts", alerts.Run}, // last: Telegram on stocks newly passing a rule, from the fresh data
}

// renderers run in order on `radar render`.
var renderers = []step{
	{"site", site.Render},
	{"calendar", calendar.Render}, // after site: reuses its static/style.css
}

func main() {
	configPath := flag.String("config", "radar.toml", "path to the config file")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: radar [-config radar.toml] collect|render")
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
	failed := 0
	for _, s := range steps {
		log.Printf("%s: starting", s.name)
		if err := s.run(ctx, cfg, st); err != nil {
			log.Printf("%s: FAILED: %v", s.name, err)
			failed++
			continue
		}
		log.Printf("%s: done", s.name)
	}
	log.Printf("%s: %d steps, %d failed", flag.Arg(0), len(steps), failed)
}
