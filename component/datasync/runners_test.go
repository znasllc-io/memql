package datasync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	engine := newStateEngine()
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
	r := stateRunner(engine, c)
	r.applier = NewApplier(r.store, writer)

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
	engine := newStateEngine()
	c := mirrorConnector(
		memqlsync.BackfillPage{NextCursor: "cursor-1"},
		memqlsync.BackfillPage{NextCursor: "cursor-2"},
		memqlsync.BackfillPage{Done: true},
	)
	r := stateRunner(engine, c)

	if _, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatalf("StartBackfill: %v", err)
	}
	writes := engine.callsContaining("SyncState(")
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
	engine := newStateEngine()
	writeKept(t, engine, SyncState{BackfillCursor: "cursor-after-crash", BackfillStatus: "running"})
	c := mirrorConnector(memqlsync.BackfillPage{Done: true})
	r := stateRunner(engine, c)

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
	id := SyncStateID(testMirrorConcept, "shopify", "inbound", partition)
	return append(engine.callsContaining(`mutation upsertSyncState(stateId: "`+id+`"`),
		engine.callsContaining(`mutation patchSyncState(stateId: "`+id+`"`)...)
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
	// Each partition's row is named by its own id (upsertsFor), so its own
	// sweep is the write onto it.
	if len(aw) != 1 || !strings.Contains(aw[0], "driftCount: 1") {
		t.Errorf("partition a's row did not record its own sweep: %v", aw)
	}
	if len(bw) != 1 || !strings.Contains(bw[0], "driftCount: 0") {
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
	engine := newStateEngine()
	p := newFakePartitioned("a", "b")
	p.partReports["a"] = memqlsync.ReconcileReport{Checked: 4, Drifted: 2, Healed: 2}
	p.partReports["b"] = memqlsync.ReconcileReport{Checked: 6, Drifted: 1, Healed: 1}
	p.partPages["a"] = []memqlsync.BackfillPage{{NextCursor: "a-1"}, {Done: true}}
	p.partPages["b"] = []memqlsync.BackfillPage{{Done: true}}
	r := stateRunner(engine, p)

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
	engine := newStateEngine()
	p := newFakePartitioned("a", "b")
	p.partPages["a"] = []memqlsync.BackfillPage{{Done: true}}
	p.partPages["b"] = []memqlsync.BackfillPage{{NextCursor: "b-1"}, {NextCursor: "b-2"}}
	r := stateRunner(engine, p)
	r.maxPages = 2

	res, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Abandoned || res.Pages != 3 {
		t.Errorf("fan-out = %+v, want three pages run and nothing abandoned", res)
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
	r, _ := clockedRunner(engine, c)
	return r
}

// passClock is the clock of a runner under test. Nothing moves it but a
// pass's own pauses and a test, so a pass that waits on its origin spends its
// budget without the test sleeping.
type passClock struct {
	at     time.Time
	pauses []time.Duration
}

func (c *passClock) now() time.Time { return c.at }

func (c *passClock) pause(ctx context.Context, d time.Duration) error {
	c.pauses = append(c.pauses, d)
	c.at = c.at.Add(d)
	return ctx.Err()
}

// clockedRunner is stateRunner with its clock handed back.
func clockedRunner(engine *stateEngine, c memqlsync.Connector) (*Runner, *passClock) {
	store := NewStore(engine)
	r := NewRunner(store, NewApplier(store, newFakeWriter()), discardLogger())
	r.lookup = func(name string) (memqlsync.Connector, bool) {
		if name == c.Name() {
			return c, true
		}
		return nil, false
	}
	r.bound = func() []string { return []string{c.Name()} }
	clock := &passClock{at: testNow}
	r.now = clock.now
	r.pause = clock.pause
	return r, clock
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
	fivePages := func() *fakeConnector {
		return mirrorConnector(
			memqlsync.BackfillPage{NextCursor: "c-1"},
			memqlsync.BackfillPage{NextCursor: "c-2"},
			memqlsync.BackfillPage{NextCursor: "c-3"},
			memqlsync.BackfillPage{NextCursor: "c-4"},
			memqlsync.BackfillPage{Done: true},
		)
	}

	// Within one pass, while its budget lasts: a round for each page budget.
	engine := newStateEngine()
	c := fivePages()
	r := stateRunner(engine, c)
	r.maxPages = 2
	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}
	if res := continueOnce(t, r); res != (ContinueResult{Started: 1, Completed: 1}) {
		t.Errorf("one pass = %+v, want the backfill started and carried to complete over its rounds", res)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "complete" || st.BackfillCursor != "" {
		t.Errorf("row = %s at %q, want complete", st.BackfillStatus, st.BackfillCursor)
	}
	if got := strings.Join(c.backfillSeen, ","); got != ",c-1,c-2,c-3,c-4" {
		t.Errorf("cursors asked for = %q, want each page once from where the last round stopped", got)
	}

	// And by the next pass, when a round uses the budget up: every page here
	// takes three minutes, so each pass has time for one round of two.
	engine = newStateEngine()
	c = fivePages()
	r, clock := clockedRunner(engine, c)
	r.maxPages = 2
	c.duringBackfill = func(string) { clock.at = clock.at.Add(3 * time.Minute) }
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
			t.Fatalf("pass %d: result %+v, row %s at %q; want %+v, %s at %q", i+1, res, st.BackfillStatus, st.BackfillCursor, w.res, w.status, w.cursor)
		}
	}
	if got := strings.Join(c.backfillSeen, ","); got != ",c-1,c-2,c-3,c-4" {
		t.Errorf("cursors asked for = %q, want each page once from where the last pass stopped", got)
	}
	if res := continueOnce(t, r); res != (ContinueResult{}) || len(c.backfillSeen) != 5 {
		t.Errorf("a complete backfill was continued again: %+v, %d asks", res, len(c.backfillSeen))
	}
}

