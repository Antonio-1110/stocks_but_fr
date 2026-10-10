package calendar

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/model"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

//go:embed templates/calendar.html
var templates embed.FS

const (
	pastDays    = 7 // the agenda starts this many days back
	monthsShown = 3 // this month and the next two
)

// Kind is one event type as the page shows it.
type Kind struct {
	ID, Label, Short string
}

var kinds = []Kind{
	{KindConference, "法說會", "法說"},
	{KindExDividend, "除權息", "除權息"},
	{KindMeeting, "股東會", "股東會"},
	{KindDeadline, "申報期限", "期限"},
}

func kindLabel(id string) string {
	for _, k := range kinds {
		if k.ID == id {
			return k.Label
		}
	}
	return id
}

type Page struct {
	Title       string
	GeneratedAt string
	Kinds       []Kind
	Months      []Month
	Days        []Day
	Total       int
}

type Month struct {
	Label string
	Weeks [][]Cell
}

// Cell is one square of a month grid. Counts are per kind, in kinds order.
type Cell struct {
	Day     int
	Date    string // empty for padding squares outside the month
	Today   bool
	Past    bool
	Counts  []Count
	HasDays bool // the agenda has an entry to jump to
}

type Count struct {
	Kind  string
	Short string
	N     int
}

// Day is one date in the agenda list.
type Day struct {
	Date  string
	Label string
	Today bool
	Past  bool
	Rows  []Row
}

type Row struct {
	Kind, KindLabel string
	Ticker, Name    string
	Title, Detail   string
	Until           string // last day of a multi-day event
	Link            string
	ETF             bool   // codes starting "00"; hidden by default on the page
	Search          string // lowercased text the filter box matches
}

// Render is the `radar render` step: it writes calendar.html into the public
// directory, next to the dashboard (whose static/style.css it reuses).
func Render(_ context.Context, cfg config.Config, st *store.Store) error {
	return render(st, cfg.Run.PublicDir, time.Now())
}

