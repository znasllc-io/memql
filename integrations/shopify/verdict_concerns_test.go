package shopify

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// verdict_concerns_test.go -- what each reason of a store's verdict stands in
// the way of.
//
// The figures of a store are drawn "as of" an earlier moment only when a
// reason stands in the way of the data they are computed from. The mirror
// says what each reason concerns, in a machine field, so nothing downstream
// guesses from a reason's code or its words for a person.

// statisticsWalk is what the store's figures read: orders, what is read with
// them, and the shop's own row.
var statisticsWalk = []string{"order", "orderLineItem", "refund", "shop"}

// reasonCodeConstants parses this package's non-test files for the constants
// whose name begins "Reason" and whose value is a string literal: name to
// value. The classification's gate reads the codes from here, never from a
// second list, so a code added or removed later cannot slip past it.
func reasonCodeConstants(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]string{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !strings.HasPrefix(name.Name, "Reason") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("%s: %s: %v", path, name.Name, err)
					}
					out[name.Name] = value
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no Reason* constant found: the gate is reading the wrong files")
	}
	return out
}

func TestEveryReasonCodeIsClassified(t *testing.T) {
	kinds := map[string]bool{ConcernsWholeStore: true, ConcernsConcepts: true, ConcernsNoRecording: true}
	constants := reasonCodeConstants(t)
	codes := map[string]bool{}
	for name, code := range constants {
		codes[code] = true
		if _, ok := reasonConcerns[code]; !ok {
			t.Errorf("%s (%q) is a reason with no entry in reasonConcerns: say what it stands in the way of", name, code)
		}
	}
	for code, allowed := range reasonConcerns {
		if !codes[code] {
			t.Errorf("reasonConcerns classifies %q, which no Reason* constant of the package declares", code)
		}
		if len(allowed) == 0 {
			t.Errorf("%q is classified with no kind", code)
		}
		seen := map[string]bool{}
		for _, kind := range allowed {
			if !kinds[kind] {
				t.Errorf("%q is classified %q, which is not one of the three kinds", code, kind)
			}
			if seen[kind] {
				t.Errorf("%q names %q twice", code, kind)
			}
			seen[kind] = true
		}
	}
}

// checkReasonKind holds one reason computeVerdict answered to the
// classification: its Concerns is one of its code's kinds, and Concepts is
// sorted, and not empty exactly when Concerns is ConcernsConcepts.
func checkReasonKind(t *testing.T, r VerdictReason) {
	t.Helper()
	if !slices.Contains(reasonConcerns[r.Code], r.Concerns) {
		t.Errorf("%s: concerns %q; its code may carry %v", r.Code, r.Concerns, reasonConcerns[r.Code])
	}
	if (r.Concerns == ConcernsConcepts) != (len(r.Concepts) > 0) {
		t.Errorf("%s: concerns %q with concepts %v; concepts are named exactly when the reason concerns concepts", r.Code, r.Concerns, r.Concepts)
	}
	if !sort.StringsAreSorted(r.Concepts) {
		t.Errorf("%s: concepts %v are not sorted", r.Code, r.Concepts)
	}
	for _, c := range r.Concepts {
		if strings.HasPrefix(c, "v1:") || generated.Types[c] == nil {
			t.Errorf("%s: concept %q is not a generated concept's name", r.Code, c)
		}
	}
}

