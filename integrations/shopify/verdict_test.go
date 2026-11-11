package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// verdict_test.go -- one word for each store, its reasons, and who may act
// on them.
//
// computeVerdict is a pure function of what was read, so most of these build
// its input by hand and call it with no engine at all. The handlers' tests at
// the end drive the three builtins through the harness.

var verdictNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// verdictStore is a store connected through the shared app, holding the
// commerce set, whose last subscription pass left everything it wanted.
func verdictStore() Store {
	s := managedStore(commerceSet...)
	s.Domain = "acme-widgets.myshopify.com"
	s.OwnerUserID = "user-1"
	s.Health = map[string]any{
		"ordersFrom": "2026-09-12",
		"subscriptions": map[string]any{
			"desired": 12, "existing": 12,
			"created": []any{}, "updated": []any{}, "removed": []any{}, "failed": []any{},
			"notGranted": []any{},
		},
	}
	return s
}

// askedOfStore is every domain the verdict asks about for this store: the
// swept domains inside its connection.
func askedOfStore(store Store) []string {
	return sweptDomains(func(spec *generated.TypeSpec) bool { return domainInConnection(store, spec) })
}

// cleanRows is a sync-state row for every asked domain, each swept clean ten
// minutes ago and backfilled.
func cleanRows(store Store) []map[string]any {
	var rows []map[string]any
	for _, concept := range askedOfStore(store) {
		rows = append(rows, map[string]any{
			"conceptId":       concept,
			"partitionKey":    store.ID,
			"backfillStatus":  "complete",
			"lastReconcileAt": verdictNow.Add(-12 * time.Minute).Format(time.RFC3339),
			"lastAttemptAt":   verdictNow.Add(-9 * time.Minute).Format(time.RFC3339),
			"lastInboundAt":   verdictNow.Add(-30 * time.Minute).Format(time.RFC3339),
		})
	}
	return rows
}

// setRow edits the row of one concept, adding it when there is none.
func setRow(rows []map[string]any, concept string, edit func(map[string]any)) []map[string]any {
	id := generated.ConceptID(concept)
	for _, r := range rows {
		if r["conceptId"] == id {
			edit(r)
			return rows
		}
	}
	r := map[string]any{"conceptId": id, "partitionKey": testStoreID}
	edit(r)
	return append(rows, r)
}

func dropRow(rows []map[string]any, concept string) []map[string]any {
	id := generated.ConceptID(concept)
	out := rows[:0:0]
	for _, r := range rows {
		if r["conceptId"] != id {
			out = append(out, r)
		}
	}
	return out
}

func verdictInputOf(store Store, rows []map[string]any) verdictInput {
	return verdictInput{
		Store:     store,
		States:    rows,
		Intervals: sweepIntervals((&Connector{}).Domains()),
		Now:       verdictNow,
	}
}

func reasonCodes(v Verdict) []string {
	out := []string{}
	for _, r := range v.Reasons {
		out = append(out, r.Code)
	}
	return out
}

func reasonNamed(t *testing.T, v Verdict, code string) VerdictReason {
	t.Helper()
	for _, r := range v.Reasons {
		if r.Code == code {
			return r
		}
	}
	t.Fatalf("no %s reason; reasons = %v", code, reasonCodes(v))
	return VerdictReason{}
}

func TestAStoreSweptCleanIsRecording(t *testing.T) {
	store := verdictStore()
	v := computeVerdict(verdictInputOf(store, cleanRows(store)))
	if v.State != VerdictRecording || len(v.Reasons) != 0 {
		t.Fatalf("state = %q, reasons = %v; a store swept clean is recording", v.State, reasonCodes(v))
	}
	if v.StoreID != testStoreID || v.Domain != "acme-widgets.myshopify.com" || v.OrdersFrom != "2026-09-12" {
		t.Errorf("verdict = %+v; it names the store, its domain and from when its orders are recorded", v)
	}
	kinds := map[string]RecordedKind{}
	for _, k := range v.Recorded {
		kinds[k.Kind] = k
	}
	for _, kind := range []string{"catalog", "orders", "customers", "discounts", "returns"} {
		k, ok := kinds[kind]
		if !ok {
			t.Errorf("recorded kinds = %+v; %s is in the connection", v.Recorded, kind)
			continue
		}
		if k.LastCheckedAt != verdictNow.Add(-9*time.Minute).Format(time.RFC3339) {
			t.Errorf("%s last checked %q; it is when the sweep last ran (lastAttemptAt)", kind, k.LastCheckedAt)
		}
		if k.LastChangeAt != verdictNow.Add(-30*time.Minute).Format(time.RFC3339) {
			t.Errorf("%s last change %q; it is when the last delivery was applied", kind, k.LastChangeAt)
		}
	}
	// Returns are shown only while the grant holds read_returns.
	without := managedStore(oldManagedList...)
	for _, k := range computeVerdict(verdictInputOf(without, cleanRows(without))).Recorded {
		if k.Kind == "returns" {
			t.Error("returns are listed for a grant without read_returns")
		}
	}
	// Last checked falls back to the watermark on a row with no attempt stamp.
	rows := setRow(cleanRows(store), "order", func(r map[string]any) { delete(r, "lastAttemptAt") })
	for _, k := range computeVerdict(verdictInputOf(store, rows)).Recorded {
		if k.Kind == "orders" && k.LastCheckedAt != verdictNow.Add(-12*time.Minute).Format(time.RFC3339) {
			t.Errorf("orders last checked %q; with no attempt stamp it is lastReconcileAt", k.LastCheckedAt)
		}
	}
}

func TestABackfillQueuedOrRunningIsStarting(t *testing.T) {
	store := verdictStore()
	for _, status := range []string{"queued", "running"} {
		rows := setRow(cleanRows(store), "order", func(r map[string]any) { r["backfillStatus"] = status })
		if v := computeVerdict(verdictInputOf(store, rows)); v.State != VerdictStarting || len(v.Reasons) != 0 {
			t.Errorf("backfill %s: state = %q, reasons = %v; want starting", status, v.State, reasonCodes(v))
		}
	}
	// A swept domain of the connection never attempted: no row, or a row
	// with neither time.
	rows := dropRow(cleanRows(store), "customer")
	if v := computeVerdict(verdictInputOf(store, rows)); v.State != VerdictStarting {
		t.Errorf("a domain with no row: state = %q; want starting", v.State)
	}
	rows = setRow(cleanRows(store), "customer", func(r map[string]any) {
		delete(r, "lastAttemptAt")
		delete(r, "lastReconcileAt")
	})
	if v := computeVerdict(verdictInputOf(store, rows)); v.State != VerdictStarting {
		t.Errorf("a domain never swept: state = %q; want starting", v.State)
	}
}

// doesNotIngest is the error the runtime writes for a store that stopped
// ingesting between being listed and being swept (ingestingStore).
const doesNotIngest = `shopify: store "acme" does not ingest, so it is not a partition of the mirror`

