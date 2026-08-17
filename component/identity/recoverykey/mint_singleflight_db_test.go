package recoverykey

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

// mint_singleflight_db_test.go -- memql#3965's headline acceptance criterion:
// two concurrent minters, exactly one row.
//
// POSTGRES-GATED BECAUSE THE THING UNDER TEST IS A POSTGRES ADVISORY LOCK.
// There is no in-memory stand-in for pg_advisory_xact_lock that would prove
// anything: a fake would serialise because the fake serialises, which is
// exactly the assumption that produced memql#3400 in the first place. CI's
// db-tests lane runs with MEMQL_REQUIRE_DB=1, so a skip there is a failure.
//
// The ENGINE half is faked, deliberately and in one specific way: the fake
// sleeps between the read and the write, widening the race window that a
// real engine would leave open for microseconds into something a scheduler
// cannot miss. Without the lock the two minters would both read "no key
// exists" inside that window and both write. TestTwoMintersWithoutTheLockRace
// below asserts exactly that, so the passing case is known to be the lock
// working rather than the race merely failing to occur.

// fakeEngine is a minimal EngineExecutor over an in-memory row set.
//
// It answers the two constructs the invariant uses and nothing else, so a
// query this package starts issuing without updating the fake fails loudly
// instead of silently returning zero rows -- which would look exactly like
// "no key exists" and make the test pass for the wrong reason.
type fakeEngine struct {
	mu   sync.Mutex
	rows []map[string]string

	// readWriteGap is slept between a read and the write that follows it,
	// inside the caller's critical section. It is what turns a theoretical
	// interleaving into a reliable one.
	readWriteGap time.Duration

	reads   int
	writes  int
	unknown []string
}

var (
	fakeActiveRe = regexp.MustCompile(`^query activeRecoveryKeys\(userId:"([^"]*)"\)$`)
	fakeCreateRe = regexp.MustCompile(`^mutation createRecoveryKeyIdentity\(identityId:"([^"]*)",userId:"([^"]*)",`)
)

func (f *fakeEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	if m := fakeActiveRe.FindStringSubmatch(q); m != nil {
		owner := m[1]
		f.mu.Lock()
		var nodes []*memqlv1.MemoryNode
		for _, r := range f.rows {
			if r["userId"] == owner && r["active"] == "true" {
				nodes = append(nodes, nodeFromFake(r))
			}
		}
		f.reads++
		gap := f.readWriteGap
		f.mu.Unlock()

		// The gap is taken on the READ so it lands between the invariant's
		// decision and its write -- the window the advisory lock exists to
		// close.
		if gap > 0 {
			time.Sleep(gap)
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}}, nil
	}

	if m := fakeCreateRe.FindStringSubmatch(q); m != nil {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.rows = append(f.rows, map[string]string{
			"id":     m[1],
			"userId": m[2],
			"active": "true",
		})
		f.writes++
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}

	f.mu.Lock()
	f.unknown = append(f.unknown, q)
	f.mu.Unlock()
	return nil, fmt.Errorf("fakeEngine: unhandled construct %q -- the invariant issues a query this "+
		"fake does not model, and answering it with zero rows would read as 'no key exists'", q)
}

func nodeFromFake(r map[string]string) *memqlv1.MemoryNode {
	return &memqlv1.MemoryNode{
		Id: r["id"],
		Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
			"id":          structpb.NewStringValue(r["id"]),
			"userId":      structpb.NewStringValue(r["userId"]),
			"active":      structpb.NewBoolValue(r["active"] == "true"),
			"credentials": structpb.NewStructValue(&structpb.Struct{Fields: map[string]*structpb.Value{}}),
		}},
	}
}

type fixedOwners []string

func (o fixedOwners) OwnerUserIds(context.Context) ([]string, error) { return []string(o), nil }

// openDirectDB opens the raw *sql.DB the advisory lock rides.
func openDirectDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := dbtest.DSN()
	bdb := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := bdb.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "recovery-key single-flight mint", dsn, err)
		return nil
	}
	t.Cleanup(func() { _ = bdb.Close() })
	return bdb.DB
}

