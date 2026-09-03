package identity

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/database/dbtest"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// github_connect_cas_db_test.go -- epic memql#4912's exactly-once criterion:
// "the state resolves and is consumed once across two replicas".
//
// POSTGRES-GATED BECAUSE THE THING UNDER TEST IS A POSTGRES ADVISORY LOCK.
// An in-memory stand-in would serialise because the fake serialises, which is
// the assumption that produces this class of race in the first place. CI's
// db-tests lane runs with MEMQL_REQUIRE_DB=1, so a skip there is a failure.
//
// WHY IT IS THE SAME TEST AS THE MAGIC LINK'S, DELIBERATELY. Two identity
// replicas share nothing but the database, and a browser redirect is
// replayable by construction: the URL sits in history, in a referrer, and in
// whatever the person pasted it into. Two consumes mean two grants written for
// one authorization -- and because a reconnect updates in place while a create
// mints a row, the second one is not even a duplicate of the first: it is a
// second row for one GitHub account, which is exactly the state externalId
// keying exists to make impossible.
//
// The ENGINE half is faked, and in one specific way: the fake sleeps between
// the read and the write, widening a window a real engine leaves open for
// microseconds into one a scheduler cannot miss. Without the lock both
// consumers read "not consumed" inside that window and both write.
// TestConcurrentConnectConsumesWithoutTheGateRace below asserts exactly that,
// so a pass above is known to be the gate working rather than the race merely
// failing to occur.

// githubConnectFakeEngine is a minimal EngineExecutor over one in-memory row.
//
// It answers the two constructs the guarded consume uses and nothing else, so
// a construct the store starts issuing without updating the fake shows up in
// `unknown` instead of silently returning zero rows -- which would read as "no
// such state" and make the test pass for the wrong reason.
type githubConnectFakeEngine struct {
	mu  sync.Mutex
	row map[string]string

	// readWriteGap is slept on the READ so it lands between the store's
	// decision and the write that follows -- the window the advisory lock
	// exists to close.
	readWriteGap time.Duration

	reads    int
	consumes int
	unknown  []string
}

var (
	fakeGCByHashRe  = regexp.MustCompile(`^query githubConnectStateByHash\(stateHash: "([^"]*)"\)$`)
	fakeGCConsumeRe = regexp.MustCompile(`^mutation consumeGithubConnectState\(stateId: "([^"]*)"`)
)

func (f *githubConnectFakeEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	switch {
	case fakeGCByHashRe.MatchString(q):
		f.mu.Lock()
		f.reads++
		node := githubConnectNodeFromFake(f.row)
		gap := f.readWriteGap
		f.mu.Unlock()
		if gap > 0 {
			time.Sleep(gap)
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{node}}}, nil

	case fakeGCConsumeRe.MatchString(q):
		f.mu.Lock()
		defer f.mu.Unlock()
		f.consumes++
		f.row["consumedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
	f.mu.Lock()
	f.unknown = append(f.unknown, q)
	f.mu.Unlock()
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func githubConnectNodeFromFake(r map[string]string) *memqlv1.MemoryNode {
	fields := map[string]*structpb.Value{}
	for k, v := range r {
		if v == "" {
			continue
		}
		fields[k] = structpb.NewStringValue(v)
	}
	return &memqlv1.MemoryNode{
		Id:      r["id"],
		Payload: &structpb.Struct{Fields: fields},
	}
}

// openDirectDBForGithubConnect opens the raw *sql.DB the advisory lock rides.
func openDirectDBForGithubConnect(t *testing.T) *sql.DB {
	t.Helper()
	dsn := dbtest.DSN()
	bdb := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := bdb.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "github-connect exactly-once consume", dsn, err)
		return nil
	}
	t.Cleanup(func() { _ = bdb.Close() })
	return bdb.DB
}

func liveConnectStateRow() map[string]string {
	return map[string]string{
		"id":        "v1:identity:githubConnectState:cas",
		"userId":    "v1:identity:user:asked",
		"stateHash": HashConnectState("the-plaintext-state"),
		"expiresAt": time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339),
	}
}

