package datasync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// partitions_db_test.go -- partitioned health rows through a real engine.
//
// The fake engine answers a read by its rendered prefix, so it would accept
// a by-id query the DSL does not declare, a field the concept refuses, or a
// page walk that never asks for the second page. These are the three claims
// only the engine can check: two tenants' rows land and read back whole and
// apart, a row written before partitions existed still reads back under its
// old id, and SyncStates reads every page. db-gated, like
// syncstate_write_db_test.go.

// Each test names a connector of its own run, so it is re-runnable against
// a database that keeps its rows, and sweeps them away afterwards.

func TestTwoStoresKeepTheirOwnCursorWatermarkAndError(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	store := NewStore(engineSeam{eng})
	ctx := OperatorContext(context.Background())
	connector := fmt.Sprintf("dbtest-partitions-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND payload->>'connector' = $1`, connector)
	})

	base := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	want := map[string]SyncState{}
	for i, part := range []string{"", "acme-widgets.myshopify.com", "second-store.myshopify.com"} {
		st := SyncState{
			ConceptID:       testMirrorConcept,
			Connector:       connector,
			Direction:       "inbound",
			Partition:       part,
			BackfillCursor:  fmt.Sprintf("cursor-%d", i),
			BackfillStatus:  "running",
			LastReconcileAt: base.Add(time.Duration(i) * time.Hour),
			LastAttemptAt:   base.Add(time.Duration(i)*time.Hour + time.Minute),
			DriftCount:      i + 1,
			LastError:       fmt.Sprintf("probe error %d", i),
		}
		if err := store.WriteSyncState(ctx, st); err != nil {
			t.Fatalf("WriteSyncState(partition %q): %v", part, err)
		}
		want[part] = st
	}

	for part, w := range want {
		got, err := store.SyncStateFor(ctx, testMirrorConcept, connector, "inbound", part)
		if err != nil {
			t.Fatalf("SyncStateFor(partition %q): %v", part, err)
		}
		if !got.Stored {
			t.Fatalf("partition %q: no row came back", part)
		}
		if !strings.HasSuffix(got.ID, ":"+SyncStateID(testMirrorConcept, connector, "inbound", part)) || got.Partition != part ||
			got.BackfillCursor != w.BackfillCursor || got.BackfillStatus != w.BackfillStatus ||
			!got.LastReconcileAt.Equal(w.LastReconcileAt) || !got.LastAttemptAt.Equal(w.LastAttemptAt) ||
			got.DriftCount != w.DriftCount || got.LastError != w.LastError {
			t.Fatalf("partition %q did not come back as written:\n  wrote %+v\n  read  %+v", part, w, got)
		}
	}
}

func TestAConnectorWithNoPartitionsKeepsItsRows(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	store := NewStore(engineSeam{eng})
	ctx := OperatorContext(context.Background())
	connector := fmt.Sprintf("dbtest-legacy-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND payload->>'connector' = $1`, connector)
	})

	// The write a runtime before partitions made: the three-part id, and no
	// partitionKey on the row at all.
	legacyID := SyncStateID(testMirrorConcept, connector, "inbound", "")
	if _, err := store.exec(ctx, call("mutation", "upsertSyncState",
		arg{"stateId", legacyID},
		arg{"conceptId", testMirrorConcept},
		arg{"connector", connector},
		arg{"direction", "inbound"},
		arg{"backfillCursor", "legacy-cursor"},
		arg{"backfillStatus", "complete"},
		arg{"lastReconcileAt", "2026-11-01T08:00:00Z"},
		arg{"lastError", "legacy error"},
	)); err != nil {
		t.Fatalf("writing a row the old way: %v", err)
	}

	got, err := store.SyncStateFor(ctx, testMirrorConcept, connector, "inbound", "")
	if err != nil {
		t.Fatalf("SyncStateFor: %v", err)
	}
	if !got.Stored || !strings.HasSuffix(got.ID, ":"+legacyID) {
		t.Fatalf("the row written before partitions did not read back under its id %q: %+v", legacyID, got)
	}
	if got.BackfillCursor != "legacy-cursor" || got.BackfillStatus != "complete" || got.LastError != "legacy error" ||
		!got.LastReconcileAt.Equal(time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("the old row lost its state: %+v", got)
	}
	if got.Partition != "" {
		t.Fatalf("the old row read back with partition %q", got.Partition)
	}
}

