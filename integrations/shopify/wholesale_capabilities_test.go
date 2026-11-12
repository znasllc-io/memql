package shopify

import (
	"context"
	"slices"
	"strconv"
	"strings"
	stdsync "sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// wholesale_capabilities_test.go -- the tag route against the package's fake
// Admin API: a buyer with no customer gets one, a second grant changes
// nothing, a refusal carries its code, and only the wholesale pack's stamped
// call reaches any of the five builtins.

// fakeCustomers is a store's customers as the fake Admin API answers them:
// the email read, customerCreate and the two tag mutations, over one list.
type fakeCustomers struct {
	mu    stdsync.Mutex
	list  []fakeCustomer
	next  int
	taken bool // customerCreate answers "Email has already been taken" once, then the customer exists
}

type fakeCustomer struct {
	id, email string
	tags      []string
}

// customersOn scripts h's Admin API with a customer list, and answers it.
func customersOn(h *testHarness, existing ...fakeCustomer) *fakeCustomers {
	fc := &fakeCustomers{list: existing, next: 100}
	h.admin.replyWith("ShopifyCustomerByEmail", func(vars map[string]any) any {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		q, _ := vars["query"].(string)
		nodes := []any{}
		for _, c := range fc.list {
			if strings.Contains(strings.ToLower(q), strings.ToLower(c.email)) {
				nodes = append(nodes, map[string]any{"id": c.id, "email": c.email, "tags": append([]string{}, c.tags...)})
			}
		}
		return map[string]any{"customers": map[string]any{"nodes": nodes}}
	})
	h.admin.replyWith("ShopifyCustomerCreate", func(vars map[string]any) any {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		input, _ := vars["input"].(map[string]any)
		email, _ := input["email"].(string)
		if fc.taken {
			// Another grant created the customer a moment ago.
			fc.taken = false
			fc.list = append(fc.list, fakeCustomer{id: "gid://shopify/Customer/77", email: email})
			return map[string]any{"customerCreate": map[string]any{
				"customer":   nil,
				"userErrors": []any{map[string]any{"field": []any{"email"}, "message": "Email has already been taken"}},
			}}
		}
		fc.next++
		id := "gid://shopify/Customer/" + strconv.Itoa(fc.next)
		fc.list = append(fc.list, fakeCustomer{id: id, email: email})
		return map[string]any{"customerCreate": map[string]any{
			"customer":   map[string]any{"id": id, "email": email},
			"userErrors": []any{},
		}}
	})
	tagOp := func(field string, add bool) func(vars map[string]any) any {
		return func(vars map[string]any) any {
			fc.mu.Lock()
			defer fc.mu.Unlock()
			id, _ := vars["id"].(string)
			tags, _ := vars["tags"].([]any)
			for i := range fc.list {
				if fc.list[i].id != id {
					continue
				}
				for _, t := range tags {
					tag, _ := t.(string)
					if add && !slices.Contains(fc.list[i].tags, tag) {
						fc.list[i].tags = append(fc.list[i].tags, tag)
					}
					if !add {
						fc.list[i].tags = slices.DeleteFunc(fc.list[i].tags, func(s string) bool { return s == tag })
					}
				}
			}
			return map[string]any{field: map[string]any{"node": map[string]any{"id": id}, "userErrors": []any{}}}
		}
	}
	h.admin.replyWith("ShopifyTagsAdd", tagOp("tagsAdd", true))
	h.admin.replyWith("ShopifyTagsRemove", tagOp("tagsRemove", false))
	return fc
}

func (fc *fakeCustomers) tagsOf(id string) []string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for _, c := range fc.list {
		if c.id == id {
			return append([]string{}, c.tags...)
		}
	}
	return nil
}

// pack is the wholesale pack's own call as engineCaller makes it: internal
// origin and the pack's mark, acting as the application's merchant.
func pack() context.Context {
	return auth.ContextWithPackCaller(auth.ContextWithInternalOrigin(
		auth.ContextWithUserActor(context.Background(), "user-1")), "wholesale")
}

