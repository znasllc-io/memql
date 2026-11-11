package datasync

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
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

	// A pause naming a partition would write a four-part row this
	// connector's runners never read: a pause that does not hold, and an
	// orphan row on the Data origins page.
	engine := newFakeEngine()
	r = testRunner(engine, newFakeWriter(), c)
	if err := r.SetPaused(context.Background(), "shopify", testMirrorConcept, "a", true); err == nil {
		t.Error("a pause of partition a succeeded on a connector that has no partitions")
	} else if !strings.Contains(err.Error(), "has no partitions") {
		t.Errorf("refusal = %q, want the runner's own has-no-partitions refusal", err)
	}
	if n := engine.countContaining("mutation "); n != 0 {
		t.Errorf("a refused pause still wrote %d rows: %v", n, engine.callsContaining("mutation "))
	}
	if r.ReconcileDue(context.Background(), "shopify", c.domains[0], "a") {
		t.Error("partition a reported itself due on a connector that has no partitions")
	}
	if n := engine.countContaining("syncStateById"); n != 0 {
		t.Errorf("a partition the connector does not have was read %d times", n)
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

// sweepOnATickingClock runs one clean sweep whose connector takes ten
// minutes, then returns the state row the sweep wrote and the moment it
// started and ended.
func sweepOnATickingClock(t *testing.T) (engine *fakeEngine, r *Runner, c *fakeConnector, start, end time.Time, lastReconcile, lastAttempt time.Time) {
	t.Helper()
	engine = newFakeEngine()
	c = mirrorConnector()
	r = testRunner(engine, newFakeWriter(), c)
	clock := testNow
	r.now = func() time.Time { return clock }
	c.duringReconcile = func() { clock = clock.Add(10 * time.Minute) }
	start = clock
	if _, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	end = clock
	writes := engine.callsContaining("upsertSyncState")
	if len(writes) == 0 {
		t.Fatal("the sweep recorded nothing")
	}
	last := writes[len(writes)-1]
	parse := func(field string) time.Time {
		m := regexp.MustCompile(field + `: "([^"]+)"`).FindStringSubmatch(last)
		if m == nil {
			t.Fatalf("%s was not written: %q", field, last)
		}
		ts, err := time.Parse(time.RFC3339, m[1])
		if err != nil {
			t.Fatalf("%s = %q: %v", field, m[1], err)
		}
		return ts
	}
	return engine, r, c, start, end, parse("lastReconcileAt"), parse("lastAttemptAt")
}

// The next sweep asks the origin for everything since the moment this
// sweep STARTED, less two minutes: a row that changed behind the page
// the sweep had already read is older than its end.
func TestTheWatermarkIsWhenTheSweepStartedLessTwoMinutes(t *testing.T) {
	engine, r, c, start, end, lastReconcile, lastAttempt := sweepOnATickingClock(t)
	if !end.Equal(start.Add(10 * time.Minute)) {
		t.Fatalf("the clock did not move inside the sweep: %v to %v", start, end)
	}
	if want := start.Add(-reconcileOverlap); !lastReconcile.Equal(want) {
		t.Errorf("lastReconcileAt = %v, want the start less two minutes, %v", lastReconcile, want)
	}
	if !lastAttempt.Equal(end) {
		t.Errorf("lastAttemptAt = %v, want when the sweep ended, %v", lastAttempt, end)
	}

	engine.seed(`query syncStateById`, []map[string]any{{
		"id": SyncStateID(testMirrorConcept, "shopify", "inbound", ""), "conceptId": testMirrorConcept,
		"connector": "shopify", "direction": "inbound",
		"lastReconcileAt": lastReconcile.Format(time.RFC3339), "lastAttemptAt": lastAttempt.Format(time.RFC3339),
	}})
	if _, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if len(c.reconcileSince) != 2 || !c.reconcileSince[1].Equal(start.Add(-2*time.Minute)) {
		t.Errorf("the next since = %v, want %v", c.reconcileSince, start.Add(-2*time.Minute))
	}
}

// A row stamped three minutes after the sweep began, behind the page it
// had read, is at or after the next sweep's since.
func TestARowChangedDuringASweepIsReadByTheNext(t *testing.T) {
	_, _, _, start, _, lastReconcile, _ := sweepOnATickingClock(t)
	changed := start.Add(3 * time.Minute)
	if changed.Before(lastReconcile) {
		t.Errorf("a row changed at %v is before the next since %v and would never be read", changed, lastReconcile)
	}
}

// The schedule still runs from when the sweep ended, not from the
// earlier watermark.
func TestTheCadenceStillRunsFromWhenTheSweepEnded(t *testing.T) {
	engine, r, c, _, end, lastReconcile, lastAttempt := sweepOnATickingClock(t)
	engine.seed(`query syncStateById`, []map[string]any{{
		"id": SyncStateID(testMirrorConcept, "shopify", "inbound", ""), "conceptId": testMirrorConcept,
		"connector": "shopify", "direction": "inbound",
		"lastReconcileAt": lastReconcile.Format(time.RFC3339), "lastAttemptAt": lastAttempt.Format(time.RFC3339),
	}})
	interval := c.domains[0].ReconcileInterval
	r.now = func() time.Time { return end.Add(interval - time.Minute) }
	if r.ReconcileDue(context.Background(), "shopify", c.domains[0], "") {
		t.Error("due before an interval had passed since the sweep ENDED")
	}
	r.now = func() time.Time { return end.Add(interval) }
	if !r.ReconcileDue(context.Background(), "shopify", c.domains[0], "") {
		t.Error("not due an interval after the sweep ended")
	}
}

// ---- a backfill that starts and continues by itself ----

// stateRunner is a runner over a state-keeping engine and one bound
// connector, without the process-global registry.
func stateRunner(engine *stateEngine, c memqlsync.Connector) *Runner {
	store := NewStore(engine)
	r := NewRunner(store, NewApplier(store, newFakeWriter()), discardLogger())
	r.lookup = func(name string) (memqlsync.Connector, bool) {
		if name == c.Name() {
			return c, true
		}
		return nil, false
	}
	r.bound = func() []string { return []string{c.Name()} }
	r.now = func() time.Time { return testNow }
	return r
}

// continueOnce runs one continuing pass and fails the test on an error.
func continueOnce(t *testing.T, r *Runner) ContinueResult {
	t.Helper()
	res, err := r.ContinueBackfills(context.Background())
	if err != nil {
		t.Fatalf("ContinueBackfills: %v", err)
	}
	return res
}

// keptState is the health row the state engine holds for one domain.
func keptState(t *testing.T, engine *stateEngine, concept, partition string) SyncState {
	t.Helper()
	row := engine.row(SyncStateID(concept, "shopify", "inbound", partition))
	if row == nil {
		t.Fatalf("no health row for %s partition %q", concept, partition)
	}
	return syncStateFromRow(row)
}

// writeKept writes one health row straight onto the state engine.
func writeKept(t *testing.T, engine *stateEngine, st SyncState) {
	t.Helper()
	st.Connector, st.Direction = "shopify", "inbound"
	if st.ConceptID == "" {
		st.ConceptID = testMirrorConcept
	}
	if err := NewStore(engine).WriteSyncState(OperatorContext(context.Background()), st); err != nil {
		t.Fatal(err)
	}
}

const secondMirrorConcept = "v1:shopify:customer"

func TestAQueuedBackfillIsStartedByTheContinuingPass(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector(memqlsync.BackfillPage{NextCursor: "c-1"}, memqlsync.BackfillPage{Done: true})
	r := stateRunner(engine, c)

	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatalf("QueueBackfill: %v", err)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "queued" || st.BackfillCursor != "" {
		t.Fatalf("queued row = %+v, want queued with an empty cursor", st)
	}
	if len(c.backfillSeen) != 0 {
		t.Fatal("queueing ran the backfill: queueing only records that one is wanted")
	}

	res := continueOnce(t, r)
	if res.Started != 1 || res.Completed != 1 || res.Advanced != 0 {
		t.Errorf("result = %+v, want started 1 and completed 1", res)
	}
	if len(c.backfillSeen) != 2 {
		t.Errorf("the connector was asked %d times, want both pages", len(c.backfillSeen))
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "complete" {
		t.Errorf("status = %q, want complete", st.BackfillStatus)
	}
}

func TestABackfillThatUsedItsPageBudgetIsContinuedToComplete(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector(
		memqlsync.BackfillPage{NextCursor: "c-1"},
		memqlsync.BackfillPage{NextCursor: "c-2"},
		memqlsync.BackfillPage{NextCursor: "c-3"},
		memqlsync.BackfillPage{NextCursor: "c-4"},
		memqlsync.BackfillPage{Done: true},
	)
	r := stateRunner(engine, c)
	r.maxPages = 2
	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}

	want := []struct {
		res    ContinueResult
		status string
		cursor string
	}{
		{ContinueResult{Started: 1}, "running", "c-2"},
		{ContinueResult{Advanced: 1}, "running", "c-4"},
		{ContinueResult{Completed: 1}, "complete", ""},
	}
	for i, w := range want {
		res := continueOnce(t, r)
		st := keptState(t, engine, testMirrorConcept, "")
		if res != w.res || st.BackfillStatus != w.status || st.BackfillCursor != w.cursor {
			t.Fatalf("call %d: result %+v, row %s at %q; want %+v, %s at %q", i+1, res, st.BackfillStatus, st.BackfillCursor, w.res, w.status, w.cursor)
		}
	}
	if got := strings.Join(c.backfillSeen, ","); got != ",c-1,c-2,c-3,c-4" {
		t.Errorf("cursors asked for = %q, want each page once from where the last call stopped", got)
	}
	if res := continueOnce(t, r); res != (ContinueResult{}) || len(c.backfillSeen) != 5 {
		t.Errorf("a complete backfill was continued again: %+v, %d asks", res, len(c.backfillSeen))
	}
}

func TestOneStoreRunsOneBackfillAtATime(t *testing.T) {
	engine := newStateEngine()
	p := newFakePartitioned("a")
	p.domains = append(p.domains, memqlsync.DomainSpec{Concept: secondMirrorConcept, Direction: memqlsync.DirectionInbound})
	p.partPages["a"] = []memqlsync.BackfillPage{{NextCursor: "a-1"}, {Done: true}}
	r := stateRunner(engine, p)
	r.maxPages = 1
	for _, concept := range []string{testMirrorConcept, secondMirrorConcept} {
		if err := r.QueueBackfill(context.Background(), "shopify", concept, "a"); err != nil {
			t.Fatal(err)
		}
	}

	if res := continueOnce(t, r); res != (ContinueResult{Started: 1}) {
		t.Errorf("first call = %+v, want one domain started", res)
	}
	if res := continueOnce(t, r); res != (ContinueResult{Completed: 1}) {
		t.Errorf("second call = %+v, want the running domain completed and nothing else started", res)
	}
	if st := keptState(t, engine, secondMirrorConcept, "a"); st.BackfillStatus != "queued" {
		t.Fatalf("the second domain is %q after the first completed in the same call; it waits for the next", st.BackfillStatus)
	}
	if res := continueOnce(t, r); res != (ContinueResult{Started: 1, Completed: 1}) {
		t.Errorf("third call = %+v, want the second domain started and completed", res)
	}
	want := "a/" + testMirrorConcept + ",a/" + testMirrorConcept + ",a/" + secondMirrorConcept
	if got := strings.Join(p.partBackfills, ","); got != want {
		t.Errorf("backfill steps = %s, want %s: the second domain may not start while the first runs", got, want)
	}
}

func TestAPollThatMadeNoProgressYieldsUntilTheNextTick(t *testing.T) {
	engine := newStateEngine()
	// Three answers of an operation still running: no writes, and the cursor
	// it was given back.
	c := mirrorConnector(
		memqlsync.BackfillPage{NextCursor: "op-1"},
		memqlsync.BackfillPage{NextCursor: "op-1"},
		memqlsync.BackfillPage{NextCursor: "op-1"},
	)
	r := stateRunner(engine, c)
	writeKept(t, engine, SyncState{BackfillStatus: "running", BackfillCursor: "op-1"})

	res := continueOnce(t, r)
	if len(c.backfillSeen) != 1 {
		t.Fatalf("the connector was asked %d times in one tick, want once: a poll that made no progress waits for the next tick", len(c.backfillSeen))
	}
	if res != (ContinueResult{}) {
		t.Errorf("result = %+v, want nothing: a poll is not progress", res)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "running" || st.BackfillCursor != "op-1" {
		t.Errorf("row = %s at %q, want still running at op-1", st.BackfillStatus, st.BackfillCursor)
	}
}

func TestAPausedDomainIsNotStarted(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector(memqlsync.BackfillPage{Done: true})
	r := stateRunner(engine, c)
	writeKept(t, engine, SyncState{BackfillStatus: "queued", Paused: true})

	if res := continueOnce(t, r); res != (ContinueResult{}) {
		t.Errorf("result = %+v, want nothing for a paused domain", res)
	}
	if len(c.backfillSeen) != 0 {
		t.Error("a paused domain's backfill was started")
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "queued" || !st.Paused {
		t.Errorf("row = %+v, want left queued and paused", st)
	}
}

func TestQueueingLeavesARunningBackfillAlone(t *testing.T) {
	engine := newStateEngine()
	r := stateRunner(engine, mirrorConnector())
	writeKept(t, engine, SyncState{BackfillStatus: "running", BackfillCursor: "c-7"})
	writes := engine.countContaining("mutation upsertSyncState")

	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "running" || st.BackfillCursor != "c-7" {
		t.Errorf("row = %s at %q, want the running backfill left at its cursor", st.BackfillStatus, st.BackfillCursor)
	}
	if n := engine.countContaining("mutation upsertSyncState"); n != writes {
		t.Errorf("queueing a running backfill wrote its row %d times", n-writes)
	}

	// A complete one is queued again, from the start.
	writeKept(t, engine, SyncState{BackfillStatus: "complete", BackfillCursor: "c-9"})
	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "queued" || st.BackfillCursor != "" {
		t.Errorf("row = %s at %q, want a complete backfill queued again with an empty cursor", st.BackfillStatus, st.BackfillCursor)
	}
}

func TestQueueingFollowsThePartitionRule(t *testing.T) {
	engine := newStateEngine()
	p := newFakePartitioned("a", "b")
	r := stateRunner(engine, p)
	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"a", "b"} {
		if st := keptState(t, engine, testMirrorConcept, part); st.BackfillStatus != "queued" {
			t.Errorf("partition %s = %q, want queued: no partition queues every one", part, st.BackfillStatus)
		}
	}
	if engine.row(SyncStateID(testMirrorConcept, "shopify", "inbound", "")) != nil {
		t.Error("the shared row of a partitioned connector was queued")
	}

	engine = newStateEngine()
	r = stateRunner(engine, mirrorConnector())
	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, "a"); err == nil || !strings.Contains(err.Error(), "has no partitions") {
		t.Errorf("a partition queued on a connector without partitions: %v", err)
	}
	if n := engine.countContaining("mutation "); n != 0 {
		t.Errorf("a refused queue wrote %d rows", n)
	}
}

func TestTheFirstCleanSweepOfARefusedDomainQueuesItsBackfill(t *testing.T) {
	for name, tc := range map[string]struct {
		lastError, status string
		want              string
	}{
		"not granted":   {memqlsync.NotGranted("shopify", "read_customers").Error(), "none", "queued"},
		"not requested": {memqlsync.NotRequested("shopify").Error(), "none", "queued"},
		"no refusal":    {"", "complete", "complete"},
		"a failure":     {"shopify admin: HTTP 502", "complete", "complete"},
		"running":       {memqlsync.NotGranted("shopify", "read_customers").Error(), "running", "running"},
	} {
		t.Run(name, func(t *testing.T) {
			engine := newStateEngine()
			c := mirrorConnector()
			c.reconcileReport = memqlsync.ReconcileReport{Checked: 3}
			r := stateRunner(engine, c)
			writeKept(t, engine, SyncState{BackfillStatus: tc.status, BackfillCursor: "c-3", LastError: tc.lastError})

			if _, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
				t.Fatal(err)
			}
			st := keptState(t, engine, testMirrorConcept, "")
			if st.BackfillStatus != tc.want || st.LastError != "" {
				t.Errorf("after a clean sweep: %s, lastError %q; want %s and the reason cleared", st.BackfillStatus, st.LastError, tc.want)
			}
			if tc.want == "queued" && st.BackfillCursor != "" {
				t.Errorf("queued at cursor %q, want from the start", st.BackfillCursor)
			}
		})
	}
}