func TestEachReasonComputeVerdictAnswersCarriesItsKind(t *testing.T) {
	stale := verdictNow.Add(-3 * time.Hour)
	fixtures := map[string]func() verdictInput{
		ReasonDataNotDeclared: func() verdictInput {
			store := verdictStore()
			return verdictInputOf(store, setRow(cleanRows(store), "order", func(r map[string]any) {
				r["lastError"] = protectedRefusal
				r["lastReconcileAt"] = ""
				r["backfillStatus"] = "none"
			}))
		},
		ReasonApprovalOutdated: func() verdictInput {
			store := managedStore(oldManagedList...)
			rows := cleanRows(store)
			for _, concept := range []string{"customer", "discountNode"} {
				rows = setRow(rows, concept, func(r map[string]any) {
					r["lastError"] = memqlsync.NotGranted(ConnectorName, "read_customers").Error()
					r["lastReconcileAt"] = ""
				})
			}
			return verdictInputOf(store, rows)
		},
		ReasonGrantLost: func() verdictInput {
			store := verdictStore()
			return verdictInputOf(store, setRow(cleanRows(store), "order", func(r map[string]any) {
				r["lastError"] = "shopify admin: HTTP 401"
				r["lastReconcileAt"] = stale.Format(time.RFC3339)
			}))
		},
		ReasonEventsNotRegistered: func() verdictInput {
			store := verdictStore()
			store.Health = map[string]any{"subscriptions": map[string]any{"desired": 12, "existing": 0, "failed": []any{subscriptionsUnreadable}}}
			return verdictInputOf(store, cleanRows(store))
		},
		ReasonCheckFailing: func() verdictInput {
			store := verdictStore()
			return verdictInputOf(store, setRow(cleanRows(store), "order", func(r map[string]any) {
				r["lastError"] = "shopify admin: HTTP 503"
				r["lastReconcileAt"] = stale.Add(-2 * time.Minute).Format(time.RFC3339)
				r["lastAttemptAt"] = stale.Format(time.RFC3339)
			}))
		},
		ReasonChangesNotRecorded: func() verdictInput {
			store := verdictStore()
			in := verdictInputOf(store, cleanRows(store))
			in.GivenUp = []map[string]any{givenUp("orders/updated", verdictNow.Add(-time.Minute))}
			return in
		},
		ReasonPrivacyRequestFailing: func() verdictInput {
			store := verdictStore()
			in := verdictInputOf(store, cleanRows(store))
			in.Privacy = privacyFailing{Count: 1, Since: verdictNow.Add(-48 * time.Hour)}
			return in
		},
	}
	for code := range reasonConcerns {
		if fixtures[code] == nil {
			t.Errorf("no fixture answers %s: add one here", code)
		}
	}
	for code, fixture := range fixtures {
		v := computeVerdict(fixture())
		r := reasonNamed(t, v, code)
		for _, each := range v.Reasons {
			checkReasonKind(t, each)
		}
		if r.Concerns == "" {
			t.Errorf("%s: no concerns written", code)
		}
	}
}

func TestAReasonSaysWhatItStandsInTheWayOf(t *testing.T) {
	walked := statisticsWalk
	for _, c := range []struct {
		name   string
		reason VerdictReason
		want   bool
	}{
		{"orders' check failing", VerdictReason{Code: ReasonCheckFailing, Concerns: ConcernsConcepts, Concepts: []string{"order"}}, true},
		{"products' check failing", VerdictReason{Code: ReasonCheckFailing, Concerns: ConcernsConcepts, Concepts: []string{"product"}}, false},
		{"a refund's parent named", VerdictReason{Code: ReasonDataNotDeclared, Concerns: ConcernsConcepts, Concepts: []string{"order"}}, true},
		{"customers undeclared", VerdictReason{Code: ReasonDataNotDeclared, Concerns: ConcernsConcepts, Concepts: []string{"customer"}}, false},
		{"an approval for products only", VerdictReason{Code: ReasonApprovalOutdated, Concerns: ConcernsConcepts, Concepts: []string{"product", "productVariant"}}, false},
		{"the grant lost", VerdictReason{Code: ReasonGrantLost, Concerns: ConcernsWholeStore}, true},
		{"events not registered", VerdictReason{Code: ReasonEventsNotRegistered, Concerns: ConcernsWholeStore}, true},
		{"changes not recorded", VerdictReason{Code: ReasonChangesNotRecorded, Concerns: ConcernsWholeStore}, true},
		{"a privacy request failing", VerdictReason{Code: ReasonPrivacyRequestFailing, Concerns: ConcernsNoRecording}, false},
		{"not classified", VerdictReason{Code: "something_new"}, true},
		{"concepts said and none named", VerdictReason{Code: ReasonCheckFailing, Concerns: ConcernsConcepts}, true},
		// What is not classified stands in the way, whatever kind it says.
		{"an unknown kind", VerdictReason{Code: ReasonCheckFailing, Concerns: "somethingElse", Concepts: []string{"product"}}, true},
		{"an unknown code saying nothing recorded", VerdictReason{Code: "something_new", Concerns: ConcernsNoRecording}, true},
		{"a kind its code may not carry", VerdictReason{Code: ReasonGrantLost, Concerns: ConcernsNoRecording}, true},
		{"a child named and walked", VerdictReason{Code: ReasonApprovalOutdated, Concerns: ConcernsConcepts, Concepts: []string{"orderLineItem"}}, true},
		// A child read with orders and not walked holds neither the orders nor
		// their other children.
		{"the returns read with orders", VerdictReason{Code: ReasonApprovalOutdated, Concerns: ConcernsConcepts, Concepts: []string{"reverseDelivery", "shopifyReturn"}}, false},
	} {
		if got := c.reason.StandsInTheWayOf(walked); got != c.want {
			t.Errorf("%s: StandsInTheWayOf = %v, want %v", c.name, got, c.want)
		}
	}
	if (VerdictReason{Code: ReasonCheckFailing, Concerns: ConcernsConcepts, Concepts: []string{"order"}}).StandsInTheWayOf([]string{"v1:shopify:refund"}) != true {
		t.Error("a concept given by id is compared through the domain that records it")
	}
}

