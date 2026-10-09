// Package model holds the types shared across collectors, the store and the site.
// Edits here must be small and additive: other packages depend on these.
package model

import "time"

// Market codes. TW covers both TWSE and TPEx listings.
const (
	MarketTW = "TW"
	MarketUS = "US"
)

type Company struct {
	Market     string
	Ticker     string // e.g. "2330"
	Name       string
	Industry   string
	Exchange   string    // "TWSE", "TPEx", ...
	ListedOn   time.Time // zero if unknown
	DelistedOn time.Time // zero while still listed
}

// Price is one trading day. AdjClose is adjusted for dividends and splits.
type Price struct {
	Market   string
	Ticker   string
	Date     time.Time
	Open     float64
	High     float64
	Low      float64
	Close    float64
	AdjClose float64
	Volume   int64   // shares
	Turnover float64 // traded value in local currency
}

// MonthlyRevenue is one company's revenue for one calendar month.
// AnnouncedOn is when it became public; backtests must not use it earlier.
type MonthlyRevenue struct {
	Market      string
	Ticker      string
	Month       time.Time // first day of the month
	Revenue     float64
	YoYPct      float64
	MoMPct      float64
	AnnouncedOn time.Time
}

// InstitutionalFlow is net shares bought (negative = sold) on one day
// by Taiwan's three institutional investor groups (三大法人).
type InstitutionalFlow struct {
	Market     string
	Ticker     string
	Date       time.Time
	ForeignNet int64
	TrustNet   int64 // 投信
	DealerNet  int64
}