// TestTwoConcurrentMintersProduceExactlyOneKey is the acceptance criterion.
func TestTwoConcurrentMintersProduceExactlyOneKey(t *testing.T) {
	db := openDirectDB(t)
	if db == nil {
		return
	}

	const owner = "user-owner-3965"
	eng := &fakeEngine{readWriteGap: 250 * time.Millisecond}
	opts := EnsureOptions{
		DB:       func() *sql.DB { return db },
		Store:    &Store{Engine: eng},
		Owners:   fixedOwners{owner},
		MintedBy: "system:identity-svc",
	}

	// Two minters, started together, exactly as two identity replicas start
	// together on a fresh cluster.
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]EnsureResult, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = EnsureForAllOwners(context.Background(), opts)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("minter %d: %v", i, err)
		}
	}
	if len(eng.unknown) > 0 {
		t.Fatalf("the invariant issued constructs the fake does not model: %s",
			strings.Join(eng.unknown, "; "))
	}

	eng.mu.Lock()
	rows, writes := len(eng.rows), eng.writes
	eng.mu.Unlock()

	if rows != 1 {
		t.Errorf("two concurrent minters produced %d recovery keys, want exactly 1.\n"+
			"Each extra row is a LIVE, UNCLAIMED, fully-valid owner-equivalent credential sitting "+
			"in the database that no operator knows about and no runbook retires. This is memql#3400's "+
			"shape -- both replicas read 'nothing exists' and both wrote -- applied to a credential "+
			"instead of a signing key.", rows)
	}
	if writes != 1 {
		t.Errorf("%d create writes reached the engine, want 1", writes)
	}

	minted := results[0].Minted + results[1].Minted
	if minted != 1 {
		t.Errorf("Minted totals %d across both minters, want 1 -- the loser must report the key as "+
			"already present, not mint its own", minted)
	}
	// The loser must see the winner's row, which is what proves it re-read
	// INSIDE the lock rather than acting on a read taken before it.
	if len(results[0].AlreadyPresent)+len(results[1].AlreadyPresent) != 1 {
		t.Error("exactly one minter should have reported the key already present")
	}
	// And the winner must return the plaintext without it ever being logged.
	var plains int
	for _, r := range results {
		plains += len(r.Plain)
	}
	if plains != 1 {
		t.Errorf("%d plaintexts returned, want 1", plains)
	}
}

// TestTwoMintersWithoutTheLockRace proves the test above is measuring the
// lock, not measuring nothing.
//
// It runs the same two minters through the same fake with the same
// read-write gap, but WITHOUT the advisory lock -- read, decide, write, no
// mutual exclusion. If this does not double-mint, the gap is too small or the
// fake serialises somewhere, and the passing case above proves nothing.
func TestTwoMintersWithoutTheLockRace(t *testing.T) {
	const owner = "user-owner-3965-unlocked"
	eng := &fakeEngine{readWriteGap: 250 * time.Millisecond}
	store := &Store{Engine: eng}

	unlockedEnsure := func() {
		live, err := store.ActiveForUser(context.Background(), owner)
		if err != nil {
			t.Error(err)
			return
		}
		if len(live) > 0 {
			return
		}
		_, hash, err := Mint()
		if err != nil {
			t.Error(err)
			return
		}
		id, err := NewId()
		if err != nil {
			t.Error(err)
			return
		}
		if err := store.Create(context.Background(), id, owner, hash, "test", "", DefaultLabel); err != nil {
			t.Error(err)
		}
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			unlockedEnsure()
		}()
	}
	close(start)
	wg.Wait()

	eng.mu.Lock()
	rows := len(eng.rows)
	eng.mu.Unlock()

	if rows != 2 {
		t.Errorf("the UNLOCKED path produced %d rows, want 2. The race window this test relies on "+
			"did not open, so TestTwoConcurrentMintersProduceExactlyOneKey is not evidence that the "+
			"advisory lock does anything -- widen readWriteGap or fix the fake.", rows)
	}
}