// refusedByTheMirror is every generated concept of the store's connection
// whose grant holds none of its scopes: what the mirror itself does not read.
func refusedByTheMirror(store Store) []string {
	var out []string
	for concept, spec := range generated.Types {
		if domainInConnection(store, spec) && len(scopesMissingFor(store, spec)) > 0 {
			out = append(out, concept)
		}
	}
	sort.Strings(out)
	return out
}

func without(scopes []string, drop ...string) []string {
	var out []string
	for _, s := range scopes {
		if !slices.Contains(drop, s) {
			out = append(out, s)
		}
	}
	return out
}

func TestAnOutdatedApprovalNamesTheConceptsTheMirrorRefuses(t *testing.T) {
	orderConcepts := []string{"order", "orderLineItem", "refund"}

	// A managed store whose grant holds no product scope.
	noProducts := managedStore(without(commerceSet, "write_products")...)
	r := reasonNamed(t, computeVerdict(verdictInputOf(noProducts, cleanRows(noProducts))), ReasonApprovalOutdated)
	checkReasonKind(t, r)
	if r.Concerns != ConcernsConcepts || !slices.Equal(r.Concepts, refusedByTheMirror(noProducts)) {
		t.Fatalf("concerns %q, concepts %v; want the concepts the mirror refuses, %v", r.Concerns, r.Concepts, refusedByTheMirror(noProducts))
	}
	for _, want := range []string{"product", "productVariant"} {
		if !slices.Contains(r.Concepts, want) {
			t.Errorf("concepts %v; %s is refused without a product scope", r.Concepts, want)
		}
	}
	for _, c := range r.Concepts {
		if sweptParentOf(c) == "order" {
			t.Errorf("concepts %v name %s, which the grant still reads with orders", r.Concepts, c)
		}
	}
	if r.StandsInTheWayOf(orderConcepts) {
		t.Errorf("an approval lacking product scopes stands in the way of orders: %+v", r)
	}
	// The shop's row is read under read_products too (shop.go refuses its
	// refresh by the same scopesMissingFor), so a walk that reads the shop is
	// held by it.
	if slices.Contains(r.Concepts, "shop") != r.StandsInTheWayOf([]string{"shop"}) {
		t.Errorf("concepts %v and StandsInTheWayOf(shop) disagree", r.Concepts)
	}
	if !slices.Contains(r.Concepts, "shop") {
		t.Errorf("concepts %v; the mirror does not refresh the shop without a product scope (shop.go)", r.Concepts)
	}

	// A store whose grant holds no order scope.
	noOrders := managedStore(without(commerceSet, "read_orders")...)
	r = reasonNamed(t, computeVerdict(verdictInputOf(noOrders, cleanRows(noOrders))), ReasonApprovalOutdated)
	checkReasonKind(t, r)
	if r.Concerns != ConcernsConcepts || !slices.Equal(r.Concepts, refusedByTheMirror(noOrders)) {
		t.Fatalf("concerns %q, concepts %v; want the concepts the mirror refuses, %v", r.Concerns, r.Concepts, refusedByTheMirror(noOrders))
	}
	for _, want := range orderConcepts {
		if !slices.Contains(r.Concepts, want) {
			t.Errorf("concepts %v; %s is refused without an order scope", r.Concepts, want)
		}
	}
	if slices.Contains(r.Concepts, "product") {
		t.Errorf("concepts %v name products, which the grant still reads", r.Concepts)
	}
	if !r.StandsInTheWayOf(statisticsWalk) {
		t.Errorf("an approval lacking every order scope does not stand in the way of the walk: %+v", r)
	}

	// The older managed list lacks read_customers, read_discounts and
	// read_returns: the returns read with orders are refused, and the orders,
	// their lines and refunds are still read.
	older := managedStore(oldManagedList...)
	r = reasonNamed(t, computeVerdict(verdictInputOf(older, cleanRows(older))), ReasonApprovalOutdated)
	checkReasonKind(t, r)
	if !slices.Equal(r.Concepts, refusedByTheMirror(older)) || !slices.Contains(r.Concepts, "shopifyReturn") || !slices.Contains(r.Concepts, "customer") {
		t.Errorf("concepts %v; want the concepts the mirror refuses, %v", r.Concepts, refusedByTheMirror(older))
	}
	if r.StandsInTheWayOf(statisticsWalk) {
		t.Errorf("an approval lacking read_returns stands in the way of the orders: %+v", r)
	}
}

