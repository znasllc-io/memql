package accounts

// export_failure_db_test.go -- each way a run stops, and what it leaves
// (design record docs/superpowers/specs/2026-11-12-account-export-design.md,
// sections E and F): a run that fails leaves no object, and one that goes on
// says what it could not carry.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/accountexport"
	"github.com/znasllc-io/memql/component/memql"
)

// requireFailure asserts the code of an ExportFailure.
func requireFailure(t *testing.T, err error, code string) *ExportFailure {
	t.Helper()
	var f *ExportFailure
	require.True(t, errors.As(err, &f), "want an ExportFailure %s, got %v", code, err)
	require.Equal(t, code, f.Code, f.Detail)
	return f
}

// requireNothingLeft asserts storage holds nothing of the run.
func (f *runFixture) requireNothingLeft(t *testing.T) {
	t.Helper()
	require.Empty(t, f.store.committed, "a failed run left a file")
	require.Equal(t, len(f.store.created), len(f.store.aborted)+len(f.store.deleted), "every attempt's file was removed")
}

func (f *runFixture) setWaiting(concept string, n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.states {
		if f.states[i].Concept == concept {
			f.states[i].Waiting = n
		}
	}
}

func TestExportRunWaitsForWaitingRowsAndAFullPassReleasesThem(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.setWaiting("v1:todos:todo", 2)
	// One row written since the run noted its time is still the job's to
	// assign, the first time the snapshot asks.
	asked := 0
	f.still = func() map[string]int64 {
		asked++
		if asked == 1 {
			return map[string]int64{"v1:todos:todo": 1}
		}
		return nil
	}
	_, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	require.Equal(t, [][]string{{"v1:todos:todo"}}, f.requested, "a full pass is asked for the kind with rows waiting, once")
	require.Equal(t, 2, asked, "the snapshot was released and taken again after a row was still waiting")
}

func TestExportRunFailsWhenRowsKeepWaiting(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.work.caughtUpTimeout = 200 * time.Millisecond
	f.work.requestFullPass = func(context.Context, []string) error { return nil }
	f.setWaiting("v1:todos:todo", 3)
	_, err := f.run(t, f.export(acme, false), "")
	fail := requireFailure(t, err, FailureRowsWaiting)
	require.Contains(t, fail.Detail, "v1:todos:todo 3")
	require.Empty(t, f.store.created, "nothing is written before the snapshot")
}

func TestExportRunCensusMismatchLeavesNoFile(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.work.census = func(ctx context.Context, q memql.AccountQuerier, account string, concepts []string) ([]memql.AccountCensusLine, error) {
		lines, err := memql.AccountCensus(ctx, q, account, concepts)
		for i := range lines {
			if lines[i].Concept == "v1:todos:todo" {
				lines[i].Versions++
			}
		}
		return lines, err
	}
	const pass = "ABCDE-FGHJK-LMNPQ-RSTUV-WXYZ2"
	_, err := f.run(t, f.export(acme, true), pass)
	fail := requireFailure(t, err, FailureCensusMismatch)
	require.Contains(t, fail.Detail, "v1:todos:todo counted 1 rows and 2 versions, 1 and 1 written")
	require.NotContains(t, err.Error(), pass)
	f.requireNothingLeft(t)
	require.Len(t, f.store.aborted, 1)
}

func TestExportRunRecordsAMissingObjectAndGoesOn(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	delete(f.store.blobs, "library/u-member/f1/b.txt")
	res, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	ms := members(t, f.store.committed[res.ObjectKey])
	var missing []map[string]any
	for _, l := range jsonLines(t, memberData(t, ms, "bytes.jsonl")) {
		if l["missing"] == true {
			missing = append(missing, l)
		}
	}
	require.Len(t, missing, 1)
	require.Equal(t, "library/u-member/f1/b.txt", missing[0]["key"])
	vr, err := accountexport.Verify(bytes.NewReader(f.store.committed[res.ObjectKey]))
	require.NoError(t, err)
	require.Equal(t, int64(1), vr.Report.Bytes.Missing)
	require.Equal(t, int64(2), vr.Report.Bytes.Objects)
}

