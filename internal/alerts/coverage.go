package alerts

import (
	"database/sql"
	"fmt"
	"time"
)

// RevenueReady reports whether a revenue month is collected for enough of the
// market to rank on: the share of listed common stocks that traded in the 30
// days before day and have revenue for month must be at least min ([revenue]
// min_coverage). While a backfill is running, or before most companies have
// filed, a ranking would only see some of the market, so rules that use
// revenue wait instead. note says what was found, for the log.
func RevenueReady(db *sql.DB, month time.Time, day time.Time, min float64) (ok bool, note string, err error) {
	if min <= 0 {
		return true, "", nil
	}
	m := month.Format(dateLayout)
	var want int
	var have sql.NullInt64
	err = db.QueryRow(`SELECT COUNT(*), SUM(EXISTS(SELECT 1 FROM monthly_revenue r
			WHERE r.market = 'TW' AND r.ticker = c.ticker AND r.month = ?))
		FROM companies c
		WHERE c.market = 'TW' AND c.delisted_on = '' AND c.ticker GLOB '[1-9][0-9][0-9][0-9]'
		AND EXISTS(SELECT 1 FROM prices p WHERE p.market = 'TW' AND p.ticker = c.ticker AND p.date >= ?)`,
		m, day.AddDate(0, 0, -30).Format(dateLayout)).Scan(&want, &have)
	if err != nil {
		return false, "", err
	}
	if want == 0 {
		return false, "no listed stocks with recent prices", nil
	}
	share := float64(have.Int64) / float64(want)
	note = fmt.Sprintf("revenue for %s covers %d of %d listed stocks (%.0f%%, need %.0f%%)",
		m[:7], have.Int64, want, share*100, min*100)
	return share >= min, note, nil
}

// revenueMonth is the newest month a revenue rule ranks on at now: last month
// once the filing deadline has passed, the month before until then.
func revenueMonth(now time.Time, deadlineDay int) time.Time {
	m := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
	if now.Day() <= deadlineDay {
		m = m.AddDate(0, -1, 0)
	}
	return m
}

// RevenueMonth is revenueMonth for the paper portfolios.
func RevenueMonth(now time.Time, deadlineDay int) time.Time { return revenueMonth(now, deadlineDay) }
