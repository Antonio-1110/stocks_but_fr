package health

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

func TestRecordAndReport(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "radar.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB.Exec(`INSERT INTO prices (market, ticker, date, close) VALUES ('TW', '2330', '2026-11-13', 1)`); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	runs := []struct {
		margin, prices error
	}{
		{nil, nil},
		{errors.New("twse 2026-11-13: blocked"), nil},
		{errors.New("twse 2026-11-14: blocked\nby | firewall"), nil},
	}
	for i, r := range runs {
		run := base.Add(time.Duration(i) * time.Hour)
		for _, s := range []struct {
			name string
			err  error
		}{{"tw-prices", r.prices}, {"tw-margin", r.margin}} {
			if err := Record(st.DB, run, s.name, run, s.err); err != nil {
				t.Fatal(err)
			}
		}
	}

	steps, err := Steps(st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(steps))
	}
	m := steps[0] // tw-margin sorts first
	if m.Step != "tw-margin" || m.OK || m.FailStreak != 2 || m.LastOK != "2026-11-14T10:00:00Z" || m.LastRun != "2026-11-14T12:00:00Z" {
		t.Errorf("tw-margin = %+v", m)
	}
	if p := steps[1]; !p.OK || p.FailStreak != 0 || p.LastOK != p.LastRun {
		t.Errorf("tw-prices = %+v", p)
	}

	var b strings.Builder
	if err := Write(&b, st.DB); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"| tw-margin | FAILED (2 in a row) | 2026-11-14T12:00:00Z | 2026-11-14T10:00:00Z |",
		"blocked by / firewall", // one table row
		"| prices | 2026-11-13 | 1 |",
		"| tw_margin | missing or empty | 0 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "tw-margin") > strings.Index(out, "tw-prices") {
		t.Errorf("failed steps should come first:\n%s", out)
	}
}
