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
					if !strings.HasPrefix(name.Name, "Reason") {
						continue
					}
					if i >= len(vs.Values) {
						t.Errorf("%s: %s takes its value implicitly; a reason code is a string literal", path, name.Name)
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Errorf("%s: %s is not a string literal; a reason code is one, so the gate can read it", path, name.Name)
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

// TestEveryReasonIsBuiltWithAReasonConstant reads every VerdictReason
// literal of the package's non-test files: each is keyed, and its Code is one
// of the package's Reason* constants, so a reason cannot be built with a bare
// string the classification does not see. VerdictReasonFromPayload, which
// reads a code back from a reply, is the one exception, by name.
func TestEveryReasonIsBuiltWithAReasonConstant(t *testing.T) {
	constants := reasonCodeConstants(t)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	built := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn := ""
			if f, ok := decl.(*ast.FuncDecl); ok {
				fn = f.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if ident, ok := lit.Type.(*ast.Ident); !ok || ident.Name != "VerdictReason" {
					return true
				}
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						t.Errorf("%s: an unkeyed VerdictReason literal", fset.Position(lit.Pos()))
						break
					}
					if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Code" || fn == "VerdictReasonFromPayload" {
						continue
					}
					built++
					if value, ok := kv.Value.(*ast.Ident); !ok || constants[value.Name] == "" {
						t.Errorf("%s: a VerdictReason's Code is not a Reason* constant", fset.Position(kv.Value.Pos()))
					}
				}
				return true
			})
		}
	}
	if built == 0 {
		t.Fatal("no VerdictReason literal with a Code found: the check is reading the wrong files")
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
	// One way, never beside: neither the orders nor the shop is read under
	// the other.
	if (VerdictReason{Code: ReasonCheckFailing, Concerns: ConcernsConcepts, Concepts: []string{"order"}}).StandsInTheWayOf([]string{"shop"}) {
		t.Error("a reason naming orders stands in the way of a walk of the shop alone")
	}
	if (VerdictReason{Code: ReasonApprovalOutdated, Concerns: ConcernsConcepts, Concepts: []string{"shop"}}).StandsInTheWayOf([]string{"order"}) {
		t.Error("a reason naming the shop stands in the way of a walk of orders alone")
	}
	// A reply from an older node: a known code and no concerns.
	older := VerdictReasonFromPayload(map[string]any{"code": ReasonCheckFailing, "domain": "products", "sentence": "The last check of products failed"})
	if older.Concerns != "" || !older.StandsInTheWayOf(walked) {
		t.Errorf("a reason read from an older node's reply = %+v; with no concerns it stands in the way", older)
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

	// A delivery the connector routes to no domain is never counted. It makes
	// the reason whole-store when it could have carried a record of any
	// domain: the bulk export's finish, or a topic the connector does not
	// know (app/scopes_update is one: Apply names it nowhere, and an
	// unreadable header is another). A topic the connector routes to its
	// privacy jobs or to the uninstall carries no mirrored record and leaves
	// the reason by concept.
	noHeader := givenUp("products/update", after)
	noHeader["headersJson"] = "{not json"
	for name, delivery := range map[string]map[string]any{
		"the bulk export's finish": givenUp("bulk_operations/finish", after),
		"an unknown topic":         givenUp("app/scopes_update", after),
		"an unreadable header":     noHeader,
	} {
		r = reason(append(slices.Clone(products), delivery), false)
		if r.Concerns != ConcernsWholeStore || len(r.Concepts) != 0 || !r.StandsInTheWayOf(walked) {
			t.Errorf("beside %s: %+v; want the whole store", name, r)
		}
		if r.Count != 2 {
			t.Errorf("beside %s: count = %d; a delivery routed to no domain is still not counted", name, r.Count)
		}
	}
	for _, topic := range []string{generated.TopicCustomersRedact, generated.TopicCustomersDataRequest, generated.TopicShopRedact, TopicAppUninstalled} {
		r = reason(append(slices.Clone(products), givenUp(topic, after)), false)
		if r.Concerns != ConcernsConcepts || !slices.Equal(r.Concepts, []string{"product"}) || r.Count != 2 {
			t.Errorf("beside %s, which carries no mirrored record: %+v; want concepts [product]", topic, r)
		}
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

func TestAnOutdatedApprovalNamesTheConceptsTheOriginRefuses(t *testing.T) {
	// The recorded grant is written at connect and never refreshed. A store on
	// its own app whose recorded grant lacks only read_all_orders frees
	// everything; when the origin then refuses its orders, the order row reads
	// not granted and that is what the reason names.
	own := Store{
		ID: testStoreID, Domain: "acme-widgets.myshopify.com", AdminTokenRef: "ACME_ADMIN", Status: StatusLive,
		ScopesGranted: without(ConnectScopes(), "read_all_orders"),
	}
	before := reasonNamed(t, computeVerdict(verdictInputOf(own, cleanRows(own))), ReasonApprovalOutdated)
	if before.Concerns != ConcernsNoRecording {
		t.Fatalf("before the refusal: %+v; want nothing recorded held", before)
	}
	rows := setRow(cleanRows(own), "order", func(r map[string]any) {
		r["lastError"] = memqlsync.NotGranted(ConnectorName, "read_orders").Error()
		r["lastReconcileAt"] = ""
	})
	v := computeVerdict(verdictInputOf(own, rows))
	if codes := reasonCodes(v); len(codes) != 1 {
		t.Fatalf("reasons = %v; the refused orders are the outdated approval's, and no failing check", codes)
	}
	r := reasonNamed(t, v, ReasonApprovalOutdated)
	checkReasonKind(t, r)
	if r.Concerns != ConcernsConcepts || !slices.Equal(r.Concepts, []string{"order"}) || !r.StandsInTheWayOf(statisticsWalk) {
		t.Errorf("after the refusal: %+v; want concepts [order], in the way of the walk", r)
	}

	// A failed backfill refused the same way counts as the row's refusal.
	rows = setRow(cleanRows(own), "order", func(r map[string]any) {
		r["backfillStatus"] = "failed"
		r["lastError"] = backfillRefusedPrefix + memqlsync.NotGranted(ConnectorName, "read_orders").Error()
	})
	r = reasonNamed(t, computeVerdict(verdictInputOf(own, rows)), ReasonApprovalOutdated)
	if !slices.Contains(r.Concepts, "order") {
		t.Errorf("a refused backfill: %+v; want order named", r)
	}

	// The undeclared-data refusal is its own reason, and not this one's.
	rows = setRow(cleanRows(own), "customer", func(r map[string]any) {
		r["lastError"] = protectedRefusal
		r["lastReconcileAt"] = ""
		r["backfillStatus"] = "none"
	})
	v = computeVerdict(verdictInputOf(own, rows))
	if r := reasonNamed(t, v, ReasonApprovalOutdated); r.Concerns != ConcernsNoRecording {
		t.Errorf("an undeclared-data refusal: approval %+v; the refusal is data_not_declared's", r)
	}
	reasonNamed(t, v, ReasonDataNotDeclared)
}

// replyOf is the verdict's payload as a client reads it, through JSON.
func replyOf(t *testing.T, v Verdict) map[string]any {
	t.Helper()
	raw, err := json.Marshal(verdictPayload(v))
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func TestTheVerdictNamesTheConceptsOutsideWhatTheStoreRecords(t *testing.T) {
	// A store row from before grants were recorded: nothing is known to be
	// missing from the grant; the origin refuses the orders, and the order row
	// reads not granted. That raises the approval by itself (fix round 2), and
	// the reply says so in machine form a second way.
	store := verdictStore()
	store.ScopesGranted = nil
	rows := setRow(cleanRows(store), "order", func(r map[string]any) {
		r["lastError"] = memqlsync.NotGranted(ConnectorName, "read_orders").Error()
		r["lastReconcileAt"] = ""
	})
	v := computeVerdict(verdictInputOf(store, rows))
	if !slices.Equal(reasonCodes(v), []string{ReasonApprovalOutdated}) {
		t.Fatalf("reasons = %v; the origin's refusal is the approval's reason", reasonCodes(v))
	}
	if !slices.Contains(v.OutsideConcepts, "order") || !sort.StringsAreSorted(v.OutsideConcepts) {
		t.Errorf("outside concepts = %v; want order, sorted", v.OutsideConcepts)
	}
	// The display lists are what they were.
	if !slices.Equal(v.Outside, outsideOf(store, rowsByConcept(rows))) || slices.Contains(v.Outside, "orders") {
		t.Errorf("outside = %v; the display list is unchanged", v.Outside)
	}
	if !slices.Equal(v.Withheld, []string{}) {
		t.Errorf("withheld = %v; unchanged", v.Withheld)
	}
	reply := replyOf(t, v)
	if _, ok := reply["outsideConcepts"].([]any); !ok {
		t.Fatalf("reply outsideConcepts = %#v; want a list", reply["outsideConcepts"])
	}
	for concept, want := range map[string]bool{"order": true, "orderLineItem": true, "refund": true, "v1:shopify:refund": true, "shop": false} {
		outside, known := OutsideWhatTheStoreRecords(reply, []string{concept})
		if !known || outside != want {
			t.Errorf("%s: outside %v, known %v; want outside %v, known", concept, outside, known, want)
		}
	}
	if outside, known := OutsideWhatTheStoreRecords(reply, statisticsWalk); !known || !outside {
		t.Errorf("the walk: outside %v, known %v; want outside", outside, known)
	}

	// Customers outside do not put orders outside.
	store = verdictStore()
	store.ScopesGranted = nil
	rows = setRow(cleanRows(store), "customer", func(r map[string]any) {
		r["lastError"] = memqlsync.NotGranted(ConnectorName, "read_customers").Error()
		r["lastReconcileAt"] = ""
	})
	v = computeVerdict(verdictInputOf(store, rows))
	if !slices.Contains(v.OutsideConcepts, "customer") || slices.Contains(v.OutsideConcepts, "order") {
		t.Errorf("outside concepts = %v; want customer and not order", v.OutsideConcepts)
	}
	if outside, known := OutsideWhatTheStoreRecords(replyOf(t, v), statisticsWalk); !known || outside {
		t.Errorf("customers outside: the walk outside %v, known %v; want clear", outside, known)
	}

	// Every domain the display list names is among the concepts, and a
	// store with nothing outside lists none.
	store = verdictStore()
	v = computeVerdict(verdictInputOf(store, cleanRows(store)))
	for _, concept := range domainsOutside(store) {
		if !slices.Contains(v.OutsideConcepts, generated.ConceptFromID(concept)) {
			t.Errorf("outside concepts = %v; %s is outside the connection", v.OutsideConcepts, concept)
		}
	}
	if outside, known := OutsideWhatTheStoreRecords(replyOf(t, v), statisticsWalk); !known || outside {
		t.Errorf("a store recording everything it asks: outside %v, known %v; want clear", outside, known)
	}
	full := managedStore(ConnectScopes()...)
	full.AdminTokenRef = "ACME_ADMIN"
	if v := computeVerdict(verdictInputOf(full, cleanRows(full))); len(v.OutsideConcepts) != 0 {
		t.Errorf("a store on its own app asking everything: outside concepts = %v; want none", v.OutsideConcepts)
	}
}

func TestAnOlderNodesVerdictLeavesWhatIsOutsideUnknown(t *testing.T) {
	store := verdictStore()
	reply := replyOf(t, computeVerdict(verdictInputOf(store, cleanRows(store))))
	delete(reply, "outsideConcepts")
	outside, known := OutsideWhatTheStoreRecords(reply, statisticsWalk)
	if known || !outside {
		t.Errorf("a reply with no outsideConcepts: outside %v, known %v; want unknown, and outside if the caller reads only the first", outside, known)
	}
	reply["outsideConcepts"] = "order"
	if _, known := OutsideWhatTheStoreRecords(reply, statisticsWalk); known {
		t.Error("an outsideConcepts that is not a list is read as known")
	}
	if outside, known := OutsideWhatTheStoreRecords(nil, statisticsWalk); known || !outside {
		t.Errorf("no reply: outside %v, known %v; want unknown", outside, known)
	}
}

// refusedAtTheOrigin marks a domain's row as the origin refused it, naming a
// scope the connection asks for: not granted.
func refusedAtTheOrigin(rows []map[string]any, concept, scope string) []map[string]any {
	return setRow(rows, concept, func(r map[string]any) {
		r["lastError"] = memqlsync.NotGranted(ConnectorName, scope).Error()
		r["lastReconcileAt"] = ""
	})
}

func TestARowTheOriginRefusesRaisesTheApprovalByItself(t *testing.T) {
	// No recorded grant: nothing is known to be missing, and the order row
	// says the origin refuses orders. It read recording before.
	noGrant := verdictStore()
	noGrant.ScopesGranted = nil
	// A recorded grant that holds every scope, and the origin refuses orders
	// all the same: the grant changed at the origin after it was recorded.
	everything := verdictStore()
	for name, store := range map[string]Store{"no recorded grant": noGrant, "a grant holding every scope": everything} {
		if len(scopesNotApproved(store)) != 0 {
			t.Fatalf("%s: the fixture lacks a scope", name)
		}
		v := computeVerdict(verdictInputOf(store, refusedAtTheOrigin(cleanRows(store), "order", "read_orders")))
		if v.State != VerdictNeedsAttention || !slices.Equal(reasonCodes(v), []string{ReasonApprovalOutdated}) {
			t.Fatalf("%s: state %q, reasons %v; want needs attention with approval_outdated alone", name, v.State, reasonCodes(v))
		}
		r := v.Reasons[0]
		checkReasonKind(t, r)
		if r.Concerns != ConcernsConcepts || !slices.Equal(r.Concepts, []string{"order"}) || !r.StandsInTheWayOf(statisticsWalk) {
			t.Errorf("%s: %+v; want concepts [order], in the way of the walk", name, r)
		}
		// The managed store's sentence and act, as for a grant lacking a
		// scope; with no scope known to be missing, the detail names what the
		// origin refuses instead of an empty list.
		if r.Sentence != "This store approved an older list of permissions." || r.Act != ActApprove ||
			r.ActURL != "https://acme-widgets.myshopify.com/admin/apps" {
			t.Errorf("%s: %+v; want the approval's sentence and act", name, r)
		}
		if r.Detail != "Shopify refuses orders under the store's grant." {
			t.Errorf("%s: detail %q", name, r.Detail)
		}
	}

	// A store on its own app keeps its own sentence and no act.
	own := Store{ID: testStoreID, Domain: "acme-widgets.myshopify.com", AdminTokenRef: "ACME_ADMIN", Status: StatusLive}
	r := reasonNamed(t, computeVerdict(verdictInputOf(own, refusedAtTheOrigin(cleanRows(own), "order", "read_orders"))), ReasonApprovalOutdated)
	if r.Sentence != "This store's own app does not allow everything MemQL records." || r.Act != "" {
		t.Errorf("own app: %+v", r)
	}

	// Customers refused, orders fine: the reason names customers, and the
	// walk is clear.
	v := computeVerdict(verdictInputOf(noGrant, refusedAtTheOrigin(cleanRows(noGrant), "customer", "read_customers")))
	r = reasonNamed(t, v, ReasonApprovalOutdated)
	if !slices.Equal(r.Concepts, []string{"customer"}) || r.StandsInTheWayOf(statisticsWalk) || r.Detail != "Shopify refuses customers under the store's grant." {
		t.Errorf("customers refused: %+v; want concepts [customer], clear of the walk", r)
	}

	// A grant lacking a scope still names the scopes, and a refused row
	// beside it adds its concept.
	older := managedStore(oldManagedList...)
	r = reasonNamed(t, computeVerdict(verdictInputOf(older, refusedAtTheOrigin(cleanRows(older), "order", "read_orders"))), ReasonApprovalOutdated)
	if !strings.Contains(r.Detail, "read_customers") || !slices.Contains(r.Concepts, "order") {
		t.Errorf("an older grant beside a refused row: %+v", r)
	}
}

func TestARefusalThatIsNotTheGrantsRaisesNoApproval(t *testing.T) {
	store := verdictStore()
	store.ScopesGranted = nil
	// Undeclared data is its own reason.
	rows := setRow(cleanRows(store), "order", func(r map[string]any) {
		r["lastError"] = protectedRefusal
		r["lastReconcileAt"] = ""
		r["backfillStatus"] = "none"
	})
	if codes := reasonCodes(computeVerdict(verdictInputOf(store, rows))); !slices.Equal(codes, []string{ReasonDataNotDeclared}) {
		t.Errorf("undeclared data: reasons %v; want data_not_declared alone", codes)
	}
	// A domain the connection does not ask for is outside, and no reason.
	rows = cleanRows(store)
	for _, concept := range domainsOutside(store) {
		rows = refusedAtTheOrigin(rows, generated.ConceptFromID(concept), "read_content")
	}
	if v := computeVerdict(verdictInputOf(store, rows)); len(v.Reasons) != 0 || v.State != VerdictRecording {
		t.Errorf("refused rows outside the connection: state %q, reasons %v; want recording", v.State, reasonCodes(v))
	}
	// A row refused as not requested is outside too.
	rows = setRow(cleanRows(store), "order", func(r map[string]any) {
		r["lastError"] = memqlsync.NotRequested(ConnectorName).Error()
		r["lastReconcileAt"] = ""
	})
	if codes := reasonCodes(computeVerdict(verdictInputOf(store, rows))); len(codes) != 0 {
		t.Errorf("not requested: reasons %v; want none", codes)
	}
}
