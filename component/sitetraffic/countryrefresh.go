package sitetraffic

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/oschwald/geoip2-golang"
	"github.com/uptrace/bun"
)

// The monthly country database download.
//
// Country data is "IP Geolocation by DB-IP": the IP-to-Country Lite database,
// licensed CC BY 4.0 (https://db-ip.com). It is fetched here, once a month,
// into edge_geo_country rather than pinned as a file in the image, because
// DB-IP serves only the current and the previous month (older months answer
// 404), so a pinned URL would rot, and a bundled file would age with the
// image. Every edge replica then picks the new release up through
// CountryLoader's hourly check of the stored label.
//
// The refresh NEVER runs on the request path. Its only caller is the
// geoCountryRefreshSweep automation on the cron leader, so a slow or absent
// DB-IP costs a day's retry and nothing else.

const (
	// defaultCountryBaseURL is where DB-IP publishes the free monthly files.
	defaultCountryBaseURL = "https://download.db-ip.com/free/"

	// The caps bound what a hostile or broken answer can make a node hold.
	// The real file is a few MiB compressed; the decompressed cap is the one
	// that matters against a gzip bomb, which is why it is enforced on the
	// stream rather than from a header the sender controls.
	maxCountryCompressed   = 32 << 20
	maxCountryDecompressed = 64 << 20
)

// CountryRefreshReport is what one refresh did. Reason says why nothing was
// downloaded (the stored release is current) and is empty after a download.
type CountryRefreshReport struct {
	Release    string `json:"release"`
	Downloaded bool   `json:"downloaded"`
	Reason     string `json:"reason,omitempty"`
}

// refreshCountries makes the stored country database the current month's, or
// the previous month's when DB-IP has not yet published the current one.
//
// Any failure leaves the stored row exactly as it was (the upsert is the last
// step and only runs on a validated file), so a stale database keeps serving
// until a refresh succeeds. The report names the release held afterwards in
// the no-op case and the release attempted otherwise.
func refreshCountries(ctx context.Context, db *bun.DB, client *http.Client, baseURL string, now time.Time) (CountryRefreshReport, error) {
	if db == nil {
		return CountryRefreshReport{}, errors.New("country refresh: no database handle")
	}
	if client == nil {
		client = http.DefaultClient
	}
	now = now.UTC()
	current := now.Format("2006-01")
	// First-of-month arithmetic: AddDate(0, -1, 0) from the 31st would
	// normalize into the same month.
	previous := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0).Format("2006-01")

	var stored string
	err := db.NewRaw(`SELECT release FROM edge_geo_country WHERE id = 1`).Scan(ctx, &stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return CountryRefreshReport{}, fmt.Errorf("country refresh: read the stored release: %w", err)
	}
	if stored == current {
		return CountryRefreshReport{Release: current, Reason: "the stored release is the current month"}, nil
	}

	release := current
	data, status, err := fetchCountryRelease(ctx, client, baseURL, release)
	if err != nil && status == http.StatusNotFound {
		release = previous
		data, _, err = fetchCountryRelease(ctx, client, baseURL, release)
	}
	if err != nil {
		return CountryRefreshReport{Release: release}, err
	}

	if err := validateCountryDB(data); err != nil {
		return CountryRefreshReport{Release: release}, fmt.Errorf("country refresh: release %s: %w", release, err)
	}
	if _, err := db.NewRaw(`INSERT INTO edge_geo_country (id, release, fetched_at, data) VALUES (1, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET release = EXCLUDED.release, fetched_at = EXCLUDED.fetched_at, data = EXCLUDED.data`,
		release, now, data).Exec(ctx); err != nil {
		return CountryRefreshReport{Release: release}, fmt.Errorf("country refresh: store release %s: %w", release, err)
	}
	return CountryRefreshReport{Release: release, Downloaded: true}, nil
}

// fetchCountryRelease downloads and gunzips one month's file under both caps.
// The HTTP status is returned alongside the error so the caller can tell a
// month DB-IP has not published (404) from every other failure.
func fetchCountryRelease(ctx context.Context, client *http.Client, baseURL, release string) ([]byte, int, error) {
	url := strings.TrimRight(baseURL, "/") + "/dbip-country-lite-" + release + ".mmdb.gz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("country refresh: build the request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("country refresh: download release %s: %w", release, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("country refresh: release %s answered HTTP %d", release, resp.StatusCode)
	}

	// Read one byte past each cap so "exactly at the cap" passes and "over"
	// is detectable without buffering the rest of the stream.
	compressed, err := io.ReadAll(io.LimitReader(resp.Body, maxCountryCompressed+1))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("country refresh: read release %s: %w", release, err)
	}
	if len(compressed) > maxCountryCompressed {
		return nil, resp.StatusCode, fmt.Errorf("country refresh: release %s is over the 32 MiB compressed limit", release)
	}
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("country refresh: release %s is not gzip: %w", release, err)
	}
	defer zr.Close()
	data, err := io.ReadAll(io.LimitReader(zr, maxCountryDecompressed+1))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("country refresh: gunzip release %s: %w", release, err)
	}
	if len(data) > maxCountryDecompressed {
		return nil, resp.StatusCode, fmt.Errorf("country refresh: release %s is over the 64 MiB decompressed limit", release)
	}
	return data, resp.StatusCode, nil
}

// validateCountryDB refuses anything the request path could not use: a file
// the reader cannot open, or one that is not a country database (a city
// database would open fine and then answer no country for every address).
// DB-IP's own type string is "DBIP-Country-Lite".
func validateCountryDB(data []byte) error {
	r, err := geoip2.FromBytes(data)
	if err != nil {
		return fmt.Errorf("not a readable MMDB: %w", err)
	}
	defer r.Close()
	if !strings.Contains(r.Metadata().DatabaseType, "Country") {
		return fmt.Errorf("database type %q is not a country database", r.Metadata().DatabaseType)
	}
	return nil
}
