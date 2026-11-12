package datasync

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// mirror_fence_test.go -- the applier's guard judges the row that is stored,
// and writes only while that row is still the one it judged.

// THE GUARD READS THE STORED ROW PAST THE RESULT CACHE. A node's result cache
// can hold a version another node has since replaced: its eviction is
// asynchronous. Judged against a cached row, a write older than what is
// stored is admitted and puts the mirror back.
func TestTheGuardReadsTheStoredRowPastTheResultCache(t *testing.T) {
	engine := newFakeEngine()
	a := NewApplier(NewStore(engine), NewEngineMirrorWriter(engine))
	if _, err := a.Apply(context.Background(), "shopify", testSpecs("updatedAt"), []memqlsync.MirrorWrite{
		{Concept: testMirrorConcept, RowId: "p1", Version: "2026-08-23T12:00:00Z", Payload: map[string]any{"updatedAt": "2026-08-23T12:00:00Z"}},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	reads := engine.freshContaining("concept==" + testMirrorConcept)
	if len(reads) != 1 || !reads[0] {
		t.Fatalf("the guard read the stored row with fresh=%v, want one read past the result cache", reads)
	}
}

// overtakenWriter is a mirror whose row another writer changes between the
// guard's read and the write: each StoredRow answers the next of `rows`, and
// the first `lose` writes are refused as the engine refuses a fenced write the
// row has moved under.
type overtakenWriter struct {
	rows    []StoredMirrorRow
	lose    int
	reads   int
	fenced  []bool
	written []memqlsync.MirrorWrite
}

func (w *overtakenWriter) StoredRow(context.Context, memqlsync.DomainSpec, string) (StoredMirrorRow, error) {
	row := w.rows[min(w.reads, len(w.rows)-1)]
	w.reads++
	return row, nil
}

func (w *overtakenWriter) WriteMirror(ctx context.Context, _ string, mw memqlsync.MirrorWrite) error {
	w.fenced = append(w.fenced, memql.HasRowVersionFence(ctx, mw.Concept, mw.RowId))
	if w.lose > 0 {
		w.lose--
		return fmt.Errorf("execute: %w", memql.ErrRowVersionChanged)
	}
	w.written = append(w.written, mw)
	return nil
}

func storedAt(updatedAt string, version time.Time) StoredMirrorRow {
	return StoredMirrorRow{Exists: true, Payload: map[string]any{"updatedAt": updatedAt}, Version: version}
}

// A WRITE IS FENCED ON THE ROW IT WAS JUDGED AGAINST. The guard's read and the
// write are two statements, and a delivery, a backfill page and a sweep write
// the same row at once. A write the row moved under is refused by the engine;
// the row is then read again and judged again: an older write is skipped as
// stale, a newer one is written against the row as it is now. A burst of
// other writers is outlasted, up to maxMirrorWriteTries tries in all, each
// after a wait; a write that loses every one of them fails, to be tried again
// with its batch.
func TestAWriteTheRowMovedUnderIsJudgedAgain(t *testing.T) {
	judged := time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC)
	moved := judged.Add(time.Minute)
	write := memqlsync.MirrorWrite{Concept: testMirrorConcept, RowId: "p1", Version: "2026-08-23T12:00:00Z", Payload: map[string]any{"updatedAt": "2026-08-23T12:00:00Z"}}
	for _, tc := range []struct {
		name          string
		rows          []StoredMirrorRow
		lose          int
		applied       int
		stale         int
		writes, reads int
		wantErr       bool
	}{
		{name: "nobody else wrote", rows: []StoredMirrorRow{storedAt("2026-08-23T11:00:00Z", judged)}, applied: 1, writes: 1, reads: 1},
		{name: "a row that does not exist yet", rows: []StoredMirrorRow{{}}, applied: 1, writes: 1, reads: 1},
		{name: "overtaken by a newer state: skipped as stale", lose: 1,
			rows:  []StoredMirrorRow{storedAt("2026-08-23T11:00:00Z", judged), storedAt("2026-08-23T13:00:00Z", moved)},
			stale: 1, writes: 1, reads: 2},
		{name: "overtaken by an older state: written against the row as it is now", lose: 1,
			rows:    []StoredMirrorRow{storedAt("2026-08-23T10:00:00Z", judged), storedAt("2026-08-23T11:30:00Z", moved)},
			applied: 1, writes: 2, reads: 2},
		{name: "created by another writer meanwhile, newer: skipped", lose: 1,
			rows:  []StoredMirrorRow{{}, storedAt("2026-08-23T13:00:00Z", moved)},
			stale: 1, writes: 1, reads: 2},
		{name: "overtaken by three writers in a row: written on the fourth try", lose: 3,
			rows:    []StoredMirrorRow{storedAt("2026-08-23T10:00:00Z", judged), storedAt("2026-08-23T11:30:00Z", moved)},
			applied: 1, writes: 4, reads: 4},
		{name: "overtaken on every try: the write fails, to be tried again", lose: maxMirrorWriteTries,
			rows:   []StoredMirrorRow{storedAt("2026-08-23T10:00:00Z", judged), storedAt("2026-08-23T11:30:00Z", moved)},
			writes: maxMirrorWriteTries, reads: maxMirrorWriteTries, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &overtakenWriter{rows: tc.rows, lose: tc.lose}
			a := NewApplier(NewStore(newFakeEngine()), w)
			var waits []int
			a.retryWait = func(attempt int) { waits = append(waits, attempt) }
			res, err := a.Apply(context.Background(), "shopify", testSpecs("updatedAt"), []memqlsync.MirrorWrite{write})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want an error: %t", err, tc.wantErr)
			}
			if tc.wantErr && (!errors.Is(err, memql.ErrRowVersionChanged) || !worthAnotherTry(err)) {
				t.Fatalf("err = %v, want the lost fence, as a failure worth another try", err)
			}
			if res.Applied != tc.applied || res.Stale != tc.stale {
				t.Fatalf("applied %d, stale %d; want %d and %d", res.Applied, res.Stale, tc.applied, tc.stale)
			}
			if len(w.fenced) != tc.writes || w.reads != tc.reads {
				t.Fatalf("%d writes after %d reads; want %d and %d", len(w.fenced), w.reads, tc.writes, tc.reads)
			}
			// One wait before each judgement after the first, numbered from
			// one: none before the first, and none after the last loss.
			wantWaits := []int{}
			for n := 1; n <= min(tc.lose, maxMirrorWriteTries-1); n++ {
				wantWaits = append(wantWaits, n)
			}
			if fmt.Sprint(waits) != fmt.Sprint(wantWaits) {
				t.Fatalf("waited before tries %v, want %v", waits, wantWaits)
			}
			for i, fenced := range w.fenced {
				if !fenced {
					t.Errorf("write %d carried no row-version fence", i+1)
				}
			}
		})
	}
}