func TestOneStoreRunsOneBackfillAtATime(t *testing.T) {
	engine := newStateEngine()
	o := newRoundsOrigin("a")
	o.preparing["a/product"] = 2
	// What the first domain's row said each time a step was asked of the
	// second.
	var firstWas []string
	o.answer = func(_, domain, _ string) (memqlsync.BackfillPage, error, bool) {
		if domain == "customer" {
			firstWas = append(firstWas, keptState(t, engine, testMirrorConcept, "a").BackfillStatus)
		}
		return memqlsync.BackfillPage{}, nil, false
	}
	r := stateRunner(engine, o)
	queueDomains(t, r, "a", testMirrorConcept, secondMirrorConcept)

	if res := continueOnce(t, r); res != (ContinueResult{Started: 2, Completed: 2}) {
		t.Errorf("the pass = %+v, want both domains started and completed, one after the other", res)
	}
	if len(firstWas) == 0 {
		t.Fatal("the second domain was never asked")
	}
	for _, status := range firstWas {
		if status != "complete" {
			t.Errorf("the second domain was asked while the first was %q: a store runs one backfill at a time", status)
		}
	}
	want := strings.Repeat("a/product,", 4) + "a/customer,a/customer"
	if got := strings.Join(o.asked, ","); got != want {
		t.Errorf("backfill steps = %s, want %s: the second domain may not start while the first runs", got, want)
	}
}

