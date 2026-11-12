package datasync

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// retry_refused_db_test.go -- datasyncRetryRefused through a real engine.
//
// A connect that records a wider grant asks the runtime to try again every
// domain whose row stands refused as not granted. The runtime decides on the
// row whether it stands refused; the connector says only whether its own
// refusal wording is one a grant can answer and whether the domain is read by
// a backfill (RefusalRetrier). db-gated, like syncstate_write_db_test.go.

const retryBulkConcept = "v1:shopify:order"

// retrierConnector is the fake connector with a word on its refusals: the
// domains it backfills, and a refusal text no grant answers.
type retrierConnector struct {
	*fakeConnector
	bulk         map[string]bool
	notRetryable string
}

func (c retrierConnector) RetryableRefusal(_, lastError string) bool {
	return c.notRetryable == "" || !strings.Contains(lastError, c.notRetryable)
}

func (c retrierConnector) BackfillsDomain(conceptID string) bool { return c.bulk[conceptID] }

// retryRig is a runner over a real engine, serving one fake connector of its
// own run.
type retryRig struct {
	t         *testing.T
	store     *Store
	runner    *Runner
	integ     *Integration
	connector retrierConnector
	now       time.Time
}

func newRetryRig(t *testing.T) *retryRig {
	t.Helper()
	eng, db := healthEngine(t)
	if eng == nil {
		return nil
	}
	name := fmt.Sprintf("dbtest-retry-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND payload->>'connector' = $1`, name)
	})
	fake := &fakeConnector{name: name, domains: []memqlsync.DomainSpec{
		{Concept: testMirrorConcept, Direction: memqlsync.DirectionInbound, ReconcileInterval: 6 * time.Hour},
		{Concept: retryBulkConcept, Direction: memqlsync.DirectionInbound, ReconcileInterval: time.Hour},
	}}
	c := retrierConnector{fakeConnector: fake, bulk: map[string]bool{retryBulkConcept: true}, notRetryable: "protected-customer-data"}
	store := NewStore(engineSeam{eng})
	r := NewRunner(store, NewApplier(store, newFakeWriter()), discardLogger())
	r.lookup = func(n string) (memqlsync.Connector, bool) {
		if n == name {
			return c, true
		}
		return nil, false
	}
	r.bound = func() []string { return []string{name} }
	now := time.Now().UTC().Truncate(time.Second)
	r.now = func() time.Time { return now }
	return &retryRig{t: t, store: store, runner: r, integ: &Integration{store: store, runner: r, logger: discardLogger()}, connector: c, now: now}
}

// write puts one row down whole.
func (g *retryRig) write(concept string, st SyncState) SyncState {
	g.t.Helper()
	st.ConceptID, st.Connector, st.Direction = concept, g.connector.Name(), "inbound"
	if st.BackfillStatus == "" {
		st.BackfillStatus = "none"
	}
	if err := g.store.WriteSyncState(OperatorContext(context.Background()), st); err != nil {
		g.t.Fatal(err)
	}
	return g.read(concept)
}

func (g *retryRig) read(concept string) SyncState {
	g.t.Helper()
	st, err := g.store.SyncStateFor(OperatorContext(context.Background()), concept, g.connector.Name(), "inbound", "")
	if err != nil {
		g.t.Fatal(err)
	}
	return st
}

// retry calls the builtin as the engine does, and answers its outcome and
// detail.
func (g *retryRig) retry(concept string) (string, string) {
	g.t.Helper()
	nodes, err := g.integ.handleRetryRefused(auth.ContextWithInternalOrigin(context.Background()),
		map[string]any{"connector": g.connector.Name(), "conceptId": concept}, 0)
	if err != nil {
		g.t.Fatalf("retryRefused(%s): %v", concept, err)
	}
	var out map[string]string
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		g.t.Fatal(err)
	}
	return out["outcome"], out["detail"]
}

func (g *retryRig) due(concept string) bool {
	for _, spec := range g.connector.Domains() {
		if spec.Concept == concept {
			return g.runner.ReconcileDue(context.Background(), g.connector.Name(), spec, "")
		}
	}
	g.t.Fatalf("no domain %s", concept)
	return false
}

// refusedRow is a non-bulk domain swept once long ago and refused since.
func (g *retryRig) refusedRow(lastError string) SyncState {
	return SyncState{
		LastReconcileAt: g.now.Add(-7 * time.Hour),
		LastAttemptAt:   g.now.Add(-time.Hour),
		LastError:       lastError,
		BackfillStatus:  "complete",
		DriftCount:      2,
	}
}

var notGrantedText = memqlsync.NotGranted("shopify", "read_products").Error()