func TestAPausedStoreIsPausedAndCarriesNoReasons(t *testing.T) {
	store := managedStore(oldManagedList...)
	store.Status = StatusPaused
	store.Health = map[string]any{"subscriptions": map[string]any{"desired": 5, "existing": 1, "failed": []any{"ORDERS_CREATE: boom"}}}
	rows := setRow(cleanRows(store), "order", func(r map[string]any) {
		r["lastError"] = doesNotIngest
		r["lastReconcileAt"] = verdictNow.Add(-72 * time.Hour).Format(time.RFC3339)
	})
	in := verdictInputOf(store, rows)
	in.GivenUp = []map[string]any{givenUp("orders/updated", verdictNow.Add(-time.Minute))}
	v := computeVerdict(in)
	if v.State != VerdictPaused || len(v.Reasons) != 0 {
		t.Fatalf("state = %q, reasons = %v; a paused store is paused and carries no reasons", v.State, reasonCodes(v))
	}
}

func TestAnUninstalledOrPurgedStoreIsNotConnected(t *testing.T) {
	for name, mark := range map[string]func(*Store){
		"uninstalled": func(s *Store) { s.UninstalledAt = verdictNow.Add(-time.Hour).Format(time.RFC3339) },
		"purged": func(s *Store) {
			s.RedactedAt = verdictNow.Add(-time.Hour).Format(time.RFC3339)
			s.Status = StatusPaused
		},
	} {
		store := verdictStore()
		mark(&store)
		rows := setRow(cleanRows(store), "order", func(r map[string]any) {
			r["lastError"] = doesNotIngest
			r["lastReconcileAt"] = verdictNow.Add(-72 * time.Hour).Format(time.RFC3339)
		})
		v := computeVerdict(verdictInputOf(store, rows))
		if v.State != VerdictNotConnected || len(v.Reasons) != 0 {
			t.Errorf("%s: state = %q, reasons = %v; want not connected with no reasons", name, v.State, reasonCodes(v))
		}
	}
}

const protectedRefusal = "connector: not granted: shopify: Order object is not approved to access protected-customer-data"

func TestAProtectedDataRefusalIsDataNotDeclared(t *testing.T) {
	store := verdictStore()
	rows := setRow(cleanRows(store), "order", func(r map[string]any) {
		r["lastError"] = protectedRefusal
		r["lastReconcileAt"] = ""
		r["backfillStatus"] = "none"
	})
	v := computeVerdict(verdictInputOf(store, rows))
	if v.State != VerdictNeedsAttention {
		t.Fatalf("state = %q; a refused domain needs attention", v.State)
	}
	if codes := reasonCodes(v); len(codes) != 1 {
		t.Fatalf("reasons = %v; the refusal is data_not_declared alone, never also a failing check", codes)
	}
	r := reasonNamed(t, v, ReasonDataNotDeclared)
	if r.Sentence != "Shopify refuses orders for this app, because the app has not declared the customer data it uses." {
		t.Errorf("sentence = %q", r.Sentence)
	}
	if r.Act != ActDeclare || r.ActURL != "https://shopify.dev/docs/apps/launch/protected-customer-data" {
		t.Errorf("act = %q %q", r.Act, r.ActURL)
	}
	// One reason names every such domain, in apply order.
	rows = setRow(rows, "customer", func(r map[string]any) { r["lastError"] = protectedRefusal })
	v = computeVerdict(verdictInputOf(store, rows))
	if len(v.Reasons) != 1 || v.Reasons[0].Sentence != "Shopify refuses customers and orders for this app, because the app has not declared the customer data it uses." {
		t.Errorf("reasons = %+v; one reason naming both domains", v.Reasons)
	}
}

func TestAnOlderGrantIsApprovalOutdatedAndNamesWhatItLacks(t *testing.T) {
	store := managedStore(oldManagedList...)
	store.Domain = "acme-widgets.myshopify.com"
	rows := cleanRows(store)
	// The domains the older grant cannot read are refused as not granted:
	// that is the same fact, and never a failing check.
	for _, concept := range []string{"customer", "discountNode"} {
		rows = setRow(rows, concept, func(r map[string]any) {
			r["lastError"] = memqlsync.NotGranted(ConnectorName, "read_customers").Error()
			r["lastReconcileAt"] = ""
		})
	}
	v := computeVerdict(verdictInputOf(store, rows))
	if codes := reasonCodes(v); len(codes) != 1 || codes[0] != ReasonApprovalOutdated {
		t.Fatalf("reasons = %v; want approval_outdated alone", codes)
	}
	r := v.Reasons[0]
	if r.Sentence != "This store approved an older list of permissions." {
		t.Errorf("sentence = %q", r.Sentence)
	}
	for _, scope := range []string{"read_customers", "write_customers", "read_discounts", "read_returns"} {
		if !strings.Contains(r.Detail, scope) {
			t.Errorf("detail %q does not name %s", r.Detail, scope)
		}
	}
	if r.Act != ActApprove || r.ActLabel != "Open MemQL OS from the store's Shopify admin and approve" ||
		r.ActURL != "https://acme-widgets.myshopify.com/admin/apps" {
		t.Errorf("act = %q %q %q", r.Act, r.ActLabel, r.ActURL)
	}
}

func TestARenewalThatFailedIsGrantLost(t *testing.T) {
	for _, lastError := range []string{
		"shopify: this store's connection could not be renewed",
		"shopify: reconnect this store",
		"shopify admin: HTTP 401",
	} {
		store := verdictStore()
		rows := setRow(cleanRows(store), "order", func(r map[string]any) {
			r["lastError"] = lastError
			r["lastReconcileAt"] = verdictNow.Add(-72 * time.Hour).Format(time.RFC3339)
		})
		v := computeVerdict(verdictInputOf(store, rows))
		if codes := reasonCodes(v); len(codes) != 1 || codes[0] != ReasonGrantLost {
			t.Errorf("%q: reasons = %v; want grant_lost alone, never also a failing check", lastError, codes)
			continue
		}
		if r := v.Reasons[0]; r.Sentence != "MemQL can no longer reach this store." || r.Act != ActReconnect {
			t.Errorf("%q: reason = %+v", lastError, r)
		}
	}
}

// topicOutsideConnection is a generated topic whose domain the store's
// connection does not ask for.
func topicOutsideConnection(t *testing.T, store Store) string {
	t.Helper()
	for _, topic := range generated.SubscribedTopics {
		route, ok := generated.Topics[topic]
		if !ok {
			continue
		}
		if spec := generated.Types[route.Concept]; spec != nil && !domainInConnection(store, spec) {
			return topic
		}
	}
	t.Fatal("every generated topic is inside the commerce connection")
	return ""
}

