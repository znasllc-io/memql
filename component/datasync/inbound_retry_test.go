package datasync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// ---- A failed delivery is tried again (U4) ----
//
// The stamp of a failed try carries the try count and, when the failure is
// worth another try, when that try is due: 1, 5 and 30 minutes after the try
// before, four tries in all. The retry pass reads what is due, claims each row
// behind the row-version fence, and dispatches it through DispatchStaged.

// retryEngine is a fakeEngine that also records, per inbound status write,
// whether it carried a row-version fence, and refuses the fenced writes of the
// ids in lose as another writer's commit would.
type retryEngine struct {
	*fakeEngine
	mu     sync.Mutex
	fenced map[string][]bool
	lose   map[string]bool
}

func newRetryEngine() *retryEngine {
	return &retryEngine{fakeEngine: newFakeEngine(), fenced: map[string][]bool{}, lose: map[string]bool{}}
}

func (r *retryEngine) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "mutation updateInboundRequestStatus(") {
		id, _ := renderedArgs(q)["requestId"].(string)
		fenced := memql.HasRowVersionFence(ctx, inboundRequestConcept, id)
		r.mu.Lock()
		r.fenced[id] = append(r.fenced[id], fenced)
		lost := fenced && r.lose[id]
		r.mu.Unlock()
		if lost {
			_, _ = r.fakeEngine.Execute(ctx, q)
			return nil, fmt.Errorf("execute: %w", memql.ErrRowVersionChanged)
		}
	}
	return r.fakeEngine.Execute(ctx, q)
}

// forgettingConnector is a fakeConnector that implements the optional
// memqlsync.InboundForgetter, recording each delivery it is told to forget.
type forgettingConnector struct {
	*fakeConnector
	forgot []string
}

func (f *forgettingConnector) ForgetInbound(_ context.Context, req memqlsync.InboundRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgot = append(f.forgot, req.RequestId)
}

// retryDispatcher wires a dispatcher over any engine and one connector.
func retryDispatcher(engine Engine, writer MirrorWriter, c memqlsync.Connector) *Dispatcher {
	store := NewStore(engine)
	d := NewDispatcher(store, NewApplier(store, writer))
	d.lookup = func(_ context.Context, name string) (memqlsync.Connector, bool) {
		if name == c.Name() {
			return c, true
		}
		return nil, false
	}
	d.now = func() time.Time { return testNow }
	return d
}

// stagedFailure is a served staged delivery that has already been tried
// `attempts` times.
func stagedFailure(id string, attempts int) StagedRequest {
	return StagedRequest{
		RequestId: id, Source: "shopify", Body: []byte("{}"), HeadersJSON: `{"x-shopify-topic":"products/update"}`,
		VerifiedBy: memqlsync.VerifiedByConnector, SignatureVerified: true, Attempts: attempts,
	}
}

// onlyStamp returns the one status write a dispatch made.
func onlyStamp(t *testing.T, engine *fakeEngine) map[string]any {
	t.Helper()
	stamps := engine.callsContaining("mutation updateInboundRequestStatus(")
	if len(stamps) != 1 {
		t.Fatalf("the request was stamped %d times, want 1: %v", len(stamps), stamps)
	}
	return renderedArgs(stamps[0])
}

// clearsNextAttempt reports whether a stamp leaves the row with no next try.
func clearsNextAttempt(stamp map[string]any) bool {
	v, named := stamp["nextAttemptAt"]
	return named && v == ""
}