func TestRetryRefusedRefusesACallWithoutInternalOrigin(t *testing.T) {
	g := newRetryRig(t)
	if g == nil {
		return
	}
	before := g.write(testMirrorConcept, g.refusedRow(notGrantedText))
	_, err := g.integ.handleRetryRefused(context.Background(), map[string]any{"connector": g.connector.Name(), "conceptId": testMirrorConcept}, 0)
	if err == nil || !strings.Contains(err.Error(), "refuses a client-origin call") {
		t.Fatalf("a client call answered %v", err)
	}
	if after := g.read(testMirrorConcept); after != before {
		t.Errorf("a refused call wrote the row:\n  before %+v\n  after  %+v", before, after)
	}
}

func TestRetryRefusedLeavesEveryOtherRowAsItWas(t *testing.T) {
	g := newRetryRig(t)
	if g == nil {
		return
	}
	for name, lastError := range map[string]string{
		"not refused":           "",
		"not requested":         memqlsync.NotRequested("shopify").Error(),
		"undeclared data":       memqlsync.ErrNotGranted.Error() + ": shopify: Order object is not approved to access protected-customer-data",
		"another error":         "shopify admin: HTTP 503",
		"a backfill's failure":  backfillFailedPrefix + "shopify: bulk operation FAILED",
		"a refused not-request": backfillRefusedPrefix + memqlsync.NotRequested("shopify").Error(),
	} {
		before := g.write(testMirrorConcept, g.refusedRow(lastError))
		outcome, _ := g.retry(testMirrorConcept)
		if outcome != "unchanged" {
			t.Errorf("%s: outcome %q, want unchanged", name, outcome)
		}
		if after := g.read(testMirrorConcept); after != before {
			t.Errorf("%s: the row was written:\n  before %+v\n  after  %+v", name, before, after)
		}
	}
	// A domain with no row at all is left without one.
	if outcome, _ := g.retry(retryBulkConcept); outcome != "unchanged" || g.read(retryBulkConcept).Stored {
		t.Errorf("a domain with no row: outcome %q, row written %v", outcome, g.read(retryBulkConcept).Stored)
	}
}

func TestRetryRefusedMakesARefusedDomainDue(t *testing.T) {
	g := newRetryRig(t)
	if g == nil {
		return
	}
	before := g.write(testMirrorConcept, g.refusedRow(notGrantedText))
	if g.due(testMirrorConcept) {
		t.Fatal("the refused domain is due before the retry; the test proves nothing")
	}
	outcome, detail := g.retry(testMirrorConcept)
	if outcome != "retried" || detail != "due" {
		t.Errorf("answer %q %q, want retried due", outcome, detail)
	}
	after := g.read(testMirrorConcept)
	if after.LastError != "" || !after.LastAttemptAt.IsZero() {
		t.Errorf("after the retry: error %q, attempt %v; want no refusal and no attempt", after.LastError, after.LastAttemptAt)
	}
	if !after.LastReconcileAt.Equal(before.LastReconcileAt) || after.DriftCount != before.DriftCount ||
		after.BackfillStatus != before.BackfillStatus || after.Paused != before.Paused {
		t.Errorf("the retry wrote more than the refusal and the attempt:\n  before %+v\n  after  %+v", before, after)
	}
	if !g.due(testMirrorConcept) {
		t.Fatal("the retried domain is not due at the next pass")
	}
	// The next pass sweeps it.
	if _, err := g.runner.Reconcile(context.Background(), g.connector.Name(), testMirrorConcept, ""); err != nil {
		t.Fatal(err)
	}
	if n := len(g.connector.reconcileSince); n != 1 {
		t.Fatalf("the sweep asked the origin %d times", n)
	}
	if swept := g.read(testMirrorConcept); swept.LastAttemptAt.IsZero() || swept.LastError != "" {
		t.Errorf("after the sweep: %+v", swept)
	}
}

func TestRetryRefusedQueuesARefusedBackfill(t *testing.T) {
	g := newRetryRig(t)
	if g == nil {
		return
	}
	for name, lastError := range map[string]string{
		"refused while the sweep reads it": backfillRefusedPrefix + notGrantedText,
		"refused before any sweep":         notGrantedText,
	} {
		g.write(retryBulkConcept, SyncState{BackfillStatus: "failed", BackfillCursor: "c-9", LastError: lastError, LastReconcileAt: g.now.Add(-2 * time.Hour)})
		outcome, detail := g.retry(retryBulkConcept)
		if outcome != "retried" || detail != "queued" {
			t.Errorf("%s: answer %q %q, want retried queued", name, outcome, detail)
		}
		after := g.read(retryBulkConcept)
		if after.BackfillStatus != "queued" || after.BackfillCursor != "" || after.LastError != "" {
			t.Errorf("%s: after the retry %+v; want queued from the start, no refusal", name, after)
		}
	}
}