func TestAnApprovalLackingOnlyAWideningScopeFreezesNothing(t *testing.T) {
	// A store on its own app asks for the full Connect list, read_orders and
	// read_all_orders among it, and its app allows all of it but
	// read_all_orders: orders past sixty days are not read, and every concept
	// is still read.
	own := Store{
		ID: testStoreID, Domain: "acme-widgets.myshopify.com", AdminTokenRef: "ACME_ADMIN", Status: StatusLive,
		ScopesGranted: without(ConnectScopes(), "read_all_orders"),
	}
	if own.Managed() || !slices.Contains(connectionScopes(own), "read_all_orders") || !slices.Contains(own.ScopesGranted, "read_orders") {
		t.Fatal("the fixture is not a store on its own app asking read_all_orders and holding read_orders")
	}
	v := computeVerdict(verdictInputOf(own, cleanRows(own)))
	r := reasonNamed(t, v, ReasonApprovalOutdated)
	checkReasonKind(t, r)
	if r.Detail != "read_all_orders" {
		t.Errorf("detail %q; the reason is listed, naming the one scope", r.Detail)
	}
	if r.Concerns != ConcernsNoRecording || len(r.Concepts) != 0 {
		t.Errorf("concerns %q, concepts %v; a scope that only widens what is read freezes nothing", r.Concerns, r.Concepts)
	}
	if r.StandsInTheWayOf(statisticsWalk) {
		t.Errorf("a missing read_all_orders stands in the way of the walk: %+v", r)
	}
}