func TestAPollThatMadeNoProgressWaitsAndIsAskedAgainInTheNextRound(t *testing.T) {
	// Three answers of an operation still running: no writes, and the cursor
	// it was given back.
	stillRunning := func() *fakeConnector {
		return mirrorConnector(
			memqlsync.BackfillPage{NextCursor: "op-1"},
			memqlsync.BackfillPage{NextCursor: "op-1"},
			memqlsync.BackfillPage{NextCursor: "op-1"},
		)
	}

	// One invocation stops at the poll: asking again at once would spend its
	// page budget waiting.
	engine := newStateEngine()
	c := stillRunning()
	r := stateRunner(engine, c)
	writeKept(t, engine, SyncState{BackfillStatus: "running", BackfillCursor: "op-1"})
	res, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.backfillSeen) != 1 || res.Done || res.Abandoned || res.Waiting {
		t.Fatalf("one invocation asked %d times and answered %+v, want one poll and a backfill still running", len(c.backfillSeen), res)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "running" || st.BackfillCursor != "op-1" {
		t.Errorf("row = %s at %q, want still running at op-1", st.BackfillStatus, st.BackfillCursor)
	}

	// The continuing pass asks again in its next round, after a pause, until
	// the origin has the export ready.
	engine = newStateEngine()
	c = stillRunning()
	r, clock := clockedRunner(engine, c)
	writeKept(t, engine, SyncState{BackfillStatus: "running", BackfillCursor: "op-1"})
	writes := writesCount(engine)
	if res := continueOnce(t, r); res != (ContinueResult{Completed: 1}) {
		t.Errorf("result = %+v, want the backfill completed once its export was ready: a poll is not progress", res)
	}
	if len(c.backfillSeen) != 4 {
		t.Errorf("the connector was asked %d times, want 4: three polls and the read", len(c.backfillSeen))
	}
	if len(clock.pauses) != 3 {
		t.Errorf("the pass paused %d times, want once after each poll that made no progress", len(clock.pauses))
	}
	for _, p := range clock.pauses {
		if p != continueIdlePause {
			t.Errorf("a pause of %s, want %s", p, continueIdlePause)
		}
	}
	// A poll writes nothing: the row is written when the backfill completes,
	// its cursor and then its status.
	if got := rowWritesSince(engine, writes); len(got) != 2 {
		t.Errorf("three polls and a read wrote the row %d times, want only the read's two writes: %v", len(got), got)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "complete" {
		t.Errorf("status = %q, want complete", st.BackfillStatus)
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
			if st.BackfillStatus != "none" || st.LastError != refusal.Error() || !st.LastAttemptAt.IsZero() {
				t.Fatalf("row = %s, lastError %q, attempt %v; want none with the reason, and the sweep's attempt stamp untouched", st.BackfillStatus, st.LastError, st.LastAttemptAt)
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

// ---- a busy origin, and a refusal the sweep contradicts ----

func TestABusyOriginLeavesAQueuedBackfillQueuedUntilItAccepts(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector(memqlsync.BackfillPage{Done: true})
	c.backfillErr = memqlsync.Busy("shopify", "a bulk query is already running for this shop")
	r := stateRunner(engine, c)
	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}

	if res := continueOnce(t, r); res != (ContinueResult{}) {
		t.Errorf("a busy answer counted %+v, want nothing", res)
	}
	st := keptState(t, engine, testMirrorConcept, "")
	if st.BackfillStatus != "queued" || st.LastError != "" {
		t.Fatalf("after a busy answer: %s, lastError %q; want still queued with no error", st.BackfillStatus, st.LastError)
	}

	c.backfillErr = nil
	if res := continueOnce(t, r); res != (ContinueResult{Started: 1, Completed: 1}) {
		t.Errorf("once the origin accepts: %+v, want started and completed", res)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "complete" {
		t.Errorf("status = %q, want complete", st.BackfillStatus)
	}
}

func TestARunningBackfillThatIsToldBusyKeepsItsCursor(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector()
	c.backfillErr = memqlsync.Busy("shopify", "a bulk query is already running for this shop")
	r := stateRunner(engine, c)
	writeKept(t, engine, SyncState{BackfillStatus: "running", BackfillCursor: "c-4"})

	if res := continueOnce(t, r); res != (ContinueResult{}) {
		t.Errorf("a busy answer counted %+v, want nothing", res)
	}
	st := keptState(t, engine, testMirrorConcept, "")
	if st.BackfillStatus != "running" || st.BackfillCursor != "c-4" || st.LastError != "" {
		t.Errorf("row = %s at %q, lastError %q; want running at c-4 with no error", st.BackfillStatus, st.BackfillCursor, st.LastError)
	}
}

func TestAnOperatorStartOnABusyStoreEndsQueuedNotFailed(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector()
	c.backfillErr = memqlsync.Busy("shopify", "a bulk query is already running for this shop")
	r := stateRunner(engine, c)
	i := &Integration{runner: r, logger: discardLogger()}

	nodes, err := i.handleStartBackfill(clusterOwnerCtx(), map[string]any{"connector": "shopify", "conceptId": testMirrorConcept}, 0)
	if err != nil {
		t.Fatalf("a busy store failed the operator's start: %v", err)
	}
	if len(nodes) != 1 || !strings.Contains(string(nodes[0].Payload), "waiting behind another export") {
		t.Errorf("the answer does not say the backfill is waiting: %v", nodes)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "queued" || st.LastError != "" {
		t.Errorf("row = %s, lastError %q; want queued, so the continuing pass starts it", st.BackfillStatus, st.LastError)
	}
}

// sweepAndContinue is one round of the two scheduled passes.
func sweepAndContinue(t *testing.T, r *Runner) {
	t.Helper()
	if _, err := r.ContinueBackfills(context.Background()); err != nil && !strings.Contains(err.Error(), "backfill refused") {
		t.Fatalf("ContinueBackfills: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func TestARefusedBackfillOfADomainTheSweepReadsIsQueuedOnceMoreThenFails(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector()
	c.backfillErr = memqlsync.NotRequested("shopify")
	c.reconcileReport = memqlsync.ReconcileReport{Checked: 4}
	r := stateRunner(engine, c)
	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}

	for round := 1; round <= 6; round++ {
		sweepAndContinue(t, r)
		if round == 2 {
			st := keptState(t, engine, testMirrorConcept, "")
			if !strings.HasPrefix(st.LastError, "backfill refused: ") || standingRefusal(st.LastError) {
				t.Errorf("the second refusal reads %q, want a failure no refusal check reads as a standing answer", st.LastError)
			}
		}
	}
	if n := len(c.backfillSeen); n != 2 {
		t.Errorf("the connector was asked %d times over six rounds, want 2: queued once more by the clean sweep, then left failed", n)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "failed" || !strings.HasPrefix(st.LastError, "backfill refused: ") {
		t.Errorf("final row = %s, %q; want failed, the refusal kept as its reason", st.BackfillStatus, st.LastError)
	}
}

func TestARefusedDomainThatIsThenGrantedIsQueuedByItsFirstCleanSweep(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector()
	refusal := memqlsync.NotGranted("shopify", "read_customers")
	c.backfillErr, c.reconcileErr = refusal, refusal
	r := stateRunner(engine, c)
	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 3; round++ {
		sweepAndContinue(t, r)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "none" || !standingRefusal(st.LastError) {
		t.Fatalf("while refused by both: %s, %q; want the standing answer", st.BackfillStatus, st.LastError)
	}
	if n := len(c.backfillSeen); n != 1 {
		t.Fatalf("a standing answer was asked again: %d calls", n)
	}

	// Granted.
	c.backfillErr, c.reconcileErr = nil, nil
	c.reconcileReport = memqlsync.ReconcileReport{Checked: 2}
	if _, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "queued" {
		t.Fatalf("the first clean sweep left %q, want queued", st.BackfillStatus)
	}
	if res := continueOnce(t, r); res != (ContinueResult{Started: 1, Completed: 1}) {
		t.Errorf("after the grant: %+v, want started and completed", res)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "complete" {
		t.Errorf("status = %q, want complete", st.BackfillStatus)
	}
}

// ---- each writer of a domain's row writes only the fields it owns ----

// rowWritesSince is the writes onto the shared row of testMirrorConcept made
// after the first `skip` of them.
func rowWritesSince(engine *stateEngine, skip int) []string {
	id := SyncStateID(testMirrorConcept, "shopify", "inbound", "")
	var out []string
	for _, w := range engine.callsContaining(`stateId: "` + id + `"`) {
		if strings.HasPrefix(w, "mutation ") {
			out = append(out, w)
		}
	}
	if skip > len(out) {
		return nil
	}
	return out[skip:]
}

func writesCount(engine *stateEngine) int { return len(rowWritesSince(engine, 0)) }

// assertWritesOmit fails when a write names a field its writer does not own.
func assertWritesOmit(t *testing.T, who string, writes []string, fields ...string) {
	t.Helper()
	if len(writes) == 0 {
		t.Fatalf("%s wrote nothing", who)
	}
	for _, w := range writes {
		for _, f := range fields {
			if strings.Contains(w, " "+f+": ") || strings.Contains(w, "("+f+": ") {
				t.Errorf("%s wrote %s, a field it does not own: %s", who, f, w)
			}
		}
	}
}

var (
	backfillFields = []string{"backfillCursor", "backfillStatus"}
	sweepFields    = []string{"lastReconcileAt", "lastAttemptAt", "driftCount"}
	inboundFields  = []string{"lastInboundAt", "lagSeconds"}
)

func TestASweepThatStartedBeforeABackfillPageDoesNotPutBackItsCursorOrStatus(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector(memqlsync.BackfillPage{NextCursor: "c-2"}, memqlsync.BackfillPage{Done: true})
	c.reconcileReport = memqlsync.ReconcileReport{Checked: 1}
	r := stateRunner(engine, c)
	writeKept(t, engine, SyncState{BackfillStatus: "running", BackfillCursor: "c-1"})
	// The backfill finishes while the sweep is out asking the origin.
	c.duringReconcile = func() {
		if _, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
			t.Errorf("StartBackfill: %v", err)
		}
	}
	before := writesCount(engine)

	if _, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}
	st := keptState(t, engine, testMirrorConcept, "")
	if st.BackfillStatus != "complete" || st.BackfillCursor != "" {
		t.Errorf("after the sweep: %s at %q, want the backfill's complete -- the sweep put back what it read before the page", st.BackfillStatus, st.BackfillCursor)
	}
	if st.LastReconcileAt.IsZero() {
		t.Error("the sweep's watermark was not written")
	}
	var sweep []string
	for _, w := range rowWritesSince(engine, before) {
		if strings.Contains(w, "lastReconcileAt") {
			sweep = append(sweep, w)
		}
	}
	assertWritesOmit(t, "the sweep", sweep, backfillFields...)
}

func TestABackfillPageWrittenDuringASweepKeepsTheSweepsErrorAndWatermark(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector(
		memqlsync.BackfillPage{NextCursor: "c-1"},
		memqlsync.BackfillPage{NextCursor: "c-2"},
		memqlsync.BackfillPage{Done: true},
	)
	r := stateRunner(engine, c)
	old := testNow.Add(-3 * time.Hour)
	writeKept(t, engine, SyncState{BackfillStatus: "queued", LastReconcileAt: old})
	c.duringBackfill = func(cursor string) {
		switch cursor {
		case "": // a clean sweep moves the watermark
			c.reconcileReport = memqlsync.ReconcileReport{Checked: 2}
			if _, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
				t.Errorf("clean sweep: %v", err)
			}
		case "c-1": // a sweep fails and records why
			c.reconcileErr = errors.New("shopify admin: HTTP 502")
			_, _ = r.Reconcile(context.Background(), "shopify", testMirrorConcept, "")
		}
	}
	before := writesCount(engine)

	if _, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}
	st := keptState(t, engine, testMirrorConcept, "")
	if st.BackfillStatus != "complete" {
		t.Fatalf("status = %q, want complete", st.BackfillStatus)
	}
	if want := testNow.Add(-reconcileOverlap); !st.LastReconcileAt.Equal(want) {
		t.Errorf("lastReconcileAt = %v, want the sweep's %v -- a page put back the watermark it read at the start", st.LastReconcileAt, want)
	}
	if !strings.Contains(st.LastError, "HTTP 502") {
		t.Errorf("lastError = %q, want the sweep's failure -- a page erased it", st.LastError)
	}
	var pages []string
	for _, w := range rowWritesSince(engine, before) {
		if strings.Contains(w, "backfillCursor") {
			pages = append(pages, w)
		}
	}
	assertWritesOmit(t, "a backfill page", pages, append(sweepFields, inboundFields...)...)
}

func TestAQueueWriteDoesNotDisturbTheSweepsFields(t *testing.T) {
	engine := newStateEngine()
	r := stateRunner(engine, mirrorConnector())
	at := testNow.Add(-time.Hour)
	writeKept(t, engine, SyncState{BackfillStatus: "complete", LastReconcileAt: at, LastAttemptAt: at, DriftCount: 4, LagSeconds: 9})
	before := writesCount(engine)

	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}
	assertWritesOmit(t, "the queue", rowWritesSince(engine, before), append(sweepFields, append(inboundFields, "paused")...)...)
	st := keptState(t, engine, testMirrorConcept, "")
	if st.BackfillStatus != "queued" || !st.LastReconcileAt.Equal(at) || st.DriftCount != 4 || st.LagSeconds != 9 {
		t.Errorf("row = %+v, want queued with the sweep's and inbound's fields untouched", st)
	}
}

func TestAnInboundHealthWriteDuringABackfillLeavesCursorAndStatusAlone(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector(memqlsync.BackfillPage{NextCursor: "c-1"}, memqlsync.BackfillPage{Done: true})
	r := stateRunner(engine, c)
	store := NewStore(engine)
	d := NewDispatcher(store, NewApplier(store, newFakeWriter())).WithLogger(discardLogger())
	d.now = func() time.Time { return testNow }
	writeKept(t, engine, SyncState{BackfillStatus: "queued"})
	var inbound []string
	c.duringBackfill = func(cursor string) {
		if cursor != "c-1" {
			return
		}
		n := writesCount(engine)
		d.recordInboundHealth(OperatorContext(context.Background()), c,
			[]memqlsync.MirrorWrite{{Concept: testMirrorConcept, Version: testNow.Add(-40 * time.Second).Format(time.RFC3339)}}, testNow)
		inbound = rowWritesSince(engine, n)
	}

	if _, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}
	assertWritesOmit(t, "the inbound health write", inbound, append(backfillFields, sweepFields...)...)
	st := keptState(t, engine, testMirrorConcept, "")
	if st.BackfillStatus != "complete" || st.LagSeconds != 40 || st.LastInboundAt.IsZero() {
		t.Errorf("row = %s, lag %d, lastInboundAt %v; want complete with the delivery's lag kept", st.BackfillStatus, st.LagSeconds, st.LastInboundAt)
	}
}

func TestAPausedRunningDomainDoesNotHoldBackAQueuedSibling(t *testing.T) {
	engine := newStateEngine()
	p := newFakePartitioned("a")
	p.domains = append(p.domains, memqlsync.DomainSpec{Concept: secondMirrorConcept, Direction: memqlsync.DirectionInbound})
	r := stateRunner(engine, p)
	writeKept(t, engine, SyncState{Partition: "a", BackfillStatus: "running", BackfillCursor: "op-1", Paused: true})
	writeKept(t, engine, SyncState{ConceptID: secondMirrorConcept, Partition: "a", BackfillStatus: "queued"})

	if res := continueOnce(t, r); res != (ContinueResult{Started: 1, Completed: 1}) {
		t.Errorf("result = %+v, want the queued sibling started and completed", res)
	}
	if got := strings.Join(p.partBackfills, ","); got != "a/"+secondMirrorConcept {
		t.Errorf("backfill steps = %s, want only the sibling: the paused domain neither runs nor holds the store", got)
	}
	if st := keptState(t, engine, testMirrorConcept, "a"); st.BackfillStatus != "running" || st.BackfillCursor != "op-1" {
		t.Errorf("the paused domain became %s at %q", st.BackfillStatus, st.BackfillCursor)
	}
}

func TestAQueuedDomainRefusedBeforeItRanIsNotCountedStarted(t *testing.T) {
	engine := newStateEngine()
	p := newFakePartitioned("a")
	p.partErrs["a"] = memqlsync.NotRequested("shopify")
	r := stateRunner(engine, p)
	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, "a"); err != nil {
		t.Fatal(err)
	}
	if res := continueOnce(t, r); res != (ContinueResult{}) {
		t.Errorf("result = %+v, want nothing counted for a domain refused before it ran", res)
	}
}

func TestQueueingClearsAStaleRefusalAndOnlyARefusal(t *testing.T) {
	for name, tc := range map[string]struct{ lastError, want string }{
		"not granted":      {memqlsync.NotGranted("shopify", "read_customers").Error(), ""},
		"not requested":    {memqlsync.NotRequested("shopify").Error(), ""},
		"a failure":        {"shopify admin: HTTP 502", "shopify admin: HTTP 502"},
		"backfill refused": {backfillRefusedPrefix + "connector: not requested", backfillRefusedPrefix + "connector: not requested"},
	} {
		t.Run(name, func(t *testing.T) {
			engine := newStateEngine()
			r := stateRunner(engine, mirrorConnector())
			writeKept(t, engine, SyncState{BackfillStatus: "none", LastError: tc.lastError})
			if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
				t.Fatal(err)
			}
			if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "queued" || st.LastError != tc.want {
				t.Errorf("row = %s, lastError %q; want queued with lastError %q", st.BackfillStatus, st.LastError, tc.want)
			}
		})
	}
}

func TestARestartQueueResetsARunningBackfillAndAnOrdinaryOneDoesNot(t *testing.T) {
	engine := newStateEngine()
	r := stateRunner(engine, mirrorConnector())
	i := &Integration{runner: r, logger: discardLogger()}
	internal := auth.ContextWithInternalOrigin(context.Background())
	args := map[string]any{"connector": "shopify", "conceptId": testMirrorConcept}
	writeKept(t, engine, SyncState{BackfillStatus: "running", BackfillCursor: "c-7"})

	if _, err := i.handleQueueBackfill(internal, args, 0); err != nil {
		t.Fatal(err)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "running" || st.BackfillCursor != "c-7" {
		t.Fatalf("an ordinary queue moved a running backfill to %s at %q", st.BackfillStatus, st.BackfillCursor)
	}
	args["restart"] = true
	if _, err := i.handleQueueBackfill(internal, args, 0); err != nil {
		t.Fatal(err)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "queued" || st.BackfillCursor != "" {
		t.Errorf("a restart left %s at %q, want queued from the start", st.BackfillStatus, st.BackfillCursor)
	}
}

// ---- an in-flight backfill writes only onto the row it is still running ----

func TestARestartThatLandsBetweenTwoPagesIsNotOverwritten(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector(memqlsync.BackfillPage{NextCursor: "c-1"}, memqlsync.BackfillPage{NextCursor: "c-2"}, memqlsync.BackfillPage{Done: true})
	r := stateRunner(engine, c)
	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}
	after := -1
	c.duringBackfill = func(cursor string) {
		if cursor == "c-1" {
			if err := r.RestartBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
				t.Errorf("RestartBackfill: %v", err)
			}
			after = writesCount(engine)
		}
	}

	res := continueOnce(t, r)
	if res != (ContinueResult{}) {
		t.Errorf("result = %+v, want nothing counted for an abandoned invocation", res)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "queued" || st.BackfillCursor != "" || st.LastError != "" {
		t.Errorf("row = %s at %q, lastError %q; want the restart's queued with an empty cursor", st.BackfillStatus, st.BackfillCursor, st.LastError)
	}
	if got := rowWritesSince(engine, after); len(got) != 0 {
		t.Errorf("the abandoned invocation wrote after the restart: %v", got)
	}
	if n := len(c.backfillSeen); n != 2 {
		t.Errorf("the connector was asked %d times, want 2: the invocation stops once its row is not its own", n)
	}
}

func TestAFailureReturnedAfterARestartDoesNotWriteFailed(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector()
	r := stateRunner(engine, c)
	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}
	c.duringBackfill = func(string) {
		// The store reconnects while a fetch made with the old token is
		// out; the fetch then fails.
		if err := r.RestartBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
			t.Errorf("RestartBackfill: %v", err)
		}
		c.backfillErr = errors.New("shopify admin: HTTP 401")
	}

	if _, err := r.ContinueBackfills(context.Background()); err != nil {
		t.Errorf("an abandoned invocation reported an error: %v", err)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "queued" || st.LastError != "" {
		t.Errorf("row = %s, lastError %q; want the restart's queued, not failed -- nothing retries a failed backfill", st.BackfillStatus, st.LastError)
	}
}

func TestTwoInvocationsOnOneQueuedDomainOnlyOneProceeds(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector(memqlsync.BackfillPage{NextCursor: "c-1"}, memqlsync.BackfillPage{Done: true})
	r := stateRunner(engine, c)
	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}
	var second BackfillResult
	once := false
	c.duringBackfill = func(string) {
		if once {
			return
		}
		once = true
		// An operator's start beside the pass, while the pass's first page is out.
		var err error
		if second, err = r.StartBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
			t.Errorf("the second invocation: %v", err)
		}
	}

	first, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Abandoned == second.Abandoned {
		t.Fatalf("first abandoned %t, second abandoned %t; want exactly one to proceed", first.Abandoned, second.Abandoned)
	}
	done := first
	if first.Abandoned {
		done = second
	}
	if !done.Done {
		t.Errorf("the invocation that proceeded did not complete: %+v", done)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "complete" || st.BackfillCursor != "" {
		t.Errorf("row = %s at %q, want complete", st.BackfillStatus, st.BackfillCursor)
	}

	i := &Integration{runner: r, logger: discardLogger()}
	writeKept(t, engine, SyncState{BackfillStatus: "running", BackfillCursor: "c-5"})
	c.duringBackfill = func(string) { writeKept(t, engine, SyncState{BackfillStatus: "running", BackfillCursor: "c-6"}) }
	nodes, err := i.handleStartBackfill(clusterOwnerCtx(), map[string]any{"connector": "shopify", "conceptId": testMirrorConcept}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || !strings.Contains(string(nodes[0].Payload), "abandoned") {
		t.Errorf("an operator's start that lost its row does not say so: %v", nodes)
	}
}

// ---- a write that loses the fence waits before it decides again ----

func TestAWriteThatLosesTheFenceWaitsThenSucceeds(t *testing.T) {
	engine := newStateEngine()
	r := stateRunner(engine, mirrorConnector())
	var waits []int
	r.store.retryWait = func(attempt int) { waits = append(waits, attempt) }
	engine.loseFences = 3

	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatalf("a write that lost the fence 3 times: %v", err)
	}
	if got := fmt.Sprint(waits); got != "[1 2 3]" {
		t.Errorf("waits = %s, want one before each fresh decision: [1 2 3]", got)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "queued" {
		t.Errorf("status = %q, want queued", st.BackfillStatus)
	}

	// Five losses: it gives up, and the runner says so.
	var logs strings.Builder
	r.logger = slog.New(slog.NewTextHandler(&logs, nil))
	waits = nil
	engine.loseFences = 5
	c := mirrorConnector()
	c.reconcileReport = memqlsync.ReconcileReport{Checked: 1}
	r.lookup = func(string) (memqlsync.Connector, bool) { return c, true }
	if _, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(waits); got != "[1 2 3 4]" {
		t.Errorf("waits = %s, want [1 2 3 4] before the four fresh decisions", got)
	}
	if !strings.Contains(logs.String(), "could not record a domain's health") {
		t.Errorf("a write given up after five losses was not logged:\n%s", logs.String())
	}
}

func TestTheFenceRetryWaitIsShortJitteredAndGrows(t *testing.T) {
	for attempt := 1; attempt < maxPatchAttempts; attempt++ {
		lo, hi := retryWaitBounds(attempt)
		if lo < time.Millisecond || hi > 80*time.Millisecond || lo >= hi {
			t.Errorf("attempt %d waits in [%v, %v), want a few milliseconds to a few tens", attempt, lo, hi)
		}
		if attempt > 1 {
			if plo, _ := retryWaitBounds(attempt - 1); lo <= plo {
				t.Errorf("attempt %d's wait does not grow: %v after %v", attempt, lo, plo)
			}
		}
		for i := 0; i < 50; i++ {
			if d := retryWaitFor(attempt); d < lo || d >= hi {
				t.Fatalf("attempt %d waited %v, outside [%v, %v)", attempt, d, lo, hi)
			}
		}
	}
}

// ---- the backfill clears only its own failure text ----

func TestAFailedThenCompletedBackfillEndsWithNoError(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector()
	c.backfillErr = errors.New("shopify admin: HTTP 502")
	r := stateRunner(engine, c)
	writeKept(t, engine, SyncState{BackfillStatus: "queued", LastReconcileAt: testNow.Add(-time.Hour)})
	if _, err := r.ContinueBackfills(context.Background()); err == nil {
		t.Fatal("the failure was not reported")
	}
	st := keptState(t, engine, testMirrorConcept, "")
	if st.BackfillStatus != "failed" || !strings.HasPrefix(st.LastError, "backfill failed: ") || standingRefusal(st.LastError) {
		t.Fatalf("after a failure: %s, %q; want failed with the backfill's own prefix", st.BackfillStatus, st.LastError)
	}

	// A clean sweep keeps the backfill's reason while the backfill is failed.
	c.reconcileReport = memqlsync.ReconcileReport{Checked: 1}
	if _, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); !strings.HasPrefix(st.LastError, "backfill failed: ") {
		t.Fatalf("a clean sweep erased the failed backfill's reason: %q", st.LastError)
	}

	c.backfillErr = nil
	if err := r.QueueBackfill(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}
	continueOnce(t, r)
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "complete" || st.LastError != "" {
		t.Errorf("after the backfill completed: %s, %q; want complete with its own failure cleared", st.BackfillStatus, st.LastError)
	}
}

