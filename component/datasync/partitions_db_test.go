package datasync

import (
	"context"
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