func TestAFailedSubscriptionPassIsEventsNotRegistered(t *testing.T) {
	store := verdictStore()
	pass := func(sub map[string]any) Verdict {
		s := store
		s.Health = map[string]any{"subscriptions": sub}
		return computeVerdict(verdictInputOf(s, cleanRows(s)))
	}
	for name, sub := range map[string]map[string]any{
		"a failed topic":     {"desired": 12, "existing": 11, "created": []any{}, "removed": []any{}, "failed": []any{"ORDERS_CREATE: shopify: webhookSubscriptionCreate userErrors: boom"}},
		"an unreadable list": {"desired": 12, "existing": 0, "failed": []any{subscriptionsUnreadable}},
		"fewer than wanted":  {"desired": 12, "existing": 9, "created": []any{"ORDERS_CREATE"}, "removed": []any{"PRODUCTS_DELETE"}, "failed": []any{}},
	} {
		v := pass(sub)
		if codes := reasonCodes(v); len(codes) != 1 || codes[0] != ReasonEventsNotRegistered {
			t.Errorf("%s: reasons = %v; want events_not_registered", name, codes)
			continue
		}
		r := v.Reasons[0]
		if r.Sentence != "Shopify is not sending every change." || r.Act != ActRegister || r.ActLabel != "Register again" {
			t.Errorf("%s: reason = %+v", name, r)
		}
	}
	// A pass that left everything it wanted is not a reason, and neither is
	// a store with no pass recorded yet.
	for name, sub := range map[string]map[string]any{
		"all there":      {"desired": 12, "existing": 10, "created": []any{"ORDERS_CREATE", "ORDERS_UPDATED"}, "removed": []any{}, "failed": []any{}},
		"extras of ours": {"desired": 12, "existing": 14, "created": []any{}, "removed": []any{"A", "B"}, "failed": []any{}},
	} {
		if codes := reasonCodes(pass(sub)); len(codes) != 0 {
			t.Errorf("%s: reasons = %v; want none", name, codes)
		}
	}
	if codes := reasonCodes(computeVerdict(verdictInputOf(managedStore(commerceSet...), cleanRows(store)))); len(codes) != 0 {
		t.Errorf("no pass recorded: reasons = %v; want none", codes)
	}
	// A topic outside the connection never makes the reason fire: its
	// failure is not counted, and neither is the subscription it left out.
	outside := topicOutsideConnection(t, store)
	v := pass(map[string]any{"desired": 12, "existing": 11, "created": []any{}, "removed": []any{}, "failed": []any{outside + ": shopify: access denied"}})
	if codes := reasonCodes(v); len(codes) != 0 {
		t.Errorf("a failure of %s, outside the connection: reasons = %v; want none", outside, codes)
	}
}

func TestADomainWithNoCleanSweepInTwoIntervalsIsCheckFailing(t *testing.T) {
	store := verdictStore()
	// Orders sweep hourly: three hours without a clean sweep is two
	// intervals missed.
	stale := verdictNow.Add(-3 * time.Hour)
	rows := setRow(cleanRows(store), "order", func(r map[string]any) {
		r["lastError"] = "shopify admin: HTTP 503"
		r["lastReconcileAt"] = stale.Add(-2 * time.Minute).Format(time.RFC3339)
		r["lastAttemptAt"] = stale.Format(time.RFC3339)
	})
	v := computeVerdict(verdictInputOf(store, rows))
	if codes := reasonCodes(v); len(codes) != 1 || codes[0] != ReasonCheckFailing {
		t.Fatalf("reasons = %v; want check_failing", codes)
	}
	r := v.Reasons[0]
	if r.Sentence != "The last check of orders failed" || r.Domain != "orders" || r.Act != "" {
		t.Errorf("reason = %+v", r)
	}
	if !strings.Contains(r.Detail, "HTTP 503") || !strings.Contains(r.Detail, "next check") {
		t.Errorf("detail = %q; the engine's reason, then the next sweep", r.Detail)
	}

	// No error, no clean sweep for two intervals: nothing has run.
	rows = setRow(cleanRows(store), "order", func(r map[string]any) {
		r["lastReconcileAt"] = stale.Add(-2 * time.Minute).Format(time.RFC3339)
		r["lastAttemptAt"] = stale.Format(time.RFC3339)
	})
	r = reasonNamed(t, computeVerdict(verdictInputOf(store, rows)), ReasonCheckFailing)
	if !strings.Contains(r.Detail, "No check has run since "+stale.Format(time.RFC3339)) {
		t.Errorf("detail = %q; want the time of the last check", r.Detail)
	}

	// One failure inside two intervals is not yet a failing check.
	recent := verdictNow.Add(-90 * time.Minute)
	rows = setRow(cleanRows(store), "order", func(r map[string]any) {
		r["lastError"] = "shopify admin: HTTP 503"
		r["lastReconcileAt"] = recent.Format(time.RFC3339)
	})
	if codes := reasonCodes(computeVerdict(verdictInputOf(store, rows))); len(codes) != 0 {
		t.Errorf("a failure within two intervals: reasons = %v; want none", codes)
	}

	// A backfill that failed is a reason while its sweeps are clean, with
	// the backfill's own text.
	rows = setRow(cleanRows(store), "order", func(r map[string]any) {
		r["backfillStatus"] = "failed"
		r["lastError"] = "backfill failed: shopify: bulk operation FAILED"
	})
	r = reasonNamed(t, computeVerdict(verdictInputOf(store, rows)), ReasonCheckFailing)
	if r.Domain != "orders" || !strings.HasPrefix(r.Detail, "backfill failed: ") || r.Act != "" {
		t.Errorf("reason = %+v; a failed backfill is reported with its own text and no act", r)
	}

	// One reason for each failing domain, in apply order.
	rows = setRow(rows, "customer", func(r map[string]any) {
		r["lastError"] = "shopify admin: HTTP 500"
		r["lastReconcileAt"] = stale.Format(time.RFC3339)
	})
	v = computeVerdict(verdictInputOf(store, rows))
	if n := len(v.Reasons); n != 2 {
		t.Errorf("reasons = %+v; want one for each failing domain", v.Reasons)
	}
}

// givenUp is a delivery that used its four tries.
func givenUp(topic string, received time.Time) map[string]any {
	headers, _ := json.Marshal(map[string]string{"x-shopify-topic": topic})
	return map[string]any{
		"id": "ir" + topic + received.Format(time.RFC3339Nano), "source": "shopify-" + testStoreID,
		"status": "failed", "attempts": 4, "receivedAt": received.Format(time.RFC3339),
		"headersJson": string(headers),
	}
}

func TestADeliveryThatUsedItsTriesCountsUntilItsDomainIsSweptClean(t *testing.T) {
	store := verdictStore()
	swept := verdictNow.Add(-20 * time.Minute)
	rows := setRow(cleanRows(store), "order", func(r map[string]any) { r["lastReconcileAt"] = swept.Format(time.RFC3339) })
	after := swept.Add(5 * time.Minute)

	in := verdictInputOf(store, rows)
	in.GivenUp = []map[string]any{givenUp("orders/updated", after)}
	r := reasonNamed(t, computeVerdict(in), ReasonChangesNotRecorded)
	if r.Count != 1 || r.Sentence != "1 change could not be recorded." || r.Act != ActRetry || r.ActLabel != "Try again" {
		t.Errorf("reason = %+v", r)
	}

	in.GivenUp = []map[string]any{
		givenUp("orders/updated", after),
		givenUp("orders/create", after.Add(time.Second)),
		givenUp("customers/update", verdictNow.Add(-time.Minute)),
		// Received before the order domain's last clean sweep: the sweep
		// read what it named.
		givenUp("orders/updated", swept.Add(-time.Minute)),
		// Routed to a domain outside the connection, and to no domain.
		givenUp(strings.ToLower(strings.ReplaceAll(topicOutsideConnection(t, store), "_", "/")), after),
		givenUp("app/scopes_update", after),
	}
	r = reasonNamed(t, computeVerdict(in), ReasonChangesNotRecorded)
	if r.Count != 3 || r.Sentence != "3 changes could not be recorded." {
		t.Errorf("reason = %+v; want three changes", r)
	}

	// Once every domain they route to is swept clean after them, they no
	// longer count.
	later := verdictNow.Add(-30 * time.Second)
	in.States = setRow(setRow(rows, "order", func(r map[string]any) { r["lastReconcileAt"] = later.Format(time.RFC3339) }),
		"customer", func(r map[string]any) { r["lastReconcileAt"] = later.Format(time.RFC3339) })
	if codes := reasonCodes(computeVerdict(in)); len(codes) != 0 {
		t.Errorf("reasons = %v; deliveries older than their domain's clean sweep do not count", codes)
	}
}

