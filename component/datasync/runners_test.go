package datasync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
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

	res, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept, "")
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

	if _, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
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
	engine.seed(`query syncStateById`, []map[string]any{{
		"id":             SyncStateID(testMirrorConcept, "shopify", "inbound", ""),
		"conceptId":      testMirrorConcept,
		"connector":      "shopify",
		"direction":      "inbound",
		"backfillCursor": "cursor-after-crash",
		"backfillStatus": "running",
	}})
	c := mirrorConnector(memqlsync.BackfillPage{Done: true})
	r := testRunner(engine, newFakeWriter(), c)

	if _, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
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
	engine.seed(`query syncStateById`, []map[string]any{{
		"id":        SyncStateID(testMirrorConcept, "shopify", "inbound", ""),
		"conceptId": testMirrorConcept,
		"connector": "shopify",
		"direction": "inbound",
		"paused":    true,
	}})
	c := mirrorConnector(memqlsync.BackfillPage{Done: true})
	c.reconcileReport = memqlsync.ReconcileReport{Checked: 5, Drifted: 2, Healed: 2}
	r := testRunner(engine, newFakeWriter(), c)

	res, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept, "")
	if err != nil {
		t.Fatalf("StartBackfill on a paused domain errored: %v", err)
	}
	if res.Pages != 0 {
		t.Errorf("a paused domain was backfilled anyway (%d pages)", res.Pages)
	}

	rep, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept, "")
	if err != nil {
		t.Fatalf("Reconcile on a paused domain errored: %v", err)
	}
	if !rep.Skipped || rep.Checked != 0 {
		t.Errorf("a paused domain was reconciled anyway: %+v", rep)
	}
	if len(c.reconcileSince) != 0 {
		t.Error("Reconcile was called on the connector for a paused domain")
	}
	if r.ReconcileDue(context.Background(), "shopify", c.domains[0], "") {
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

	rep, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept, "")
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

	rep, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept, "")
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

	if !r.ReconcileDue(context.Background(), "shopify", c.domains[0], "") {
		t.Error("a domain that has never been reconciled is not due")
	}
	if r.ReconcileDue(context.Background(), "shopify", memqlsync.DomainSpec{Concept: testMirrorConcept}, "") {
		t.Error("a domain with no interval reported itself due -- that domain is operator-driven only")
	}

	engine.seed(`query syncStateById`, []map[string]any{{
		"id":              SyncStateID(testMirrorConcept, "shopify", "inbound", ""),
		"conceptId":       testMirrorConcept,
		"connector":       "shopify",
		"direction":       "inbound",
		"lastReconcileAt": testNow.Add(-time.Hour).Format(time.RFC3339),
	}})
	if r.ReconcileDue(context.Background(), "shopify", c.domains[0], "") {
		t.Error("a domain reconciled an hour ago is due on a six-hour interval")
	}
}

// A backfill for a domain the connector does not declare fails loudly:
// there is nothing to page through, and a clean return would look like a
// completed backfill of an empty origin.
func TestABackfillOfAnUndeclaredDomainRefuses(t *testing.T) {
	r := testRunner(newFakeEngine(), newFakeWriter(), mirrorConnector())
	_, err := r.StartBackfill(context.Background(), "shopify", "v1:nowhere:thing", "")
	if err == nil {
		t.Fatal("a backfill of a domain the connector does not serve reported success")
	}
	if !strings.Contains(err.Error(), "does not serve") {
		t.Errorf("error = %q, want it to name the missing domain", err)
	}
}

// ---- partitions (a connector that serves many tenants) ----

// testPartitionedRunner is testRunner over a partitioned connector.
func testPartitionedRunner(engine *fakeEngine, p *fakePartitioned) *Runner {
	store := NewStore(engine)
	r := NewRunner(store, NewApplier(store, newFakeWriter()), discardLogger())
	r.lookup = func(name string) (memqlsync.Connector, bool) {
		if name == p.name {
			return p, true
		}
		return nil, false
	}
	r.now = func() time.Time { return testNow }
	return r
}

