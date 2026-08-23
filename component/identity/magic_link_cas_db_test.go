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

// magic_link_cas_db_test.go -- memql#4301's headline acceptance criterion:
// two concurrent consumers of one magic-link row, exactly one success.
//
// POSTGRES-GATED BECAUSE THE THING UNDER TEST IS A POSTGRES ADVISORY LOCK.
// An in-memory stand-in would serialise because the fake serialises, which is
// the assumption that produced the race in the first place. CI's db-tests
// lane runs with MEMQL_REQUIRE_DB=1, so a skip there is a failure.
//
// The ENGINE half is faked, deliberately and in one specific way: the fake
// sleeps between the read and the write, widening a window a real engine
// leaves open for microseconds into one a scheduler cannot miss. Without the
// lock both consumers read "not consumed" inside that window and both write.
// TestConcurrentConsumesWithoutTheGateRace below asserts exactly that, so a
// pass above is known to be the gate working rather than the race merely
// failing to occur.

// magicLinkFakeEngine is a minimal EngineExecutor over one in-memory row.
//
// It answers the two constructs the guarded writes use and nothing else, so a
// construct the store starts issuing without updating the fake shows up in
// `unknown` instead of silently returning zero rows -- which would read as
// "no such request" and make the test pass for the wrong reason.
type magicLinkFakeEngine struct {
	mu  sync.Mutex
	row map[string]string

	// readWriteGap is slept on the READ so it lands between the store's
	// decision and the write that follows -- the window the advisory lock
	// exists to close.
	readWriteGap time.Duration

	reads    int
	consumes int
	approves int
	unknown  []string
}

var (
	fakeMLByIdRe    = regexp.MustCompile(`^query magicLinkRequestById\(requestId: "([^"]*)"\)$`)
	fakeMLConsumeRe = regexp.MustCompile(`^mutation consumeMagicLinkRequest\(requestId: "([^"]*)"`)
	fakeMLApproveRe = regexp.MustCompile(`^mutation approveMagicLinkRequest\(requestId: "([^"]*)"`)
)

func (f *magicLinkFakeEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	switch {
	case fakeMLByIdRe.MatchString(q):
		f.mu.Lock()
		f.reads++
		node := magicLinkNodeFromFake(f.row)
		gap := f.readWriteGap
		f.mu.Unlock()
		if gap > 0 {
			time.Sleep(gap)
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{node}}}, nil

	case fakeMLConsumeRe.MatchString(q):
		f.mu.Lock()
		defer f.mu.Unlock()
		f.consumes++
		f.row["consumedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil

	case fakeMLApproveRe.MatchString(q):
		f.mu.Lock()
		defer f.mu.Unlock()
		f.approves++
		f.row["approvedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
	f.mu.Lock()
	f.unknown = append(f.unknown, q)
	f.mu.Unlock()
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func magicLinkNodeFromFake(r map[string]string) *memqlv1.MemoryNode {
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

// openDirectDBForMagicLink opens the raw *sql.DB the advisory lock rides.
func openDirectDBForMagicLink(t *testing.T) *sql.DB {
	t.Helper()
	dsn := dbtest.DSN()
	bdb := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := bdb.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "magic-link exactly-once consume", dsn, err)
		return nil
	}
	t.Cleanup(func() { _ = bdb.Close() })
	return bdb.DB
}

// TestConcurrentConsumesYieldExactlyOneSuccess is the acceptance criterion of
// memql#4301: "two concurrent consumers of one row produce one success and one
// already-used".
//
// Four rather than two, because the device-bound flow has more than two
// plausible finishers in flight (the poller, a same-device click, a retry) and
// the property is "exactly one", not "at most two".
func TestConcurrentConsumesYieldExactlyOneSuccess(t *testing.T) {
	db := openDirectDBForMagicLink(t)
	if db == nil {
		return
	}

	const requestId = "ml-req-4301-cas"
	eng := &magicLinkFakeEngine{
		row:          map[string]string{"id": requestId, "email": "team@example.test"},
		readWriteGap: 250 * time.Millisecond,
	}
	store := &Store{Engine: eng, DirectDB: func() *sql.DB { return db }}

	const consumers = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, consumers)
	for i := 0; i < consumers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = store.ConsumeMagicLinkRequest(context.Background(), requestId, "203.0.113.9")
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
		case err == ErrMagicLinkAlreadyConsumed:
			alreadyUsed++
		default:
			t.Fatalf("consumer %d: unexpected error: %v", i, err)
		}
	}

	if won != 1 {
		t.Errorf("%d of %d concurrent consumers succeeded, want exactly 1.\n"+
			"Each extra success is a second auth code minted from ONE magic link -- the single-use "+
			"property the whole credential rests on. With approve-on-click there are two LEGITIMATE "+
			"finishers of a request (the /check-email poller and a same-device click), so this is not "+
			"a theoretical interleaving: it is the ordinary shape of the flow.", won, consumers)
	}
	if alreadyUsed != consumers-1 {
		t.Errorf("%d consumers reported already-consumed, want %d -- a loser must be TOLD it lost, "+
			"so the page can say 'this link has already been used' instead of silently doing nothing",
			alreadyUsed, consumers-1)
	}

	eng.mu.Lock()
	writes := eng.consumes
	eng.mu.Unlock()
	if writes != 1 {
		t.Errorf("%d consume writes reached the engine, want 1", writes)
	}
}