func TestARefusedQueuedBackfillIsAStandingAnswerNotAFailure(t *testing.T) {
	for name, refusal := range map[string]error{
		"not requested": memqlsync.NotRequested("shopify"),
		"not granted":   memqlsync.NotGranted("shopify", "read_customers"),
	} {
		t.Run(name, func(t *testing.T) {
			engine := newStateEngine()
			p := newFakePartitioned("a")
			p.partErrs["a"] = refusal
			r := stateRunner(engine, p)
			if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, "a"); err != nil {
				t.Fatal(err)
			}

			if _, err := r.ContinueBackfills(context.Background()); err != nil {
				t.Fatalf("a refused backfill failed the pass: %v", err)
			}
			st := keptState(t, engine, testMirrorConcept, "a")
			if st.BackfillStatus != "none" || st.LastError != refusal.Error() || st.LastAttemptAt.IsZero() {
				t.Fatalf("row = %s, lastError %q, attempt %v; want none with the reason and the attempt recorded", st.BackfillStatus, st.LastError, st.LastAttemptAt)
			}
			continueOnce(t, r)
			if len(p.partBackfills) != 1 {
				t.Errorf("the refused backfill was tried %d times, want once: a standing answer is not retried on every tick", len(p.partBackfills))
			}
		})
	}

	engine := newStateEngine()
	p := newFakePartitioned("a")
	p.partErrs["a"] = errors.New("shopify admin: HTTP 502")
	r := stateRunner(engine, p)
	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ContinueBackfills(context.Background()); err == nil {
		t.Error("a real failure was not reported by the pass")
	}
	if st := keptState(t, engine, testMirrorConcept, "a"); st.BackfillStatus != "failed" {
		t.Errorf("a real failure is %q, want failed", st.BackfillStatus)
	}
}