func TestADomainOutsideTheConnectionIsNeverAReason(t *testing.T) {
	store := verdictStore()
	rows := cleanRows(store)
	var outsideWords []string
	for _, concept := range domainsOutside(store) {
		rows = setRow(rows, generated.ConceptFromID(concept), func(r map[string]any) {
			r["lastError"] = "shopify admin: HTTP 500"
			r["backfillStatus"] = "failed"
		})
		outsideWords = append(outsideWords, plainDomain(concept))
	}
	// A plan-gated domain inside the connection, refused for a scope the
	// connection never asks for: recorded on its row as not requested.
	planGated := ""
	for _, concept := range askedOfStore(store) {
		if concept != generated.ConceptID("order") && concept != generated.ConceptID("product") {
			planGated = concept
			break
		}
	}
	rows = setRow(rows, generated.ConceptFromID(planGated), func(r map[string]any) {
		r["lastError"] = memqlsync.NotRequested(ConnectorName).Error()
		r["lastReconcileAt"] = ""
		r["backfillStatus"] = "none"
	})
	v := computeVerdict(verdictInputOf(store, rows))
	if v.State != VerdictRecording || len(v.Reasons) != 0 {
		t.Fatalf("state = %q, reasons = %+v; nothing outside the connection is a reason", v.State, v.Reasons)
	}
	outside := strings.Join(v.Outside, "|")
	for _, w := range append(outsideWords, plainDomain(planGated)) {
		if !strings.Contains("|"+outside+"|", "|"+w+"|") {
			t.Errorf("outside = %v; want %q", v.Outside, w)
		}
	}
	for _, w := range v.Outside {
		if strings.Contains(w, "v1:") || strings.Contains(w, "does not ask") {
			t.Errorf("outside carries %q; plain words only", w)
		}
	}
}

func TestAChildDomainIsNeverAsked(t *testing.T) {
	store := verdictStore()
	child := ""
	for _, concept := range generated.ApplyOrder {
		spec := generated.Types[concept]
		if spec != nil && spec.Reconcile == generated.ReconcileNone && spec.Parent != "" && domainInConnection(store, spec) {
			child = concept
			break
		}
	}
	if child == "" {
		t.Fatal("no child domain inside the commerce connection")
	}
	rows := setRow(cleanRows(store), child, func(r map[string]any) {
		r["lastError"] = "shopify admin: HTTP 500"
		r["lastReconcileAt"] = verdictNow.Add(-72 * time.Hour).Format(time.RFC3339)
		r["lastAttemptAt"] = verdictNow.Add(-72 * time.Hour).Format(time.RFC3339)
	})
	in := verdictInputOf(store, rows)
	if v := computeVerdict(in); v.State != VerdictRecording || len(v.Reasons) != 0 {
		t.Errorf("a failing child %s: state = %q, reasons = %v; a child is read with its parent and never asked", child, v.State, reasonCodes(v))
	}
}

func TestADomainIsNamedInPlainWords(t *testing.T) {
	for concept, want := range map[string]string{
		"order": "orders", "customer": "customers", "product": "products", "productVariant": "product variants",
		"collection": "collections", "discountNode": "discounts", "inventoryItem": "inventory", "location": "locations",
		"abandonedCheckout": "abandoned checkouts", "tenderTransaction": "payments", "segment": "customer segments",
		"company": "companies", "companyLocation": "company locations", "priceList": "price lists",
		"sellingPlanGroup": "selling plans", "inventoryShipment": "inventory shipments", "inventoryTransfer": "inventory transfers",
		// Any other concept: its camel-case name in lower-case words.
		"giftCard": "gift card", "marketingEvent": "marketing event", "urlRedirect": "url redirect",
		"shopifyPaymentsDispute": "shopify payments dispute",
	} {
		if got := plainDomain(concept); got != want {
			t.Errorf("plainDomain(%q) = %q, want %q", concept, got, want)
		}
		if got := plainDomain(generated.ConceptID(concept)); got != want {
			t.Errorf("plainDomain(%q) = %q, want %q", generated.ConceptID(concept), got, want)
		}
	}
	// Withheld fields are plain words too.
	in := verdictInputOf(verdictStore(), cleanRows(verdictStore()))
	in.Denied = map[string][]string{"v1:shopify:productVariant": {"deliveryProfile", "availablePublicationsCount"}}
	got := strings.Join(computeVerdict(in).Withheld, "|")
	if got != "product variants: available publications count|product variants: delivery profile" {
		t.Errorf("withheld = %q", got)
	}
}

func TestSinceIsKeptWhileTheStateHolds(t *testing.T) {
	store := verdictStore()
	earlier := verdictNow.Add(-6 * time.Hour)
	in := verdictInputOf(store, cleanRows(store))

	in.Previous = map[string]any{"state": VerdictRecording, "since": earlier.Format(time.RFC3339)}
	if v := computeVerdict(in); !v.Since.Equal(earlier) {
		t.Errorf("since = %v; the state held, so it is kept from %v", v.Since, earlier)
	}
	for name, previous := range map[string]map[string]any{
		"another state": {"state": VerdictNeedsAttention, "since": earlier.Format(time.RFC3339)},
		"no since":      {"state": VerdictRecording},
		"a bad since":   {"state": VerdictRecording, "since": "yesterday"},
		"none":          nil,
	} {
		in.Previous = previous
		if v := computeVerdict(in); !v.Since.Equal(verdictNow) {
			t.Errorf("%s: since = %v; want now", name, v.Since)
		}
	}
}

func TestTheDailyPassRecordsAFailedSubscriptionRead(t *testing.T) {
	h := newHarness(t)
	h.admin.httpStatus("ShopifyWebhookSubscriptions", http.StatusInternalServerError)

	if err := h.conn.EnsureSubscriptions(context.Background()); err == nil {
		t.Fatal("a pass that could not read the store's subscriptions reported success")
	}
	calls := h.engine.callsTo("recordStoreHealth")
	if len(calls) != 1 {
		t.Fatalf("recordStoreHealth ran %d times; the failed read is recorded on the store's health", len(calls))
	}
	if !strings.Contains(calls[0], subscriptionsUnreadable) {
		t.Errorf("health write does not record the failed read:\n%s", calls[0])
	}
}