func TestQueueBackfillClearsARefusalInEitherSpelling(t *testing.T) {
	g := newRetryRig(t)
	if g == nil {
		return
	}
	for _, lastError := range []string{notGrantedText, backfillRefusedPrefix + notGrantedText} {
		g.write(retryBulkConcept, SyncState{BackfillStatus: "failed", LastError: lastError})
		if err := g.runner.QueueBackfill(context.Background(), g.connector.Name(), retryBulkConcept, ""); err != nil {
			t.Fatal(err)
		}
		if after := g.read(retryBulkConcept); after.BackfillStatus != "queued" || after.LastError != "" {
			t.Errorf("%q queued: %+v; want queued, no refusal", lastError, after)
		}
	}
	// A backfill's own failure that is not a refusal is not the queue's to
	// clear.
	g.write(retryBulkConcept, SyncState{BackfillStatus: "failed", LastError: backfillFailedPrefix + "boom"})
	if err := g.runner.QueueBackfill(context.Background(), g.connector.Name(), retryBulkConcept, ""); err != nil {
		t.Fatal(err)
	}
	if after := g.read(retryBulkConcept); after.LastError != backfillFailedPrefix+"boom" {
		t.Errorf("a failure was cleared by a queue: %+v", after)
	}
}

func TestRetryRefusedOnAPausedDomain(t *testing.T) {
	g := newRetryRig(t)
	if g == nil {
		return
	}
	row := g.refusedRow(notGrantedText)
	row.Paused = true
	g.write(testMirrorConcept, row)
	if outcome, detail := g.retry(testMirrorConcept); outcome != "retried" || detail != "due" {
		t.Errorf("answer %q %q", outcome, detail)
	}
	after := g.read(testMirrorConcept)
	if !after.Paused || after.LastError != "" || !after.LastAttemptAt.IsZero() {
		t.Errorf("a paused domain after the retry: %+v; want still paused, no refusal, no attempt", after)
	}
	if g.due(testMirrorConcept) {
		t.Error("a paused domain is due: the pass must keep skipping it")
	}
}

func TestRetryRefusedAndASweepAtOnceEndInOneOutcome(t *testing.T) {
	g := newRetryRig(t)
	if g == nil {
		return
	}
	// The sweep began under the old grant and answers a refusal worded
	// otherwise than the row's, so it writes it.
	sweepRefusal := memqlsync.NotGranted("shopify", "read_products", "write_products").Error()
	g.connector.reconcileErr = memqlsync.NotGranted("shopify", "read_products", "write_products")
	refusedAgain := func(st SyncState) bool { return st.LastError == sweepRefusal && st.LastAttemptAt.Equal(g.now) }
	cleared := func(st SyncState) bool { return st.LastError == "" && st.LastAttemptAt.IsZero() }

	// The retry lands while the sweep is asking the origin: the sweep's
	// refusal is written after it, and wins.
	g.write(testMirrorConcept, g.refusedRow(notGrantedText))
	g.connector.duringReconcile = func() { g.retry(testMirrorConcept) }
	_, _ = g.runner.Reconcile(context.Background(), g.connector.Name(), testMirrorConcept, "")
	if st := g.read(testMirrorConcept); !refusedAgain(st) {
		t.Errorf("a retry during the sweep: %+v; want the sweep's refusal", st)
	}
	// The retry after the sweep: cleared.
	g.connector.duringReconcile = nil
	g.write(testMirrorConcept, g.refusedRow(notGrantedText))
	_, _ = g.runner.Reconcile(context.Background(), g.connector.Name(), testMirrorConcept, "")
	g.retry(testMirrorConcept)
	if st := g.read(testMirrorConcept); !cleared(st) {
		t.Errorf("a retry after the sweep: %+v; want cleared", st)
	}
	// At once, many times: always one of the two, never a mix.
	for i := 0; i < 5; i++ {
		g.write(testMirrorConcept, g.refusedRow(notGrantedText))
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = g.runner.Reconcile(context.Background(), g.connector.Name(), testMirrorConcept, "")
		}()
		go func() {
			defer wg.Done()
			_, _ = g.integ.handleRetryRefused(auth.ContextWithInternalOrigin(context.Background()),
				map[string]any{"connector": g.connector.Name(), "conceptId": testMirrorConcept}, 0)
		}()
		wg.Wait()
		if st := g.read(testMirrorConcept); !refusedAgain(st) && !cleared(st) {
			t.Errorf("run %d: %+v is neither the sweep's refusal nor the retry's clearing", i, st)
		}
	}
}

func TestRetryRefusedNamesOneDomainOfOnePartition(t *testing.T) {
	g := newRetryRig(t)
	if g == nil {
		return
	}
	internal := auth.ContextWithInternalOrigin(context.Background())
	for name, args := range map[string]map[string]any{
		"no connector":           {"conceptId": testMirrorConcept},
		"no concept":             {"connector": g.connector.Name()},
		"a partition on no part": {"connector": g.connector.Name(), "conceptId": testMirrorConcept, "partitionKey": "a"},
		"an undeclared concept":  {"connector": g.connector.Name(), "conceptId": "v1:shopify:nothing"},
	} {
		if _, err := g.integ.handleRetryRefused(internal, args, 0); err == nil {
			t.Errorf("%s: answered without an error", name)
		}
	}
}