func TestAFailureWorthAnotherTryIsStampedWithItsNextTime(t *testing.T) {
	for _, tc := range []struct {
		name     string
		applyErr error
		writeErr error
	}{
		{name: "the connector's Apply failed", applyErr: errors.New("origin timed out")},
		{name: "the origin answered busy", applyErr: memqlsync.Busy("shopify", "the store's cost bucket is empty")},
		// The connector fetched the object and the runtime could not write
		// it: the connector has already recorded the delivery as seen.
		{name: "the runtime's applier failed", writeErr: errors.New("mirror write refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := newFakeEngine()
			c := &forgettingConnector{fakeConnector: &fakeConnector{
				name:        "shopify",
				domains:     []memqlsync.DomainSpec{{Concept: testMirrorConcept, Direction: memqlsync.DirectionInbound}},
				applyWrites: []memqlsync.MirrorWrite{{Concept: testMirrorConcept, RowId: "p1", Version: testNow.Format(time.RFC3339)}},
				applyErr:    tc.applyErr,
			}}
			writer := newFakeWriter()
			writer.writeErr = tc.writeErr
			d := retryDispatcher(engine, writer, c)

			if _, err := d.DispatchStaged(context.Background(), stagedFailure("req-1", 0)); err == nil {
				t.Fatal("a failed try reported success")
			}
			stamp := onlyStamp(t, engine)
			if stamp["status"] != "failed" || stamp["attempts"] != 1 {
				t.Fatalf("stamped %v, want status failed and attempts 1", stamp)
			}
			if want := testNow.Add(time.Minute).Format(time.RFC3339); stamp["nextAttemptAt"] != want {
				t.Fatalf("nextAttemptAt = %v, want %s: the first retry comes one minute after the first try", stamp["nextAttemptAt"], want)
			}
			if len(c.forgot) != 1 || c.forgot[0] != "req-1" {
				t.Fatalf("the connector was told to forget %v, want [req-1]: a short-circuit that remembers the "+
					"delivery answers the retry 'already seen' and the change is never fetched", c.forgot)
			}
		})
	}
}

func TestTheThreeRetriesComeAtOneFiveAndThirtyMinutes(t *testing.T) {
	for prior, wait := range []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute} {
		t.Run(fmt.Sprintf("after try %d", prior+1), func(t *testing.T) {
			engine := newFakeEngine()
			c := &fakeConnector{name: "shopify", applyErr: errors.New("origin timed out")}
			d := retryDispatcher(engine, newFakeWriter(), c)
			_, _ = d.DispatchStaged(context.Background(), stagedFailure("req-1", prior))
			stamp := onlyStamp(t, engine)
			if stamp["attempts"] != prior+1 {
				t.Fatalf("attempts = %v, want %d", stamp["attempts"], prior+1)
			}
			if want := testNow.Add(wait).Format(time.RFC3339); stamp["nextAttemptAt"] != want {
				t.Fatalf("nextAttemptAt = %v, want %s", stamp["nextAttemptAt"], want)
			}
		})
	}
	if maxInboundAttempts != 4 || inboundRetryAfter != [3]time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute} {
		t.Fatalf("maxInboundAttempts = %d, inboundRetryAfter = %v", maxInboundAttempts, inboundRetryAfter)
	}
}

func TestAfterTheFourthTryTheRowStaysFailed(t *testing.T) {
	engine := newFakeEngine()
	c := &forgettingConnector{fakeConnector: &fakeConnector{name: "shopify", applyErr: errors.New("origin timed out")}}
	d := retryDispatcher(engine, newFakeWriter(), c)
	_, _ = d.DispatchStaged(context.Background(), stagedFailure("req-1", 3))
	stamp := onlyStamp(t, engine)
	if stamp["status"] != "failed" || stamp["attempts"] != 4 {
		t.Fatalf("stamped %v, want failed at attempts 4", stamp)
	}
	if !clearsNextAttempt(stamp) {
		t.Fatalf("stamped %v: the fourth try is the last, so the row keeps no next time", stamp)
	}
}