func TestSyncStatesReadsPastTheFirstPage(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	store := NewStore(engineSeam{eng})
	ctx := OperatorContext(context.Background())
	connector := fmt.Sprintf("dbtest-pages-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND payload->>'connector' = $1`, connector)
	})

	const n = 120 // past syncStatesAll's page of 100
	for i := 0; i < n; i++ {
		if err := store.WriteSyncState(ctx, SyncState{
			ConceptID: testMirrorConcept,
			Connector: connector,
			Direction: "inbound",
			Partition: fmt.Sprintf("store-%03d", i),
		}); err != nil {
			t.Fatalf("WriteSyncState %d: %v", i, err)
		}
	}

	states, err := store.SyncStates(ctx, connector)
	if err != nil {
		t.Fatalf("SyncStates: %v", err)
	}
	seen := map[string]bool{}
	for _, st := range states {
		if st.Connector != connector {
			t.Fatalf("SyncStates(%q) returned another connector's row: %+v", connector, st)
		}
		seen[st.Partition] = true
	}
	if len(seen) != n {
		t.Fatalf("SyncStates read %d distinct partitions (%d rows), want %d -- it stopped at the first page", len(seen), len(states), n)
	}
}

// A queued backfill is carried to complete by the continuing pass, through a
// real engine: the queue lands, syncStatesBackfilling lists the row while it
// is queued or running and only at its current version, and drops it once it
// is complete although every earlier version of it was running.
func TestAQueuedBackfillRunsToCompleteAgainstARealEngine(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	store := NewStore(engineSeam{eng})
	ctx := OperatorContext(context.Background())
	connector := fmt.Sprintf("dbtest-continue-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND payload->>'connector' = $1`, connector)
	})

	c := mirrorConnector(
		memqlsync.BackfillPage{NextCursor: "c-1"},
		memqlsync.BackfillPage{NextCursor: "c-2"},
		memqlsync.BackfillPage{Done: true},
	)
	c.name = connector
	r := NewRunner(store, NewApplier(store, newFakeWriter()), discardLogger())
	r.lookup = func(name string) (memqlsync.Connector, bool) { return c, name == connector }
	r.bound = func() []string { return []string{connector} }
	r.maxPages = 1

	backfilling := func() []map[string]any {
		t.Helper()
		rows, err := store.rows(ctx, call("query", "syncStatesBackfilling", arg{"connector", connector}))
		if err != nil {
			t.Fatalf("syncStatesBackfilling: %v", err)
		}
		return rows
	}

	if err := r.QueueBackfill(ctx, connector, testMirrorConcept, ""); err != nil {
		t.Fatalf("QueueBackfill: %v", err)
	}
	if rows := backfilling(); len(rows) != 1 || stringField(rows[0], "backfillStatus") != "queued" {
		t.Fatalf("syncStatesBackfilling after the queue = %v, want the one queued row", rows)
	}

	var total ContinueResult
	for i := 0; i < 3; i++ {
		res, err := r.ContinueBackfills(ctx)
		if err != nil {
			t.Fatalf("ContinueBackfills %d: %v", i+1, err)
		}
		total.Started += res.Started
		total.Advanced += res.Advanced
		total.Completed += res.Completed
		if i == 1 {
			rows := backfilling()
			if len(rows) != 1 || stringField(rows[0], "backfillStatus") != "running" || stringField(rows[0], "backfillCursor") != "c-2" {
				t.Fatalf("syncStatesBackfilling mid-way = %v, want one row, running at its current cursor", rows)
			}
		}
	}
	if total != (ContinueResult{Started: 1, Advanced: 1, Completed: 1}) {
		t.Errorf("three calls = %+v, want started, advanced, completed", total)
	}
	st, err := store.SyncStateFor(ctx, testMirrorConcept, connector, "inbound", "")
	if err != nil {
		t.Fatal(err)
	}
	if st.BackfillStatus != "complete" {
		t.Fatalf("status = %q after three calls, want complete", st.BackfillStatus)
	}
	if rows := backfilling(); len(rows) != 0 {
		t.Fatalf("syncStatesBackfilling still lists a complete row: %v -- a list returning an older version would continue it forever", rows)
	}
}