// requeueEngine records the builtins the handlers execute, with whether each
// carried internal origin.
type requeueEngine struct {
	*fakeEngine
	internal map[string]bool
}

func (e *requeueEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(q, "builtin ") {
		e.fakeEngine.mu.Lock()
		e.internal[q] = auth.OriginFromContext(ctx).IsInternal()
		e.fakeEngine.mu.Unlock()
	}
	return e.fakeEngine.Execute(ctx, q)
}

func verdictHarness(t *testing.T) (*testHarness, *requeueEngine, *Integration) {
	t.Helper()
	h := newHarness(t)
	engine := &requeueEngine{fakeEngine: h.engine, internal: map[string]bool{}}
	h.conn.engine = engine
	h.conn.stores.engine = engine
	h.admin.reply("ShopifyWebhookSubscriptions", webhookList())
	h.admin.reply("ShopifyWebhookCreate", map[string]any{"webhookSubscriptionCreate": map[string]any{"userErrors": []any{}}})
	return h, engine, NewIntegration(h.conn)
}

func TestAStoreActIsRefusedForSomebodyWhoIsNeitherItsOwnerNorAClusterOwner(t *testing.T) {
	h, engine, integ := verdictHarness(t)
	args := map[string]any{"storeId": testStoreID}
	stranger := actorCtx("user-2", auth.RoleWriter)

	handlers := map[string]func(context.Context, map[string]any, int) (any, error){
		"verdict": func(ctx context.Context, a map[string]any, n int) (any, error) {
			return integ.handleStoreVerdict(ctx, a, n)
		},
		"register": func(ctx context.Context, a map[string]any, n int) (any, error) {
			return integ.handleStoreRegisterEvents(ctx, a, n)
		},
		"retry": func(ctx context.Context, a map[string]any, n int) (any, error) {
			return integ.handleStoreRetryDeliveries(ctx, a, n)
		},
	}
	for name, handle := range handlers {
		_, err := handle(stranger, args, 0)
		if err == nil {
			t.Errorf("%s: a caller who is neither the store's owner nor a cluster owner was admitted", name)
		} else if name == "verdict" && !strings.Contains(err.Error(), "may read stores") {
			t.Errorf("verdict refusal = %q; it names everybody the read admits", err)
		}
	}
	if n := len(h.admin.seen()); n != 0 {
		t.Errorf("Shopify was asked %d times for a refused caller", n)
	}
	for _, name := range []string{"syncStateById", "inboundRequestsGivenUp", "recordStoreHealth"} {
		if calls := engine.callsTo(name); len(calls) != 0 {
			t.Errorf("%s ran for a refused caller: %v", name, calls)
		}
	}
	if len(engine.internal) != 0 {
		t.Errorf("a builtin ran for a refused caller: %v", engine.internal)
	}

	// The store's owner, at any rank, and a cluster owner may act.
	for _, who := range []context.Context{actorCtx("user-1", auth.RoleWriter), actorCtx("user-9", auth.RoleOwner)} {
		if _, err := integ.handleStoreRegisterEvents(who, args, 0); err != nil {
			t.Errorf("register: %v", err)
		}
		if _, err := integ.handleStoreRetryDeliveries(who, args, 0); err != nil {
			t.Errorf("retry: %v", err)
		}
		nodes, err := integ.handleStoreVerdict(who, args, 0)
		if err != nil {
			t.Fatalf("verdict: %v", err)
		}
		var reply map[string]any
		_ = json.Unmarshal(nodes[0].Payload, &reply)
		if may, _ := reply["may"].(map[string]any); may["act"] != true {
			t.Errorf("verdict reply may = %v; the owner may act", reply["may"])
		}
	}
	if h.admin.countOp("ShopifyWebhookSubscriptions") == 0 || len(engine.callsTo("recordStoreHealth")) == 0 {
		t.Error("Register again neither asked Shopify nor recorded the pass")
	}
	requeue := `builtin datasyncRequeueInbound(source: "shopify-acme")`
	if internal, ran := engine.internal[requeue]; !ran || !internal {
		t.Errorf("Try again ran %v with internal origin %v; want %s under the operator context", ran, internal, requeue)
	}
}

func TestTheVerdictReplyCarriesEveryKey(t *testing.T) {
	_, _, integ := verdictHarness(t)
	nodes, err := integ.handleStoreVerdict(actorCtx("user-1", auth.RoleWriter), map[string]any{"storeId": testStoreID}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &reply); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"storeId", "domain", "state", "since", "resumedAt", "connectedAt", "reasons", "recorded", "outside", "withheld", "ordersFrom", "alert", "may"} {
		if _, ok := reply[key]; !ok {
			t.Errorf("reply has no %q: %v", key, reply)
		}
	}
	if alert, _ := reply["alert"].(map[string]any); alert == nil || alert["state"] == nil || alert["reason"] == nil {
		t.Errorf("alert = %v; want state and reason", reply["alert"])
	}
}

// pagedGivenUpEngine answers inboundRequestsGivenUp in pages of one.
type pagedGivenUpEngine struct {
	*fakeEngine
	pages [][]map[string]any
}

func (e *pagedGivenUpEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if callName(q) != "inboundRequestsGivenUp" {
		return e.fakeEngine.Execute(ctx, q)
	}
	page := 0
	if c := memql.CursorFromContext(ctx); c != "" {
		fmt.Sscanf(c, "page-%d", &page)
	}
	res := resultWithRows(e.pages[page])
	if page+1 < len(e.pages) {
		res.Meta = &memql.ResultMeta{Cursor: fmt.Sprintf("page-%d", page+1), HasMore: true}
	}
	return res, nil
}

func TestEveryPageOfGivenUpDeliveriesIsRead(t *testing.T) {
	h := newHarness(t)
	var pages [][]map[string]any
	for n := 0; n < 3; n++ {
		pages = append(pages, []map[string]any{givenUp("orders/updated", verdictNow.Add(time.Duration(n)*time.Second))})
	}
	h.conn.engine = &pagedGivenUpEngine{fakeEngine: h.engine, pages: pages}
	rows, _, err := h.conn.givenUpDeliveries(context.Background(), testStoreID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("read %d given-up deliveries; want every page's", len(rows))
	}
}

// staleRows is every asked domain last swept clean three days ago: far more
// than twice any interval.
func staleRows(store Store) []map[string]any {
	rows := cleanRows(store)
	old := verdictNow.Add(-72 * time.Hour)
	for _, r := range rows {
		r["lastReconcileAt"] = old.Add(-2 * time.Minute).Format(time.RFC3339)
		r["lastAttemptAt"] = old.Format(time.RFC3339)
	}
	return rows
}

func failingDomains(v Verdict) []string {
	out := []string{}
	for _, r := range v.Reasons {
		if r.Code == ReasonCheckFailing {
			out = append(out, r.Domain)
		}
	}
	return out
}