// Not worth another try: the failures memqlsync says will recur, and every
// refusal DispatchStaged makes before the connector is asked -- a delivery
// another tier verified (verifiedBy env or registered), one nothing verified
// (signatureVerified false) and staged metadata that does not parse.
func TestARefusalThatWillNotChangeIsNeverRetried(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		staged func(*StagedRequest)
	}{
		{name: "permanent", err: memqlsync.Permanentf("the store is pinned to another Admin API version")},
		{name: "not granted", err: memqlsync.NotGranted("shopify", "read_orders")},
		{name: "not requested", err: memqlsync.NotRequested("shopify")},
		{name: "not implemented", err: memqlsync.NotImplemented("shopify", "Apply")},
		{name: "another tier verified it", staged: func(s *StagedRequest) { s.VerifiedBy = "env" }},
		{name: "nothing verified it", staged: func(s *StagedRequest) { s.SignatureVerified = false }},
		{name: "its headers do not parse", staged: func(s *StagedRequest) { s.HeadersJSON = "[]" }},
		{name: "its receipt time does not parse", staged: func(s *StagedRequest) { s.ReceivedAt = "yesterday" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := newFakeEngine()
			c := &forgettingConnector{fakeConnector: &fakeConnector{name: "shopify", applyErr: tc.err}}
			d := retryDispatcher(engine, newFakeWriter(), c)
			staged := stagedFailure("req-1", 1)
			if tc.staged != nil {
				tc.staged(&staged)
			}
			if _, err := d.DispatchStaged(context.Background(), staged); err == nil {
				t.Fatal("the refusal reported success")
			}
			stamp := onlyStamp(t, engine)
			if stamp["status"] != "failed" || stamp["attempts"] != 2 {
				t.Fatalf("stamped %v, want failed and the try counted", stamp)
			}
			if !clearsNextAttempt(stamp) {
				t.Fatalf("stamped %v: a refusal no retry changes keeps no next time, so the retry pass never answers it", stamp)
			}
			if len(c.forgot) != 0 {
				t.Fatalf("the connector was told to forget %v for a delivery nothing will retry", c.forgot)
			}
		})
	}
}

// dueRow is a failed delivery as inboundRequestsDueForRetry answers it.
func dueRow(id string, attempts int) map[string]any {
	row := stagedInbound("shopify")
	row["id"] = id
	row["status"] = "failed"
	row["attempts"] = attempts
	row["nextAttemptAt"] = testNow.Add(-time.Second).Format(time.RFC3339)
	row["createdAt"] = testNow.Add(-time.Minute).Format(time.RFC3339Nano)
	return row
}

func TestARetryThatSucceedsStampsProcessed(t *testing.T) {
	engine := newRetryEngine()
	c := &fakeConnector{
		name:        "shopify",
		domains:     []memqlsync.DomainSpec{{Concept: testMirrorConcept, Direction: memqlsync.DirectionInbound}},
		applyWrites: []memqlsync.MirrorWrite{{Concept: testMirrorConcept, RowId: "p1", Version: testNow.Format(time.RFC3339)}},
	}
	d := retryDispatcher(engine, newFakeWriter(), c)
	engine.seed("query inboundRequestsDueForRetry", []map[string]any{dueRow("req-1", 1)})

	res, err := d.RetryDue(context.Background())
	if err != nil {
		t.Fatalf("RetryDue: %v", err)
	}
	if res != (RetryResult{Tried: 1, Recovered: 1}) {
		t.Fatalf("RetryDue = %+v, want one tried and one recovered", res)
	}
	stamps := engine.callsContaining("mutation updateInboundRequestStatus(")
	if len(stamps) != 2 {
		t.Fatalf("status writes = %v, want the claim and the outcome", stamps)
	}
	claim, outcome := renderedArgs(stamps[0]), renderedArgs(stamps[1])
	if claim["status"] != "failed" || claim["attempts"] != nil || claim["nextAttemptAt"] != testNow.Add(inboundRetryClaim).Format(time.RFC3339) {
		t.Fatalf("claim = %v, want the row moved forward by the claim window with its attempts untouched", claim)
	}
	if got := engine.fenced["req-1"]; len(got) != 2 || !got[0] {
		t.Fatalf("fences = %v, want the claim fenced on the version the due query read", got)
	}
	if outcome["status"] != "processed" || outcome["attempts"] != 2 || !clearsNextAttempt(outcome) {
		t.Fatalf("outcome = %v, want processed, the second try counted, and no next time", outcome)
	}
	if c.applyCalls != 1 {
		t.Fatalf("Apply called %d times, want 1", c.applyCalls)
	}
}

