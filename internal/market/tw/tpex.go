package tw

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

const (
	tpexURL = "https://www.tpex.org.tw"
	// TPEx served every request at a 2s gap without blocking; 3s leaves room.
	tpexGap = 3 * time.Second
	// How long one run may spend on TPEx days. Newest days go first, so the
	// dashboard is current after the first run and history fills in behind.
	tpexRunTime = 40 * time.Minute
)

// daysTable records each exchange day already fetched (rows = 0 for a day
// without trading), so a backfill resumes where it stopped.
const daysTable = `CREATE TABLE IF NOT EXISTS tw_price_days (
	exchange TEXT NOT NULL,
	date     TEXT NOT NULL,
	rows     INTEGER NOT NULL,
	PRIMARY KEY (exchange, date)
)`

// eventYearsTable records when each source's events for a year were fetched.
// A year is fetched again until one fetch happened after it ended.
const eventYearsTable = `CREATE TABLE IF NOT EXISTS tw_event_years (
	source     TEXT NOT NULL,
	year       INTEGER NOT NULL,
	fetched_on TEXT NOT NULL,
	PRIMARY KEY (source, year)
)`

// TPEx fetches the OTC market's daily quotes and adjustment events, one
// request per day or per year for every stock at once.
type TPEx struct {
	BaseURL string
	Gap     time.Duration
	RunTime time.Duration
	HTTP    *http.Client

	last time.Time
}

func NewTPEx() *TPEx {
	return &TPEx{BaseURL: tpexURL, Gap: tpexGap, RunTime: tpexRunTime, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

func (c *TPEx) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	if wait := c.Gap - time.Since(c.last); wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
	c.last = time.Now()
	q.Set("response", "json")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d: %.120q", path, resp.StatusCode, body)
	}
	return body, nil
}

// tpexEvents fetches ex-rights/dividend and capital reduction results for
// every year from history_start that isn't settled yet.
func tpexEvents(ctx context.Context, c *TPEx, st *store.Store, d *dirtySet, startYear int, now time.Time) error {
	type source struct {
		name, path string
		parse      func([]byte) ([]tickerEvent, error)
	}
	sources := []source{
		{"tpex-dividends", "/www/zh-tw/bulletin/exDailyQ", parseTPExDividends},
		{"tpex-reductions", "/www/zh-tw/bulletin/revivt", parseTPExReductions},
	}
	today := now.Format("2006-01-02")
	for _, s := range sources {
		for y := startYear; y <= now.Year(); y++ {
			var fetched string
			err := st.DB.QueryRow(`SELECT fetched_on FROM tw_event_years WHERE source = ? AND year = ?`, s.name, y).Scan(&fetched)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if fetched > strconv.Itoa(y)+"-12-31" {
				continue
			}
			body, err := c.get(ctx, s.path, url.Values{
				"startDate": {fmt.Sprintf("%d/01/01", y)},
				"endDate":   {fmt.Sprintf("%d/12/31", y)},
			})
			if err != nil {
				return fmt.Errorf("%s %d: %w", s.name, y, err)
			}
			events, err := s.parse(body)
			if err != nil {
				return fmt.Errorf("%s %d: %w", s.name, y, err)
			}
			if err := writeEvents(st, d, events, now); err != nil {
				return err
			}
			if _, err := st.DB.Exec(`INSERT OR REPLACE INTO tw_event_years (source, year, fetched_on) VALUES (?, ?, ?)`,
				s.name, y, today); err != nil {
				return err
			}
		}
	}
	return nil
}

// tpexDays fetches daily quotes for weekdays from start to the latest day
// with published data, newest first, until done or out of run time.
func tpexDays(ctx context.Context, c *TPEx, st *store.Store, d *dirtySet, start, now time.Time) (fetched, left int, err error) {
	done := map[string]bool{}
	rows, err := st.DB.Query(`SELECT date FROM tw_price_days WHERE exchange = 'TPEx'`)
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var day string
		if err := rows.Scan(&day); err != nil {
			rows.Close()
			return 0, 0, err
		}
		done[day] = true
	}
	rows.Close()

	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	last := today
	if now.Hour() < 16 { // quotes are published after the 14:30 close
		last = today.AddDate(0, 0, -1)
	}
	var todo []time.Time
	for day := last; !day.Before(start); day = day.AddDate(0, 0, -1) {
		if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday && !done[day.Format("2006-01-02")] {
			todo = append(todo, day)
		}
	}

	deadline := time.Now().Add(c.RunTime)
	failures := 0
	for i, day := range todo {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fetched, len(todo) - i, ctx.Err()
		}
		body, err := c.get(ctx, "/www/zh-tw/afterTrading/otc", url.Values{
			"date": {day.Format("2006/01/02")},
			"type": {"EW"},
		})
		var prices []model.Price
		if err == nil {
			prices, err = parseDaily(body, day, tpexColumns)
		}
		if err != nil {
			log.Printf("tw: TPEx %s: %v", day.Format("2006-01-02"), err)
			if failures++; failures >= 5 {
				return fetched, len(todo) - i, fmt.Errorf("TPEx: %d failures in a row, stopping for this run", failures)
			}
			continue
		}
		failures = 0
		// An empty answer for today may only mean not published yet.
		if len(prices) == 0 && !day.Before(today) {
			continue
		}
		if err := writeRaw(st, d, prices); err != nil {
			return fetched, len(todo) - i, err
		}
		if _, err := st.DB.Exec(`INSERT OR REPLACE INTO tw_price_days (exchange, date, rows) VALUES ('TPEx', ?, ?)`,
			day.Format("2006-01-02"), len(prices)); err != nil {
			return fetched, len(todo) - i, err
		}
		fetched++
	}
	return fetched, 0, nil
}