func TestAResumedStoreIsNotLateUntilTwoIntervalsAfterItResumed(t *testing.T) {
	store := verdictStore()
	in := verdictInputOf(store, staleRows(store))
	for _, was := range []string{VerdictPaused, VerdictNotConnected} {
		in.Previous = map[string]any{"state": was, "since": verdictNow.Add(-72 * time.Hour).Format(time.RFC3339)}
		v := computeVerdict(in)
		if failing := failingDomains(v); len(failing) != 0 {
			t.Errorf("resumed from %s: check_failing for %v; nothing is late the moment a store resumes", was, failing)
		}
		if v.State != VerdictRecording {
			t.Errorf("resumed from %s: state = %q; want recording", was, v.State)
		}
		if !v.ResumedAt.Equal(verdictNow) {
			t.Errorf("resumed from %s: resumedAt = %v; want now", was, v.ResumedAt)
		}
	}

	// Later, the verdict recorded as recording with that resumedAt: orders
	// sweep hourly, so they are late two hours after the resume and not
	// before.
	resumed := verdictNow
	in.Previous = map[string]any{"state": VerdictRecording, "since": resumed.Format(time.RFC3339), "resumedAt": resumed.Format(time.RFC3339)}
	in.Now = resumed.Add(90 * time.Minute)
	if failing := failingDomains(computeVerdict(in)); len(failing) != 0 {
		t.Errorf("90 minutes after the resume: check_failing for %v; want none", failing)
	}
	in.Now = resumed.Add(3 * time.Hour)
	v := computeVerdict(in)
	if !strings.Contains("|"+strings.Join(failingDomains(v), "|")+"|", "|orders|") {
		t.Errorf("three hours after the resume: check_failing for %v; want orders", failingDomains(v))
	}
	if !v.ResumedAt.Equal(resumed) {
		t.Errorf("resumedAt = %v; carried from the recorded verdict, want %v", v.ResumedAt, resumed)
	}
}

func TestAFirstSweepErrorIsNotLateUntilTwoIntervalsAfterTheConnect(t *testing.T) {
	store := verdictStore()
	rows := setRow(cleanRows(store), "order", func(r map[string]any) {
		r["lastError"] = "shopify admin: HTTP 503"
		delete(r, "lastReconcileAt")
		delete(r, "lastAttemptAt")
		r["backfillStatus"] = "complete"
	})
	in := verdictInputOf(store, rows)
	in.ConnectedAt = verdictNow.Add(-time.Minute)
	if failing := failingDomains(computeVerdict(in)); len(failing) != 0 {
		t.Errorf("connected a minute ago: check_failing for %v; a first error is not a late check", failing)
	}
	in.ConnectedAt = verdictNow.Add(-3 * time.Hour)
	r := reasonNamed(t, computeVerdict(in), ReasonCheckFailing)
	if r.Domain != "orders" || !strings.Contains(r.Detail, "HTTP 503") {
		t.Errorf("reason = %+v; want orders, with the error as the detail", r)
	}
}

func TestResumedAtIsCarriedWhileTheStateHolds(t *testing.T) {
	store := verdictStore()
	earlier := verdictNow.Add(-5 * time.Hour)
	in := verdictInputOf(store, cleanRows(store))

	in.Previous = map[string]any{"state": VerdictRecording, "since": earlier.Format(time.RFC3339), "resumedAt": earlier.Format(time.RFC3339)}
	if v := computeVerdict(in); !v.ResumedAt.Equal(earlier) {
		t.Errorf("resumedAt = %v; carried unchanged while the state holds, want %v", v.ResumedAt, earlier)
	}
	paused := store
	paused.Status = StatusPaused
	in.Store = paused
	in.Previous = map[string]any{"state": VerdictPaused, "since": earlier.Format(time.RFC3339), "resumedAt": earlier.Format(time.RFC3339)}
	if v := computeVerdict(in); !v.ResumedAt.Equal(earlier) {
		t.Errorf("still paused: resumedAt = %v; carried unchanged, want %v", v.ResumedAt, earlier)
	}
	in.Store = store
	for name, previous := range map[string]map[string]any{
		"never recorded":           nil,
		"never paused or detached": {"state": VerdictRecording, "since": earlier.Format(time.RFC3339)},
	} {
		in.Previous = previous
		if v := computeVerdict(in); !v.ResumedAt.IsZero() {
			t.Errorf("%s: resumedAt = %v; want absent", name, v.ResumedAt)
		}
	}
}

// connectedAt is the store row's own; a store connected before it existed
// has none.
func TestTheConnectTimeIsTheStoreRowsConnectedAt(t *testing.T) {
	store := verdictStore()
	store.ConnectedAt = "2026-11-11T11:59:00Z"
	if got, want := connectedAtOf(store), time.Date(2026, 11, 11, 11, 59, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("connectedAtOf = %v, want %v", got, want)
	}
	if got := connectedAtOf(verdictStore()); !got.IsZero() {
		t.Errorf("connectedAtOf with no connectedAt = %v, want zero: ordersFrom is not a connect time", got)
	}
}

// A store connected before connectedAt existed, with no clean sweep and never
// resumed, is judged as it was before the baseline: an attempted domain with
// no clean sweep is late.
func TestAStoreWithNoBaselineIsJudgedAsBefore(t *testing.T) {
	store := verdictStore()
	rows := setRow(cleanRows(store), "order", func(r map[string]any) {
		r["lastError"] = "shopify admin: HTTP 503"
		delete(r, "lastReconcileAt")
		delete(r, "lastAttemptAt")
	})
	r := reasonNamed(t, computeVerdict(verdictInputOf(store, rows)), ReasonCheckFailing)
	if r.Domain != "orders" || !strings.Contains(r.Detail, "HTTP 503") {
		t.Errorf("reason = %+v", r)
	}
}

// ---- Fix round 1 ----

// givenUpPages is an engine answering inboundRequestsGivenUp from fixed
// pages, counting the pages read.
type givenUpPages struct {
	*fakeEngine
	pages [][]map[string]any
	read  int
}

func (e *givenUpPages) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if callName(q) != "inboundRequestsGivenUp" {
		return e.fakeEngine.Execute(ctx, q)
	}
	page := 0
	if c := memql.CursorFromContext(ctx); c != "" {
		fmt.Sscanf(c, "page-%d", &page)
	}
	e.fakeEngine.mu.Lock()
	e.read++
	e.fakeEngine.mu.Unlock()
	res := resultWithRows(e.pages[page])
	if page+1 < len(e.pages) {
		res.Meta = &memql.ResultMeta{Cursor: fmt.Sprintf("page-%d", page+1), HasMore: true}
	}
	return res, nil
}

// writtenAt is a given-up delivery received and last written at `at`.
func writtenAt(topic string, at time.Time) map[string]any {
	row := givenUp(topic, at)
	row["createdAt"] = at.Format(time.RFC3339)
	return row
}

func pagesOf(n int, at func(int) time.Time) [][]map[string]any {
	var out [][]map[string]any
	for i := 0; i < n; i++ {
		out = append(out, []map[string]any{writtenAt("orders/updated", at(i))})
	}
	return out
}

