package shopify

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// connect_retry_test.go -- a connect that records a grant gives every refused
// domain a fresh try (ledger ruling 17).
//
// The connect asks the sync runtime (datasyncRetryRefused) about every domain
// of the connection whose scopes the recorded grant holds; the runtime decides
// on the row whether it stands refused, and clears the refusal by queueing its
// backfill or making it due. The runtime's side is proven in
// component/datasync (retry_refused_db_test.go and
// shopify_retry_refused_db_test.go); here, which domains a connect asks about,
// what the connector says of its refusals, and what the verdict and the watch
// make of a row the runtime retried.

// retried is the domains a connect asked the runtime to retry, in order.
func (h *writeHarness) retried(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, c := range h.engine.callsNamed("datasyncRetryRefused") {
		if !strings.HasPrefix(c.query, "builtin ") || !strings.Contains(c.query, `partitionKey: "`+connectStoreID+`"`) || !strings.Contains(c.query, `connector: "shopify"`) {
			t.Errorf("a retry does not name the builtin, the store and the connector: %s", c.query)
		}
		if c.role != auth.RoleOwner || !c.internal || c.userId == connectDev {
			t.Errorf("a retry ran as %+v, want operatorContext", c)
		}
		m := queuedConceptRE.FindStringSubmatch(c.query)
		if m == nil {
			t.Fatalf("a retry names no mirrored concept: %s", c.query)
		}
		out = append(out, m[1])
	}
	return out
}

// heldDomains is every domain, by name, the verdict reads rows of for a store
// whose scopes the store's grant holds: what a connect asks the runtime about.
func heldDomains(store Store) []string {
	var out []string
	for _, id := range verdictDomains(store) {
		concept := generated.ConceptFromID(id)
		if len(scopesMissingFor(store, generated.Types[concept])) == 0 {
			out = append(out, concept)
		}
	}
	return out
}

func TestAReApprovalAsksForEveryHeldDomainToBeTriedAgain(t *testing.T) {
	h := reconnectHarness(t, managedRow(map[string]any{"scopesGranted": olderManagedScopes()}))
	h.managedConnect(t, managedConnectScopes())
	approved := managedStore(managedConnectScopes()...)
	got := h.retried(t)
	if want := heldDomains(approved); !slices.Equal(got, want) || len(want) == 0 {
		t.Fatalf("retried %v,\nwant every domain the new grant holds %v", got, want)
	}
	// Segments (no backfill) and customers (a backfill) are both asked.
	for _, want := range []string{"segment", "customer", "order"} {
		if !slices.Contains(got, want) {
			t.Errorf("retried %v; %s is held by the new grant", got, want)
		}
	}
	// A domain outside the connection is not asked.
	if slices.Contains(got, "blog") {
		t.Errorf("retried %v; blog is outside the connection", got)
	}
	// After the queue of what the old grant lacked, never before the row.
	if first, created := h.statementIndex("datasyncRetryRefused"), h.statementIndex("updateStore"); first < created {
		t.Errorf("a retry (%d) was asked before the store row was written (%d)", first, created)
	}
}

func TestADomainTheNewGrantStillLacksIsNotTriedAgain(t *testing.T) {
	h := reconnectHarness(t, managedRow(map[string]any{"scopesGranted": olderManagedScopes()}))
	lacking := without(managedConnectScopes(), "read_discounts", "read_returns")
	h.managedConnect(t, lacking)
	got := h.retried(t)
	if slices.Contains(got, "discountNode") {
		t.Errorf("retried %v; the new grant still lacks read_discounts", got)
	}
	if want := heldDomains(managedStore(lacking...)); !slices.Equal(got, want) {
		t.Errorf("retried %v, want %v", got, want)
	}
	for _, q := range h.engine.callsNamed("datasyncQueueBackfill") {
		if strings.Contains(q.query, "v1:shopify:discountNode") {
			t.Errorf("discounts were queued under a grant that lacks them: %s", q.query)
		}
	}
}

func TestAConnectThatFailsBeforeItRecordsAGrantRetriesNothing(t *testing.T) {
	h := newWriteHarness(t)
	h.engine.fail["createStore"] = errors.New("the database went away")
	h.integ.connector.background = func(func()) {}
	grant := h.grant()
	grant.Scopes, grant.OfflineGrant = managedConnectScopes(), `{"accessToken":"`+callbackAdminToken+`"}`
	if got, _ := h.integ.connector.writeConnect(t.Context(), callbackState(managedCredentialSource), grant); got != connectReasonExchangeFailed {
		t.Fatalf("result = %q", got)
	}
	if n := len(h.engine.callsNamed("datasyncRetryRefused")); n != 0 {
		t.Errorf("a connect whose grant was never recorded asked %d retries", n)
	}
}