func TestACompletingBackfillKeepsTheSweepsOwnError(t *testing.T) {
	engine := newStateEngine()
	r := stateRunner(engine, mirrorConnector())
	writeKept(t, engine, SyncState{BackfillStatus: "queued", LastError: "shopify admin: HTTP 502"})
	continueOnce(t, r)
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "complete" || st.LastError != "shopify admin: HTTP 502" {
		t.Errorf("row = %s, %q; want complete with the sweep's error kept", st.BackfillStatus, st.LastError)
	}
}

// ---- a failed backfill started again from the start (Task 13b) ----

func TestStartingFromTheStartClearsAFailedBackfillsCursor(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector(memqlsync.BackfillPage{NextCursor: "c-1"}, memqlsync.BackfillPage{Done: true})
	r := stateRunner(engine, c)
	i := &Integration{runner: r, logger: discardLogger()}
	writeKept(t, engine, SyncState{
		BackfillStatus: "failed", BackfillCursor: "c-9",
		LastError: "backfill failed: shopify: bulk download HTTP 403 (a signed URL expires after one week; restart the operation)",
	})

	args := map[string]any{"connector": "shopify", "conceptId": testMirrorConcept, "restart": true}
	if _, err := i.handleStartBackfill(clusterOwnerCtx(), args, 0); err != nil {
		t.Fatalf("startBackfill with restart: %v", err)
	}
	if len(c.backfillSeen) == 0 || c.backfillSeen[0] != "" {
		t.Fatalf("the connector was asked from %q, want an empty cursor: the backfill starts from nothing", c.backfillSeen)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "complete" || st.BackfillCursor != "" || st.LastError != "" {
		t.Errorf("row = %s at %q, lastError %q; want complete with its failure cleared", st.BackfillStatus, st.BackfillCursor, st.LastError)
	}
	queued := false
	for _, w := range rowWritesSince(engine, 0) {
		if strings.Contains(w, `"queued"`) {
			queued = true
		}
	}
	if !queued {
		t.Error("the row was never reset to queued: a restart writes through the queue's fenced write")
	}
}

func TestStartingFromTheStartIsRefusedWhileTheBackfillRuns(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector()
	r := stateRunner(engine, c)
	writeKept(t, engine, SyncState{BackfillStatus: "running", BackfillCursor: "c-7"})
	before := writesCount(engine)

	_, err := r.StartBackfillFromTheStart(context.Background(), "shopify", testMirrorConcept, "")
	if err == nil || err.Error() != "datasync: this backfill is running; it can be started again once it has stopped" {
		t.Fatalf("err = %v, want the running refusal", err)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "running" || st.BackfillCursor != "c-7" {
		t.Errorf("a refused restart moved the running backfill to %s at %q", st.BackfillStatus, st.BackfillCursor)
	}
	if got := rowWritesSince(engine, before); len(got) != 0 {
		t.Errorf("a refused restart wrote: %v", got)
	}
	if len(c.backfillSeen) != 0 {
		t.Error("a refused restart ran the backfill")
	}
}

// A partitioned connector asked to start again with no partition is refused:
// pressing one row must never re-read every store's history.
func TestStartingFromTheStartNamesOnePartition(t *testing.T) {
	engine := newStateEngine()
	p := newFakePartitioned("a", "b")
	r := stateRunner(engine, p)
	_, err := r.StartBackfillFromTheStart(context.Background(), "shopify", testMirrorConcept, "")
	if err == nil || err.Error() != `datasync: "shopify" keeps each store's backfill apart; name the store to start it again from the start` {
		t.Fatalf("err = %v, want a refusal naming one partition", err)
	}
	if n := engine.countContaining("mutation "); n != 0 {
		t.Errorf("a refused restart wrote %d rows", n)
	}

	writeKept(t, engine, SyncState{Partition: "a", BackfillStatus: "failed", BackfillCursor: "c-9"})
	writeKept(t, engine, SyncState{Partition: "b", BackfillStatus: "complete", BackfillCursor: "c-4"})
	if _, err := r.StartBackfillFromTheStart(context.Background(), "shopify", testMirrorConcept, "a"); err != nil {
		t.Fatal(err)
	}
	if got := p.partCursors["a"]; len(got) == 0 || got[0] != "" {
		t.Errorf("partition a was asked from %q, want from nothing", got)
	}
	if len(p.partCursors["b"]) != 0 {
		t.Error("starting partition a again ran partition b")
	}
	if st := keptState(t, engine, testMirrorConcept, "b"); st.BackfillStatus != "complete" || st.BackfillCursor != "c-4" {
		t.Errorf("partition b = %s at %q, want it left as it was", st.BackfillStatus, st.BackfillCursor)
	}
}

func TestAnOrdinaryStartStillContinuesFromTheCursor(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector(memqlsync.BackfillPage{Done: true})
	r := stateRunner(engine, c)
	i := &Integration{runner: r, logger: discardLogger()}
	writeKept(t, engine, SyncState{BackfillStatus: "failed", BackfillCursor: "c-9", LastError: "backfill failed: shopify admin: HTTP 502"})

	args := map[string]any{"connector": "shopify", "conceptId": testMirrorConcept}
	if _, err := i.handleStartBackfill(clusterOwnerCtx(), args, 0); err != nil {
		t.Fatal(err)
	}
	if len(c.backfillSeen) != 1 || c.backfillSeen[0] != "c-9" {
		t.Errorf("the connector was asked from %q, want the stored cursor c-9", c.backfillSeen)
	}
	args["restart"] = false
	writeKept(t, engine, SyncState{BackfillStatus: "failed", BackfillCursor: "c-9"})
	c.backfillPages = []memqlsync.BackfillPage{{Done: true}}
	if _, err := i.handleStartBackfill(clusterOwnerCtx(), args, 0); err != nil {
		t.Fatal(err)
	}
	if len(c.backfillSeen) != 2 || c.backfillSeen[1] != "c-9" {
		t.Errorf("restart false was asked from %q, want the stored cursor c-9", c.backfillSeen)
	}
}

// Starting again from the start clears the failure of the backfill it
// replaces, so a backfill running again is not drawn beside its old failure;
// an error that is not the backfill's own (a sweep's) is kept.
func TestStartingFromTheStartClearsTheReplacedBackfillsOwnError(t *testing.T) {
	cases := map[string]struct{ held, want string }{
		"its own failure": {"backfill failed: shopify: bulk download HTTP 403 (a signed URL expires after one week; restart the operation)", ""},
		"its own refusal": {"backfill refused: shopify: not granted", ""},
		"a sweep's error": {"shopify admin: HTTP 502", "shopify admin: HTTP 502"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			engine := newStateEngine()
			c := mirrorConnector(memqlsync.BackfillPage{Done: true})
			r := stateRunner(engine, c)
			writeKept(t, engine, SyncState{BackfillStatus: "failed", BackfillCursor: "c-9", LastError: tc.held})
			seen := "unread"
			c.duringBackfill = func(string) { seen = keptState(t, engine, testMirrorConcept, "").LastError }

			if _, err := r.StartBackfillFromTheStart(context.Background(), "shopify", testMirrorConcept, ""); err != nil {
				t.Fatal(err)
			}
			if seen != tc.want {
				t.Errorf("while the backfill ran again, lastError = %q, want %q", seen, tc.want)
			}
			if st := keptState(t, engine, testMirrorConcept, ""); st.LastError != tc.want {
				t.Errorf("after it, lastError = %q, want %q", st.LastError, tc.want)
			}
		})
	}
}