func TestTheGivenUpWalkStopsAtThePageOlderThanEverySweep(t *testing.T) {
	h := newHarness(t)
	bound := verdictNow.Add(-time.Hour)
	// Newest first: pages 0 and 1 after the bound, page 2 entirely before it.
	engine := &givenUpPages{fakeEngine: h.engine, pages: pagesOf(6, func(i int) time.Time {
		return verdictNow.Add(-time.Duration(i) * 40 * time.Minute)
	})}
	h.conn.engine = engine
	rows, floor, err := h.conn.givenUpDeliveries(context.Background(), testStoreID, bound)
	if err != nil || floor {
		t.Fatalf("err = %v, floor = %v", err, floor)
	}
	if engine.read != 3 || len(rows) != 2 {
		t.Errorf("read %d pages and kept %d rows; want the walk to stop after the third page, the first older than every sweep", engine.read, len(rows))
	}
	// With no bound the walk reads on.
	engine.read = 0
	if rows, _, _ := h.conn.givenUpDeliveries(context.Background(), testStoreID, time.Time{}); engine.read != 6 || len(rows) != 6 {
		t.Errorf("unbounded: read %d pages, %d rows; want all six", engine.read, len(rows))
	}
}

func TestTheGivenUpBoundIsTheOldestCleanSweepOfTheAskedDomains(t *testing.T) {
	store := verdictStore()
	rows := cleanRows(store)
	oldest := verdictNow.Add(-5 * time.Hour)
	rows = setRow(rows, "customer", func(r map[string]any) { r["lastReconcileAt"] = oldest.Format(time.RFC3339) })
	if got := givenUpBound(store, rows); !got.Equal(oldest) {
		t.Errorf("bound = %v, want the oldest clean sweep %v", got, oldest)
	}
	// A domain never swept clean counts every delivery routed to it, so
	// nothing bounds the walk.
	rows = setRow(rows, "order", func(r map[string]any) { delete(r, "lastReconcileAt") })
	if got := givenUpBound(store, rows); !got.IsZero() {
		t.Errorf("bound = %v with a domain never swept clean; want none", got)
	}
}

func TestAPausedOrDisconnectedStoreReadsNoGivenUpPage(t *testing.T) {
	for name, fields := range map[string]map[string]any{
		"paused":       {"status": StatusPaused},
		"uninstalled":  {"uninstalledAt": "2026-08-01T00:00:00Z"},
		"data removed": {"redactedAt": "2026-08-01T00:00:00Z"},
	} {
		h := newHarness(t)
		row := h.engine.rows["stores"][0]
		for k, v := range fields {
			row[k] = v
		}
		engine := &givenUpPages{fakeEngine: h.engine, pages: pagesOf(2, func(int) time.Time { return verdictNow })}
		h.conn.engine = engine
		h.conn.stores.engine = engine
		if _, err := h.conn.storeVerdict(context.Background(), h.store(t)); err != nil {
			t.Fatal(err)
		}
		if engine.read != 0 {
			t.Errorf("%s: read %d pages of given-up deliveries; a store with no reasons reads none", name, engine.read)
		}
	}
}

func TestABudgetThatRunsOutIsAFloorNotAnError(t *testing.T) {
	saved := givenUpPageBudget
	givenUpPageBudget = 2
	t.Cleanup(func() { givenUpPageBudget = saved })
	h := newHarness(t)
	engine := &givenUpPages{fakeEngine: h.engine, pages: pagesOf(5, func(int) time.Time { return h.now })}
	h.conn.engine = engine
	v, err := h.conn.storeVerdict(context.Background(), h.store(t))
	if err != nil {
		t.Fatalf("a walk that ran out of budget failed the verdict: %v", err)
	}
	r := reasonNamed(t, v, ReasonChangesNotRecorded)
	if !r.AtLeast || r.Count != 2 || r.Sentence != "At least 2 changes could not be recorded." {
		t.Errorf("reason = %+v; want a floor of two", r)
	}
	// The exact case keeps the constraints' sentence.
	in := verdictInputOf(verdictStore(), cleanRows(verdictStore()))
	in.GivenUp = []map[string]any{givenUp("orders/updated", verdictNow)}
	if r := reasonNamed(t, computeVerdict(in), ReasonChangesNotRecorded); r.AtLeast || r.Sentence != "1 change could not be recorded." {
		t.Errorf("exact count: reason = %+v", r)
	}
	payload := verdictPayload(v)
	if reasons, _ := payload["reasons"].([]any); len(reasons) == 0 || reasons[len(reasons)-1].(map[string]any)["atLeast"] != true {
		t.Errorf("the reply does not say the count is a floor: %v", payload["reasons"])
	}
}

// childTopic is a generated topic whose domain is read with its parent, and
// that parent.
func childTopic(t *testing.T, store Store) (header, parent string) {
	t.Helper()
	for _, topic := range generated.SubscribedTopics {
		route, ok := generated.Topics[topic]
		if !ok {
			continue
		}
		spec := generated.Types[route.Concept]
		if spec == nil || spec.Reconcile != generated.ReconcileNone || spec.Parent == "" {
			continue
		}
		if p := generated.Types[spec.Parent]; p != nil && p.Reconcile != generated.ReconcileNone && domainInConnection(store, p) {
			return generated.EnumToTopicHeader(topic), spec.Parent
		}
	}
	t.Fatal("no child topic whose parent is swept inside the connection")
	return "", ""
}

func TestAChildDomainDeliveryCountsTowardItsParent(t *testing.T) {
	store := verdictStore()
	header, parent := childTopic(t, store)
	swept := verdictNow.Add(-20 * time.Minute)
	rows := setRow(cleanRows(store), parent, func(r map[string]any) { r["lastReconcileAt"] = swept.Format(time.RFC3339) })
	in := verdictInputOf(store, rows)
	in.GivenUp = []map[string]any{givenUp(header, swept.Add(time.Minute)), givenUp(header, swept.Add(-time.Minute))}
	r := reasonNamed(t, computeVerdict(in), ReasonChangesNotRecorded)
	if r.Count != 1 {
		t.Errorf("%s (read with %s): count = %d; the one after the parent's clean sweep counts, the one before does not", header, parent, r.Count)
	}
}

func TestAMalformedOrMissingTopicIsNotCounted(t *testing.T) {
	in := verdictInputOf(verdictStore(), cleanRows(verdictStore()))
	broken := givenUp("orders/updated", verdictNow)
	broken["headersJson"] = "{not json"
	none := givenUp("orders/updated", verdictNow)
	none["headersJson"] = `{"x-shopify-hmac-sha256":"abc"}`
	empty := givenUp("orders/updated", verdictNow)
	delete(empty, "headersJson")
	in.GivenUp = []map[string]any{broken, none, empty}
	if codes := reasonCodes(computeVerdict(in)); len(codes) != 0 {
		t.Errorf("reasons = %v; a delivery with no readable topic is not counted", codes)
	}
}

