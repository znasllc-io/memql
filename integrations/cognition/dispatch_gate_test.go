package cognition

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

func dispatchGateTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestDispatchGate_NilGateProceeds verifies a nil *dispatchGate is treated as
// fail-safe: the cardinal rule is that a missing gate must never DROP a reply.
func TestDispatchGate_NilGateProceeds(t *testing.T) {
	var g *dispatchGate
	proceed, release, err := g.tryDispatch(context.Background(), "v1:cognition:utterance:nil-gate")
	if err != nil || !proceed {
		t.Fatalf("nil gate must proceed (fail safe): proceed=%v err=%v", proceed, err)
	}
	release() // must be a safe no-op
}

// TestDispatchGate_NilGetterProceeds verifies a gate with no DB accessor fails
// safe (always proceeds), so single-binary / no-DB builds are unaffected.
func TestDispatchGate_NilGetterProceeds(t *testing.T) {
	g := newDispatchGate(nil, dispatchGateTestLogger())
	proceed, release, err := g.tryDispatch(context.Background(), "v1:cognition:utterance:nil-getter")
	if err != nil || !proceed {
		t.Fatalf("nil getter must proceed (fail safe): proceed=%v err=%v", proceed, err)
	}
	release()
}

// TestDispatchGate_NilDBProceeds verifies a getter that returns a nil *bun.DB
// (DB not Ready yet) fails safe.
func TestDispatchGate_NilDBProceeds(t *testing.T) {
	g := newDispatchGate(func() *bun.DB { return nil }, dispatchGateTestLogger())
	proceed, release, err := g.tryDispatch(context.Background(), "v1:cognition:utterance:nil-db")
	if err != nil || !proceed {
		t.Fatalf("nil db must proceed (fail safe): proceed=%v err=%v", proceed, err)
	}
	release()
}

// TestDispatchGate_ErroredConnProceeds is the fail-safe path for a broken DB
// handle: a *bun.DB whose underlying connection cannot be acquired must NOT
// drop the dispatch -- tryDispatch returns proceed=true with a non-nil err for
// logging. Modeled on the read-before-write check still running downstream.
func TestDispatchGate_ErroredConnProceeds(t *testing.T) {
	// A connector pointed at an unroutable DSN: Conn() / the lock query fails,
	// the gate must fail safe.
	connector := pgdriver.NewConnector(pgdriver.WithDSN("postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable"))
	db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
	defer db.Close()

	g := newDispatchGate(func() *bun.DB { return db }, dispatchGateTestLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	proceed, release, err := g.tryDispatch(ctx, "v1:cognition:utterance:bad-conn")
	if !proceed {
		t.Fatalf("a DB/lock error must fail SAFE (proceed=true), got proceed=false")
	}
	if err == nil {
		t.Fatalf("expected a non-nil error to log on the fail-safe path")
	}
	release() // no-op on the error path
}

func TestDispatchGateLockKey_StableAndDistinct(t *testing.T) {
	if dispatchGateLockKey("v1:cognition:utterance:a") != dispatchGateLockKey("v1:cognition:utterance:a") {
		t.Fatal("lock key must be stable for the same utterance id")
	}
	if dispatchGateLockKey("v1:cognition:utterance:a") == dispatchGateLockKey("v1:cognition:utterance:b") {
		t.Fatal("different utterance ids should (almost always) hash to different keys")
	}
}

// openDispatchGateTestDB connects to Postgres for the contention integration
// test, skipping when none is reachable so it never blocks CI. DSN comes from
// MEMORY_NODES_DATABASE_DSN, else the dev-compose default. Mirrors
// integrations/planner/admission_test.go's harness.
func openDispatchGateTestDB(t *testing.T) *bun.DB {
	t.Helper()
	dsn := os.Getenv("MEMORY_NODES_DATABASE_DSN")
	if dsn == "" {
		dsn = "postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable"
	}
	connector := pgdriver.NewConnector(pgdriver.WithDSN(dsn))
	db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("no Postgres reachable for the dispatch-gate integration test: %v", err)
	}
	return db
}

// TestDispatchGate_SingleReplicaWins is the no-op-on-single-replica acceptance
// check: the lone gate (one cognition replica) always wins pg_try_advisory_lock
// and proceeds.
func TestDispatchGate_SingleReplicaWins(t *testing.T) {
	db := openDispatchGateTestDB(t)
	defer db.Close()

	g := newDispatchGate(func() *bun.DB { return db }, dispatchGateTestLogger())
	utteranceId := fmt.Sprintf("v1:cognition:utterance:solo-%d", time.Now().UnixNano())

	proceed, release, err := g.tryDispatch(context.Background(), utteranceId)
	if err != nil {
		t.Fatalf("single-replica tryDispatch errored: %v", err)
	}
	if !proceed {
		t.Fatalf("the only replica must win and proceed")
	}
	release()

	// After release, the same utterance can be re-acquired (lock is freed).
	proceed2, release2, err := g.tryDispatch(context.Background(), utteranceId)
	if err != nil || !proceed2 {
		t.Fatalf("after release the lock must be re-acquirable: proceed=%v err=%v", proceed2, err)
	}
	release2()
}

// TestDispatchGate_ExactlyOneWinsUnderContention is the core acceptance check
// for #1217: of TWO contending dispatchers for the SAME utterance.id (the two
// cognition replicas that both received the broadcast event), EXACTLY ONE
// proceeds and the other bails. The winner holds the lock across the simulated
// turn; the loser must see proceed=false (not block, not error). Each replica
// uses an INDEPENDENT *bun.DB (separate session, like separate pods) so the
// advisory lock is the only thing serializing them.
func TestDispatchGate_ExactlyOneWinsUnderContention(t *testing.T) {
	// Two independent connections == two replicas / two pods.
	dbA := openDispatchGateTestDB(t)
	defer dbA.Close()
	dbB := openDispatchGateTestDB(t)
	defer dbB.Close()

	gateA := newDispatchGate(func() *bun.DB { return dbA }, dispatchGateTestLogger())
	gateB := newDispatchGate(func() *bun.DB { return dbB }, dispatchGateTestLogger())

	utteranceId := fmt.Sprintf("v1:cognition:utterance:contend-%d", time.Now().UnixNano())

	var (
		wg        sync.WaitGroup
		proceeded int32
		bailed    int32
		errs      int32
		// The winner holds the lock until AFTER both replicas have raced
		// (released past wg.Wait), so the loser necessarily attempts while the
		// winner's simulated turn is still in flight -- the real #1217 race.
		releaseOnce   sync.Once
		winnerRelease func()
	)

	attempt := func(g *dispatchGate) {
		defer wg.Done()
		proceed, release, err := g.tryDispatch(context.Background(), utteranceId)
		if err != nil {
			atomic.AddInt32(&errs, 1)
			return
		}
		if proceed {
			atomic.AddInt32(&proceeded, 1)
			releaseOnce.Do(func() { winnerRelease = release })
		} else {
			atomic.AddInt32(&bailed, 1)
			release() // loser's release is a no-op
		}
	}

	wg.Add(2)
	go attempt(gateA)
	go attempt(gateB)
	wg.Wait()
	if winnerRelease != nil {
		winnerRelease()
	}

	if got := atomic.LoadInt32(&errs); got != 0 {
		t.Fatalf("no replica should error in the happy path, got %d errors", got)
	}
	if p, b := atomic.LoadInt32(&proceeded), atomic.LoadInt32(&bailed); p != 1 || b != 1 {
		t.Fatalf("exactly one replica must proceed and one bail: proceeded=%d bailed=%d", p, b)
	}
}