func TestTheConnectorSaysWhichRefusalsAGrantAnswers(t *testing.T) {
	c := &Connector{}
	scopeRefusal := memqlsync.NotGranted(ConnectorName, "read_customers").Error()
	originRefusal := memqlsync.ErrNotGranted.Error() + ": shopify admin: Access denied. Required access: `read_customers` access scope"
	for text, want := range map[string]bool{
		scopeRefusal:                            true,
		originRefusal:                           true,
		"backfill refused: " + scopeRefusal:     true,
		protectedRefusal:                        false,
		"backfill refused: " + protectedRefusal: false,
	} {
		if got := c.RetryableRefusal(generated.ConceptID("customer"), text); got != want {
			t.Errorf("RetryableRefusal(%q) = %v, want %v", text, got, want)
		}
	}
	for concept, want := range map[string]bool{"order": true, "customer": true, "segment": false, "shop": false, "nothing": false} {
		if got := c.BackfillsDomain(generated.ConceptID(concept)); got != want {
			t.Errorf("BackfillsDomain(%s) = %v, want %v", concept, got, want)
		}
	}
}

// retriedRow is the row the runtime leaves for a domain it made due: no
// refusal, no attempt, its last finished sweep kept.
func retriedRow(r map[string]any) {
	r["lastError"] = ""
	r["lastAttemptAt"] = ""
}

func TestAReApprovedStoreReadsClearOnceItsRefusedDomainIsRetried(t *testing.T) {
	approved := verdictStore()
	// Before: the segment row stands refused as not granted under the older
	// list, and the store needs attention.
	refused := refusedAtTheOrigin(cleanRows(approved), "segment", "read_customers")
	v := computeVerdict(verdictInputOf(approved, refused))
	if !slices.Equal(reasonCodes(v), []string{ReasonApprovalOutdated}) {
		t.Fatalf("before the retry: reasons %v; the test proves nothing", reasonCodes(v))
	}
	// Straight after the connect: the runtime cleared the refusal and made the
	// domain due. No approval reason.
	retried := setRow(cleanRows(approved), "segment", retriedRow)
	v = computeVerdict(verdictInputOf(approved, retried))
	if slices.Contains(reasonCodes(v), ReasonApprovalOutdated) {
		t.Errorf("straight after the connect: reasons %v; want no approval reason", reasonCodes(v))
	}
	// The origin refuses the fresh try: the reason is back, naming segments.
	again := refusedAtTheOrigin(retried, "segment", "read_customers")
	r := reasonNamed(t, computeVerdict(verdictInputOf(approved, again)), ReasonApprovalOutdated)
	if r.Detail != "Shopify refuses customer segments under the store's grant." {
		t.Errorf("refused again: detail %q", r.Detail)
	}
}

func TestTheRecoveryIsMailedAtThePassThatFindsTheStoreClear(t *testing.T) {
	approved := verdictStore()
	since := watchNow.Add(-3 * time.Hour)
	// The episode the older approval opened, mailed once.
	troubled := computeVerdict(verdictInputOf(approved, refusedAtTheOrigin(cleanRows(approved), "segment", "read_customers")))
	troubled.Since = since
	previous := recordedAs(troubled, mailedFor(since))
	if plan := planWatch(previous, troubled, defaultStoreAlertWait, watchNow); plan.Alert {
		t.Fatalf("the mailed episode was alerted again: %+v", plan)
	}
	// The pass after the connect: the retried row reads clear, the store
	// records, and the recovery goes out at this pass.
	in := verdictInputOf(approved, setRow(cleanRows(approved), "segment", retriedRow))
	in.Now = watchNow
	clear := computeVerdict(in)
	if clear.State != VerdictRecording {
		t.Fatalf("after the retry: state %q, reasons %v", clear.State, reasonCodes(clear))
	}
	plan := planWatch(previous, clear, defaultStoreAlertWait, watchNow)
	if plan.Alert || plan.Recover != verdictStamp(since) {
		t.Errorf("the pass that finds the store clear: %+v; want the recovery of %s", plan, verdictStamp(since))
	}
}