// A BACKFILL PAGE THAT MEETS A BURST OF OTHER WRITERS IS NOT FAILED. A batch
// that returns an error ends its backfill as failed, and nothing starts a
// failed backfill again (Runner.backfillOne); so a row three deliveries write
// while the page is being applied is outlasted inside the batch, and the
// backfill completes.
func TestABackfillPageThatMeetsABurstOfOtherWritersIsNotFailed(t *testing.T) {
	engine := newStateEngine()
	judged := time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)
	writer := &overtakenWriter{lose: 3, rows: []StoredMirrorRow{
		storedAt("2026-08-23T09:00:00Z", judged), storedAt("2026-08-23T09:30:00Z", judged.Add(time.Minute)),
	}}
	c := mirrorConnector(memqlsync.BackfillPage{
		Writes: []memqlsync.MirrorWrite{{Concept: testMirrorConcept, RowId: "p1", Version: "2026-08-23T10:00:00Z", Payload: map[string]any{"updatedAt": "2026-08-23T10:00:00Z"}}},
		Done:   true,
	})
	r := stateRunner(engine, c)
	r.applier = NewApplier(r.store, writer)
	r.applier.retryWait = func(int) {}

	res, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept, "")
	if err != nil {
		t.Fatalf("StartBackfill: %v", err)
	}
	if !res.Done || res.Applied != 1 || len(writer.written) != 1 {
		t.Fatalf("done=%t applied=%d written=%d; want the page applied and the backfill done", res.Done, res.Applied, len(writer.written))
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "complete" || st.LastError != "" {
		t.Fatalf("the backfill is %q with error %q, want complete and none", st.BackfillStatus, st.LastError)
	}
}