func render(st *store.Store, dir string, now time.Time) error {
	if err := migrate(st.DB); err != nil {
		return err
	}
	local := now.In(taipei)
	today := dayOf(local)
	from := today.AddDate(0, 0, -pastDays)
	firstMonth := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	to := firstMonth.AddDate(0, monthsShown, -1)

	evs, err := loadEvents(st.DB, model.MarketTW, from, to)
	if err != nil {
		return err
	}
	evs = append(evs, deadlines(from, to)...)
	page := build(evs, today, from, firstMonth, to)
	page.GeneratedAt = local.Format("2006-01-02 15:04") + " (台北)"

	tmpl, err := template.ParseFS(templates, "templates/calendar.html")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Write to a temp file and rename, so a failed render never leaves a half page.
	tmp := filepath.Join(dir, ".calendar.html.tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := tmpl.Execute(f, page); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "calendar.html"))
}

func build(evs []Event, today, from, firstMonth, to time.Time) Page {
	sort.SliceStable(evs, func(i, j int) bool {
		a, b := evs[i], evs[j]
		if !a.Date.Equal(b.Date) {
			return a.Date.Before(b.Date)
		}
		if a.Kind != b.Kind {
			return kindOrder[a.Kind] < kindOrder[b.Kind]
		}
		if a.Title != b.Title {
			return a.Title < b.Title
		}
		return a.Ticker < b.Ticker
	})

	byDate := map[string][]Event{}
	var dates []string
	for _, e := range evs {
		d := e.Date
		if d.Before(from) { // multi-day event that started earlier: show from the first visible day
			d = from
		}
		if d.After(to) {
			continue
		}
		k := fmtDate(d)
		if _, ok := byDate[k]; !ok {
			dates = append(dates, k)
		}
		byDate[k] = append(byDate[k], e)
	}
	sort.Strings(dates)

	p := Page{Title: "Taiwan radar · Calendar", Kinds: kinds}
	for _, k := range dates {
		d, _ := time.Parse(dateLayout, k)
		day := Day{Date: k, Label: d.Format("01/02") + " (" + weekday(d) + ")", Today: d.Equal(today), Past: d.Before(today)}
		for _, e := range byDate[k] {
			r := Row{
				Kind: e.Kind, KindLabel: kindLabel(e.Kind), Ticker: e.Ticker, Name: e.Name,
				Title: e.Title, Detail: e.Detail,
			}
			if !e.EndDate.IsZero() && e.EndDate.After(e.Date) {
				r.Until = e.EndDate.Format("01/02")
			}
			r.ETF = strings.HasPrefix(e.Ticker, "00")
			if e.Ticker != "" {
				r.Link = "https://goodinfo.tw/tw/StockDetail.asp?STOCK_ID=" + e.Ticker
			}
			r.Search = strings.ToLower(strings.Join([]string{e.Ticker, e.Name, e.Title, e.Detail}, " "))
			day.Rows = append(day.Rows, r)
		}
		p.Total += len(day.Rows)
		p.Days = append(p.Days, day)
	}

	for m := 0; m < monthsShown; m++ {
		p.Months = append(p.Months, monthGrid(firstMonth.AddDate(0, m, 0), today, byDate))
	}
	return p
}

// monthGrid lays a month out in Monday-first weeks.
func monthGrid(first, today time.Time, byDate map[string][]Event) Month {
	m := Month{Label: fmt.Sprintf("%d-%02d · %d月", first.Year(), int(first.Month()), int(first.Month()))}
	lead := (int(first.Weekday()) + 6) % 7 // Monday = 0
	var week []Cell
	for i := 0; i < lead; i++ {
		week = append(week, Cell{})
	}
	for d := first; d.Month() == first.Month(); d = d.AddDate(0, 0, 1) {
		k := fmtDate(d)
		c := Cell{Day: d.Day(), Date: k, Today: d.Equal(today), Past: d.Before(today)}
		n := map[string]int{}
		for _, e := range byDate[k] {
			n[e.Kind]++
		}
		for _, kd := range kinds {
			if n[kd.ID] > 0 {
				c.Counts = append(c.Counts, Count{Kind: kd.ID, Short: kd.Short, N: n[kd.ID]})
			}
		}
		c.HasDays = len(byDate[k]) > 0
		week = append(week, c)
		if len(week) == 7 {
			m.Weeks = append(m.Weeks, week)
			week = nil
		}
	}
	if len(week) > 0 {
		for len(week) < 7 {
			week = append(week, Cell{})
		}
		m.Weeks = append(m.Weeks, week)
	}
	return m
}

func weekday(d time.Time) string {
	return [...]string{"日", "一", "二", "三", "四", "五", "六"}[d.Weekday()]
}

// deadlines are the market-wide filing deadlines for general (non-financial)
// listed companies, from the rules rather than any feed: monthly revenue by
// the 10th, annual report by 3/31, Q1 by 5/15, Q2 by 8/14, Q3 by 11/14.
// When a deadline falls on a holiday it moves to the next business day; the
// calendar shows the statutory date.
func deadlines(from, to time.Time) []Event {
	var out []Event
	add := func(d time.Time, title, detail string) {
		if !d.Before(from) && !d.After(to) {
			out = append(out, Event{Market: model.MarketTW, Date: d, Kind: KindDeadline, Title: title, Detail: detail})
		}
	}
	for m := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC); !m.After(to); m = m.AddDate(0, 1, 0) {
		prev := m.AddDate(0, -1, 0)
		add(m.AddDate(0, 0, 9), fmt.Sprintf("%d月營收公布期限", int(prev.Month())),
			fmt.Sprintf("上市櫃公司須於每月10日前公告%d年%d月營收", prev.Year()-1911, int(prev.Month())))
		y := m.Year()
		switch m.Month() {
		case time.March:
			add(time.Date(y, 3, 31, 0, 0, 0, 0, time.UTC), fmt.Sprintf("%d年報期限", y-1), "一般產業年度財報公告期限")
		case time.May:
			add(time.Date(y, 5, 15, 0, 0, 0, 0, time.UTC), fmt.Sprintf("%dQ1財報期限", y), "一般產業第一季財報公告期限")
		case time.August:
			add(time.Date(y, 8, 14, 0, 0, 0, 0, time.UTC), fmt.Sprintf("%dQ2財報期限", y), "一般產業半年報公告期限")
		case time.November:
			add(time.Date(y, 11, 14, 0, 0, 0, 0, time.UTC), fmt.Sprintf("%dQ3財報期限", y), "一般產業第三季財報公告期限")
		}
	}
	return out
}