func TestExportRunFailsOnAnObjectOfTheWrongLength(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.store.reported["library/u-member/f1/b.txt"] = 5
	_, err := f.run(t, f.export(acme, false), "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "the reader is long")
	f.requireNothingLeft(t)
}

func TestExportRunChangedByteFailsVerificationAndDeletesTheFile(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.store.corrupt = true
	_, err := f.run(t, f.export(acme, false), "")
	requireFailure(t, err, FailureVerifyFailed)
	require.Empty(t, f.store.committed)
	require.Len(t, f.store.deleted, 1)
	require.True(t, strings.HasPrefix(f.store.deleted[0], "exports/accounts/exp-acme/1/"), f.store.deleted[0])
}

func TestExportRunChangedByteInAProtectedFileFailsVerification(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.store.corrupt = true
	const pass = "ABCDE-FGHJK-LMNPQ-RSTUV-WXYZ2"
	_, err := f.run(t, f.export(acme, true), pass)
	requireFailure(t, err, FailureVerifyFailed)
	require.NotContains(t, err.Error(), pass)
	f.requireNothingLeft(t)
}

func TestExportRunEmptyAccountProducesAFileThatChecks(t *testing.T) {
	f := newRunFixture(t)
	f.schema(t)
	f.put(t, accountConcept, canonicalPrefix+"empty", 1, "system", map[string]any{"name": "Empty"}, nil)
	res, err := f.run(t, f.export("empty", false), "")
	require.NoError(t, err)
	vr, err := accountexport.Verify(bytes.NewReader(f.store.committed[res.ObjectKey]))
	require.NoError(t, err)
	for _, k := range vr.Report.Kinds {
		if k.Concept == accountConcept {
			require.Equal(t, int64(1), k.WrittenRows)
			continue
		}
		require.Zero(t, k.WrittenVersions, k.Concept)
	}
}

func TestExportRunRefusesAnAccountThatIsNotReady(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.x.followPending = func(context.Context, memql.AccountQuerier, string) (int64, error) { return 1, nil }
	_, err := f.run(t, f.export(acme, false), "")
	fail := requireFailure(t, err, FailureNotReady)
	require.Contains(t, fail.Detail, "Rows following a moved parent")
	require.Empty(t, f.store.created)
}

func TestExportRunCancelledLeavesNothingStaged(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancelled once the file is open and the walk has begun.
	f.work.inSnapshot = nil
	calls := 0
	f.work.handOvers = func() []string {
		calls++
		cancel()
		return accountexport.HandOvers()
	}
	_, err := f.work.run(ctx, f.export(acme, false), "", nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls)
	f.requireNothingLeft(t)
	require.Len(t, f.store.aborted, 1)
}

func TestExportRunRowsPhaseIsBounded(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.work.rowsTimeout = 300 * time.Millisecond
	f.work.inSnapshot = func(ctx context.Context, q memql.AccountQuerier) error {
		rows, err := q.QueryContext(ctx, `SELECT pg_sleep(2)`)
		if err == nil {
			rows.Close()
		}
		return err
	}
	_, err := f.run(t, f.export(acme, false), "")
	requireFailure(t, err, FailureRowsTooSlow)
	f.requireNothingLeft(t)
}

// ---------------------------------------------------------------------------
// A pass still owed, a panic, a file that cannot be read back, a cancel

func (f *runFixture) setPending(concept string, pending bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.states {
		if f.states[i].Concept == concept {
			f.states[i].FullPassPending = pending
		}
	}
}

func TestExportRunWaitsForAPendingFullPass(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.work.caughtUpTimeout = 300 * time.Millisecond
	f.setPending("v1:todos:todo", true)
	_, err := f.run(t, f.export(acme, false), "")
	fail := requireFailure(t, err, FailureRowsWaiting)
	require.Contains(t, fail.Detail, "v1:todos:todo full pass pending")
	require.Empty(t, f.store.created)

	f.work.caughtUpTimeout = 5 * time.Second
	go func() {
		time.Sleep(200 * time.Millisecond)
		f.setPending("v1:todos:todo", false)
	}()
	_, err = f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
}

func TestExportReadinessIsNotMetWhileAFullPassIsPending(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.setPending("v1:todos:todo", true)
	r, err := f.x.Readiness(context.Background(), acme)
	require.NoError(t, err)
	require.False(t, r.Ready())
	require.Contains(t, r.unmet(), conditionAssigned)
	f.setPending("v1:todos:todo", false)
	r, err = f.x.Readiness(context.Background(), acme)
	require.NoError(t, err)
	require.True(t, r.Ready(), r.unmet())
}

func TestExportRunPanicIsAnErrorAndLeavesNoFile(t *testing.T) {
	t.Run("before the commit", func(t *testing.T) {
		f := newRunFixture(t)
		f.seedAcme(t)
		f.work.handOvers = func() []string { panic("a hand-over list blew up") }
		_, err := f.run(t, f.export(acme, false), "")
		require.Error(t, err)
		require.Contains(t, err.Error(), "string: a hand-over list blew up")
		require.NotContains(t, err.Error(), "goroutine")
		f.requireNothingLeft(t)
	})
	t.Run("after the commit", func(t *testing.T) {
		f := newRunFixture(t)
		f.seedAcme(t)
		f.store.panicOpen = true
		_, err := f.run(t, f.export(acme, false), "")
		require.Error(t, err)
		require.Contains(t, err.Error(), "storage blew up")
		f.requireNothingLeft(t)
		require.Len(t, f.store.deleted, 1)
	})
}

func TestExportRunCountsTheAccountRowTwice(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.work.accountCensus = func(context.Context, memql.AccountQuerier, string) (int64, int64, error) { return 1, 3, nil }
	_, err := f.run(t, f.export(acme, false), "")
	fail := requireFailure(t, err, FailureCensusMismatch)
	require.Contains(t, fail.Detail, accountConcept+" counted 1 rows and 3 versions, 1 and 2 written")
	f.requireNothingLeft(t)
}

func TestExportRunFileThatCannotBeReadBackFailsVerification(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.store.failOpen = true
	_, err := f.run(t, f.export(acme, false), "")
	requireFailure(t, err, FailureVerifyFailed)
	f.requireNothingLeft(t)
	require.Len(t, f.store.deleted, 1)
}

func TestExportRunCancelInTheMiddleOfAKindStopsPromptly(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	for i := 0; i < 50; i++ {
		f.put(t, "v1:todos:todo", fmt.Sprintf("v1:todos:todo:bulk%03d", i), 20, "system", acmeIn(map[string]any{"title": "Bulk"}), nil)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	todoRows := 0
	f.work.onRow = func(concept string) {
		if concept == "v1:todos:todo" {
			todoRows++
			if todoRows == 3 {
				cancel()
			}
		}
	}
	_, err := f.work.run(ctx, f.export(acme, false), "", nil)
	require.ErrorIs(t, err, context.Canceled)
	require.LessOrEqual(t, todoRows, 4, "the walk went on after the cancel")
	f.requireNothingLeft(t)
}

// ---------------------------------------------------------------------------
// A cancel after the rows

func TestExportRunCancelledAfterTheRowsCommitsNothing(t *testing.T) {
	for _, phase := range []string{phaseBytes, phaseSealing} {
		t.Run(phase, func(t *testing.T) {
			f := newRunFixture(t)
			f.seedAcme(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := f.work.run(ctx, f.export(acme, false), "", func(p ExportProgress) {
				if p.Phase == phase {
					cancel()
				}
			})
			require.ErrorIs(t, err, context.Canceled)
			f.requireNothingLeft(t)
			require.Len(t, f.store.aborted, 1)
		})
	}
}

// The run records its file's key on the row as soon as the commit lands,
// before it reads the file back: a node that stops in between leaves a key
// the take-over deletes.
func TestExportRunRecordsTheKeyRightAfterTheCommit(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	var recorded []string
	f.work.recordObject = func(_ context.Context, x Export, key string) (bool, error) {
		f.store.mu.Lock()
		_, committed := f.store.committed[key]
		f.store.mu.Unlock()
		require.True(t, committed, "the key is recorded once the file is committed")
		require.Equal(t, "exp-acme", x.ID)
		recorded = append(recorded, key)
		return true, nil
	}
	res, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	require.Equal(t, []string{res.ObjectKey}, recorded)
}

// An attempt that can no longer record its key no longer holds the run: it
// deletes the file it committed and stops.
func TestExportRunThatCannotRecordItsKeyDeletesTheFile(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.work.recordObject = func(context.Context, Export, string) (bool, error) { return false, nil }
	_, err := f.run(t, f.export(acme, false), "")
	require.Error(t, err)
	require.Empty(t, f.store.committed)
	require.Len(t, f.store.deleted, 1)
}

// A conflict recorded during the wait, by the very full pass the run asked
// for, is read again inside the snapshot.
func TestExportRunRereadsConflictsInsideTheSnapshot(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.setWaiting("v1:todos:todo", 2)
	pass := f.work.requestFullPass
	f.work.requestFullPass = func(ctx context.Context, concepts []string) error {
		_, err := f.db.ExecContext(ctx, `INSERT INTO account_assignment_conflict (concept, row_id, kind, detail, candidates)
			VALUES ('v1:todos:todo', 'v1:todos:todo:conflicted', 'store_bound_twice', 'test', ?)`, "{"+acme+",other}")
		require.NoError(t, err)
		return pass(ctx, concepts)
	}
	_, err := f.run(t, f.export(acme, false), "")
	fail := requireFailure(t, err, FailureNotReady)
	require.Contains(t, fail.Detail, "Rows awaiting a decision")
	require.Empty(t, f.store.created, "no file is started")
}

// A change record written during the wait is read again inside the snapshot.
func TestExportRunRereadsChangeRecordsInsideTheSnapshot(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.x.followPending = memql.AccountFollowPending
	f.setWaiting("v1:todos:todo", 2)
	pass := f.work.requestFullPass
	f.work.requestFullPass = func(ctx context.Context, concepts []string) error {
		_, err := f.db.ExecContext(ctx, `INSERT INTO account_follow_tasks (parent_concept, parent_id, from_account, to_account)
			VALUES ('v1:todos:todo', 'v1:todos:todo:moved', 'other', ?)`, acme)
		require.NoError(t, err)
		return pass(ctx, concepts)
	}
	_, err := f.run(t, f.export(acme, false), "")
	fail := requireFailure(t, err, FailureNotReady)
	require.Contains(t, fail.Detail, "Rows following a moved parent")
	require.Empty(t, f.store.created)
}