// Two writers of one health row, interleaved on a real engine. A partial
// write keeps every field it does not name (the read-merge), and a write
// decided on a read that another writer has since overtaken is refused by the
// row-version fence and decided again on a fresh read -- so the queue, which
// must never write over a running backfill, sees the backfill the pass
// started between its read and its write and leaves it alone.
func TestTwoWritersOfOneSyncStateRowKeepEachOthersFields(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	store := NewStore(engineSeam{eng})
	ctx := OperatorContext(context.Background())
	connector := fmt.Sprintf("dbtest-writers-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND payload->>'connector' = $1`, connector)
	})
	swept := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	if err := store.WriteSyncState(ctx, SyncState{
		ConceptID: testMirrorConcept, Connector: connector, Direction: "inbound",
		BackfillStatus: "complete", BackfillCursor: "c-9", LastReconcileAt: swept, DriftCount: 2, LastError: "sweep said so",
	}); err != nil {
		t.Fatal(err)
	}
	read := func() SyncState {
		t.Helper()
		st, err := store.SyncStateFor(ctx, testMirrorConcept, connector, "inbound", "")
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	// The partial write: only the inbound health is named, and nothing else moves.
	if err := store.PatchSyncState(ctx, testMirrorConcept, connector, "inbound", "", func(SyncState) (SyncPatch, bool) {
		return SyncPatch{"lagSeconds": 12}, true
	}); err != nil {
		t.Fatalf("a partial write: %v", err)
	}
	if st := read(); st.LagSeconds != 12 || st.BackfillStatus != "complete" || st.BackfillCursor != "c-9" ||
		!st.LastReconcileAt.Equal(swept) || st.DriftCount != 2 || st.LastError != "sweep said so" {
		t.Fatalf("a partial write did not keep the fields it does not name: %+v", st)
	}

	// The race: the queue reads the row complete, the pass starts the
	// backfill, then the queue writes.
	decided := 0
	err := store.PatchSyncState(ctx, testMirrorConcept, connector, "inbound", "", func(cur SyncState) (SyncPatch, bool) {
		decided++
		if decided == 1 {
			if err := store.PatchSyncState(ctx, testMirrorConcept, connector, "inbound", "", func(SyncState) (SyncPatch, bool) {
				return SyncPatch{"backfillStatus": "running", "backfillCursor": "c-1"}, true
			}); err != nil {
				t.Fatalf("the other writer: %v", err)
			}
		}
		if cur.BackfillStatus == "running" {
			return nil, false
		}
		return SyncPatch{"backfillStatus": "queued", "backfillCursor": ""}, true
	})
	if err != nil {
		t.Fatalf("the queue's write: %v", err)
	}
	if decided != 2 {
		t.Errorf("the queue decided %d times, want twice: its first write rested on a read another writer had overtaken", decided)
	}
	if st := read(); st.BackfillStatus != "running" || st.BackfillCursor != "c-1" || st.LagSeconds != 12 || !st.LastReconcileAt.Equal(swept) {
		t.Fatalf("after the race: %+v, want the pass's running backfill with every other writer's field kept", st)
	}
}

// ---- A failed delivery is tried again (U4), through a real engine ----

