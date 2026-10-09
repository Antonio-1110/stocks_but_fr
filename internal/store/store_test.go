package store

import (
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