// automationStep is a registered automation's step, of any package on the
// instance: internal origin, which the automation executor stamps, and no
// pack's mark, which nothing in DSL can set.
func automationStep() context.Context {
	return auth.ContextWithInternalOrigin(actorCtx("system:automation", auth.RoleReader))
}

func tagArgs(email, name string) map[string]any {
	return map[string]any{"storeId": testStoreID, "buyerEmail": email, "buyerName": name}
}

func TestAMissingCustomerIsCreatedOnceAndTagged(t *testing.T) {
	h := newHarness(t)
	fc := customersOn(h)
	i := NewIntegration(h.conn)

	out := invoke(t, pack(), i.handleTagWholesaleCustomer, tagArgs("  Ada@Example.com ", "Ada Example"))
	if out["status"] != "ok" || out["customerCreated"] != true {
		t.Fatalf("reply = %v, want ok with customerCreated true", out)
	}
	id, _ := out["customerId"].(string)
	if id == "" {
		t.Fatalf("the reply names no customer: %v", out)
	}
	if got := h.admin.countOp("ShopifyCustomerCreate"); got != 1 {
		t.Fatalf("customerCreate was called %d times, want 1", got)
	}
	for _, r := range h.admin.seen() {
		if r.Operation == "ShopifyCustomerByEmail" {
			if q, _ := r.Variables["query"].(string); !strings.Contains(q, `"ada@example.com"`) {
				t.Errorf("the read asked %q; the address is sent trimmed and lower-cased", q)
			}
		}
		if r.Operation != "ShopifyCustomerCreate" {
			continue
		}
		input, _ := r.Variables["input"].(map[string]any)
		if input["email"] != "ada@example.com" || input["firstName"] != "Ada" || input["lastName"] != "Example" {
			t.Errorf("customerCreate input = %v, want the address and the name split", input)
		}
		for key := range input {
			if strings.Contains(strings.ToLower(key), "marketing") {
				t.Errorf("customerCreate sent %s; a grant gives no marketing consent", key)
			}
		}
	}
	if !slices.Contains(fc.tagsOf(id), WholesaleTagDefault) {
		t.Fatalf("the created customer carries %v, want %s", fc.tagsOf(id), WholesaleTagDefault)
	}
	if ref, _ := out["reference"].(string); ref != "customer="+id+";tag="+WholesaleTagDefault {
		t.Fatalf("reference = %q", ref)
	}
}

func TestASecondTagCreatesNothingAndAddsNoSecondTag(t *testing.T) {
	h := newHarness(t)
	customersOn(h)
	i := NewIntegration(h.conn)
	first := invoke(t, pack(), i.handleTagWholesaleCustomer, tagArgs("ada@example.com", "Ada Example"))
	creates, adds := h.admin.countOp("ShopifyCustomerCreate"), h.admin.countOp("ShopifyTagsAdd")

	second := invoke(t, pack(), i.handleTagWholesaleCustomer, tagArgs("ada@example.com", "Ada Example"))
	if second["status"] != "ok" || second["customerCreated"] != false || second["customerId"] != first["customerId"] {
		t.Fatalf("second reply = %v, want ok on the same customer with customerCreated false", second)
	}
	if got := h.admin.countOp("ShopifyCustomerCreate") - creates; got != 0 {
		t.Fatalf("the second grant called customerCreate %d times", got)
	}
	if got := h.admin.countOp("ShopifyTagsAdd") - adds; got != 0 {
		t.Fatalf("the second grant sent %d tagsAdd for a tag the customer already carries", got)
	}
}

func TestADuplicateEmailRefusalFromCreateReadsAgainAndTags(t *testing.T) {
	h := newHarness(t)
	fc := customersOn(h)
	fc.taken = true
	i := NewIntegration(h.conn)

	out := invoke(t, pack(), i.handleTagWholesaleCustomer, tagArgs("ada@example.com", "Ada Example"))
	if out["status"] != "ok" || out["customerId"] != "gid://shopify/Customer/77" || out["customerCreated"] != false {
		t.Fatalf("reply = %v, want ok on the customer the other grant created", out)
	}
	if got := h.admin.countOp("ShopifyCustomerByEmail"); got != 2 {
		t.Fatalf("the address was read %d times, want 2 (before the create and after it was refused)", got)
	}
	if !slices.Contains(fc.tagsOf("gid://shopify/Customer/77"), WholesaleTagDefault) {
		t.Fatal("the customer found again was not tagged")
	}
}