// testSweep is the reconcile builtin over one connector, without the
// process-global registry.
func testSweep(r *Runner, c memqlsync.Connector) *Integration {
	return &Integration{
		runner: r,
		logger: discardLogger(),
		bound:  func() []string { return []string{c.Name()} },
		lookup: func(name string) (memqlsync.Connector, bool) {
			if name == c.Name() {
				return c, true
			}
			return nil, false
		},
	}
}

func sweepOnce(t *testing.T, i *Integration) {
	t.Helper()
	if _, err := i.handleReconcileDomains(auth.ContextWithInternalOrigin(context.Background()), nil, 0); err != nil {
		t.Fatalf("reconcileDomains: %v", err)
	}
}

// seedPartitionState answers the runtime's by-id read of one partition's row.
func seedPartitionState(engine *fakeEngine, partition string, fields map[string]any) string {
	id := SyncStateID(testMirrorConcept, "shopify", "inbound", partition)
	row := map[string]any{
		"id":           id,
		"conceptId":    testMirrorConcept,
		"connector":    "shopify",
		"direction":    "inbound",
		"partitionKey": partition,
	}
	for k, v := range fields {
		row[k] = v
	}
	engine.seed(`query syncStateById(stateId: "`+id+`")`, []map[string]any{row})
	return id
}

// upsertsFor returns the health writes onto one partition's row.
func upsertsFor(engine *fakeEngine, partition string) []string {
	return engine.callsContaining(`mutation upsertSyncState(stateId: "` + SyncStateID(testMirrorConcept, "shopify", "inbound", partition) + `"`)
}

func TestAPartitionedConnectorIsSweptOnePartitionAtATime(t *testing.T) {
	engine := newFakeEngine()
	p := newFakePartitioned("a", "b")
	aRan := testNow.Add(-7 * time.Hour)
	bRan := testNow.Add(-9 * time.Hour)
	seedPartitionState(engine, "a", map[string]any{"lastReconcileAt": aRan.Format(time.RFC3339)})
	seedPartitionState(engine, "b", map[string]any{"lastReconcileAt": bRan.Format(time.RFC3339)})
	p.partReports["a"] = memqlsync.ReconcileReport{Checked: 4, Drifted: 1, Healed: 1}
	p.partReports["b"] = memqlsync.ReconcileReport{Checked: 2}
	r := testPartitionedRunner(engine, p)

	sweepOnce(t, testSweep(r, p))

	if got := p.partSince["a"]; len(got) != 1 || !got[0].Equal(aRan) {
		t.Errorf("partition a was swept since %v, want its own watermark %v", got, aRan)
	}
	if got := p.partSince["b"]; len(got) != 1 || !got[0].Equal(bRan) {
		t.Errorf("partition b was swept since %v, want its own watermark %v", got, bRan)
	}
	if len(p.partOrder) != 2 || p.partOrder[0] != "a" || p.partOrder[1] != "b" {
		t.Errorf("partitions swept in order %v, want [a b]", p.partOrder)
	}
	aw, bw := upsertsFor(engine, "a"), upsertsFor(engine, "b")
	if len(aw) != 1 || !strings.Contains(aw[0], "driftCount: 1") || !strings.Contains(aw[0], `partitionKey: "a"`) {
		t.Errorf("partition a's row did not record its own sweep: %v", aw)
	}
	if len(bw) != 1 || !strings.Contains(bw[0], "driftCount: 0") || !strings.Contains(bw[0], `partitionKey: "b"`) {
		t.Errorf("partition b's row did not record its own sweep: %v", bw)
	}
}

func TestOnePartitionsRefusalLeavesTheOthersSweepClean(t *testing.T) {
	engine := newFakeEngine()
	p := newFakePartitioned("a", "b")
	p.partErrs["a"] = errors.New("shopify admin: HTTP 502")
	p.partReports["b"] = memqlsync.ReconcileReport{Checked: 3}
	r := testPartitionedRunner(engine, p)

	sweepOnce(t, testSweep(r, p))

	aw, bw := upsertsFor(engine, "a"), upsertsFor(engine, "b")
	if len(aw) != 1 || !strings.Contains(aw[0], "HTTP 502") || strings.Contains(aw[0], "lastReconcileAt") {
		t.Errorf("partition a's failure was not recorded on its own row alone: %v", aw)
	}
	if len(bw) != 1 || !strings.Contains(bw[0], `lastError: ""`) || !strings.Contains(bw[0], "lastReconcileAt") {
		t.Errorf("partition b was not swept clean after a's failure: %v", bw)
	}
	if strings.Contains(strings.Join(bw, "\n"), "HTTP 502") {
		t.Errorf("a's error reached b's row: %v", bw)
	}
}

