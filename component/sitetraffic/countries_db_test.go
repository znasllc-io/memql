package sitetraffic

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/uptrace/bun"
)

func storeCountryDB(t *testing.T, db *bun.DB, release string, data []byte) {
	t.Helper()
	_, err := db.NewRaw(`INSERT INTO edge_geo_country (id, release, fetched_at, data) VALUES (1, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET release = EXCLUDED.release, fetched_at = EXCLUDED.fetched_at, data = EXCLUDED.data`,
		release, time.Now().UTC(), data).Exec(context.Background())
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoaderSwapsInANewerRelease(t *testing.T) {
	db := edgeTestDB(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = db.NewRaw("DELETE FROM edge_geo_country").Exec(ctx) })
	addr := netip.MustParseAddr("203.0.113.9")

	storeCountryDB(t, db, "2026-10", writeCountryDB(t, "GeoLite2-Country", map[string]string{"203.0.113.0/24": "NL"}))
	c := NewCountryLoader(quietLogger(), func() *bun.DB { return db })
	if err := c.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := c.Country(addr); got != "NL" {
		t.Fatalf("want NL, got %q", got)
	}

	storeCountryDB(t, db, "2026-11", writeCountryDB(t, "GeoLite2-Country", map[string]string{"203.0.113.0/24": "DE"}))
	if err := c.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := c.Country(addr); got != "DE" {
		t.Fatalf("want DE after the newer release, got %q", got)
	}
	if got := c.Country(netip.MustParseAddr("198.51.100.1")); got != "" {
		t.Fatalf("an unmapped address must have no country, got %q", got)
	}
}

func TestLoaderWithNoRowRecordsNoCountry(t *testing.T) {
	db := edgeTestDB(t)
	ctx := context.Background()
	_, _ = db.NewRaw("DELETE FROM edge_geo_country").Exec(ctx)
	c := NewCountryLoader(quietLogger(), func() *bun.DB { return db })
	if err := c.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := c.Country(netip.MustParseAddr("203.0.113.9")); got != "" {
		t.Fatalf("no row means no country, got %q", got)
	}
}
