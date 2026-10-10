package calendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "radar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func day(s string) time.Time {
	d, err := time.Parse(dateLayout, s)
	if err != nil {
		panic(err)
	}
	return d
}

func find(evs []Event, ticker string) *Event {
	for i := range evs {
		if evs[i].Ticker == ticker {
			return &evs[i]
		}
	}
	return nil
}

func TestRocDate(t *testing.T) {
	for in, want := range map[string]string{
		"115年10月08日": "2026-10-08",
		"115/10/02":  "2026-10-02",
		"1150930":    "2026-09-30",
		" 99/01/05 ": "2010-01-05",
	} {
		got, ok := rocDate(in)
		if !ok || fmtDate(got) != want {
			t.Errorf("rocDate(%q) = %v %v, want %s", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "尚未公告", "115/02/30", "115/13/01"} {
		if _, ok := rocDate(bad); ok {
			t.Errorf("rocDate(%q) should fail", bad)
		}
	}
}

func TestParseTWSEExDiv(t *testing.T) {
	evs, err := parseTWSEExDiv(readFile(t, "twse_twt48u.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 7 {
		t.Fatalf("got %d events, want 7", len(evs))
	}
	e := find(evs, "2614")
	if e == nil || fmtDate(e.Date) != "2026-10-06" || e.Title != "除權息" || e.Name != "東森" {
		t.Fatalf("2614: %+v", e)
	}
	if e.Detail != "現金 0.4 元 · 配股 0.08 股/股 · 現增 0.382 股/股 @12.8" {
		t.Errorf("2614 detail = %q", e.Detail)
	}
	if e := find(evs, "00401A"); e == nil || e.Title != "除息" || e.Detail != "配息金額待公告" {
		t.Errorf("00401A: %+v", e)
	}
	// Subscription price not announced yet.
	if e := find(evs, "1711"); e == nil || e.Detail != "現增 0.0913 股/股" {
		t.Errorf("1711: %+v", e)
	}
}

func TestParseTPExExDiv(t *testing.T) {
	evs, err := parseTPExExDiv(readFile(t, "tpex_exright_prepost.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Fatalf("got %d events, want 4", len(evs))
	}
	for _, e := range evs {
		if e.Kind != KindExDividend || e.Ticker == "" || e.Name == "" || e.Date.IsZero() {
			t.Errorf("bad event %+v", e)
		}
		if e.Title != "除息" && e.Title != "除權" {
			t.Errorf("title %q", e.Title)
		}
	}
}

func TestParseMeetings(t *testing.T) {
	evs, err := parseMeetings(readFile(t, "tpex_t187ap41_O.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Fatalf("got %d events, want 4", len(evs))
	}
	if e := find(evs, "1259"); e == nil || fmtDate(e.Date) != "2026-05-22" || e.Title != "股東常會" || !strings.HasPrefix(e.Detail, "改選董監 · ") {
		t.Errorf("1259: %+v", e)
	}
	if e := find(evs, "2073"); e == nil || fmtDate(e.Date) != "2026-10-28" || e.Title != "股東臨時會" {
		t.Errorf("2073: %+v", e)
	}
}

func TestParseConferences(t *testing.T) {
	evs := parseConferences(readFile(t, "mops_t100sb02_1.html"))
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(evs), evs)
	}
	e := evs[0]
	if e.Ticker != "2330" || e.Name != "台積電" || fmtDate(e.Date) != "2026-10-16" || e.Title != "法說會 14:00" ||
		e.Detail != "線上法說會 · 本公司115年第三季營運成果說明&展望" || !e.EndDate.IsZero() {
		t.Errorf("2330: %+v", e)
	}
	e = evs[1]
	if e.Ticker != "2603" || fmtDate(e.EndDate) != "2026-11-06" || e.Title != "法說會 09:30" ||
		!strings.Contains(e.Detail, "台北W飯店 (台北市") {
		t.Errorf("2603: %+v", e)
	}
}

func TestDeadlines(t *testing.T) {
	evs := deadlines(day("2026-10-03"), day("2026-12-31"))
	var got []string
	for _, e := range evs {
		got = append(got, fmtDate(e.Date)+" "+e.Title)
	}
	want := "2026-10-10 9月營收公布期限|2026-11-10 10月營收公布期限|2026-11-14 2026Q3財報期限|2026-12-10 11月營收公布期限"
	if strings.Join(got, "|") != want {
		t.Errorf("got %v", got)
	}
}

// fakeSources serves every endpoint from testdata and counts requests.
func fakeSources(t *testing.T, hits *int) *Collector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		if r.Header.Get("User-Agent") != userAgent {
			t.Errorf("missing User-Agent on %s", r.URL)
		}
		switch r.URL.Path {
		case "/twse-exdiv":
			w.Write(readFile(t, "twse_twt48u.json"))
		case "/tpex-exdiv":
			w.Write(readFile(t, "tpex_exright_prepost.json"))
		case "/twse-meetings":
			http.Error(w, "down", http.StatusBadGateway) // one source failing must not stop the rest
		case "/tpex-meetings":
			w.Write(readFile(t, "tpex_t187ap41_O.json"))
		case "/mops":
			if err := r.ParseForm(); err != nil || r.PostForm.Get("year") != "115" {
				t.Errorf("MOPS form: %v %v", r.PostForm, err)
			}
			if r.PostForm.Get("TYPEK") == "sii" {
				w.Write(readFile(t, "mops_t100sb02_1.html"))
			} else {
				w.Write([]byte("<html>查無資料</html>"))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Collector{
		Client: srv.Client(), Gap: 0,
		// Early enough in the year that MOPS is asked about one year only.
		Now:             func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, taipei) },
		TWSEExDivURL:    srv.URL + "/twse-exdiv",
		TPExExDivURL:    srv.URL + "/tpex-exdiv",
		TWSEMeetingsURL: srv.URL + "/twse-meetings",
		TPExMeetingsURL: srv.URL + "/tpex-meetings",
		MOPSConfURL:     srv.URL + "/mops",
	}
}

func TestRunAndRender(t *testing.T) {
	st := openStore(t)
	hits := 0
	c := fakeSources(t, &hits)
	if err := c.Run(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if hits != 6 {
		t.Errorf("first run made %d requests, want 6", hits)
	}
	var n int
	st.DB.QueryRow(`SELECT COUNT(*) FROM corporate_events`).Scan(&n)
	if n != 7+4+4+2 {
		t.Errorf("stored %d events", n)
	}

	// Within refetchAfter only the failed and empty sources are retried.
	hits = 0
	if err := c.Run(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Errorf("second run made %d requests, want 2 (failed TWSE meetings, empty MOPS otc)", hits)
	}

	// A moved event replaces the old date instead of adding a second one.
	if err := replaceSource(st.DB, "mops-conferences-sii", day("2026-10-10"), []Event{
		{Market: "TW", Ticker: "2330", Name: "台積電", Date: day("2026-10-17"), Kind: KindConference, Title: "法說會 14:00"},
		{Market: "TW", Ticker: "2603", Name: "長榮", Date: day("2026-11-05"), EndDate: day("2026-11-06"), Kind: KindConference, Title: "法說會 09:30"},
	}); err != nil {
		t.Fatal(err)
	}
	var dates string
	st.DB.QueryRow(`SELECT group_concat(date) FROM corporate_events WHERE ticker = '2330'`).Scan(&dates)
	if dates != "2026-10-17" {
		t.Errorf("2330 dates = %q", dates)
	}

	dir := t.TempDir()
	if err := render(st, dir, time.Date(2026, 10, 10, 12, 0, 0, 0, taipei)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "calendar.html"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	for _, want := range []string{
		`id="d-2026-10-17"`,    // moved conference
		`id="d-2026-10-06"`,    // ex-div within the past week
		`id="d-2026-10-28"`,    // extraordinary meeting
		`2603</b>長榮`,           // multi-day conference
		`→ 11/06`,              // its last day
		`9月營收公布期限`,             // deadline row
		`2026-12 · 12月`,        // third month grid
		`href="#d-2026-10-15"`, // grid day linking to 00401A's ex-div
		`<td class="today" data-date="2026-10-10">`,
		`goodinfo.tw/tw/StockDetail.asp?STOCK_ID=2614`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// Meetings earlier than the past week stay out of the agenda.
	if strings.Contains(page, `id="d-2026-05-22"`) {
		t.Error("page shows a May meeting")
	}
}
