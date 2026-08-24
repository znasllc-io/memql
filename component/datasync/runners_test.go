package datasync

import (
	"context"
	"strings"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

func testRunner(engine *fakeEngine, writer MirrorWriter, c *fakeConnector) *Runner {
	store := NewStore(engine)
	r := NewRunner(store, NewApplier(store, writer), discardLogger())
	r.lookup = func(name string) (memqlsync.Connector, bool) {
		if name == c.name {
			return c, true
		}
		return nil, false
	}
	r.now = func() time.Time { return testNow }
	return r
}

func mirrorConnector(pages ...memqlsync.BackfillPage) *fakeConnector {
	return &fakeConnector{
		name:          "shopify",
		domains:       []memqlsync.DomainSpec{{Concept: testMirrorConcept, Direction: memqlsync.DirectionInbound, ReconcileInterval: 6 * time.Hour}},
		backfillPages: pages,
	}
}

// A backfill pages through the origin and applies every page.
func TestABackfillPagesThroughTheOriginAndApplies(t *testing.T) {
	engine := newFakeEngine()
	writer := newFakeWriter()
	c := mirrorConnector(
		memqlsync.BackfillPage{
			Writes:     []memqlsync.MirrorWrite{{Concept: testMirrorConcept, RowId: "p1", Version: "2026-08-23T10:00:00Z"}},
			NextCursor: "cursor-1",
		},
		memqlsync.BackfillPage{
			Writes: []memqlsync.MirrorWrite{{Concept: testMirrorConcept, RowId: "p2", Version: "2026-08-23T10:00:01Z"}},
			Done:   true,
		},
	)
	r := testRunner(engine, writer, c)

	res, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept)
	if err != nil {
		t.Fatalf("StartBackfill: %v", err)
	}
	if res.Pages != 2 || res.Applied != 2 || !res.Done {
		t.Fatalf("pages=%d applied=%d done=%t, want 2/2/true", res.Pages, res.Applied, res.Done)
	}
	if ids := writer.writtenIDs(); len(ids) != 2 || ids[0] != "p1" || ids[1] != "p2" {
		t.Errorf("wrote %v, want [p1 p2]", ids)
	}
	// The second page must have been asked for from the FIRST page's
	// cursor, not from the start.
	if len(c.backfillSeen) != 2 || c.backfillSeen[0] != "" || c.backfillSeen[1] != "cursor-1" {
		t.Errorf("cursors asked for = %v, want [\"\" cursor-1]", c.backfillSeen)
	}
}

// The cursor is persisted after EVERY page. A restart mid-backfill has
// to resume, not restart -- a backfill that can never finish is worse
// than one that is slow.
func TestABackfillPersistsItsCursorAfterEveryPage(t *testing.T) {
	engine := newFakeEngine()
	c := mirrorConnector(
		memqlsync.BackfillPage{NextCursor: "cursor-1"},
		memqlsync.BackfillPage{NextCursor: "cursor-2"},
		memqlsync.BackfillPage{Done: true},
	)
	r := testRunner(engine, newFakeWriter(), c)

	if _, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept); err != nil {
		t.Fatalf("StartBackfill: %v", err)
	}
	writes := engine.callsContaining("upsertSyncState")
	var sawFirst, sawSecond bool
	for _, w := range writes {
		if strings.Contains(w, `backfillCursor: "cursor-1"`) {
			sawFirst = true
		}
		if strings.Contains(w, `backfillCursor: "cursor-2"`) {
			sawSecond = true
		}
	}
	if !sawFirst || !sawSecond {
		t.Fatalf("the cursor was not persisted after every page (saw cursor-1=%t cursor-2=%t) in:\n%s",
			sawFirst, sawSecond, strings.Join(writes, "\n"))
	}
}

// A backfill resumes from the cursor syncState holds, which is what
// makes a restart a resume.
func TestABackfillResumesFromTheStoredCursor(t *testing.T) {
	engine := newFakeEngine()
	engine.seed(`query syncStateFor`, []map[string]any{{
		"id":             SyncStateID(testMirrorConcept, "shopify", "inbound"),
		"conceptId":      testMirrorConcept,
		"connector":      "shopify",
		"direction":      "inbound",
		"backfillCursor": "cursor-after-crash",
		"backfillStatus": "running",
	}})
	c := mirrorConnector(memqlsync.BackfillPage{Done: true})
	r := testRunner(engine, newFakeWriter(), c)

	if _, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept); err != nil {
		t.Fatalf("StartBackfill: %v", err)
	}
	if len(c.backfillSeen) != 1 || c.backfillSeen[0] != "cursor-after-crash" {
		t.Fatalf("the backfill restarted from %v instead of resuming from the stored cursor", c.backfillSeen)
	}
}