// retryFixture stages inbound deliveries on a source of this run's own and
// works them through a dispatcher over the real engine, its clock set by the
// test. The connector's Apply answers whatever applyErr holds at the time.
type retryFixture struct {
	t      *testing.T
	store  *Store
	d      *Dispatcher
	c      *fakeConnector
	ctx    context.Context
	clock  time.Time
	source string
}

func newRetryFixture(t *testing.T, eng engineSeam, source string) *retryFixture {
	t.Helper()
	f := &retryFixture{t: t, store: NewStore(eng), ctx: OperatorContext(context.Background()), source: source}
	f.c = &fakeConnector{name: source, domains: []memqlsync.DomainSpec{{Concept: testMirrorConcept, Direction: memqlsync.DirectionInbound}}}
	f.d = NewDispatcher(f.store, NewApplier(f.store, newFakeWriter()))
	f.d.lookup = func(_ context.Context, name string) (memqlsync.Connector, bool) { return f.c, name == source }
	f.d.now = func() time.Time { return f.clock }
	return f
}

// stage writes one delivery as the receiver stages it and returns its id.
func (f *retryFixture) stage(name string) string {
	f.t.Helper()
	id := f.source + "-" + name
	if _, err := f.store.exec(f.ctx, call("mutation", "stageInboundRequest",
		arg{"requestId", id}, arg{"source", f.source}, arg{"medium", "webhook"},
		arg{"body", `{"admin_graphql_api_id":"gid://shopify/Product/` + name + `"}`},
		arg{"headersJson", `{"x-shopify-topic":"products/update"}`},
		arg{"signatureVerified", true}, arg{"verifiedBy", "connector"},
		arg{"receivedAt", f.clock.Format(time.RFC3339)})); err != nil {
		f.t.Fatalf("staging %s: %v", name, err)
	}
	return id
}

// try dispatches the row as it is stored now, at the fixture's clock.
func (f *retryFixture) try(id string, at time.Time, applyErr error) {
	f.t.Helper()
	f.clock, f.c.applyErr = at, applyErr
	staged, found, err := f.d.Staged(f.ctx, id)
	if err != nil || !found {
		f.t.Fatalf("reading %s back: found=%t err=%v", id, found, err)
	}
	_, _ = f.d.DispatchStaged(f.ctx, staged)
}

// row reads one delivery's stored fields.
func (f *retryFixture) row(id string) map[string]any {
	f.t.Helper()
	rows, err := f.store.rows(f.ctx, call("query", "inboundRequestById", arg{"requestId", id}))
	if err != nil || len(rows) != 1 {
		f.t.Fatalf("inboundRequestById(%s): %d rows, %v", id, len(rows), err)
	}
	return rows[0]
}

// shortIDs is the short ids of rows, in order.
func shortIDs(rows []StagedRequest) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.RequestId[strings.LastIndex(r.RequestId, ":")+1:])
	}
	return out
}

func cleanInbound(t *testing.T, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, sources ...string) {
	t.Cleanup(func() {
		for _, s := range sources {
			_, _ = db.ExecContext(context.Background(),
				`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:inboundRequest' AND payload->>'source' = $1`, s)
		}
	})
}

