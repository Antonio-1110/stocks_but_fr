package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/model"
)

func TestOpenMigratesAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "radar.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Opening twice must be safe (schema uses IF NOT EXISTS).
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()

	delisted := time.Date(2019, 3, 1, 0, 0, 0, 0, time.UTC)
	err = s.UpsertCompanies([]model.Company{
		{Market: model.MarketTW, Ticker: "2330", Name: "台積電", Exchange: "TWSE"},
		{Market: model.MarketTW, Ticker: "9999", Name: "Gone Co", DelistedOn: delisted},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Companies(model.MarketTW)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "台積電" || !got[1].DelistedOn.Equal(delisted) || !got[0].DelistedOn.IsZero() {
		t.Fatalf("got %+v", got)
	}

	day := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	if err := s.UpsertPrices([]model.Price{{Market: "TW", Ticker: "2330", Date: day, Close: 1000}}); err != nil {
		t.Fatal(err)
	}
	latest, err := s.LatestPriceDate("TW", "2330")
	if err != nil || !latest.Equal(day) {
		t.Fatalf("latest = %v, %v", latest, err)
	}
	if none, _ := s.LatestPriceDate("TW", "0000"); !none.IsZero() {
		t.Errorf("unknown ticker should have zero date, got %v", none)
	}
}

// The state database is attached on every connection, and MoveToState moves
// tables an older version kept in the main file (issue #46).
func TestStateAttachedAndMoved(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "radar.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// An old main-database table with rows, then the new state table.
	for _, q := range []string{
		`CREATE TABLE main.things (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
		`INSERT INTO main.things VALUES (1, 'a'), (2, 'b')`,
		`CREATE TABLE IF NOT EXISTS state.things (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
		`INSERT INTO state.things VALUES (2, 'kept')`,
	} {
		if _, err := st.DB.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := MoveToState(st.DB, "things"); err != nil {
		t.Fatal(err)
	}
	if err := MoveToState(st.DB, "things"); err != nil { // nothing left to move
		t.Fatal(err)
	}
	// Many connections at once all see the attached table, unqualified.
	st.DB.SetMaxIdleConns(0)
	for i := 0; i < 3; i++ {
		var got string
		if err := st.DB.QueryRow(`SELECT group_concat(name, ',') FROM (SELECT name FROM things ORDER BY id)`).Scan(&got); err != nil || got != "a,kept" {
			t.Fatalf("things = %q, %v; want a,kept", got, err)
		}
	}
	var n int
	st.DB.QueryRow(`SELECT COUNT(*) FROM main.sqlite_master WHERE name = 'things'`).Scan(&n)
	if n != 0 {
		t.Error("main.things not dropped")
	}
	if _, err := os.Stat(filepath.Join(dir, StateFile)); err != nil {
		t.Errorf("state file: %v", err)
	}
}