func TestChangesNotRecordedNameTheirDomains(t *testing.T) {
	walked := statisticsWalk
	store := verdictStore()
	after := verdictNow.Add(-time.Minute)
	products := []map[string]any{givenUp("products/update", after), givenUp("products/update", after.Add(time.Second))}

	reason := func(givenUp []map[string]any, floor bool) VerdictReason {
		t.Helper()
		in := verdictInputOf(store, cleanRows(store))
		in.GivenUp, in.GivenUpFloor = givenUp, floor
		r := reasonNamed(t, computeVerdict(in), ReasonChangesNotRecorded)
		checkReasonKind(t, r)
		return r
	}

	r := reason(products, false)
	if r.Concerns != ConcernsConcepts || !slices.Equal(r.Concepts, []string{"product"}) || r.StandsInTheWayOf(walked) {
		t.Errorf("products only: %+v; want concepts [product], not in the way of the walk", r)
	}

	r = reason(append(slices.Clone(products), givenUp("orders/updated", after)), false)
	if r.Concerns != ConcernsConcepts || !slices.Equal(r.Concepts, []string{"order", "product"}) || !r.StandsInTheWayOf(walked) {
		t.Errorf("one of orders among them: %+v; want concepts [order product], in the way of the walk", r)
	}

	// A child's delivery is named by the domain whose sweep records it.
	header, parent := childTopic(t, store)
	r = reason([]map[string]any{givenUp(header, after)}, false)
	if !slices.Equal(r.Concepts, []string{parent}) {
		t.Errorf("%s: concepts %v; want its parent %s", header, r.Concepts, parent)
	}

	r = reason(products, true)
	if r.Concerns != ConcernsWholeStore || len(r.Concepts) != 0 || !r.StandsInTheWayOf(walked) {
		t.Errorf("a floor: %+v; the deliveries not read may be of any domain", r)
	}

	r = reason(append(slices.Clone(products), givenUp("app/scopes_update", after)), false)
	if r.Concerns != ConcernsWholeStore || len(r.Concepts) != 0 || !r.StandsInTheWayOf(walked) {
		t.Errorf("beside a delivery routed to no domain: %+v; want the whole store", r)
	}
	if r.Count != 2 {
		t.Errorf("count = %d; the delivery routed to no domain is still not counted", r.Count)
	}

	// A delivery of a domain outside the connection is neither counted nor
	// named, and leaves the reason by concept.
	outside := strings.ToLower(strings.ReplaceAll(topicOutsideConnection(t, store), "_", "/"))
	r = reason(append(slices.Clone(products), givenUp(outside, after)), false)
	if r.Concerns != ConcernsConcepts || !slices.Equal(r.Concepts, []string{"product"}) || r.Count != 2 {
		t.Errorf("beside %s, outside the connection: %+v", outside, r)
	}
}

func TestAFailingCheckNamesItsConcept(t *testing.T) {
	store := verdictStore()
	stale := verdictNow.Add(-3 * time.Hour)
	rows := setRow(cleanRows(store), "order", func(r map[string]any) {
		r["lastError"] = "shopify admin: HTTP 503"
		r["lastReconcileAt"] = stale.Add(-2 * time.Minute).Format(time.RFC3339)
		r["lastAttemptAt"] = stale.Format(time.RFC3339)
	})
	r := reasonNamed(t, computeVerdict(verdictInputOf(store, rows)), ReasonCheckFailing)
	checkReasonKind(t, r)
	if r.Concerns != ConcernsConcepts || !slices.Equal(r.Concepts, []string{"order"}) || !r.StandsInTheWayOf(statisticsWalk) {
		t.Errorf("reason = %+v; want concepts [order], in the way of the walk", r)
	}

	// A failed backfill of products is the products' check, and leaves the
	// walk clear.
	rows = setRow(cleanRows(store), "product", func(r map[string]any) {
		r["backfillStatus"] = "failed"
		r["lastError"] = "backfill failed: shopify: bulk operation FAILED"
	})
	r = reasonNamed(t, computeVerdict(verdictInputOf(store, rows)), ReasonCheckFailing)
	if !slices.Equal(r.Concepts, []string{"product"}) || r.StandsInTheWayOf(statisticsWalk) {
		t.Errorf("reason = %+v; want concepts [product], clear of the walk", r)
	}
}

func TestADataNotDeclaredReasonNamesItsConcepts(t *testing.T) {
	store := verdictStore()
	rows := setRow(cleanRows(store), "customer", func(r map[string]any) {
		r["lastError"] = protectedRefusal
		r["lastReconcileAt"] = ""
		r["backfillStatus"] = "none"
	})
	r := reasonNamed(t, computeVerdict(verdictInputOf(store, rows)), ReasonDataNotDeclared)
	checkReasonKind(t, r)
	if !slices.Equal(r.Concepts, []string{"customer"}) || r.StandsInTheWayOf(statisticsWalk) {
		t.Errorf("reason = %+v; want concepts [customer], clear of the walk", r)
	}
	rows = setRow(rows, "order", func(r map[string]any) { r["lastError"] = protectedRefusal })
	r = reasonNamed(t, computeVerdict(verdictInputOf(store, rows)), ReasonDataNotDeclared)
	if !slices.Equal(r.Concepts, []string{"customer", "order"}) || r.Domain != "customers and orders" || !r.StandsInTheWayOf(statisticsWalk) {
		t.Errorf("reason = %+v; want concepts [customer order], in the way of the walk", r)
	}
}