func TestACustomerCreateRefusedForAnotherReasonIsCustomerRefused(t *testing.T) {
	h := newHarness(t)
	customersOn(h)
	h.admin.replyWith("ShopifyCustomerCreate", func(map[string]any) any {
		return map[string]any{"customerCreate": map[string]any{
			"customer":   nil,
			"userErrors": []any{map[string]any{"field": []any{"email"}, "message": "Email is invalid"}},
		}}
	})
	out := invoke(t, pack(), NewIntegration(h.conn).handleTagWholesaleCustomer, tagArgs("ada@example.com", "Ada Example"))
	if out["status"] != "refused" || out["code"] != "customer_refused" {
		t.Fatalf("reply = %v, want refused with code customer_refused", out)
	}
	if strings.Contains(out["reason"].(string), "ada@example.com") {
		t.Fatalf("the refusal carries the address: %v", out["reason"])
	}
	if h.admin.countOp("ShopifyTagsAdd") != 0 {
		t.Fatal("a refused create still sent a tag")
	}
}

func TestACustomerAccessDeniedIsScopeMissing(t *testing.T) {
	for _, op := range []string{"ShopifyCustomerByEmail", "ShopifyCustomerCreate"} {
		h := newHarness(t)
		customersOn(h)
		h.admin.graphQLError(op, "ACCESS_DENIED", "Access denied for customers field. Required access: `read_customers` access scope.")
		out := invoke(t, pack(), NewIntegration(h.conn).handleTagWholesaleCustomer, tagArgs("ada@example.com", "Ada Example"))
		if out["status"] != "refused" || out["code"] != "scope_missing" {
			t.Errorf("%s denied: reply = %v, want refused with code scope_missing", op, out)
		}
		if h.admin.countOp("ShopifyTagsAdd") != 0 {
			t.Errorf("%s denied: a tag was still sent", op)
		}
	}
}

func TestACeilingRefusalCarriesPlanCeiling(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyCatalogsCount", map[string]any{"catalogsCount": map[string]any{"count": 3}})
	out := invoke(t, pack(), NewIntegration(h.conn).handleProvisionWholesale, map[string]any{
		"storeId": testStoreID, "applicationId": "v1:wholesale:application:app-1",
		"companyName": "Example Cycles", "buyerEmail": "ada@example.com",
	})
	if out["status"] != "refused" || out["code"] != "plan_ceiling" {
		t.Fatalf("reply = %v, want refused with code plan_ceiling", out)
	}
}

// AN OUTAGE IS AN ERROR, NEVER A REFUSAL: the entitlement stays pending and is
// retried.
func TestAnAdminOutageOnTheTagRouteIsAnError(t *testing.T) {
	h := newHarness(t)
	customersOn(h)
	h.admin.httpStatus("ShopifyCustomerByEmail", 503)
	h.conn.admin.MaxRetries = 0
	nodes, err := NewIntegration(h.conn).handleTagWholesaleCustomer(pack(), tagArgs("ada@example.com", "Ada Example"), 0)
	if err == nil {
		t.Fatalf("an outage answered %v with no error", nodes)
	}
	if strings.Contains(err.Error(), "ada@example.com") {
		t.Fatalf("the error carries the address: %v", err)
	}
}

func TestTwoCustomersForOneAddressTagTheFirstAndSaySo(t *testing.T) {
	h := newHarness(t)
	fc := customersOn(h,
		fakeCustomer{id: "gid://shopify/Customer/1", email: "ada@example.com"},
		fakeCustomer{id: "gid://shopify/Customer/2", email: "Ada@example.com"})
	out := invoke(t, pack(), NewIntegration(h.conn).handleTagWholesaleCustomer, tagArgs("ada@example.com", "Ada Example"))
	if out["status"] != "ok" || out["customerId"] != "gid://shopify/Customer/1" {
		t.Fatalf("reply = %v, want ok on the customer the origin lists first", out)
	}
	if detail, _ := out["detail"].(string); !strings.Contains(detail, "2 customers") {
		t.Fatalf("detail = %q, want it to say two customers have the address", detail)
	}
	if len(fc.tagsOf("gid://shopify/Customer/2")) != 0 {
		t.Fatal("the second customer was tagged")
	}
}