// A write whose concept the connector declared no domain for was never
// judged: there is no row to fence it on, and it is written as it always was.
func TestAWriteThatWasNeverJudgedIsNotFenced(t *testing.T) {
	w := &overtakenWriter{rows: []StoredMirrorRow{{}}}
	a := NewApplier(NewStore(newFakeEngine()), w)
	res, err := a.Apply(context.Background(), "shopify", map[string]memqlsync.DomainSpec{}, []memqlsync.MirrorWrite{
		{Concept: "v1:other:thing", RowId: "x1"},
	})
	if err != nil || res.Applied != 1 {
		t.Fatalf("applied %d, %v; want the write applied", res.Applied, err)
	}
	if len(w.fenced) != 1 || w.fenced[0] {
		t.Fatalf("fenced = %v, want one unfenced write", w.fenced)
	}
}

// AN EXISTING ROW WHOSE VERSION THE READ DID NOT CARRY IS NOT FENCED. A zero
// version on the fence means "this row must not exist"; applied to a row that
// does, every write would lose for good. Such a row is written as before the
// fence existed.
func TestAnExistingRowReadWithoutItsVersionIsWrittenUnfenced(t *testing.T) {
	w := &overtakenWriter{rows: []StoredMirrorRow{{Exists: true, Payload: map[string]any{"updatedAt": "2026-08-23T11:00:00Z"}}}}
	a := NewApplier(NewStore(newFakeEngine()), w)
	res, err := a.Apply(context.Background(), "shopify", testSpecs("updatedAt"), []memqlsync.MirrorWrite{
		{Concept: testMirrorConcept, RowId: "p1", Version: "2026-08-23T12:00:00Z"},
	})
	if err != nil || res.Applied != 1 {
		t.Fatalf("applied %d, %v; want the write applied", res.Applied, err)
	}
	if len(w.fenced) != 1 || w.fenced[0] {
		t.Fatalf("fenced = %v, want one unfenced write", w.fenced)
	}
}

// The version the guard hands the fence is the stored row's own: a raw read
// answers it as created_at, in seconds and nanoseconds.
func TestTheStoredRowsVersionIsReadOffTheRawRead(t *testing.T) {
	engine := newFakeEngine()
	engine.seed("concept=="+testMirrorConcept, []map[string]any{{
		"id":         testMirrorConcept + ":p1",
		"created_at": map[string]any{"seconds": float64(1794448513), "nanos": float64(49752000)},
		"payload":    map[string]any{"updatedAt": "2026-08-23T11:00:00Z"},
	}})
	row, err := NewEngineMirrorWriter(engine).StoredRow(context.Background(), testSpecs("updatedAt")[testMirrorConcept], "p1")
	if err != nil || !row.Exists {
		t.Fatalf("StoredRow: %+v, %v", row, err)
	}
	if want := time.Unix(1794448513, 49752000).UTC(); !row.Version.Equal(want) {
		t.Fatalf("version = %s, want %s", row.Version, want)
	}
	if row.Payload["updatedAt"] != "2026-08-23T11:00:00Z" {
		t.Fatalf("payload = %v", row.Payload)
	}
}