// TestConcurrentConnectConsumesYieldExactlyOneSuccess is the criterion: N
// concurrent callbacks for one state produce exactly one success.
//
// Four rather than two, because a replay has more than two plausible sources
// in flight -- the browser's own retry, a prefetcher, and somebody re-opening
// the URL -- and the property is "exactly one", not "at most two".
func TestConcurrentConnectConsumesYieldExactlyOneSuccess(t *testing.T) {
	db := openDirectDBForGithubConnect(t)
	if db == nil {
		return
	}

	eng := &githubConnectFakeEngine{
		row:          liveConnectStateRow(),
		readWriteGap: 250 * time.Millisecond,
	}
	store := &Store{Engine: eng, DirectDB: func() *sql.DB { return db }}
	hash := HashConnectState("the-plaintext-state")

	const consumers = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, consumers)
	rows := make([]*GithubConnectStateRow, consumers)
	for i := 0; i < consumers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rows[i], errs[i] = store.ConsumeGithubConnectState(context.Background(), hash, "203.0.113.9")
		}(i)
	}
	close(start)
	wg.Wait()

	if len(eng.unknown) > 0 {
		t.Fatalf("the store issued constructs the fake does not model: %s", strings.Join(eng.unknown, "; "))
	}

	won, alreadyUsed := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
			if rows[i] == nil || rows[i].UserId != "v1:identity:user:asked" {
				t.Errorf("consumer %d won but got no usable row back; the winner is the one that "+
					"writes the grant, and it needs the state's user to write it under", i)
			}
		case err == ErrGithubConnectStateAlreadyConsumed:
			alreadyUsed++
			if rows[i] != nil {
				t.Errorf("consumer %d lost and still received a row. A loser holding the state's "+
					"user is one refactor away from writing a second grant with it", i)
			}
		default:
			t.Fatalf("consumer %d: unexpected error: %v", i, err)
		}
	}

	if won != 1 {
		t.Errorf("%d of %d concurrent callbacks succeeded, want exactly 1.\n"+
			"Each extra success is a second sourceCredential row for ONE GitHub account -- not a "+
			"duplicate the next reconnect would tidy up, because the update path keys on externalId "+
			"and would then have two rows to choose between. A browser redirect is replayable by "+
			"construction, so this is the ordinary shape of the flow rather than a theoretical "+
			"interleaving.", won, consumers)
	}
	if alreadyUsed != consumers-1 {
		t.Errorf("%d callbacks reported already-consumed, want %d -- a loser must be TOLD it lost, "+
			"so the OS renders connect_state_invalid instead of a silent nothing",
			alreadyUsed, consumers-1)
	}

	eng.mu.Lock()
	writes := eng.consumes
	eng.mu.Unlock()
	if writes != 1 {
		t.Errorf("%d consume writes reached the engine, want 1", writes)
	}
}

// TestConcurrentConnectConsumesWithoutTheGateRace proves the test above
// measures the gate rather than measuring nothing.
//
// Same fake, same read-write gap, same store code -- but with DirectDB nil, so
// withGithubConnectGate degrades to running the body unlocked. If this does not
// double-consume, the gap is too small or the fake serialises somewhere, and
// the passing case above proves nothing. It asserts the RACE, which is why it
// needs no database of its own.
func TestConcurrentConnectConsumesWithoutTheGateRace(t *testing.T) {
	eng := &githubConnectFakeEngine{
		row:          liveConnectStateRow(),
		readWriteGap: 250 * time.Millisecond,
	}
	store := &Store{Engine: eng} // no DirectDB: no lock
	hash := HashConnectState("the-plaintext-state")

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = store.ConsumeGithubConnectState(context.Background(), hash, "203.0.113.9")
		}()
	}
	close(start)
	wg.Wait()

	eng.mu.Lock()
	writes := eng.consumes
	eng.mu.Unlock()
	if writes < 2 {
		t.Fatalf("the unlocked path produced %d consume writes; expected the race to double-write.\n"+
			"This test exists to prove the gated test above is measuring the advisory lock. If the "+
			"race cannot be reproduced here, a green result there says nothing.", writes)
	}
}

// TestAnExpiredConnectStateIsRefusedWithoutAWrite pins the third outcome the
// consume distinguishes. It needs no lock and no database: the refusal is a
// property of the compare, and the compare runs whether the gate is there or
// not.
func TestAnExpiredConnectStateIsRefusedWithoutAWrite(t *testing.T) {
	row := liveConnectStateRow()
	row["expiresAt"] = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	eng := &githubConnectFakeEngine{row: row}
	store := &Store{Engine: eng}

	got, err := store.ConsumeGithubConnectState(context.Background(),
		HashConnectState("the-plaintext-state"), "203.0.113.9")
	if err != ErrGithubConnectStateExpired {
		t.Fatalf("err = %v, want ErrGithubConnectStateExpired", err)
	}
	if got != nil {
		t.Error("an expired state returned a row; the caller must have nothing to write a grant with")
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if eng.consumes != 0 {
		t.Errorf("%d consume write(s) on an expired state, want 0 -- an expired row must stay "+
			"expired rather than becoming a consumed one, so the audit trail keeps telling a "+
			"person who walked away apart from a replay", eng.consumes)
	}
}