func TestUntaggingAnAddressWithNoCustomerSucceeds(t *testing.T) {
	h := newHarness(t)
	customersOn(h)
	out := invoke(t, pack(), NewIntegration(h.conn).handleUntagWholesaleCustomer,
		map[string]any{"storeId": testStoreID, "buyerEmail": "ada@example.com"})
	if out["status"] != "ok" {
		t.Fatalf("reply = %v, want ok: an address with no customer carries no tag to remove", out)
	}
	if ops := h.admin.seenOps(); !slices.Equal(ops, []string{"ShopifyCustomerByEmail"}) {
		t.Fatalf("Shopify was asked %v; only the read may be sent", ops)
	}
}

func TestUntaggingACustomerWithoutTheTagSendsNoMutation(t *testing.T) {
	h := newHarness(t)
	fc := customersOn(h, fakeCustomer{id: "gid://shopify/Customer/1", email: "ada@example.com", tags: []string{"vip"}})
	out := invoke(t, pack(), NewIntegration(h.conn).handleUntagWholesaleCustomer,
		map[string]any{"storeId": testStoreID, "buyerEmail": "ada@example.com"})
	if out["status"] != "ok" || h.admin.countOp("ShopifyTagsRemove") != 0 {
		t.Fatalf("reply = %v with %d tagsRemove; a tag that is absent is not removed", out, h.admin.countOp("ShopifyTagsRemove"))
	}
	if !slices.Equal(fc.tagsOf("gid://shopify/Customer/1"), []string{"vip"}) {
		t.Fatal("another tag was touched")
	}

	fc.list[0].tags = []string{"vip", WholesaleTagDefault}
	invoke(t, pack(), NewIntegration(h.conn).handleUntagWholesaleCustomer,
		map[string]any{"storeId": testStoreID, "buyerEmail": "ada@example.com"})
	if !slices.Equal(fc.tagsOf("gid://shopify/Customer/1"), []string{"vip"}) || h.admin.countOp("ShopifyTagsRemove") != 1 {
		t.Fatalf("the present tag was not removed once: %v", fc.tagsOf("gid://shopify/Customer/1"))
	}
}

// A FIRST GRANT THAT FAILED names its adapter with an empty reference, so a
// later revoke reaches the B2B builtin with nothing to draft: a clean success
// that sends nothing to Shopify.
func TestRevokingAnEmptyReferenceSucceedsAndSendsNothing(t *testing.T) {
	h := newHarness(t)
	out := invoke(t, pack(), NewIntegration(h.conn).handleRevokeWholesale,
		map[string]any{"storeId": testStoreID, "reference": ""})
	if out["status"] != "ok" {
		t.Fatalf("reply = %v, want ok", out)
	}
	if n := len(h.admin.seen()); n != 0 {
		t.Fatalf("Shopify was asked %d times for a revoke with nothing to draft", n)
	}
}

