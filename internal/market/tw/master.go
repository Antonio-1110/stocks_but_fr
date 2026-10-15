package tw

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

// The security master (issue #56): when each stock was listed, and the
// industries sources have given it over time. Listing dates go into
// companies.listed_on, which the backtester uses to decide which stocks
// existed on a past day. FinMind's price history for a stock includes its
// emerging-board (興櫃) days before listing (issue #57); listed_on is what
// keeps those days out of the universe.

// listingsTable holds each ticker's listing date and where it came from:
// "twse" or "tpex" (the exchange's company open data), or "first_price"
// (the earliest stored price, for ETFs and delisted stocks no source lists).
// A source date is kept after the stock leaves the source's list.
const listingsTable = `CREATE TABLE IF NOT EXISTS tw_listings (
	ticker    TEXT PRIMARY KEY,
	listed_on TEXT NOT NULL,
	source    TEXT NOT NULL
)`

// industryHistoryTable gets a row when a source gives a ticker a different
// industry than last time. "finmind" is the name kept in companies.industry;
// "twse" and "tpex" are the exchange's own industry codes, as given. History
// starts on the day this collector first ran.
const industryHistoryTable = `CREATE TABLE IF NOT EXISTS tw_industry_history (
	ticker     TEXT NOT NULL,
	source     TEXT NOT NULL,
	industry   TEXT NOT NULL,
	first_seen TEXT NOT NULL,
	last_seen  TEXT NOT NULL,
	PRIMARY KEY (ticker, source, first_seen)
)`

// masterFetchesTable records the day each source was last fetched, so a
// second collect on the same day asks nothing.
const masterFetchesTable = `CREATE TABLE IF NOT EXISTS tw_master_fetches (
	source     TEXT PRIMARY KEY,
	fetched_on TEXT NOT NULL
)`

// OpenData fetches the exchanges' company basic data (公司基本資料): one
// JSON file per exchange, every listed company with its listing date and
// industry code. The ISIN pages have the same fields but are Big5 HTML.
type OpenData struct {
	TWSE, TPEx string // full URLs
	HTTP       *http.Client
}

func NewOpenData() *OpenData {
	return &OpenData{
		TWSE: "https://openapi.twse.com.tw/v1/opendata/t187ap03_L",
		TPEx: tpexURL + "/openapi/v1/mopsfin_t187ap03_O",
		HTTP: &http.Client{Timeout: 5 * time.Minute}, // about 1 MB each, served slowly
	}
}

// listing is one company in a source's list.
type listing struct {
	Ticker, ListedOn, Industry string // ListedOn as YYYY-MM-DD
}

// parseTWSECompanies reads TWSE t187ap03_L (上市公司基本資料).
func parseTWSECompanies(body []byte) ([]listing, error) {
	var rows []map[string]string
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("decode: %w (%.120q)", err, body)
	}
	return listings(rows, "公司代號", "上市日期", "產業別")
}

// parseTPExCompanies reads TPEx mopsfin_t187ap03_O (上櫃公司基本資料).
func parseTPExCompanies(body []byte) ([]listing, error) {
	var rows []map[string]string
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("decode: %w (%.120q)", err, body)
	}
	return listings(rows, "SecuritiesCompanyCode", "DateOfListing", "SecuritiesIndustryCode")
}

// listings picks the code, date (YYYYMMDD) and industry fields. Rows with a
// code that isn't a stock or an unreadable date are skipped; a file where
// no row has the fields is an error (the format changed).
func listings(rows []map[string]string, code, date, industry string) ([]listing, error) {
	var out []listing
	for _, r := range rows {
		t := strings.TrimSpace(r[code])
		d, err := time.Parse("20060102", strings.TrimSpace(r[date]))
		if !tickerRe.MatchString(t) || err != nil {
			continue
		}
		out = append(out, listing{Ticker: t, ListedOn: d.Format("2006-01-02"), Industry: strings.TrimSpace(r[industry])})
	}
	if len(out) == 0 && len(rows) > 0 {
		return nil, fmt.Errorf("no rows with %s and %s", code, date)
	}
	return out, nil
}

func (o *OpenData) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if strings.Contains(string(body[:min(len(body), 2000)]), "FOR SECURITY REASONS") {
		return nil, errBlocked
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %.120q", resp.StatusCode, body)
	}
	return body, nil
}