func TestAnUnpartitionedRowOfAPartitionedConnectorIsNotContinued(t *testing.T) {
	engine := newStateEngine()
	p := newFakePartitioned("a")
	r := stateRunner(engine, p)
	writeKept(t, engine, SyncState{BackfillStatus: "queued"})
	shared := SyncStateID(testMirrorConcept, "shopify", "inbound", "")
	before := engine.countContaining(`stateId: "` + shared + `"`)

	if res := continueOnce(t, r); res != (ContinueResult{}) {
		t.Errorf("result = %+v, want nothing", res)
	}
	if len(p.backfillSeen) != 0 || len(p.partBackfills) != 0 {
		t.Errorf("the shared row was continued: shared %d, partitions %v", len(p.backfillSeen), p.partBackfills)
	}
	if n := engine.countContaining(`stateId: "`+shared+`"`) - before; n != 0 {
		t.Errorf("the shared row was read or written %d times", n)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "queued" {
		t.Errorf("the shared row became %q", st.BackfillStatus)
	}
}

func TestAContinuingPassStopsWhenItsContextIsDone(t *testing.T) {
	engine := newStateEngine()
	p := newFakePartitioned("a", "b")
	r := stateRunner(engine, p)
	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.ContinueBackfills(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the context's", err)
	}
	if len(p.partBackfills) != 0 {
		t.Errorf("a cancelled pass still ran %v", p.partBackfills)
	}
}

func TestQueueAndContinueRefuseACallWithoutInternalOrigin(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector()
	i := &Integration{runner: stateRunner(engine, c), logger: discardLogger()}
	args := map[string]any{"connector": "shopify", "conceptId": testMirrorConcept}

	for name, handler := range map[string]func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error){
		"queueBackfill":     i.handleQueueBackfill,
		"continueBackfills": i.handleContinueBackfills,
	} {
		if _, err := handler(context.Background(), args, 0); err == nil || !strings.Contains(err.Error(), "refuses a client-origin call") {
			t.Errorf("%s answered a client call: %v", name, err)
		}
	}
	if n := len(engine.callsContaining("")); n != 0 {
		t.Fatalf("a refused call reached the engine %d times", n)
	}

	internal := auth.ContextWithInternalOrigin(context.Background())
	if _, err := i.handleQueueBackfill(internal, args, 0); err != nil {
		t.Fatalf("queueBackfill under internal origin: %v", err)
	}
	if _, err := i.handleContinueBackfills(internal, nil, 0); err != nil {
		t.Fatalf("continueBackfills under internal origin: %v", err)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "complete" {
		t.Errorf("the engine's own calls left the row %q, want complete", st.BackfillStatus)
	}
}