// TestConcurrentApprovalsYieldExactlyOneWinner pins the approval's
// conditional write: a second click on the same link is idempotent, and the
// device facts recorded stay the FIRST approver's.
func TestConcurrentApprovalsYieldExactlyOneWinner(t *testing.T) {
	db := openDirectDBForMagicLink(t)
	if db == nil {
		return
	}

	const requestId = "ml-req-4301-approve"
	eng := &magicLinkFakeEngine{
		row:          map[string]string{"id": requestId, "email": "team@example.test"},
		readWriteGap: 250 * time.Millisecond,
	}
	store := &Store{Engine: eng, DirectDB: func() *sql.DB { return db }}

	const clickers = 3
	var wg sync.WaitGroup
	start := make(chan struct{})
	wonFlags := make([]bool, clickers)
	errs := make([]error, clickers)
	for i := 0; i < clickers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			wonFlags[i], errs[i] = store.ApproveMagicLinkRequest(context.Background(), requestId, "198.51.100.7", "Mozilla/5.0")
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("clicker %d: %v", i, err)
		}
	}
	won := 0
	for _, w := range wonFlags {
		if w {
			won++
		}
	}
	if won != 1 {
		t.Errorf("%d of %d concurrent approvals reported winning, want exactly 1 -- a second "+
			"approval must be idempotent, not a second row-write carrying a different device's IP", won, clickers)
	}
	eng.mu.Lock()
	writes := eng.approves
	eng.mu.Unlock()
	if writes != 1 {
		t.Errorf("%d approve writes reached the engine, want 1", writes)
	}
}

// TestConcurrentConsumesWithoutTheGateRace proves the test above measures the
// gate rather than measuring nothing.
//
// Same fake, same read-write gap, same store code -- but with DirectDB nil, so
// withMagicLinkGate degrades to running the body unlocked. If this does not
// double-consume, the gap is too small or the fake serialises somewhere, and
// the passing case above proves nothing. It asserts the RACE, which is why it
// needs no database of its own.
func TestConcurrentConsumesWithoutTheGateRace(t *testing.T) {
	const requestId = "ml-req-4301-unlocked"
	eng := &magicLinkFakeEngine{
		row:          map[string]string{"id": requestId, "email": "team@example.test"},
		readWriteGap: 250 * time.Millisecond,
	}
	store := &Store{Engine: eng} // no DirectDB: no lock

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = store.ConsumeMagicLinkRequest(context.Background(), requestId, "203.0.113.9")
		}()
	}
	close(start)
	wg.Wait()

	eng.mu.Lock()
	writes := eng.consumes
	eng.mu.Unlock()
	if writes < 2 {
		t.Fatalf("the unlocked path produced %d consume writes; expected the race to double-write.\n"+
			"This test exists to prove the gated tests are measuring the advisory lock. If the race "+
			"cannot be reproduced here, a green result there says nothing.", writes)
	}
}
