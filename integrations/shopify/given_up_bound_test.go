package shopify

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// given_up_bound_test.go -- the given-up walk is bounded for a store whose
// grant does not hold every domain its connection asks for.
//
// The walk over a store's given-up deliveries stops at the oldest clean sweep
// among the domains a delivery can count toward. A domain the grant holds no
// scope of is never swept clean, so it left the walk with no bound at all:
// every managed store between a release that widens the permission list and
// its re-approval read its whole history of given-up deliveries, to the page
// budget, on every verdict. Such a domain receives no deliveries either -- no
// topic is registered for it -- so it is left out of the bound.

// approvedBefore is the managed store as it approved the list before the
// customer scopes were added to it.
func approvedBefore() Store {
	var older []string
	for _, scope := range commerceSet {
		if scope != "read_customers" && scope != "write_customers" {
			older = append(older, scope)
		}
	}
	store := verdictStore()
	store.ScopesGranted = older
	return store
}

// refusedRows is cleanRows with the domains the grant does not hold as the
// sweep leaves them: a standing not-granted answer and no clean sweep.
func refusedRows(t *testing.T, store Store) ([]map[string]any, int) {
	t.Helper()
	rows := cleanRows(store)
	notHeld := 0
	for _, concept := range askedOfStore(store) {
		spec := generated.Types[generated.ConceptFromID(concept)]
		if len(scopesMissingFor(store, spec)) == 0 {
			continue
		}
		notHeld++
		rows = setRow(rows, spec.Concept, func(r map[string]any) {
			delete(r, "lastReconcileAt")
			r["lastError"] = "connector: not granted: shopify needs read_customers"
		})
	}
	if notHeld == 0 {
		t.Fatal("the store holds every domain its connection asks for, so this test proves nothing")
	}
	return rows, notHeld
}

func TestADomainTheGrantDoesNotHoldDoesNotUnboundTheGivenUpWalk(t *testing.T) {
	store := approvedBefore()
	rows, _ := refusedRows(t, store)
	swept := verdictNow.Add(-12 * time.Minute)
	if got := givenUpBound(store, rows); !got.Equal(swept) {
		t.Fatalf("bound = %v, want the oldest clean sweep among the domains the grant holds, %v", got, swept)
	}
	// A domain the grant DOES hold and that was never swept clean still
	// leaves the walk unbounded: every delivery routed to it counts.
	rows = setRow(rows, "order", func(r map[string]any) { delete(r, "lastReconcileAt") })
	if got := givenUpBound(store, rows); !got.IsZero() {
		t.Fatalf("bound = %v with a held domain never swept clean; want none", got)
	}
}

// Through the verdict: the walk stops at the first page older than the
// bound, where it used to read every page it was given.
func TestTheVerdictOfAStoreAwaitingReapprovalDoesNotWalkItsWholeHistory(t *testing.T) {
	h := newHarness(t)
	store := approvedBefore()
	rows, _ := refusedRows(t, store)
	scopes := make([]any, 0, len(store.ScopesGranted))
	for _, s := range store.ScopesGranted {
		scopes = append(scopes, s)
	}
	h.engine.setRows("stores", []map[string]any{{
		"id": testStoreID, "domain": store.Domain, "adminTokenRef": store.AdminTokenRef, "status": StatusLive,
		"ownerUserId": "user-1", "scopesGranted": scopes, "health": store.Health,
	}})
	h.now = verdictNow
	// Forty pages of given-up deliveries, each a day older than the last: all
	// of them from before every clean sweep.
	engine := &givenUpPages{fakeEngine: h.engine, pages: pagesOf(40, func(i int) time.Time {
		return verdictNow.Add(-time.Duration(i+1) * 24 * time.Hour)
	})}
	states := &statesByID{givenUpPages: engine, rows: map[string]map[string]any{}}
	for _, row := range rows {
		states.rows[stateIDOf(row)] = row
	}
	h.conn.engine = states
	h.conn.stores.engine = states

	v, err := h.conn.storeVerdict(context.Background(), h.store(t))
	if err != nil {
		t.Fatal(err)
	}
	if engine.read != 1 {
		t.Fatalf("the verdict read %d pages of given-up deliveries, want the walk to stop at the first: every page is older than every clean sweep", engine.read)
	}
	for _, reason := range v.Reasons {
		if reason.Code == ReasonChangesNotRecorded {
			t.Errorf("deliveries older than every clean sweep were counted: %+v", reason)
		}
	}
}

// statesByID is the given-up pages engine that also answers each domain's
// sync state by its id.
type statesByID struct {
	*givenUpPages
	rows map[string]map[string]any
}

func (e *statesByID) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if callName(q) != "syncStateById" {
		return e.givenUpPages.Execute(ctx, q)
	}
	m := stateIDArgRE.FindStringSubmatch(q)
	if m == nil || e.rows[m[1]] == nil {
		return resultWithRows(nil), nil
	}
	return resultWithRows([]map[string]any{e.rows[m[1]]}), nil
}

// stateIDOf is the id a domain's sync-state row is read by.
func stateIDOf(row map[string]any) string {
	concept, _ := row["conceptId"].(string)
	partition, _ := row["partitionKey"].(string)
	return memqlsync.StateID(concept, ConnectorName, string(memqlsync.DirectionInbound), partition)
}