func TestAFailedDeliveryIsDueOnlyWhenItsTimeHasCome(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	source := fmt.Sprintf("dbtest-retry-%d", time.Now().UnixNano())
	cleanInbound(t, db, source)
	f := newRetryFixture(t, engineSeam{eng}, source)
	at := time.Now().UTC().Truncate(time.Second)
	f.clock = at.Add(-time.Hour)
	transient, permanent := errors.New("origin timed out"), memqlsync.Permanentf("pinned to another version")

	due := f.stage("due")
	f.try(due, at.Add(-2*time.Minute), transient) // next try at -1m: due
	notYet := f.stage("not-yet")
	f.try(notYet, at, transient) // next try at +1m: not yet
	noTime := f.stage("no-time")
	f.try(noTime, at.Add(-10*time.Minute), transient) // next try at -9m ...
	f.try(noTime, at.Add(-9*time.Minute), permanent)  // ... and then a refusal no retry changes
	fourTries := f.stage("four-tries")
	for i, wait := range []time.Duration{40, 39, 34, 4} {
		f.try(fourTries, at.Add(-wait*time.Minute), transient)
		if got := intField(f.row(fourTries), "attempts"); got != i+1 {
			t.Fatalf("after try %d the row counts %d tries", i+1, got)
		}
	}
	if r := f.row(fourTries); stringField(r, "status") != "failed" || stringField(r, "nextAttemptAt") != "" {
		t.Fatalf("after its fourth try the row is %v, want failed with no next time", r)
	}
	if r := f.row(noTime); intField(r, "attempts") != 2 || stringField(r, "nextAttemptAt") != "" {
		t.Fatalf("a refusal after a retry left %v, want two tries and no next time", r)
	}

	rows, err := f.store.InboundRequestsDueForRetry(f.ctx, at)
	if err != nil {
		t.Fatalf("InboundRequestsDueForRetry: %v", err)
	}
	var mine []StagedRequest
	for _, r := range rows {
		if r.Source == source {
			mine = append(mine, r)
		}
	}
	if got := shortIDs(mine); len(got) != 1 || got[0] != source+"-due" {
		t.Fatalf("due as of now: %v, want only the row whose time has come -- not the one with no time, "+
			"not the one at four tries and not the one whose time is still ahead", got)
	}

	// And the pass works it: claimed behind the fence, dispatched, processed.
	f.clock, f.c.applyErr = at, nil
	if _, err := f.d.RetryDue(f.ctx); err != nil {
		t.Fatalf("RetryDue: %v", err)
	}
	if r := f.row(due); stringField(r, "status") != "processed" || intField(r, "attempts") != 2 {
		t.Fatalf("the retried row is %v, want processed at its second try", r)
	}
	if r := f.row(notYet); stringField(r, "status") != "failed" || intField(r, "attempts") != 1 {
		t.Fatalf("a row whose time had not come was touched: %v", r)
	}
}

func TestRequeueReturnsTheGivenUpRowsOfOneSourceOnly(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	nanos := time.Now().UnixNano()
	mine, other := fmt.Sprintf("dbtest-requeue-%d", nanos), fmt.Sprintf("dbtest-requeue-other-%d", nanos)
	cleanInbound(t, db, mine, other)
	at := time.Now().UTC().Truncate(time.Second)
	transient := errors.New("origin timed out")

	giveUp := func(f *retryFixture, name string) string {
		id := f.stage(name)
		for i := 4; i > 0; i-- {
			f.try(id, at.Add(-time.Duration(i)*time.Hour), transient)
		}
		return id
	}
	f := newRetryFixture(t, engineSeam{eng}, mine)
	f.clock = at.Add(-5 * time.Hour)
	givenUp := giveUp(f, "given-up")
	waiting := f.stage("waiting")
	f.try(waiting, at.Add(-time.Minute), transient)

	g := newRetryFixture(t, engineSeam{eng}, other)
	g.clock = at.Add(-5 * time.Hour)
	othersGivenUp := giveUp(g, "given-up")

	f.clock = at
	n, err := f.d.Requeue(f.ctx, mine)
	if err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	if n != 1 {
		t.Fatalf("Requeue put %d rows back in line, want the one given-up row of the source", n)
	}
	if r := f.row(givenUp); stringField(r, "status") != "failed" || intField(r, "attempts") != 0 ||
		!timeField(r, "nextAttemptAt").Equal(at) {
		t.Fatalf("the given-up row is %v, want failed with no tries counted and due now", r)
	}
	if r := f.row(waiting); intField(r, "attempts") != 1 || !timeField(r, "nextAttemptAt").Equal(at) {
		t.Fatalf("a row merely waiting for its next try was touched: %v", r)
	}
	if r := g.row(othersGivenUp); intField(r, "attempts") != 4 || stringField(r, "nextAttemptAt") != "" {
		t.Fatalf("another source's given-up row was touched: %v", r)
	}
}
