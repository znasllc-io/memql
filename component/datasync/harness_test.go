package datasync

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// harness_test.go -- a fake engine and a scriptable connector.
//
// The engine seam is `Execute(ctx, string) (any, error)` precisely so a
// test can drive the runtime without a database (the campaigns
// precedent). fakeEngine records every call and answers reads from a
// table the test seeds, so an assertion can be about WHAT THE RUNTIME
// DID -- which mutation, with which arguments, in which order -- rather
// than about a row that happens to be in a database afterwards.

// fakeEngine records calls and answers reads from seeded rows.
type fakeEngine struct {
	mu    sync.Mutex
	calls []string
	// rows answers a read whose rendered call STARTS WITH the key, so a
	// test seeds `query outboxPending` without restating its arguments.
	rows map[string][]map[string]any
	// failOn makes any call containing the substring return an error.
	failOn map[string]error
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{rows: map[string][]map[string]any{}, failOn: map[string]error{}}
}

func (f *fakeEngine) Execute(_ context.Context, q string) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, q)
	for needle, err := range f.failOn {
		if strings.Contains(q, needle) {
			return nil, err
		}
	}
	for prefix, rows := range f.rows {
		if strings.HasPrefix(q, prefix) {
			return rows, nil
		}
	}
	return []map[string]any{}, nil
}

// seed answers any call starting with prefix.
func (f *fakeEngine) seed(prefix string, rows []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[prefix] = rows
}

// callsContaining returns every recorded call carrying the substring.
func (f *fakeEngine) callsContaining(needle string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.Contains(c, needle) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeEngine) countContaining(needle string) int { return len(f.callsContaining(needle)) }

// fakeConnector is a scriptable Connector. Every method answers from a
// field the test sets, so a suite says what the origin does rather than
// mocking a vendor API.
type fakeConnector struct {
	name    string
	domains []memqlsync.DomainSpec

	applyWrites []memqlsync.MirrorWrite
	applyErr    error

	// propagate is consulted per call with the attempt number, so a test
	// can script "fail twice then succeed".
	propagateResults []error
	propagateCalls   []memqlsync.OutboxEntry
	propagateRetry   time.Duration

	backfillPages []memqlsync.BackfillPage
	backfillErr   error
	backfillSeen  []string

	reconcileReport memqlsync.ReconcileReport
	reconcileErr    error
	reconcileSince  []time.Time

	mu sync.Mutex
}

func (c *fakeConnector) Name() string                    { return c.name }
func (c *fakeConnector) Domains() []memqlsync.DomainSpec { return c.domains }
func (c *fakeConnector) EnsureSubscriptions(context.Context) error {
	return memqlsync.NotImplemented(c.name, "EnsureSubscriptions")
}

func (c *fakeConnector) Apply(context.Context, memqlsync.InboundRequest) ([]memqlsync.MirrorWrite, error) {
	if c.applyErr != nil {
		return nil, c.applyErr
	}
	return c.applyWrites, nil
}

func (c *fakeConnector) Backfill(_ context.Context, conceptName, cursor string) (memqlsync.BackfillPage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.backfillSeen = append(c.backfillSeen, cursor)
	if c.backfillErr != nil {
		return memqlsync.BackfillPage{}, c.backfillErr
	}
	if len(c.backfillPages) == 0 {
		return memqlsync.BackfillPage{Done: true}, nil
	}
	page := c.backfillPages[0]
	c.backfillPages = c.backfillPages[1:]
	return page, nil
}

func (c *fakeConnector) Reconcile(_ context.Context, _ string, since time.Time) (memqlsync.ReconcileReport, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reconcileSince = append(c.reconcileSince, since)
	return c.reconcileReport, c.reconcileErr
}

func (c *fakeConnector) Propagate(_ context.Context, e memqlsync.OutboxEntry) (memqlsync.PropagateResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.propagateCalls = append(c.propagateCalls, e)
	res := memqlsync.PropagateResult{RetryAfter: c.propagateRetry}
	if len(c.propagateResults) == 0 {
		return res, nil
	}
	err := c.propagateResults[0]
	c.propagateResults = c.propagateResults[1:]
	return res, err
}

func (c *fakeConnector) propagateCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.propagateCalls)
}

// fakeWriter records mirror writes and answers stored versions from a
// table, so the version guard is testable without a database.
type fakeWriter struct {
	mu       sync.Mutex
	written  []memqlsync.MirrorWrite
	versions map[string]string
	writeErr error
}

func newFakeWriter() *fakeWriter { return &fakeWriter{versions: map[string]string{}} }

func (w *fakeWriter) WriteMirror(_ context.Context, _ string, mw memqlsync.MirrorWrite) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeErr != nil {
		return w.writeErr
	}
	w.written = append(w.written, mw)
	w.versions[mw.Concept+"|"+mw.RowId] = mw.Version
	return nil
}

func (w *fakeWriter) StoredVersion(_ context.Context, spec memqlsync.DomainSpec, rowID string) (string, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	v, ok := w.versions[spec.Concept+"|"+rowID]
	return v, ok, nil
}

func (w *fakeWriter) writtenIDs() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.written))
	for _, mw := range w.written {
		out = append(out, mw.RowId)
	}
	return out
}

// alwaysClaim / neverClaim are the two cluster-claim outcomes.
type alwaysClaim struct{ calls int }

func (a *alwaysClaim) ClaimWithTTL(context.Context, string, string, time.Duration) bool {
	a.calls++
	return true
}

type neverClaim struct{ calls int }

func (n *neverClaim) ClaimWithTTL(context.Context, string, string, time.Duration) bool {
	n.calls++
	return false
}

// testWorker builds a drain worker over a fake engine and one connector,
// bypassing the process-global registry.
func testWorker(engine *fakeEngine, claimer ExecutionClaimer, c *fakeConnector, now time.Time) *Worker {
	w := NewWorker(engine, claimer, discardLogger())
	w.cfg.Enabled = true
	w.lookup = func(name string) (memqlsync.Connector, bool) {
		if name == c.name {
			return c, true
		}
		return nil, false
	}
	w.bound = func() []string { return []string{c.name} }
	w.now = func() time.Time { return now }
	return w
}

// outboxRow renders one seeded v1:platform:outboxEntry row.
func outboxRow(id, target string, attempts int, nextAttemptAt string) map[string]any {
	row := map[string]any{
		"id":             id,
		"conceptId":      "v1:wholesale:priceList",
		"rowRef":         "row-" + id,
		"action":         "upsert",
		"version":        "2026-08-23T00:00:00Z",
		"target":         target,
		"status":         "pending",
		"attempts":       attempts,
		"idempotencyKey": fmt.Sprintf("v1:wholesale:priceList|row-%s|v|%s", id, target),
	}
	if nextAttemptAt != "" {
		row["nextAttemptAt"] = nextAttemptAt
	}
	return row
}