func TestAPartitionTheConnectorLeavesOutKeepsItsWatermark(t *testing.T) {
	engine := newFakeEngine()
	p := newFakePartitioned("b")
	seedPartitionState(engine, "a", map[string]any{"lastReconcileAt": testNow.Add(-30 * time.Hour).Format(time.RFC3339)})
	r := testPartitionedRunner(engine, p)

	sweepOnce(t, testSweep(r, p))

	if got := upsertsFor(engine, "a"); len(got) != 0 {
		t.Errorf("a partition the connector no longer answers was written: %v", got)
	}
	if _, swept := p.partSince["a"]; swept {
		t.Error("a partition the connector no longer answers was swept")
	}
	if got := upsertsFor(engine, "b"); len(got) != 1 {
		t.Errorf("partition b writes = %d, want 1", len(got))
	}
}

func TestTheRunnerNeverCallsAPartitionedConnectorsSharedSweep(t *testing.T) {
	engine := newFakeEngine()
	p := newFakePartitioned("a", "b")
	p.partReports["a"] = memqlsync.ReconcileReport{Checked: 4, Drifted: 2, Healed: 2}
	p.partReports["b"] = memqlsync.ReconcileReport{Checked: 6, Drifted: 1, Healed: 1}
	p.partPages["a"] = []memqlsync.BackfillPage{{NextCursor: "a-1"}, {Done: true}}
	p.partPages["b"] = []memqlsync.BackfillPage{{Done: true}}
	r := testPartitionedRunner(engine, p)

	// The sweep.
	sweepOnce(t, testSweep(r, p))

	// The runner, asked for no partition: it fans out over the partitions.
	rep, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept, "")
	if err != nil {
		t.Fatalf("Reconcile with no partition: %v", err)
	}
	if rep.Checked != 10 || rep.Drifted != 3 || rep.Healed != 3 {
		t.Errorf("fan-out report = %+v, want the sum 10/3/3", rep)
	}
	res, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept, "")
	if err != nil {
		t.Fatalf("StartBackfill with no partition: %v", err)
	}
	if res.Pages != 3 || !res.Done {
		t.Errorf("fan-out backfill = %+v, want 3 pages and done", res)
	}
	if got := p.partCursors["a"]; len(got) != 2 || got[0] != "" || got[1] != "a-1" {
		t.Errorf("partition a's cursors = %v, want [\"\" a-1]", got)
	}

	if len(p.reconcileSince) != 0 || len(p.backfillSeen) != 0 {
		t.Fatalf("the shared sweep was called (reconcile %d, backfill %d): a partitioned connector's shared cursor and watermark are the defect partitions remove",
			len(p.reconcileSince), len(p.backfillSeen))
	}
	if n := engine.countContaining(`stateId: "` + SyncStateID(testMirrorConcept, "shopify", "inbound", "") + `"`); n != 0 {
		t.Errorf("the shared row was read or written %d times for a partitioned connector", n)
	}
}

func TestAFanOutBackfillIsDoneOnlyWhenEveryPartitionIs(t *testing.T) {
	engine := newFakeEngine()
	p := newFakePartitioned("a", "b")
	p.partPages["a"] = []memqlsync.BackfillPage{{Done: true}}
	p.partPages["b"] = []memqlsync.BackfillPage{{NextCursor: "b-1"}, {NextCursor: "b-2"}}
	r := testPartitionedRunner(engine, p)
	r.maxPages = 2

	res, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Done {
		t.Errorf("reported done while partition b has pages left: %+v", res)
	}
}

