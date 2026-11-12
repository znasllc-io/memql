package datasync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageparser "github.com/znasllc-io/memql/component/language/parser"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify"
)

// inbound_retry_db_test.go -- the retry pass through a real engine. db-gated,
// like syncstate_write_db_test.go.

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

// fail writes a row straight to failed with a try count and a next time, as a
// dispatcher on another node would have left it.
func (f *retryFixture) fail(id string, attempts int, next time.Time) {
	f.t.Helper()
	if _, err := f.store.exec(f.ctx, call("mutation", "updateInboundRequestStatus",
		arg{"requestId", id}, arg{"status", "failed"}, arg{"attempts", attempts},
		arg{"nextAttemptAt", next.UTC().Format(time.RFC3339)})); err != nil {
		f.t.Fatalf("failing %s: %v", id, err)
	}
}

// versions counts the stored versions of one delivery.
func versions(t *testing.T, db *sql.DB, id string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE id = $1`,
		inboundRequestConcept+":"+id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A due row whose source no connector serves -- its store was removed, or this
// node binds no connector -- is written ONCE and then never again: it leaves
// the due set with no next time and its try count as it was. Fifty of them do
// not hold the page against a served row behind them, which the next pass
// retries.
func TestAnUnservedDueRowIsWrittenOnceAndDoesNotHoldThePage(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	nanos := time.Now().UnixNano()
	served, unserved := fmt.Sprintf("dbtest-served-%d", nanos), fmt.Sprintf("dbtest-unserved-%d", nanos)
	cleanInbound(t, db, served, unserved)
	at := time.Now().UTC().Truncate(time.Second)

	f := newRetryFixture(t, engineSeam{eng}, served)
	gone := newRetryFixture(t, engineSeam{eng}, unserved) // its rows are written here; f's dispatcher never serves them
	gone.clock, f.clock = at.Add(-time.Hour), at.Add(-time.Hour)
	var parked []string
	for i := 0; i < 50; i++ {
		id := gone.stage(fmt.Sprintf("row-%02d", i))
		gone.fail(id, 2, at.Add(-time.Minute))
		parked = append(parked, id)
	}
	waiting := f.stage("waiting")
	f.fail(waiting, 1, at.Add(-time.Minute))

	before := versions(t, db, parked[0])
	for pass := 1; pass <= 3; pass++ {
		// Each pass past the last one's claim window, so a claim alone
		// would make the row due again.
		f.clock = at.Add(time.Duration(pass-1) * (inboundRetryClaim + time.Minute))
		if _, err := f.d.RetryDue(f.ctx); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	if got := versions(t, db, parked[0]); got != before+1 {
		t.Fatalf("an unserved row was written %d times across three passes, want once", got-before)
	}
	r := gone.row(parked[0])
	if intField(r, "attempts") != 2 || stringField(r, "nextAttemptAt") != "" || !strings.Contains(stringField(r, "lastError"), "no connector") {
		t.Fatalf("the unserved row is %v, want its two tries, no next time and a lastError naming why", r)
	}
	due, err := f.store.InboundRequestsDueForRetry(f.ctx, at.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.Source == unserved {
			t.Fatalf("the due read still answers unserved row %s", d.RequestId)
		}
	}
	if r := f.row(waiting); stringField(r, "status") != "processed" || intField(r, "attempts") != 2 {
		t.Fatalf("the served row behind fifty unserved ones is %v, want it retried and processed", r)
	}
}

// TWO CLAIMERS, ONE ROW. Both read the row at the same version and claim it
// at once, as an overlapping pass or a second cron leader would: the fence
// lets exactly one through, and neither claim moves the try count.
func TestOfTwoClaimersOfOneDueRowExactlyOneWins(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	source := fmt.Sprintf("dbtest-claim-%d", time.Now().UnixNano())
	cleanInbound(t, db, source)
	f := newRetryFixture(t, engineSeam{eng}, source)
	at := time.Now().UTC().Truncate(time.Second)
	f.clock = at.Add(-time.Hour)
	id := f.stage("contended")
	f.fail(id, 2, at.Add(-time.Minute))

	staged, found, err := f.d.Staged(f.ctx, id)
	if err != nil || !found {
		t.Fatalf("Staged: %v %t", err, found)
	}
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			won, err := f.store.ClaimInboundRetry(f.ctx, staged, at.Add(inboundRetryClaim))
			results <- won
			errs <- err
		}()
	}
	wins := 0
	for i := 0; i < 2; i++ {
		if <-results {
			wins++
		}
		if err := <-errs; err != nil {
			t.Fatalf("a claim errored rather than losing: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d of two claimers won, want exactly one", wins)
	}
	if r := f.row(id); intField(r, "attempts") != 2 || !timeField(r, "nextAttemptAt").Equal(at.Add(inboundRetryClaim)) {
		t.Fatalf("after the claims the row is %v, want its two tries and the winner's window", r)
	}
}

// A STORE'S OWNER WHO IS NOT A CLUSTER OWNER STILL GETS "TRY AGAIN". The Store
// page's act asks its own question (the store's owner, or a cluster owner) and
// then reaches datasyncRequeueInbound as the operator, with internal origin --
// which is what the requeue's own gate admits. The same person calling the
// requeue builtin directly is refused, in the gate's words, and nothing moves.
// Through a real engine, with both integrations registered on it.
func TestAStoresRequeueStillRunsThroughTheConnector(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	nanos := time.Now().UnixNano()
	storeID := fmt.Sprintf("dbtest-requeue-store-%d", nanos)
	source := shopify.ConnectorName + "-" + storeID
	storeOwner := fmt.Sprintf("dbtest-store-owner-%d", nanos)
	cleanInbound(t, db, source)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND id = $1`, "v1:shopify:store:"+storeID)
	})

	claims := map[string]any{"sub": "dbtest-owner", "role": "owner"}
	clusterOwner := auth.ContextWithClaims(context.Background(), claims)
	clusterOwner = auth.ContextWithToken(clusterOwner, auth.BuildTokenInfo(claims))
	clusterOwner = auth.ContextWithAccess(clusterOwner, &auth.AccessContext{UserId: "dbtest-owner", Role: auth.RoleOwner})
	if _, err := eng.Execute(clusterOwner, fmt.Sprintf(`mutation createStore(storeId: %s, domain: "acme-widgets.myshopify.com", adminTokenRef: "test-admin", apiVersion: "2026-07", ownerUserId: %s)`,
		languageparser.QuoteString(storeID), languageparser.QuoteString(storeOwner))); err != nil {
		t.Fatalf("write the test store: %v", err)
	}

	// One delivery of the store's source that used its four tries.
	f := newRetryFixture(t, engineSeam{eng}, source)
	at := time.Now().UTC().Truncate(time.Second)
	f.clock = at.Add(-5 * time.Hour)
	givenUp := f.stage("given-up")
	for i := 4; i > 0; i-- {
		f.try(givenUp, at.Add(-time.Duration(i)*time.Hour), errors.New("origin timed out"))
	}
	if r := f.row(givenUp); intField(r, "attempts") != 4 {
		t.Fatalf("the delivery was not given up: %v", r)
	}

	logger := slog.New(slog.DiscardHandler)
	if err := eng.RegisterIntegration(NewIntegration(engineSeam{eng}, logger)); err != nil {
		t.Fatalf("register the sync runtime: %v", err)
	}
	stores := shopify.NewStoreRegistry(eng, func(context.Context, string) (string, error) { return "test-only-admin-token", nil })
	var tryAgain func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error)
	for _, c := range shopify.NewIntegration(shopify.NewConnector(eng, logger, stores, shopify.NewAdminClient())).Capabilities() {
		if c.Name == "storeRetryDeliveries" {
			tryAgain = c.Handler
		}
	}
	if tryAgain == nil {
		t.Fatal("the Shopify integration has no storeRetryDeliveries")
	}

	// The store's owner, a writer: not a cluster owner.
	ownerClaims := map[string]any{"sub": storeOwner, "role": "writer"}
	writer := auth.ContextWithClaims(context.Background(), ownerClaims)
	writer = auth.ContextWithToken(writer, auth.BuildTokenInfo(ownerClaims))
	writer = auth.ContextWithAccess(writer, &auth.AccessContext{UserId: storeOwner, Role: auth.RoleWriter})

	direct := `builtin datasyncRequeueInbound(source: ` + languageparser.QuoteString(source) + `)`
	if _, err := eng.Execute(writer, direct); err == nil || !strings.Contains(err.Error(), "datasync: requeueInbound is for a cluster owner") {
		t.Fatalf("the store's owner called the requeue directly and got %v, want the gate's refusal", err)
	}
	if r := f.row(givenUp); intField(r, "attempts") != 4 {
		t.Fatalf("a refused requeue moved the delivery: %v", r)
	}

	if _, err := tryAgain(writer, map[string]any{"storeId": storeID}, 0); err != nil {
		t.Fatalf("Try again refused the store's owner: %v", err)
	}
	if r := f.row(givenUp); stringField(r, "status") != "failed" || intField(r, "attempts") != 0 || timeField(r, "nextAttemptAt").IsZero() {
		t.Fatalf("after Try again the delivery is %v, want failed with no tries counted and a next time", r)
	}
}
