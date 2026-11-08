package sitetraffic

import (
	"context"
	"testing"
	"time"

	"github.com/uptrace/bun"
)

// sink_db_test.go -- the visit columns against the real edge_request table.
//
// The insert seam (sink_test.go) proves the mapping from a Record to a Row.
// Only the database proves the migration: that every column exists under the
// name the rollups will read and holds what the sink wrote, and that a row
// written without them -- every row recorded before they existed, and every
// row a previous release's edge writes during a rolling upgrade -- reads back
// as unmeasured instead of failing to insert or reading as a page view.
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
