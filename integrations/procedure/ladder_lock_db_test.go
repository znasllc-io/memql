package procedure

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/work"
)

// ladder_lock_db_test.go -- the ladder's per-construct advisory lock against
// a REAL Postgres (review finding C1, epic memql#5408).
//
// Everything else in this package runs over a fake engine, and a fake cannot
// show a lock working: it serializes because it serializes, which is the
// assumption the race was made of. So the rows here are still the fake's --
// what is under test is who may hold a construct's ladder at once -- and the
// lock is Postgres's own.

// ladderLockDB is a handle on the db-gated suites' database, or a skip (a
// failure under MEMQL_REQUIRE_DB) when there is none.
func ladderLockDB(t *testing.T) *bun.DB {
	t.Helper()
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		dbtest.Unreachable(t, "the certification ladder's advisory lock", dsn, err)
		return nil
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestTheLadderLockHoldsOneConstructAndNotAnother: while one move holds a
// construct's ladder, a second move of the SAME construct -- spelled bare or
// canonically -- waits for it, and a move of another construct does not.
func TestTheLadderLockHoldsOneConstructAndNotAnother(t *testing.T) {
	db := ladderLockDB(t)
	if db == nil {
		return
	}
	i := newTestIntegration(newFakeEngine())
	i.SetLadderLockDB(func() *bun.DB { return db })
	ctx := context.Background()

	release, err := i.lockLadder(ctx, "v1:authoring:construct:lock-a")
	if err != nil {
		t.Fatalf("lockLadder: %v", err)
	}
	acquired := make(chan time.Time, 1)
	go func() {
		second, err := i.lockLadder(ctx, "lock-a") // the same construct, bare
		if err != nil {
			t.Errorf("the second move of the construct: %v", err)
			acquired <- time.Time{}
			return
		}
		acquired <- time.Now()
		second()
	}()

	other, err := i.lockLadder(ctx, "v1:authoring:construct:lock-b")
	if err != nil {
		t.Fatalf("another construct's ladder waited on this one: %v", err)
	}
	other()

	select {
	case <-acquired:
		t.Fatal("a second move of the construct took its ladder while the first held it")
	case <-time.After(300 * time.Millisecond):
	}
	released := time.Now()
	release()
	select {
	case at := <-acquired:
		if at.IsZero() || at.Before(released) {
			t.Fatalf("the second move took the ladder at %v, before the first released it at %v", at, released)
		}
	case <-time.After(ladderLockTimeout):
		t.Fatal("the second move never took the ladder after the first released it")
	}
}

// TestTwoConcurrentFinishersOnOneConstructSerialize: two shadow comparisons of
// two recordings finish at the same moment, each with a ladder write slow
// enough that the other's read lands inside it. With the lock, the second
// reads what the first wrote, and both matches count. The CONTROL is the same
// race with no lock installed: one match is lost -- the two finishers read the
// same streak and wrote the same number -- which is what makes the locked
// result evidence rather than luck.
func TestTwoConcurrentFinishersOnOneConstructSerialize(t *testing.T) {
	db := ladderLockDB(t)
	if db == nil {
		return
	}
	race := func(t *testing.T, locked bool) any {
		t.Helper()
		w := newReplayWorld(t, "shadow")
		if locked {
			w.i.SetLadderLockDB(func() *bun.DB { return db })
		}
		// The window between a finisher's read and its write.
		w.eng.mu.Lock()
		apply := w.eng.hooks["recordConstructLadder"]
		w.eng.mu.Unlock()
		w.eng.onWrite("recordConstructLadder", func(c recordedCall) {
			time.Sleep(300 * time.Millisecond)
			apply(c)
		})
		// Both comparisons reach their last step before either goes on, so
		// their finishes start together.
		arrived := make(chan struct{}, 2)
		together := make(chan struct{})
		var once sync.Once
		w.i.SetDispatcher(work.TargetWorkbench, &hookDispatcher{inner: w.d, before: func(req DispatchRequest) {
			if req.StepKey != "step1" {
				return
			}
			arrived <- struct{}{}
			if len(arrived) == cap(arrived) {
				once.Do(func() { close(together) })
			}
			select {
			case <-together:
			case <-time.After(5 * time.Second):
			}
		}})

		var wg sync.WaitGroup
		for _, rec := range []struct{ run, file string }{{"v1:work:run:rec-c", "c.txt"}, {"v1:work:run:rec-d", "d.txt"}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out, err := w.i.Replay(context.Background(), ReplayRequest{
					OwnerUserId: replayOwner, ConstructId: w.constructId, Mode: ReplayShadow, GoalRunId: rec.run,
					Bindings: map[string]string{"s0.command.7": rec.file}, AppActions: appObservations(),
					AppArgs: []map[string]any{
						{"command": "mkdir -p out && echo hello > " + rec.file},
						{"file_path": relativeReportPath, "content": "hello\n"},
					},
				})
				if err != nil || !out.Match {
					t.Errorf("comparing %s = %+v, %v; want a match", rec.file, out, err)
				}
			}()
		}
		wg.Wait()
		return w.lc.get("shadowMatches")
	}

	if got := race(t, true); got != float64(2) {
		t.Fatalf("with the lock, shadowMatches = %v after two matching comparisons, want 2", got)
	}
	if got := race(t, false); got != float64(1) {
		t.Fatalf("the control (no lock) counted %v, want the one lost update the lock exists to prevent -- "+
			"without it, the locked result above proves nothing", got)
	}
}