// One pass reads ONE page of what is due, at the pass's own time, and asks for
// no second page: the next minute's pass takes the rest. A row whose claim
// loses the fence -- an overlapping pass or a second cron leader claimed it
// first -- is skipped, not dispatched.
func TestTheRetryPassTakesOneBoundedPage(t *testing.T) {
	engine := newRetryEngine()
	c := &fakeConnector{name: "shopify"}
	d := retryDispatcher(engine, newFakeWriter(), c)
	engine.seed("query inboundRequestsDueForRetry", []map[string]any{dueRow("req-1", 1), dueRow("req-2", 2), dueRow("req-3", 3)})
	engine.lose["req-2"] = true

	res, err := d.RetryDue(context.Background())
	if err != nil {
		t.Fatalf("RetryDue: %v", err)
	}
	reads := engine.callsContaining("query inboundRequestsDueForRetry")
	if len(reads) != 1 {
		t.Fatalf("the pass read the due rows %d times, want one page", len(reads))
	}
	if args := renderedArgs(reads[0]); args["asOf"] != testNow.Format(time.RFC3339) {
		t.Fatalf("the due read asked as of %v, want the pass's own time", args["asOf"])
	}
	if res.Tried != 2 || c.applyCalls != 2 {
		t.Fatalf("tried %d, Apply called %d; want the two rows this pass claimed and not the one it lost", res.Tried, c.applyCalls)
	}
	for _, req := range c.applied {
		if req.RequestId == "req-2" {
			t.Fatal("a row another pass claimed was dispatched here as well")
		}
	}
}

func TestRetryInboundRefusesACallWithoutInternalOrigin(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"a signed-in person": auth.ContextWithUserActor(context.Background(), "v1:identity:user:someone"),
		"a cluster owner": auth.ContextWithAccess(context.Background(),
			&auth.AccessContext{UserId: "v1:identity:user:owner", Role: auth.RoleOwner}),
		"nobody": context.Background(),
	} {
		t.Run(name, func(t *testing.T) {
			engine := newFakeEngine()
			i := &Integration{dispatcher: testDispatcher(engine, newFakeWriter(), &fakeConnector{name: "shopify"}), logger: discardLogger()}
			if _, err := i.handleRetryInbound(ctx, nil, 0); err == nil {
				t.Fatal("a client-origin call ran the retry pass")
			}
			if n := len(engine.callsContaining("")); n != 0 {
				t.Fatalf("a refused call reached the engine %d times", n)
			}
		})
	}
	t.Run("internal origin", func(t *testing.T) {
		engine := newFakeEngine()
		i := &Integration{dispatcher: testDispatcher(engine, newFakeWriter(), &fakeConnector{name: "shopify"}), logger: discardLogger()}
		if _, err := i.handleRetryInbound(auth.ContextWithInternalOrigin(context.Background()), nil, 0); err != nil {
			t.Fatalf("the automation's call was refused: %v", err)
		}
		if engine.countContaining("query inboundRequestsDueForRetry") != 1 {
			t.Fatal("the automation's call did not read what is due")
		}
	})
}