// CollectMaster is the `radar collect` step "tw-master". It runs after
// tw-prices, which fills the universe and the prices the fallback uses.
func CollectMaster(ctx context.Context, cfg config.Config, st *store.Store) error {
	return collectMaster(ctx, st, NewOpenData(), time.Now().In(taipei))
}

func collectMaster(ctx context.Context, st *store.Store, o *OpenData, now time.Time) error {
	for _, q := range []string{listingsTable, industryHistoryTable, masterFetchesTable} {
		if _, err := st.DB.Exec(q); err != nil {
			return err
		}
	}
	today := now.Format("2006-01-02")
	sources := []struct {
		name, url string
		parse     func([]byte) ([]listing, error)
	}{
		{"twse", o.TWSE, parseTWSECompanies},
		{"tpex", o.TPEx, parseTPExCompanies},
	}
	for _, s := range sources {
		var last string
		st.DB.QueryRow(`SELECT fetched_on FROM tw_master_fetches WHERE source = ?`, s.name).Scan(&last)
		if last == today {
			continue
		}
		body, err := o.get(ctx, s.url)
		var rows []listing
		if err == nil {
			rows, err = s.parse(body)
		}
		if err != nil {
			// Fail soft: stored dates stay, and the fallback covers the rest.
			log.Printf("tw: %s company list: %v (keeping stored listing dates)", s.name, err)
			continue
		}
		if err := saveListings(st, s.name, rows, today); err != nil {
			return err
		}
		log.Printf("tw: %s company list: %d listing dates", s.name, len(rows))
	}

	// FinMind's industry, as the tw-prices step stored it.
	var fm []listing
	cos, err := st.Companies(model.MarketTW)
	if err != nil {
		return err
	}
	for _, c := range cos {
		if c.DelistedOn.IsZero() && c.Industry != "" {
			fm = append(fm, listing{Ticker: c.Ticker, Industry: c.Industry})
		}
	}
	if err := observeIndustries(st, "finmind", fm, today); err != nil {
		return err
	}

	// Everything no source lists: the earliest stored price.
	if _, err := st.DB.Exec(`INSERT INTO tw_listings (ticker, listed_on, source)
		SELECT ticker, MIN(date), 'first_price' FROM prices WHERE market = ? GROUP BY ticker
		ON CONFLICT (ticker) DO UPDATE SET listed_on = excluded.listed_on
		WHERE source = 'first_price' AND listed_on <> excluded.listed_on`, model.MarketTW); err != nil {
		return err
	}
	res, err := st.DB.Exec(`UPDATE companies SET listed_on = l.listed_on FROM tw_listings l
		WHERE companies.market = ? AND companies.ticker = l.ticker AND companies.listed_on <> l.listed_on`, model.MarketTW)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	var missing int
	st.DB.QueryRow(`SELECT COUNT(*) FROM companies WHERE market = ? AND listed_on = ''`, model.MarketTW).Scan(&missing)
	log.Printf("tw: listing dates updated for %d companies, %d still without one", n, missing)
	return nil
}

// saveListings stores a source's listing dates (a source date replaces the
// first-price fallback) and records its industries.
func saveListings(st *store.Store, source string, rows []listing, today string) error {
	tx, err := st.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range rows {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO tw_listings (ticker, listed_on, source) VALUES (?, ?, ?)`,
			r.Ticker, r.ListedOn, source); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO tw_master_fetches (source, fetched_on) VALUES (?, ?)`, source, today); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return observeIndustries(st, source, rows, today)
}

// observeIndustries extends a ticker's current industry row, or starts a
// new one when the source now says something else.
func observeIndustries(st *store.Store, source string, rows []listing, today string) error {
	tx, err := st.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range rows {
		if r.Industry == "" {
			continue
		}
		var cur, first string
		err := tx.QueryRow(`SELECT industry, first_seen FROM tw_industry_history
			WHERE ticker = ? AND source = ? ORDER BY first_seen DESC LIMIT 1`, r.Ticker, source).Scan(&cur, &first)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil && cur == r.Industry {
			_, err = tx.Exec(`UPDATE tw_industry_history SET last_seen = ? WHERE ticker = ? AND source = ? AND first_seen = ?`,
				today, r.Ticker, source, first)
		} else {
			_, err = tx.Exec(`INSERT OR REPLACE INTO tw_industry_history (ticker, source, industry, first_seen, last_seen)
				VALUES (?, ?, ?, ?, ?)`, r.Ticker, source, r.Industry, today, today)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
