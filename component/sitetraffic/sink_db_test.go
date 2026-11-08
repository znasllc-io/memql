package sitetraffic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/edge/visit"
)

// sink_db_test.go -- the visit columns against the real edge_request table.
//
// The insert seam (sink_test.go) proves the mapping from a Record to a Row.
// Only the database proves the migration: that every column exists under the
// name the rollups will read and holds what the sink wrote, and that a row
// written without them -- every row recorded before they existed, and every
// row a previous release's edge writes during a rolling upgrade -- reads back
// as unmeasured instead of failing to insert or reading as a page view. And
// only the database can say whether a row derived from a crafted request is
// one it accepts, which is what decides whether the rest of its batch lands.
//
// Postgres-gated: edgeTestDB skips without a database and fails under
// MEMQL_REQUIRE_DB=1, which is how CI's db-tests lane runs this package.

// visitFieldsServedAt is far from the reader tests' window, and every row is
// deleted afterwards by site id: the table is shared with the rest of the lane.
var visitFieldsServedAt = time.Date(2032, 7, 14, 12, 0, 0, 0, time.UTC)

// readVisitColumns selects the visit columns BY NAME, independently of the
// Row's bun tags, so a column the migration spelled differently from the
// sink fails here rather than in a rollup.
func readVisitColumns(t *testing.T, db *bun.DB, id string) visitColumns {
	t.Helper()
	var got visitColumns
	err := db.QueryRowContext(context.Background(),
		`SELECT measured, page_view, bot, path, source, country, device, browser, os, visitor
		   FROM edge_request WHERE id = ?`, id).
		Scan(&got.Measured, &got.PageView, &got.Bot, &got.Path, &got.Source, &got.Country, &got.Device, &got.Browser, &got.OS, &got.Visitor)
	if err != nil {
		t.Fatalf("read the visit columns of %s: %v", id, err)
	}
	return got
}

func deleteSiteRequests(t *testing.T, db *bun.DB, siteID string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = db.NewRaw("DELETE FROM edge_request WHERE site_id = ?", siteID).Exec(context.Background())
	})
}

// A page view and a bot's request, written through the sink's own insert and
// read back column by column.
func TestRequestRowsWithVisitFieldsRoundTrip(t *testing.T) {
	db := edgeTestDB(t)
	siteID := "site-visit-fields-" + suffix(t)
	deleteSiteRequests(t, db, siteID)

	view := Row{
		ServedAt: visitFieldsServedAt, Id: newRowId(), SiteId: siteID, Node: "edge-test",
		Status: 200, PathClass: PathClassDocument, Bytes: 512, DurationNs: 2_000_000,
		Measured: true, PageView: true, Path: "/products/a", Source: "google", Country: "NL",
		Device: "desktop", Browser: "Chrome", OS: "Windows", Visitor: "0123456789abcdef0123456789abcdef",
	}
	bot := Row{
		ServedAt: visitFieldsServedAt.Add(time.Second), Id: newRowId(), SiteId: siteID, Node: "edge-test",
		Status: 200, PathClass: PathClassAsset, Bytes: 64, DurationNs: 1_000_000,
		Measured: true, Bot: true,
	}

	s := NewSink(db, SinkOptions{Logger: quietLogger()})
	if err := s.insertBatch(context.Background(), []Row{view, bot}); err != nil {
		t.Fatalf("insert the rows: %v", err)
	}
	for _, want := range []Row{view, bot} {
		if got := readVisitColumns(t, db, want.Id); got != visitColumnsOf(want) {
			t.Errorf("row %s reads the visit columns %+v, want %+v", want.Id, got, visitColumnsOf(want))
		}
	}
}

// A ROW WRITTEN WITHOUT THE VISIT COLUMNS READS AS UNMEASURED. The insert
// names only the eight columns the table had before them, as a previous
// release's edge does; rows already stored took the same defaults when the
// migration added the columns. measured=false is what lets a statistic say
// "not measured" for such a day instead of "nobody visited", while the row
// still counts for health.
func TestARowWrittenWithoutTheVisitFieldsReadsAsUnmeasured(t *testing.T) {
	db := edgeTestDB(t)
	siteID := "site-before-visit-fields-" + suffix(t)
	deleteSiteRequests(t, db, siteID)

	id := newRowId()
	if _, err := db.NewRaw(`INSERT INTO edge_request (served_at, id, site_id, node, status, path_class, bytes, duration_ns)
		VALUES (?, ?, ?, 'edge-previous-release', 200, 'document', 100, 1000000)`,
		visitFieldsServedAt, id, siteID).Exec(context.Background()); err != nil {
		t.Fatalf("an insert naming only the original columns was refused: %v", err)
	}
	if got := readVisitColumns(t, db, id); got != (visitColumns{}) {
		t.Errorf("a row written without the visit columns reads %+v, want every column at its default: unmeasured, no page view, no bot, no dimension", got)
	}
}

// ONE CRAFTED REQUEST MUST NOT ERASE A BATCH. A request path and a utm_source
// arrive percent-decoded, so "/%00x" carries a NUL byte, and the database
// driver refuses a NUL in a string outright: the whole INSERT fails, and with
// it every row in the batch -- up to 256 requests, other sites' among them.
//
// The crafted row's fields are derived exactly as the edge derives them, from
// the request itself, and the batch must land with the ordinary row beside it.
func TestARequestWithANulPathStillRecordsTheBatch(t *testing.T) {
	db := edgeTestDB(t)
	siteID := "site-nul-path-" + suffix(t)
	deleteSiteRequests(t, db, siteID)

	crafted := httptest.NewRequest(http.MethodGet, "/%00x?utm_source=news%00letter", nil)
	crafted.Host = "shop.example.com"
	poisoned := Row{
		ServedAt: visitFieldsServedAt, Id: newRowId(), SiteId: siteID, Node: "edge-test",
		Status: 200, PathClass: PathClassDocument, Measured: true, PageView: true,
		Path:   visit.NormalizePath(crafted.URL.Path),
		Source: visit.Source(crafted, "shop.example.com"),
	}
	ordinary := Row{
		ServedAt: visitFieldsServedAt.Add(time.Second), Id: newRowId(), SiteId: siteID, Node: "edge-test",
		Status: 200, PathClass: PathClassAsset, Measured: true,
	}

	s := NewSink(db, SinkOptions{Logger: quietLogger()})
	if err := s.insertBatch(context.Background(), []Row{poisoned, ordinary}); err != nil {
		t.Fatalf("a batch holding a row built from a NUL-bearing request was refused, so every row in it would be lost: %v", err)
	}
	var landed int
	if err := db.NewRaw("SELECT count(*) FROM edge_request WHERE site_id = ?", siteID).Scan(context.Background(), &landed); err != nil {
		t.Fatalf("count the batch: %v", err)
	}
	if landed != 2 {
		t.Errorf("%d of the batch's 2 rows landed", landed)
	}
	if got := readVisitColumns(t, db, poisoned.Id); got.Path != "/x" || got.Source != "newsletter" {
		t.Errorf("the crafted row stored path %q and source %q, want /x and newsletter -- the NUL dropped, the rest kept", got.Path, got.Source)
	}
}