func TestThePrivacyReasonConcernsNoRecording(t *testing.T) {
	recording := verdictStore()
	paused := verdictStore()
	paused.Status = StatusPaused
	for name, store := range map[string]Store{"recording": recording, "paused": paused} {
		in := verdictInputOf(store, cleanRows(store))
		in.Privacy = privacyFailing{Count: 2, Since: verdictNow.Add(-48 * time.Hour)}
		r := reasonNamed(t, computeVerdict(in), ReasonPrivacyRequestFailing)
		checkReasonKind(t, r)
		if r.Concerns != ConcernsNoRecording || len(r.Concepts) != 0 || r.StandsInTheWayOf(statisticsWalk) {
			t.Errorf("%s: reason = %+v; a privacy request is not about recording", name, r)
		}
	}
}

func TestTheVerdictReplyCarriesEachReasonsKindAndConcepts(t *testing.T) {
	store := verdictStore()
	stale := verdictNow.Add(-3 * time.Hour)
	in := verdictInputOf(store, setRow(cleanRows(store), "order", func(r map[string]any) {
		r["lastError"] = "shopify admin: HTTP 503"
		r["lastReconcileAt"] = stale.Add(-2 * time.Minute).Format(time.RFC3339)
		r["lastAttemptAt"] = stale.Format(time.RFC3339)
	}))
	in.GivenUp = []map[string]any{givenUp("products/update", verdictNow.Add(-time.Minute))}
	in.GivenUpFloor = true
	in.Privacy = privacyFailing{Count: 1}
	v := computeVerdict(in)
	if len(v.Reasons) != 3 {
		t.Fatalf("reasons = %v; want a failing check, changes not recorded and a privacy request", reasonCodes(v))
	}
	// Read back as a client reads it: through JSON.
	raw, err := json.Marshal(verdictPayload(v))
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	reasons, _ := reply["reasons"].([]any)
	if len(reasons) != len(v.Reasons) {
		t.Fatalf("reply reasons = %v", reply["reasons"])
	}
	for i, each := range reasons {
		m := each.(map[string]any)
		concepts, isList := m["concepts"].([]any)
		if _, ok := m["concerns"].(string); !ok || !isList {
			t.Errorf("reason %d = %v; want concerns and concepts, a list even when empty", i, m)
		}
		if v.Reasons[i].Concerns != ConcernsConcepts && len(concepts) != 0 {
			t.Errorf("reason %d = %v; concepts are empty unless it concerns concepts", i, m)
		}
		back := VerdictReasonFromPayload(m)
		want := v.Reasons[i]
		if back.Code != want.Code || back.Concerns != want.Concerns || !slices.Equal(back.Concepts, want.Concepts) {
			t.Errorf("reason %d read back = %+v; want code %q, concerns %q, concepts %v", i, back, want.Code, want.Concerns, want.Concepts)
		}
		if back.StandsInTheWayOf(statisticsWalk) != want.StandsInTheWayOf(statisticsWalk) {
			t.Errorf("reason %d read back answers otherwise than the verdict's own", i)
		}
	}
	// The map the reply is built from, before JSON, reads back the same.
	direct := verdictPayload(v)["reasons"].([]any)[0].(map[string]any)
	if back := VerdictReasonFromPayload(direct); !slices.Equal(back.Concepts, []string{"order"}) {
		t.Errorf("read back from the reply's own map = %+v", back)
	}
	if back := VerdictReasonFromPayload(map[string]any{"code": ReasonCheckFailing, "concerns": ConcernsConcepts, "concepts": []string{"order"}}); !slices.Equal(back.Concepts, []string{"order"}) {
		t.Errorf("a []string of concepts read back = %+v", back)
	}
}
