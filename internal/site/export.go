package site

import (
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

// LoadTW returns the Taiwan rows exactly as the dashboard ranks them, with
// revenue cut off at asOf, and the latest price date. Used by internal/alerts
// (issue #34) so an alert and the page never disagree.
func LoadTW(st *store.Store, asOf time.Time, cfg config.Config) ([]Row, string, error) {
	return loadRows(st, model.MarketTW, asOf, cfg.Revenue.LowBase, cfg.Site.Unpriced)
}