func TestRequeueRefusesACallerWhoIsNotAClusterOwner(t *testing.T) {
	args := map[string]any{"source": "shopify"}
	for name, ctx := range map[string]context.Context{
		"a signed-in person": auth.ContextWithUserActor(context.Background(), "v1:identity:user:someone"),
		"an admin": auth.ContextWithAccess(context.Background(),
			&auth.AccessContext{UserId: "v1:identity:user:admin", Role: auth.RoleAdmin}),
		"nobody": context.Background(),
	} {
		t.Run(name, func(t *testing.T) {
			engine := newFakeEngine()
			i := &Integration{dispatcher: testDispatcher(engine, newFakeWriter(), &fakeConnector{name: "shopify"}), logger: discardLogger()}
			if _, err := i.handleRequeueInbound(ctx, args, 0); err == nil {
				t.Fatal("a caller who is not a cluster owner put given-up deliveries back in line")
			}
			if n := len(engine.callsContaining("")); n != 0 {
				t.Fatalf("a refused call reached the engine %d times", n)
			}
		})
	}
	for name, ctx := range map[string]context.Context{
		"a cluster owner": auth.ContextWithAccess(context.Background(),
			&auth.AccessContext{UserId: "v1:identity:user:owner", Role: auth.RoleOwner}),
		"internal origin": auth.ContextWithInternalOrigin(context.Background()),
	} {
		t.Run(name, func(t *testing.T) {
			engine := newFakeEngine()
			i := &Integration{dispatcher: testDispatcher(engine, newFakeWriter(), &fakeConnector{name: "shopify"}), logger: discardLogger()}
			if _, err := i.handleRequeueInbound(ctx, args, 0); err != nil {
				t.Fatalf("refused: %v", err)
			}
			if engine.countContaining(`query inboundRequestsGivenUp(source: "shopify")`) != 1 {
				t.Fatalf("the given-up rows of the source were not read: %v", engine.callsContaining(""))
			}
		})
	}
	t.Run("a source is required", func(t *testing.T) {
		i := &Integration{dispatcher: testDispatcher(newFakeEngine(), newFakeWriter(), &fakeConnector{name: "shopify"}), logger: discardLogger()}
		if _, err := i.handleRequeueInbound(auth.ContextWithInternalOrigin(context.Background()), map[string]any{"source": " "}, 0); err == nil {
			t.Fatal("a requeue naming no source was accepted")
		}
	})
}

// The event automation fires on EVERY write of an inbound row, not only its
// first: a status stamp, a retry pass's claim and a requeue each publish
// node.created. A row that has been worked is not the event path's to dispatch
// again -- a failed one is the retry pass's, and dispatching it here as well
// would try it from two places at once.
func TestTheEventPathDispatchesOnlyADeliveryNobodyHasWorked(t *testing.T) {
	for _, status := range []string{"failed", "processed", "processing"} {
		t.Run(status, func(t *testing.T) {
			engine := newFakeEngine()
			c := &fakeConnector{name: "shopify", domains: []memqlsync.DomainSpec{{Concept: testMirrorConcept}}}
			i := &Integration{dispatcher: testDispatcher(engine, newFakeWriter(), c)}
			row := stagedInbound("shopify")
			row["status"] = status
			stageInbound(engine, row)
			nodes, err := dispatchByID(i, "req-worked")
			if err != nil {
				t.Fatalf("a worked row raised %v", err)
			}
			if c.applyCalls != 0 || engine.countContaining("updateInboundRequestStatus") != 0 {
				t.Fatalf("a %s row was dispatched again by its own write's event: applyCalls=%d", status, c.applyCalls)
			}
			if len(nodes) != 1 || !strings.Contains(string(nodes[0].Payload), `"outcome":"skipped"`) {
				t.Fatalf("result %v, want one skipped node", nodes)
			}
		})
	}
}

// THE EVENT PATH'S GUARD READS THE ROW FRESH. The status that keeps the event
// path off a claimed or requeued row is only as good as the read behind it:
// inboundRequestById is cached, and another node whose cache still holds the
// `received` version would dispatch the row unfenced while the retry pass does.
func TestTheEventPathReadsTheRowsStatusFresh(t *testing.T) {
	engine := &cachedStatusEngine{fakeEngine: newFakeEngine()}
	c := &fakeConnector{name: "shopify", domains: []memqlsync.DomainSpec{{Concept: testMirrorConcept}}}
	i := &Integration{dispatcher: testDispatcher(engine.fakeEngine, newFakeWriter(), c)}
	i.dispatcher.store = NewStore(engine)

	nodes, err := dispatchByID(i, "req-claimed")
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if fresh := engine.freshContaining("query inboundRequestById"); len(fresh) != 1 || !fresh[0] {
		t.Fatalf("the staged row was read with fresh=%v, want one fresh read", fresh)
	}
	if c.applyCalls != 0 || len(nodes) != 1 || !strings.Contains(string(nodes[0].Payload), `"outcome":"skipped"`) {
		t.Fatalf("a row stored failed but cached received was dispatched: applyCalls=%d nodes=%v", c.applyCalls, nodes)
	}
}