// A paused domain is skipped cleanly. "Paused" is a state, not a fault:
// a caller polling this must not see a stream of errors for a switch
// somebody flipped on purpose.
func TestAPausedDomainStopsBothRunners(t *testing.T) {
	engine := newFakeEngine()
	engine.seed(`query syncStateFor`, []map[string]any{{
		"id":        SyncStateID(testMirrorConcept, "shopify", "inbound"),
		"conceptId": testMirrorConcept,
		"connector": "shopify",
		"direction": "inbound",
		"paused":    true,
	}})
	c := mirrorConnector(memqlsync.BackfillPage{Done: true})
	c.reconcileReport = memqlsync.ReconcileReport{Checked: 5, Drifted: 2, Healed: 2}
	r := testRunner(engine, newFakeWriter(), c)

	res, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept)
	if err != nil {
		t.Fatalf("StartBackfill on a paused domain errored: %v", err)
	}
	if res.Pages != 0 {
		t.Errorf("a paused domain was backfilled anyway (%d pages)", res.Pages)
	}

	rep, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept)
	if err != nil {
		t.Fatalf("Reconcile on a paused domain errored: %v", err)
	}
	if !rep.Skipped || rep.Checked != 0 {
		t.Errorf("a paused domain was reconciled anyway: %+v", rep)
	}
	if len(c.reconcileSince) != 0 {
		t.Error("Reconcile was called on the connector for a paused domain")
	}
	if r.ReconcileDue(context.Background(), "shopify", c.domains[0]) {
		t.Error("a paused domain reported itself due for reconciliation")
	}
}

// A sweep that finds drift heals it and RECORDS the count -- recurring
// drift is a webhook stream that is not arriving, and the heal hides the
// symptom while leaving the cause.
func TestASweepRecordsTheDriftItHealed(t *testing.T) {
	engine := newFakeEngine()
	c := mirrorConnector()
	c.reconcileReport = memqlsync.ReconcileReport{Checked: 10, Drifted: 3, Healed: 3}
	r := testRunner(engine, newFakeWriter(), c)

	rep, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rep.Checked != 10 || rep.Drifted != 3 || rep.Healed != 3 {
		t.Fatalf("report = %+v, want 10/3/3", rep)
	}
	writes := engine.callsContaining("upsertSyncState")
	if len(writes) == 0 {
		t.Fatal("the sweep recorded nothing")
	}
	last := writes[len(writes)-1]
	if !strings.Contains(last, "driftCount: 3") {
		t.Errorf("the drift count was not recorded: %q", last)
	}
	if !strings.Contains(last, "lastReconcileAt") {
		t.Errorf("lastReconcileAt was not stamped: %q -- the next sweep's `since` comes from it", last)
	}
}

// A connector that does not reconcile is a configuration fact, not a
// failure, and must not be written onto lastError.
func TestAConnectorThatDoesNotReconcileIsSkippedNotFailed(t *testing.T) {
	engine := newFakeEngine()
	c := mirrorConnector()
	c.reconcileErr = memqlsync.NotImplemented("shopify", "Reconcile")
	r := testRunner(engine, newFakeWriter(), c)

	rep, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept)
	if err != nil {
		t.Fatalf("an unimplemented Reconcile was reported as an error: %v", err)
	}
	if !rep.Skipped {
		t.Error("an unimplemented Reconcile did not report itself skipped")
	}
	for _, w := range engine.callsContaining("upsertSyncState") {
		if strings.Contains(w, "lastError") && !strings.Contains(w, `lastError: ""`) {
			t.Errorf("an unimplemented capability was written onto lastError, which is for things that went wrong: %q", w)
		}
	}
}

// The schedule comes from the DOMAIN's own interval, and a domain with
// no interval is never due -- reconciliation is then operator-driven.
func TestReconciliationIsDueOnTheDomainsOwnSchedule(t *testing.T) {
	engine := newFakeEngine()
	c := mirrorConnector()
	r := testRunner(engine, newFakeWriter(), c)

	if !r.ReconcileDue(context.Background(), "shopify", c.domains[0]) {
		t.Error("a domain that has never been reconciled is not due")
	}
	if r.ReconcileDue(context.Background(), "shopify", memqlsync.DomainSpec{Concept: testMirrorConcept}) {
		t.Error("a domain with no interval reported itself due -- that domain is operator-driven only")
	}

	engine.seed(`query syncStateFor`, []map[string]any{{
		"id":              SyncStateID(testMirrorConcept, "shopify", "inbound"),
		"conceptId":       testMirrorConcept,
		"connector":       "shopify",
		"direction":       "inbound",
		"lastReconcileAt": testNow.Add(-time.Hour).Format(time.RFC3339),
	}})
	if r.ReconcileDue(context.Background(), "shopify", c.domains[0]) {
		t.Error("a domain reconciled an hour ago is due on a six-hour interval")
	}
}

// A backfill for a domain the connector does not declare fails loudly:
// there is nothing to page through, and a clean return would look like a
// completed backfill of an empty origin.
func TestABackfillOfAnUndeclaredDomainRefuses(t *testing.T) {
	r := testRunner(newFakeEngine(), newFakeWriter(), mirrorConnector())
	_, err := r.StartBackfill(context.Background(), "shopify", "v1:nowhere:thing")
	if err == nil {
		t.Fatal("a backfill of a domain the connector does not serve reported success")
	}
	if !strings.Contains(err.Error(), "does not serve") {
		t.Errorf("error = %q, want it to name the missing domain", err)
	}
}
