package sitetraffic

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func storedRelease(t *testing.T, ctx context.Context) string {
	t.Helper()
	db := edgeTestDB(t)
	var rel string
	if err := db.NewRaw(`SELECT COALESCE((SELECT release FROM edge_geo_country WHERE id = 1), '')`).Scan(ctx, &rel); err != nil {
		t.Fatal(err)
	}
	return rel
}

var refreshNow = time.Date(2026, 11, 8, 4, 10, 0, 0, time.UTC)

// serveFiles answers each path in files with its bytes and every other path with 404.
func serveFiles(t *testing.T, files map[string][]byte, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRefreshDownloadsTheCurrentMonth(t *testing.T) {
	db := edgeTestDB(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = db.NewRaw("DELETE FROM edge_geo_country").Exec(ctx) })
	_, _ = db.NewRaw("DELETE FROM edge_geo_country").Exec(ctx)

	mmdb := writeCountryDB(t, "DBIP-Country-Lite", map[string]string{"203.0.113.0/24": "NL"})
	srv := serveFiles(t, map[string][]byte{"/dbip-country-lite-2026-11.mmdb.gz": gz(t, mmdb)}, nil)

	rep, err := refreshCountries(ctx, db, srv.Client(), srv.URL+"/", refreshNow)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Downloaded || rep.Release != "2026-11" {
		t.Fatalf("report = %+v", rep)
	}
	if got := storedRelease(t, ctx); got != "2026-11" {
		t.Fatalf("stored release = %q", got)
	}
}

func TestRefreshFallsBackToThePreviousMonthOn404(t *testing.T) {
	db := edgeTestDB(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = db.NewRaw("DELETE FROM edge_geo_country").Exec(ctx) })
	_, _ = db.NewRaw("DELETE FROM edge_geo_country").Exec(ctx)

	mmdb := writeCountryDB(t, "DBIP-Country-Lite", map[string]string{"203.0.113.0/24": "NL"})
	srv := serveFiles(t, map[string][]byte{"/dbip-country-lite-2026-10.mmdb.gz": gz(t, mmdb)}, nil)

	rep, err := refreshCountries(ctx, db, srv.Client(), srv.URL+"/", refreshNow)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Downloaded || rep.Release != "2026-10" {
		t.Fatalf("report = %+v", rep)
	}
	if got := storedRelease(t, ctx); got != "2026-10" {
		t.Fatalf("stored release = %q", got)
	}
}

func TestRefreshRefusesANonCountryDatabase(t *testing.T) {
	db := edgeTestDB(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = db.NewRaw("DELETE FROM edge_geo_country").Exec(ctx) })
	_, _ = db.NewRaw("DELETE FROM edge_geo_country").Exec(ctx)

	mmdb := writeCountryDB(t, "GeoLite2-City", map[string]string{"203.0.113.0/24": "NL"})
	srv := serveFiles(t, map[string][]byte{"/dbip-country-lite-2026-11.mmdb.gz": gz(t, mmdb)}, nil)

	rep, err := refreshCountries(ctx, db, srv.Client(), srv.URL+"/", refreshNow)
	if err == nil || rep.Downloaded {
		t.Fatalf("want a refusal, got %+v, %v", rep, err)
	}
	if got := storedRelease(t, ctx); got != "" {
		t.Fatalf("a refused file must not be stored, got %q", got)
	}
}

func TestRefreshRefusesAnOversizedBody(t *testing.T) {
	db := edgeTestDB(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = db.NewRaw("DELETE FROM edge_geo_country").Exec(ctx) })
	_, _ = db.NewRaw("DELETE FROM edge_geo_country").Exec(ctx)

	// Compresses to almost nothing, expands past the 64 MiB cap: the decompression bomb.
	bomb := gz(t, bytes.Repeat([]byte{0}, maxCountryDecompressed+1024))
	srv := serveFiles(t, map[string][]byte{"/dbip-country-lite-2026-11.mmdb.gz": bomb}, nil)
	_, err := refreshCountries(ctx, db, srv.Client(), srv.URL+"/", refreshNow)
	if err == nil || !strings.Contains(err.Error(), "64 MiB") {
		t.Fatalf("want the decompressed cap refused, got %v", err)
	}

	// An over-cap compressed body (raw junk is enough: the cap is checked before gunzip).
	big := bytes.Repeat([]byte{1}, maxCountryCompressed+1024)
	srv2 := serveFiles(t, map[string][]byte{"/dbip-country-lite-2026-11.mmdb.gz": big}, nil)
	_, err = refreshCountries(ctx, db, srv2.Client(), srv2.URL+"/", refreshNow)
	if err == nil || !strings.Contains(err.Error(), "32 MiB") {
		t.Fatalf("want the compressed cap refused, got %v", err)
	}
	if got := storedRelease(t, ctx); got != "" {
		t.Fatalf("nothing may be stored, got %q", got)
	}
}

func TestAFailedDownloadKeepsTheStoredRelease(t *testing.T) {
	db := edgeTestDB(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = db.NewRaw("DELETE FROM edge_geo_country").Exec(ctx) })
	storeCountryDB(t, db, "2026-09", writeCountryDB(t, "DBIP-Country-Lite", map[string]string{"203.0.113.0/24": "NL"}))

	srv := serveFiles(t, nil, nil) // every month 404s
	rep, err := refreshCountries(ctx, db, srv.Client(), srv.URL+"/", refreshNow)
	if err == nil || rep.Downloaded {
		t.Fatalf("want an error, got %+v, %v", rep, err)
	}
	if got := storedRelease(t, ctx); got != "2026-09" {
		t.Fatalf("the stored release must survive a failed download, got %q", got)
	}
}

func TestRefreshIsANoOpWhenTheCurrentMonthIsStored(t *testing.T) {
	db := edgeTestDB(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = db.NewRaw("DELETE FROM edge_geo_country").Exec(ctx) })
	storeCountryDB(t, db, "2026-11", writeCountryDB(t, "DBIP-Country-Lite", map[string]string{"203.0.113.0/24": "NL"}))

	var calls atomic.Int32
	srv := serveFiles(t, nil, &calls)
	rep, err := refreshCountries(ctx, db, srv.Client(), srv.URL+"/", refreshNow)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Downloaded || rep.Release != "2026-11" || rep.Reason == "" {
		t.Fatalf("report = %+v", rep)
	}
	if calls.Load() != 0 {
		t.Fatalf("the server was called %d times", calls.Load())
	}
}
