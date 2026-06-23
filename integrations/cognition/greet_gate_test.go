package cognition

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// TestGreetGate_NilGateProceeds verifies a nil *dispatchGate fails safe for the
// greet path: a missing gate must never SUPPRESS a legitimate greeting.
func TestGreetGate_NilGateProceeds(t *testing.T) {
	var g *dispatchGate
	proceed, release, err := g.tryGreet(context.Background(), "v1:cognition:space:nil-gate", "v1:agents:agent:a")
	if err != nil || !proceed {
		t.Fatalf("nil gate must proceed (fail safe): proceed=%v err=%v", proceed, err)
	}
	release() // must be a safe no-op
}

// TestGreetGate_NilGetterProceeds verifies a gate with no DB accessor fails safe
// (always proceeds), so single-binary / no-DB builds keep greeting.
func TestGreetGate_NilGetterProceeds(t *testing.T) {
	g := newDispatchGate(nil, dispatchGateTestLogger())
	proceed, release, err := g.tryGreet(context.Background(), "v1:cognition:space:nil-getter", "v1:agents:agent:a")
	if err != nil || !proceed {
		t.Fatalf("nil getter must proceed (fail safe): proceed=%v err=%v", proceed, err)
	}
	release()
}

// TestGreetGate_NilDBProceeds verifies a getter that returns a nil *bun.DB (DB
// not Ready yet) fails safe.
func TestGreetGate_NilDBProceeds(t *testing.T) {
	g := newDispatchGate(func() *bun.DB { return nil }, dispatchGateTestLogger())
	proceed, release, err := g.tryGreet(context.Background(), "v1:cognition:space:nil-db", "v1:agents:agent:a")
	if err != nil || !proceed {
		t.Fatalf("nil db must proceed (fail safe): proceed=%v err=%v", proceed, err)
	}
	release()
}

// TestGreetGate_ErroredConnProceeds is the fail-safe path for a broken DB
// handle: a connection that cannot be acquired must NOT suppress the greeting --
// tryGreet returns proceed=true with a non-nil err for logging.
func TestGreetGate_ErroredConnProceeds(t *testing.T) {
	connector := pgdriver.NewConnector(pgdriver.WithDSN("postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable"))
	db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
	defer db.Close()

	g := newDispatchGate(func() *bun.DB { return db }, dispatchGateTestLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	proceed, release, err := g.tryGreet(ctx, "v1:cognition:space:bad-conn", "v1:agents:agent:a")
	if !proceed {
		t.Fatalf("a DB/lock error must fail SAFE (proceed=true), got proceed=false")
	}
	if err == nil {
		t.Fatalf("expected a non-nil error to log on the fail-safe path")
	}
	release() // no-op on the error path
}

// TestGreetGate_ErrorNeverReturnsBail is the no-suppress guard: under ANY
// infrastructure failure the greet gate must NEVER return proceed=false.
// proceed=false means "another replica owns this greeting, bail" -- returning it
// on an error (rather than a real lock loss) could silently SUPPRESS a greeting.
func TestGreetGate_ErrorNeverReturnsBail(t *testing.T) {
	connector := pgdriver.NewConnector(pgdriver.WithDSN("postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable"))
	db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
	defer db.Close()

	g := newDispatchGate(func() *bun.DB { return db }, dispatchGateTestLogger())
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		proceed, release, err := g.tryGreet(ctx, fmt.Sprintf("v1:cognition:space:err-%d", i), "v1:agents:agent:a")
		cancel()
		if !proceed {
			t.Fatalf("attempt %d: a gate-infra error returned proceed=false -- this can SUPPRESS a greeting (must fail SAFE)", i)
		}
		if err == nil {
			t.Fatalf("attempt %d: expected a non-nil error on the fail-safe path", i)
		}
		release()
	}
}

// TestGreetGateLockKey_StableAndDistinct asserts the (space, agent) lock key is
// deterministic across processes and discriminates on both components.
func TestGreetGateLockKey_StableAndDistinct(t *testing.T) {
	if greetGateLockKey("space-a", "agent-1") != greetGateLockKey("space-a", "agent-1") {
		t.Fatal("lock key must be stable for the same (space, agent)")
	}
	if greetGateLockKey("space-a", "agent-1") == greetGateLockKey("space-a", "agent-2") {
		t.Fatal("different agents in the same space should hash to different keys")
	}
	if greetGateLockKey("space-a", "agent-1") == greetGateLockKey("space-b", "agent-1") {
		t.Fatal("the same agent in different spaces should hash to different keys")
	}
	// The delimiter must prevent concatenation aliasing: ("ab","c") != ("a","bc").
	if greetGateLockKey("ab", "c") == greetGateLockKey("a", "bc") {
		t.Fatal("delimiter must prevent concatenation aliasing between (space, agent) pairs")
	}
}

// TestGreetGate_NoCollisionWithDispatchClass guards the namespacing invariant:
// the greet gate uses a DISTINCT advisory-lock class from the utterance dispatch
// gate, so a greeting and an utterance can never contend for the same lock even
// if their hashed object keys happen to collide.
func TestGreetGate_NoCollisionWithDispatchClass(t *testing.T) {
	if greetGateLockClass == dispatchGateLockClass {
		t.Fatalf("greet gate and dispatch gate MUST use distinct lock classes (both = %#x)", greetGateLockClass)
	}
}