// cachedStatusEngine answers inboundRequestById as a result cache would: the
// `received` version to a cached read, the stored `failed` one to a fresh read.
type cachedStatusEngine struct{ *fakeEngine }

func (c *cachedStatusEngine) Execute(ctx context.Context, q string) (any, error) {
	if _, err := c.fakeEngine.Execute(ctx, q); err != nil {
		return nil, err
	}
	if strings.HasPrefix(q, "query inboundRequestById") {
		row := stagedInbound("shopify")
		if memql.FreshReadFromContext(ctx) {
			row["status"] = "failed"
		}
		return []map[string]any{row}, nil
	}
	return []map[string]any{}, nil
}

// Each claim's window counts from when the claim is taken, not from the start
// of the pass: in a long pass, a window counted from the start would already
// have passed for the later rows.
func TestEachClaimCountsItsWindowFromWhenItIsTaken(t *testing.T) {
	engine := newRetryEngine()
	c := &fakeConnector{name: "shopify"}
	d := retryDispatcher(engine, newFakeWriter(), c)
	clock := testNow
	d.now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	engine.seed("query inboundRequestsDueForRetry", []map[string]any{dueRow("req-1", 1), dueRow("req-2", 1)})

	if _, err := d.RetryDue(context.Background()); err != nil {
		t.Fatalf("RetryDue: %v", err)
	}
	var claims []map[string]any
	for _, call := range engine.callsContaining("mutation updateInboundRequestStatus(") {
		if args := renderedArgs(call); args["lastError"] == nil {
			claims = append(claims, args)
		}
	}
	if len(claims) != 2 {
		t.Fatalf("claims = %v, want two", claims)
	}
	first, _ := time.Parse(time.RFC3339, claims[0]["nextAttemptAt"].(string))
	second, _ := time.Parse(time.RFC3339, claims[1]["nextAttemptAt"].(string))
	if !second.After(first) {
		t.Fatalf("the second claim's window ends at %s, no later than the first's %s: it was counted from the pass's start", second, first)
	}
}

// A due row whose source no connector on this node serves leaves the due set
// with ONE write: no next time, the try count as it was, and a lastError that
// says why. It is neither claimed nor dispatched.
func TestAnUnservedDueRowLeavesTheDueSetWithOneWrite(t *testing.T) {
	engine := newRetryEngine()
	c := &fakeConnector{name: "shopify"}
	d := retryDispatcher(engine, newFakeWriter(), c)
	unserved := dueRow("req-gone", 2)
	unserved["source"] = "shopify-removed-store"
	engine.seed("query inboundRequestsDueForRetry", []map[string]any{unserved})

	res, err := d.RetryDue(context.Background())
	if err != nil {
		t.Fatalf("RetryDue: %v", err)
	}
	writes := engine.callsContaining("mutation updateInboundRequestStatus(")
	if len(writes) != 1 {
		t.Fatalf("writes = %v, want exactly one", writes)
	}
	w := renderedArgs(writes[0])
	if w["status"] != "failed" || !clearsNextAttempt(w) || w["attempts"] != nil {
		t.Fatalf("wrote %v, want failed with no next time and the try count untouched", w)
	}
	if msg, _ := w["lastError"].(string); !strings.Contains(msg, "no connector") || !strings.Contains(msg, "shopify-removed-store") {
		t.Fatalf("lastError = %q, want it to say no connector serves the source", msg)
	}
	if res.Tried != 0 || c.applyCalls != 0 {
		t.Fatalf("an unserved row was tried: %+v, applyCalls=%d", res, c.applyCalls)
	}
}