func TestAProtectedDataBackfillRefusalIsDataNotDeclared(t *testing.T) {
	store := verdictStore()
	rows := setRow(cleanRows(store), "order", func(r map[string]any) {
		r["backfillStatus"] = "failed"
		r["lastError"] = "backfill refused: " + protectedRefusal
	})
	v := computeVerdict(verdictInputOf(store, rows))
	if codes := reasonCodes(v); len(codes) != 1 || codes[0] != ReasonDataNotDeclared {
		t.Fatalf("reasons = %v; want data_not_declared alone", codes)
	}
	if r := v.Reasons[0]; r.Domain != "orders" || r.Act != ActDeclare {
		t.Errorf("reason = %+v", r)
	}
	// Any other refused backfill is still a failing check.
	rows = setRow(cleanRows(store), "order", func(r map[string]any) {
		r["backfillStatus"] = "failed"
		r["lastError"] = "backfill refused: shopify admin: HTTP 500"
	})
	if codes := reasonCodes(computeVerdict(verdictInputOf(store, rows))); len(codes) != 1 || codes[0] != ReasonCheckFailing {
		t.Errorf("reasons = %v; want check_failing", codes)
	}
}

// A baseline in the future counts as now: a clock that ran ahead neither
// makes a domain late nor holds it back past twice its interval from now.
func TestAFutureBaselineCountsAsNow(t *testing.T) {
	store := verdictStore()
	rows := setRow(cleanRows(store), "order", func(r map[string]any) {
		r["lastError"] = "shopify admin: HTTP 503"
		r["lastReconcileAt"] = verdictNow.Add(5 * time.Hour).Format(time.RFC3339)
	})
	in := verdictInputOf(store, rows)
	in.ConnectedAt = verdictNow.Add(10 * time.Hour)
	if got := baselineOf(rowTime(rows[len(rows)-1], "lastReconcileAt"), in.ConnectedAt, verdictNow); !got.Equal(verdictNow) {
		t.Errorf("baseline = %v; a baseline in the future counts as now (%v)", got, verdictNow)
	}
	if failing := failingDomains(computeVerdict(in)); len(failing) != 0 {
		t.Errorf("check_failing for %v; a future baseline is now, and nothing is late yet", failing)
	}
}

func TestTheDailyPassClearsAnUnreadableFailureOnceItReads(t *testing.T) {
	h := newHarness(t)
	h.admin.httpStatus("ShopifyWebhookSubscriptions", http.StatusInternalServerError)
	_ = h.conn.EnsureSubscriptions(context.Background())
	h.admin.httpStatus("ShopifyWebhookSubscriptions", http.StatusOK)
	h.admin.reply("ShopifyWebhookSubscriptions", webhookList())
	h.admin.reply("ShopifyWebhookCreate", map[string]any{"webhookSubscriptionCreate": map[string]any{"userErrors": []any{}}})
	if err := h.conn.EnsureSubscriptions(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := h.engine.callsTo("recordStoreHealth")
	if len(calls) != 2 || !strings.Contains(calls[0], subscriptionsUnreadable) || strings.Contains(calls[1], subscriptionsUnreadable) {
		t.Fatalf("health writes = %d; the later pass must record without the unreadable failure", len(calls))
	}
	store := verdictStore()
	store.Health = map[string]any{"subscriptions": map[string]any{"desired": 12, "existing": 0, "created": toAny(generated.SubscribedTopics[:12]), "failed": []any{}}}
	if codes := reasonCodes(computeVerdict(verdictInputOf(store, cleanRows(store)))); len(codes) != 0 {
		t.Errorf("after a pass that read and registered everything: reasons = %v", codes)
	}
}

func TestRegisterAgainWaitsAMinuteAfterAPass(t *testing.T) {
	for name, tc := range map[string]struct {
		ago  time.Duration
		runs bool
	}{"30 seconds ago": {30 * time.Second, false}, "90 seconds ago": {90 * time.Second, true}} {
		h, _, integ := verdictHarness(t)
		h.engine.rows["stores"][0]["health"] = map[string]any{"subscriptions": map[string]any{"at": h.now.Add(-tc.ago).Format(time.RFC3339)}}
		_, err := integ.handleStoreRegisterEvents(actorCtx("user-1", auth.RoleWriter), map[string]any{"storeId": testStoreID}, 0)
		ran := h.admin.countOp("ShopifyWebhookSubscriptions") > 0
		if ran != tc.runs {
			t.Errorf("%s: the pass ran = %v, want %v (err %v)", name, ran, tc.runs, err)
		}
		if !tc.runs && (err == nil || !strings.Contains(err.Error(), "just now")) {
			t.Errorf("%s: err = %v; want a sentence saying it ran just now", name, err)
		}
	}
}

func TestRegisterAgainSaysWhyAPassFailed(t *testing.T) {
	h, _, integ := verdictHarness(t)
	h.admin.httpStatus("ShopifyWebhookSubscriptions", http.StatusInternalServerError)
	nodes, err := integ.handleStoreRegisterEvents(actorCtx("user-1", auth.RoleWriter), map[string]any{"storeId": testStoreID}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	_ = json.Unmarshal(nodes[0].Payload, &reply)
	if reply["status"] != "failed" || !strings.Contains(fmt.Sprint(reply["detail"]), "500") {
		t.Errorf("reply = %v; a failed pass says why", reply)
	}
}

func TestAVerdictReadsOnlyTheDomainsItAsks(t *testing.T) {
	h := newHarness(t)
	withManagedScopes(t, h, commerceSet...)
	if _, err := h.conn.storeVerdict(context.Background(), h.store(t)); err != nil {
		t.Fatal(err)
	}
	want := len(verdictDomains(h.store(t)))
	if got := len(h.engine.callsTo("syncStateById")); got != want || got >= len(generated.ApplyOrder) {
		t.Errorf("a verdict made %d point reads; want one for each of the %d domains it reads, not all %d", got, want, len(generated.ApplyOrder))
	}
}

// shapedPages answers inboundRequestsGivenUp as the real engine's shaped
// read does: rows whose createdAt is a time.Time (measured against the real
// engine), not a string.
type shapedPages struct {
	*fakeEngine
	pages [][]map[string]any
	read  int
}

func (e *shapedPages) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if callName(q) != "inboundRequestsGivenUp" {
		return e.fakeEngine.Execute(ctx, q)
	}
	page := 0
	if c := memql.CursorFromContext(ctx); c != "" {
		fmt.Sscanf(c, "page-%d", &page)
	}
	e.read++
	rows := []any{}
	for _, r := range e.pages[page] {
		row := map[string]any{}
		for k, v := range r {
			row[k] = v
		}
		at, _ := time.Parse(time.RFC3339, r["createdAt"].(string))
		row["createdAt"] = at
		rows = append(rows, row)
	}
	res := memql.NewResultWithOutput(rows)
	if page+1 < len(e.pages) {
		res.Meta = &memql.ResultMeta{Cursor: fmt.Sprintf("page-%d", page+1), HasMore: true}
	}
	return res, nil
}

func TestTheWalkBoundReadsTheEnginesOwnTimeValue(t *testing.T) {
	h := newHarness(t)
	engine := &shapedPages{fakeEngine: h.engine, pages: pagesOf(4, func(i int) time.Time {
		return verdictNow.Add(-time.Duration(i) * 40 * time.Minute)
	})}
	h.conn.engine = engine
	rows, _, err := h.conn.givenUpDeliveries(context.Background(), testStoreID, verdictNow.Add(-time.Hour))
	if err != nil || engine.read != 3 || len(rows) != 2 {
		t.Errorf("read %d pages, kept %d rows, err %v; a time.Time createdAt must bound the walk", engine.read, len(rows), err)
	}
}