func TestEachWholesaleBuiltinRefusesACallWithoutInternalOrigin(t *testing.T) {
	for n := range wholesaleBuiltins(&Integration{}) {
		for name, who := range map[string]context.Context{
			"no caller":         context.Background(),
			"a client call":     auth.ContextWithClientOrigin(actorCtx("user-1", auth.RoleOwner)),
			"the store's owner": actorCtx("user-1", auth.RoleWriter),
			"a cluster owner":   actorCtx("user-9", auth.RoleOwner),
			"a developer":       actorCtx("user-2", auth.RoleDeveloper),
			// RULING 57: internal origin alone is any package's automation.
			"an automation's step":           automationStep(),
			"internal origin, no actor":      auth.ContextWithInternalOrigin(context.Background()),
			"the pack's mark, client origin": auth.ContextWithPackCaller(auth.ContextWithClientOrigin(actorCtx("user-1", auth.RoleOwner)), "wholesale"),
			"the pack's mark, no origin":     auth.ContextWithPackCaller(context.Background(), "wholesale"),
			"another pack's mark, internal":  auth.ContextWithPackCaller(auth.ContextWithInternalOrigin(context.Background()), "reviews"),
		} {
			h := newHarness(t)
			withStoreRow(h, nil)
			b := wholesaleBuiltins(NewIntegration(h.conn))[n]
			nodes, err := b.handle(who, onStore(b.args, testStoreID), 0)
			want := "shopify: " + b.builtin + " is called by the wholesale pack, never by a client"
			if err == nil || err.Error() != want {
				t.Errorf("%s by %s: nodes %v, err %v; want %q", b.builtin, name, nodes, err, want)
			}
			if calls := h.engine.calls(); len(calls) != 0 {
				t.Errorf("%s by %s: the engine was asked %v before the refusal", b.builtin, name, calls)
			}
			if got := len(h.admin.seen()); got != 0 {
				t.Errorf("%s by %s: Shopify was asked %d times", b.builtin, name, got)
			}
		}
	}
	// Refused before the connector is looked at, too.
	for _, b := range wholesaleBuiltins(&Integration{}) {
		if _, err := b.handle(context.Background(), map[string]any{}, 0); err == nil {
			t.Errorf("%s on an unconfigured connector answered a client", b.builtin)
		}
	}
}

func TestTheCustomAppPathAsksForWriteCustomers(t *testing.T) {
	if !slices.Contains(ConnectScopes(), "write_customers") {
		t.Fatalf("ConnectScopes() = %v, want write_customers", ConnectScopes())
	}
	own := Store{ID: testStoreID}
	if own.Managed() {
		t.Fatal("the fixture store must be one connected with its own app")
	}
	if !slices.Contains(connectionScopes(own), "write_customers") {
		t.Fatalf("a store connected with its own app asks for %v, want write_customers", connectionScopes(own))
	}
	if !grantIncludes([]string{"write_customers"})["read_customers"] {
		t.Fatal("write_customers must reach read_customers, or the mirror's customer read is reported missing")
	}
	managed := managedConnectScopes()
	if !slices.Contains(managed, "read_customers") || !slices.Contains(managed, "write_customers") {
		t.Fatalf("the managed app asks for %v, want read_customers and write_customers", managed)
	}
}

// companyAt scripts one company, at externalID, for the company search.
func companyAt(h *testHarness, externalID string) {
	h.admin.replyWith("ShopifyCompanyByExternalID", func(vars map[string]any) any {
		nodes := []any{}
		if q, _ := vars["query"].(string); q == "external_id:\""+externalID+"\"" {
			nodes = append(nodes, map[string]any{
				"id": "gid://shopify/Company/1", "externalId": externalID,
				"locations": map[string]any{"nodes": []any{map[string]any{"id": "gid://shopify/CompanyLocation/1"}}},
			})
		}
		return map[string]any{"companies": map[string]any{"nodes": nodes}}
	})
}