func TestARunnerRefusesAPartitionToAConnectorWithNone(t *testing.T) {
	c := mirrorConnector()
	r := testRunner(newFakeEngine(), newFakeWriter(), c)
	if _, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept, "a"); err == nil {
		t.Error("a backfill of partition a ran on a connector that has no partitions")
	}
	if _, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept, "a"); err == nil {
		t.Error("a sweep of partition a ran on a connector that has no partitions")
	}
	if len(c.backfillSeen) != 0 || len(c.reconcileSince) != 0 {
		t.Error("the shared methods were called for a named partition")
	}
}

func TestAPauseIsOnePartitionsOwn(t *testing.T) {
	engine := newFakeEngine()
	p := newFakePartitioned("a", "b")
	r := testPartitionedRunner(engine, p)

	if err := r.SetPaused(context.Background(), "shopify", testMirrorConcept, "a", true); err != nil {
		t.Fatal(err)
	}
	aw := upsertsFor(engine, "a")
	if len(aw) != 1 || !strings.Contains(aw[0], "paused: true") || !strings.Contains(aw[0], `partitionKey: "a"`) {
		t.Fatalf("the pause did not land on partition a's own row: %v", aw)
	}
	if got := upsertsFor(engine, "b"); len(got) != 0 {
		t.Fatalf("pausing a wrote b's row: %v", got)
	}

	seedPartitionState(engine, "a", map[string]any{"paused": true})
	sweepOnce(t, testSweep(r, p))
	if _, swept := p.partSince["a"]; swept {
		t.Error("a paused partition was swept")
	}
	if got := p.partSince["b"]; len(got) != 1 {
		t.Errorf("partition b was not swept while a was paused: %v", got)
	}
}

func TestAPauseWithNoPartitionHoldsForEveryPartition(t *testing.T) {
	engine := newFakeEngine()
	p := newFakePartitioned("a", "b")
	r := testPartitionedRunner(engine, p)

	if err := r.SetPaused(context.Background(), "shopify", testMirrorConcept, "", true); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"a", "b"} {
		w := upsertsFor(engine, part)
		if len(w) != 1 || !strings.Contains(w[0], "paused: true") || !strings.Contains(w[0], `partitionKey: "`+part+`"`) {
			t.Errorf("a pause naming no partition did not land on partition %s's row: %v", part, w)
		}
	}
	if n := engine.countContaining(`stateId: "` + SyncStateID(testMirrorConcept, "shopify", "inbound", "") + `"`); n != 0 {
		t.Errorf("the shared row was read or written %d times: a partitioned connector's pause would not hold", n)
	}

	seedPartitionState(engine, "a", map[string]any{"paused": true})
	seedPartitionState(engine, "b", map[string]any{"paused": true})
	sweepOnce(t, testSweep(r, p))
	if len(p.partOrder) != 0 || len(p.reconcileSince) != 0 {
		t.Errorf("a paused connector was swept anyway: partitions %v, shared %d", p.partOrder, len(p.reconcileSince))
	}
}

func TestReconcileDueWithNoPartitionAsksEveryPartition(t *testing.T) {
	engine := newFakeEngine()
	p := newFakePartitioned("a", "b")
	r := testPartitionedRunner(engine, p)
	spec := p.domains[0]
	recent := testNow.Add(-time.Hour).Format(time.RFC3339)
	old := testNow.Add(-7 * time.Hour).Format(time.RFC3339)

	seedPartitionState(engine, "a", map[string]any{"lastReconcileAt": recent})
	seedPartitionState(engine, "b", map[string]any{"lastReconcileAt": old})
	if !r.ReconcileDue(context.Background(), "shopify", spec, "") {
		t.Error("not due with no partition while partition b is due")
	}

	seedPartitionState(engine, "b", map[string]any{"lastReconcileAt": recent})
	if r.ReconcileDue(context.Background(), "shopify", spec, "") {
		t.Error("due with no partition while neither partition is")
	}
	if n := engine.countContaining(`stateId: "` + SyncStateID(testMirrorConcept, "shopify", "inbound", "") + `"`); n != 0 {
		t.Errorf("the shared row was read %d times for a partitioned connector", n)
	}
}