// TestGreetGate_SingleReplicaWins is the no-op-on-single-replica acceptance
// check: the lone replica always wins and proceeds, and after release the same
// (space, agent) is re-claimable.
func TestGreetGate_SingleReplicaWins(t *testing.T) {
	db := openDispatchGateTestDB(t)
	defer db.Close()

	g := newDispatchGate(func() *bun.DB { return db }, dispatchGateTestLogger())
	partitionId := fmt.Sprintf("v1:cognition:space:greet-solo-%d", time.Now().UnixNano())
	agentId := "v1:agents:agent:solo"

	proceed, release, err := g.tryGreet(context.Background(), partitionId, agentId)
	if err != nil {
		t.Fatalf("single-replica tryGreet errored: %v", err)
	}
	if !proceed {
		t.Fatalf("the only replica must win and proceed")
	}
	release()

	proceed2, release2, err := g.tryGreet(context.Background(), partitionId, agentId)
	if err != nil || !proceed2 {
		t.Fatalf("after release the lock must be re-acquirable: proceed=%v err=%v", proceed2, err)
	}
	release2()
}

// TestGreetGate_ExactlyOneReplicaGreets is the core acceptance check for #1386:
// of TWO contending replicas firing the greeting for the SAME (partitionId, agentId)
// -- the two cognition replicas that both received the broadcast
// participant.created event -- EXACTLY ONE proceeds and the other bails. Each
// replica uses an INDEPENDENT *bun.DB (separate session, like separate pods) so
// the advisory lock is the only thing serializing them. This is the multi-node
// scenario: it FAILS against the pre-fix code (no gate -> both greet) and PASSES
// with the gate.
func TestGreetGate_ExactlyOneReplicaGreets(t *testing.T) {
	dbA := openDispatchGateTestDB(t)
	defer dbA.Close()
	dbB := openDispatchGateTestDB(t)
	defer dbB.Close()

	gateA := newDispatchGate(func() *bun.DB { return dbA }, dispatchGateTestLogger())
	gateB := newDispatchGate(func() *bun.DB { return dbB }, dispatchGateTestLogger())

	partitionId := fmt.Sprintf("v1:cognition:space:greet-contend-%d", time.Now().UnixNano())
	agentId := "v1:agents:agent:greeter"

	var (
		wg            sync.WaitGroup
		proceeded     int32
		bailed        int32
		errs          int32
		releaseOnce   sync.Once
		winnerRelease func()
	)

	attempt := func(g *dispatchGate) {
		defer wg.Done()
		// Each replica re-checks greetingExists then takes the gate; we model
		// only the gate contention here (greetingExists passes on both, exactly
		// as in production before either inserts).
		proceed, release, err := g.tryGreet(context.Background(), partitionId, agentId)
		if err != nil {
			atomic.AddInt32(&errs, 1)
			return
		}
		if proceed {
			atomic.AddInt32(&proceeded, 1)
			// Winner holds the lock across the simulated initial-delay + LLM turn,
			// so the loser necessarily contends while the greeting is in flight.
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
		t.Fatalf("exactly one replica must greet and one bail: greeted=%d bailed=%d", p, b)
	}
}

// TestGreetGate_LoserStrictlyEnforcedWhileWinnerHolds proves the greet gate is a
// genuine exactly-once boundary: while the winner holds the lock across its
// (simulated) greeting turn, a second replica MUST get proceed=false with a nil
// error -- a clean "another replica owns this greeting" answer, not a fail-open.
// After the winner releases, the (space, agent) is re-claimable (no wedge).
func TestGreetGate_LoserStrictlyEnforcedWhileWinnerHolds(t *testing.T) {
	dbWinner := openDispatchGateTestDB(t)
	defer dbWinner.Close()
	dbLoser := openDispatchGateTestDB(t)
	defer dbLoser.Close()

	winner := newDispatchGate(func() *bun.DB { return dbWinner }, dispatchGateTestLogger())
	loser := newDispatchGate(func() *bun.DB { return dbLoser }, dispatchGateTestLogger())

	partitionId := fmt.Sprintf("v1:cognition:space:greet-strict-%d", time.Now().UnixNano())
	agentId := "v1:agents:agent:greeter"

	proceed, release, err := winner.tryGreet(context.Background(), partitionId, agentId)
	if err != nil || !proceed {
		t.Fatalf("winner must acquire the greet lock: proceed=%v err=%v", proceed, err)
	}

	lProceed, lRelease, lErr := loser.tryGreet(context.Background(), partitionId, agentId)
	if lErr != nil {
		t.Fatalf("loser must not error on a clean contention: %v", lErr)
	}
	if lProceed {
		t.Fatalf("loser MUST be strictly enforced (proceed=false) while the winner holds the greet lock")
	}
	lRelease()

	release()

	reProceed, reRelease, reErr := loser.tryGreet(context.Background(), partitionId, agentId)
	if reErr != nil || !reProceed {
		t.Fatalf("after the winner releases, the greet lock must be re-acquirable (no wedge): proceed=%v err=%v", reProceed, reErr)
	}
	reRelease()
}