func TestACompanyMadeUnderEitherFormOfTheApplicationIdIsFoundAgain(t *testing.T) {
	for _, legacy := range []string{"app-1", "v1:wholesale:application:app-1"} {
		h := newHarness(t)
		withStoreRow(h, map[string]any{"plan": "Shopify Plus"})
		customersOn(h, fakeCustomer{id: "gid://shopify/Customer/1", email: "ada@example.com"})
		companyAt(h, wholesaleExternalIDPrefix+legacy)
		h.admin.reply("ShopifyCompanyAssignContact", map[string]any{"companyAssignCustomerAsContact": map[string]any{"companyContact": map[string]any{"id": "gid://shopify/CompanyContact/1"}, "userErrors": []any{}}})
		h.admin.reply("ShopifyCatalogCreate", map[string]any{"catalogCreate": map[string]any{"catalog": map[string]any{"id": "gid://shopify/CompanyLocationCatalog/1"}, "userErrors": []any{}}})
		h.admin.reply("ShopifyPriceListCreate", map[string]any{"priceListCreate": map[string]any{"priceList": map[string]any{"id": "gid://shopify/PriceList/1"}, "userErrors": []any{}}})

		out := invoke(t, pack(), NewIntegration(h.conn).handleProvisionWholesale, map[string]any{
			"storeId": testStoreID, "applicationId": "v1:wholesale:application:app-1",
			"companyName": "Example Cycles", "buyerName": "Ada Example", "buyerEmail": "ada@example.com",
		})
		if out["status"] != "ok" {
			t.Fatalf("a company at %q: reply = %v", legacy, out)
		}
		if got := h.admin.countOp("ShopifyCompanyCreate"); got != 0 {
			t.Errorf("a company at %q: companyCreate was called %d times, want 0", legacy, got)
		}
		if refs := DecodeWholesaleRefs(out["reference"].(string)); refs.CompanyGID != "gid://shopify/Company/1" {
			t.Errorf("a company at %q: the grant landed on %q", legacy, refs.CompanyGID)
		}
	}
}

// A NEW COMPANY IS CREATED UNDER THE ID IT WAS GIVEN, after both forms were
// looked for.
func TestANewCompanyIsMadeUnderTheIdItWasGiven(t *testing.T) {
	h := newHarness(t)
	withStoreRow(h, map[string]any{"plan": "Shopify Plus"})
	customersOn(h, fakeCustomer{id: "gid://shopify/Customer/1", email: "ada@example.com"})
	companyAt(h, "nobody")
	h.admin.reply("ShopifyCompanyCreate", map[string]any{"companyCreate": map[string]any{"company": map[string]any{"id": "gid://shopify/Company/2", "locations": map[string]any{"nodes": []any{map[string]any{"id": "gid://shopify/CompanyLocation/2"}}}}, "userErrors": []any{}}})
	h.admin.reply("ShopifyCompanyAssignContact", map[string]any{"companyAssignCustomerAsContact": map[string]any{"companyContact": map[string]any{"id": "gid://shopify/CompanyContact/1"}, "userErrors": []any{}}})
	h.admin.reply("ShopifyCatalogCreate", map[string]any{"catalogCreate": map[string]any{"catalog": map[string]any{"id": "gid://shopify/CompanyLocationCatalog/1"}, "userErrors": []any{}}})
	h.admin.reply("ShopifyPriceListCreate", map[string]any{"priceListCreate": map[string]any{"priceList": map[string]any{"id": "gid://shopify/PriceList/1"}, "userErrors": []any{}}})

	invoke(t, pack(), NewIntegration(h.conn).handleProvisionWholesale, map[string]any{
		"storeId": testStoreID, "applicationId": "v1:wholesale:application:app-1",
		"companyName": "Example Cycles", "buyerEmail": "ada@example.com",
	})
	var searched []string
	for _, r := range h.admin.seen() {
		switch r.Operation {
		case "ShopifyCompanyByExternalID":
			q, _ := r.Variables["query"].(string)
			searched = append(searched, q)
		case "ShopifyCompanyCreate":
			input, _ := r.Variables["input"].(map[string]any)
			company, _ := input["company"].(map[string]any)
			if company["externalId"] != wholesaleExternalIDPrefix+"v1:wholesale:application:app-1" {
				t.Errorf("the company was made under %v, want the id it was given", company["externalId"])
			}
		}
	}
	want := []string{`external_id:"memql-wholesale:v1:wholesale:application:app-1"`, `external_id:"memql-wholesale:app-1"`}
	if !slices.Equal(searched, want) {
		t.Fatalf("searched %v, want %v", searched, want)
	}
}

// RULING 58: A REVOKE ACTS ON THE CUSTOMER THE GRANT RECORDED.

func untagArgs(customerID, email string) map[string]any {
	return map[string]any{"storeId": testStoreID, "customerId": customerID, "buyerEmail": email}
}

// customerByIdOn answers the by-id read from the same customer list.
func (fc *fakeCustomers) byIdOn(h *testHarness) {
	h.admin.replyWith("ShopifyCustomerById", func(vars map[string]any) any {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		id, _ := vars["id"].(string)
		for _, c := range fc.list {
			if c.id == id {
				return map[string]any{"customer": map[string]any{"id": c.id, "tags": append([]string{}, c.tags...)}}
			}
		}
		return map[string]any{"customer": nil}
	})
}

func TestARevokeAfterTheBuyerChangedTheirAddressUntagsTheRecordedCustomer(t *testing.T) {
	h := newHarness(t)
	fc := customersOn(h)
	fc.byIdOn(h)
	i := NewIntegration(h.conn)
	granted := invoke(t, pack(), i.handleTagWholesaleCustomer, tagArgs("ada@example.com", "Ada Example"))
	id := granted["customerId"].(string)
	fc.mu.Lock()
	fc.list[0].email = "ada.new@example.com"
	fc.mu.Unlock()

	out := invoke(t, pack(), i.handleUntagWholesaleCustomer, untagArgs(id, "ada@example.com"))
	if out["status"] != "ok" {
		t.Fatalf("reply = %v", out)
	}
	if slices.Contains(fc.tagsOf(id), WholesaleTagDefault) {
		t.Fatal("the recorded customer still carries the tag after a revoke that reported success")
	}
}

func TestARevokeUntagsTheRecordedCustomerAmongTwoWithOneAddress(t *testing.T) {
	h := newHarness(t)
	fc := customersOn(h,
		fakeCustomer{id: "gid://shopify/Customer/1", email: "ada@example.com"},
		fakeCustomer{id: "gid://shopify/Customer/2", email: "ada@example.com", tags: []string{WholesaleTagDefault}})
	fc.byIdOn(h)
	invoke(t, pack(), NewIntegration(h.conn).handleUntagWholesaleCustomer, untagArgs("gid://shopify/Customer/2", "ada@example.com"))
	if slices.Contains(fc.tagsOf("gid://shopify/Customer/2"), WholesaleTagDefault) {
		t.Fatal("the tag is still on the customer the reference names")
	}
	if h.admin.countOp("ShopifyCustomerByEmail") != 0 {
		t.Fatal("a reference naming a customer was revoked by address")
	}
}

func TestARevokeByAddressRemovesTheTagFromEveryCustomerHoldingIt(t *testing.T) {
	h := newHarness(t)
	fc := customersOn(h,
		fakeCustomer{id: "gid://shopify/Customer/1", email: "ada@example.com", tags: []string{WholesaleTagDefault}},
		fakeCustomer{id: "gid://shopify/Customer/2", email: "ada@example.com", tags: []string{WholesaleTagDefault}},
		fakeCustomer{id: "gid://shopify/Customer/3", email: "ada@example.com"})
	invoke(t, pack(), NewIntegration(h.conn).handleUntagWholesaleCustomer, untagArgs("", "ada@example.com"))
	for _, id := range []string{"gid://shopify/Customer/1", "gid://shopify/Customer/2"} {
		if slices.Contains(fc.tagsOf(id), WholesaleTagDefault) {
			t.Errorf("%s still carries the tag", id)
		}
	}
	if got := h.admin.countOp("ShopifyTagsRemove"); got != 2 {
		t.Fatalf("tagsRemove sent %d times, want 2 (only to the customers carrying the tag)", got)
	}
}

func TestARevokeOfACustomerSinceDeletedSucceeds(t *testing.T) {
	h := newHarness(t)
	fc := customersOn(h)
	fc.byIdOn(h)
	out := invoke(t, pack(), NewIntegration(h.conn).handleUntagWholesaleCustomer, untagArgs("gid://shopify/Customer/404", "ada@example.com"))
	if out["status"] != "ok" {
		t.Fatalf("reply = %v, want ok: a customer that no longer exists carries no tag", out)
	}
	if ops := h.admin.seenOps(); !slices.Equal(ops, []string{"ShopifyCustomerById"}) {
		t.Fatalf("Shopify was asked %v; only the by-id read may be sent", ops)
	}
}
