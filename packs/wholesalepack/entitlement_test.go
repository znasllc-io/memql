package wholesalepack

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// entitlement_test.go -- the seam, exercised without Shopify (epic
// memql#5533, issues memql#5557, memql#5558 and memql#5559).
//
// Every adapter reaches the outside world through the Caller, so a fake
// Caller is the whole of what these tests need: the state machine, the
// pending-versus-failed distinction and the revoke-through-the-granting-
// adapter rule are decisions over values here, exactly as they are in
// production.

// fakeCaller records what an adapter asked for and answers with a script.
type fakeCaller struct {
	replies map[string]map[string]any
	err     error
	calls   []string
	args    []map[string]any
	actors  []string // the actor each call ran as
}

func (f *fakeCaller) Call(ctx context.Context, construct string, args map[string]any) (map[string]any, error) {
	f.calls = append(f.calls, construct)
	f.args = append(f.args, args)
	f.actors = append(f.actors, actorOf(ctx))
	if f.err != nil {
		return nil, f.err
	}
	if reply, ok := f.replies[construct]; ok {
		return reply, nil
	}
	return map[string]any{"status": "ok"}, nil
}

// internalAsMerchant is a direct call as the provisioning builtins accept
// one: internal origin, as the store's automation arrives, acting as the
// merchant -- the internal-package twin of the live tests'
// auth.ContextWithInternalOrigin(asSiteOwner(merchant)).
func internalAsMerchant() context.Context {
	return auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), "merchant-1"))
}

func approvedFixture() *fixtureReader {
	return &fixtureReader{
		apps: map[string][]map[string]any{
			"app-1": {{
				"id": "app-1", "ownerUserId": "merchant-1", "storeId": "store-live", "companyName": "Acme Trading",
				"applicantName": "Sam Rivers", "applicantEmail": "sam@acme.example",
			}},
		},
		decisions: map[string][]map[string]any{
			"app-1": {{"transition": TransitionApprove}},
		},
		settings: map[string][]map[string]any{
			"store-live": {{
				"storeId": "store-live", "applicationsOpen": true,
				"entitlementAdapter": AdapterCustomerTag,
			}},
		},
		stores: map[string][]map[string]any{
			"store-live": {{"id": "store-live", "plan": "Shopify"}},
		},
		ents: map[string][]map[string]any{},
	}
}

// ---------------------------------------------------------------------------
// Only an approved application has an entitlement
// ---------------------------------------------------------------------------

// THE DECISION LOG DECIDES, not an argument. Provisioning an application
// nobody approved would grant trade terms on the strength of a caller's
// say-so.
func TestOnlyAnApprovedApplicationIsProvisioned(t *testing.T) {
	registerShippedAdapters()
	reader := approvedFixture()
	reader.decisions["app-1"] = nil // back to submitted
	p := &Provider{reader: reader, caller: &fakeCaller{}, writer: &fakeWriter{}}

	_, err := p.provisionEntitlement(internalAsMerchant(),
		map[string]any{"applicationId": "app-1"}, 0)
	if err == nil {
		t.Fatal("an application nobody approved has no entitlement to provision")
	}
	if !strings.Contains(err.Error(), StateSubmitted) {
		t.Fatalf("the refusal must name the state it found, got: %v", err)
	}
}

// A REVOKE NEEDS A REVOKE DECISION FIRST, so the pack's state and its
// history agree. Revoking an entitlement with no decision behind it would
// leave the log saying the buyer is still approved.
func TestRevokeNeedsItsDecisionFirst(t *testing.T) {
	registerShippedAdapters()
	p := &Provider{reader: approvedFixture(), caller: &fakeCaller{}, writer: &fakeWriter{}}
	_, err := p.revokeEntitlement(internalAsMerchant(),
		map[string]any{"applicationId": "app-1"}, 0)
	if err == nil {
		t.Fatal("revoking an approved (not revoked) application must be refused")
	}
}

// ---------------------------------------------------------------------------
// A failed push does not undo an approval
// ---------------------------------------------------------------------------

// PENDING, NOT FAILED, AND THE APPLICATION STAYS APPROVED. An approval is a
// decision somebody made; a push that failed is an operational fact about
// somebody else's API. Collapsing the two would let a Shopify outage
// silently un-approve a merchant's customers.
func TestATransientFailureLeavesTheEntitlementPending(t *testing.T) {
	registerShippedAdapters()
	writer := &fakeWriter{}
	p := &Provider{
		reader: approvedFixture(),
		writer: writer,
		caller: &fakeCaller{err: errors.New("shopify: 503 from the Admin API")},
	}
	if _, err := p.provisionEntitlement(internalAsMerchant(),
		map[string]any{"applicationId": "app-1"}, 0); err != nil {
		t.Fatalf("a push failure must not be returned as an error: %v", err)
	}
	out := writer.only(t, "writeEntitlementRow")
	if out["state"] != EntitlementPending {
		t.Fatalf("state = %v, want %v -- a transient failure is outstanding work, not a refusal",
			out["state"], EntitlementPending)
	}
	if !strings.Contains(asString(out["error"]), "503") {
		t.Fatalf("the error must be recorded on the row, got %v", out["error"])
	}
	if out["adapter"] != AdapterCustomerTag {
		t.Fatalf("adapter = %v, want %v", out["adapter"], AdapterCustomerTag)
	}
}

// A REFUSAL IS `failed` AND FINAL. A store below Plus that already holds
// three catalogs will hold three catalogs tomorrow; retrying it for ever is
// how a queue stops draining.
func TestAnAdapterRefusalIsRecordedAsFailed(t *testing.T) {
	registerShippedAdapters()
	writer := &fakeWriter{}
	p := &Provider{
		reader: approvedFixture(),
		writer: writer,
		caller: &fakeCaller{replies: map[string]map[string]any{
			"shopifyTagWholesaleCustomer": {
				"status": "refused",
				"reason": "no customer on this store has that email",
			},
		}},
	}
	if _, err := p.provisionEntitlement(internalAsMerchant(),
		map[string]any{"applicationId": "app-1"}, 0); err != nil {
		t.Fatalf("provision: %v", err)
	}
	out := writer.only(t, "writeEntitlementRow")
	if out["state"] != EntitlementFailed {
		t.Fatalf("state = %v, want %v -- a refusal will fail identically for ever and must "+
			"not be retried like an outage", out["state"], EntitlementFailed)
	}
	if !strings.Contains(asString(out["error"]), "no customer") {
		t.Fatalf("the reason must survive onto the row, got %v", out["error"])
	}
}

// THE ROW IS WRITTEN EITHER WAY. A builtin that returned the error would
// leave NO row at all, and the merchant would have an approved application
// with nothing anywhere saying a push had been attempted and failed.
func TestAFailedPushStillWritesARow(t *testing.T) {
	registerShippedAdapters()
	writer := &fakeWriter{}
	p := &Provider{
		reader: approvedFixture(),
		writer: writer,
		caller: &fakeCaller{err: errors.New("network unreachable")},
	}
	if _, err := p.provisionEntitlement(internalAsMerchant(),
		map[string]any{"applicationId": "app-1"}, 0); err != nil {
		t.Fatalf("a push failure must not be returned as an error: %v", err)
	}
	wrote := writer.only(t, "writeEntitlementRow")
	if wrote["entitlementId"] != EntitlementRowID("app-1") {
		t.Fatalf("row id = %q, want the derived one", wrote["entitlementId"])
	}
	// A DERIVED ID MUST BE A BARE SLUG, or every write of it is refused.
	if strings.Contains(asString(wrote["entitlementId"]), ":") {
		t.Fatalf("derived entitlement id %q carries a colon; the engine refuses it",
			wrote["entitlementId"])
	}
}

// ---------------------------------------------------------------------------
// A success
// ---------------------------------------------------------------------------

func TestASuccessfulGrantRecordsItsReference(t *testing.T) {
	registerShippedAdapters()
	caller := &fakeCaller{replies: map[string]map[string]any{
		"shopifyTagWholesaleCustomer": {
			"status":    "ok",
			"reference": "customer=gid://shopify/Customer/1;tag=memql-wholesale",
			"detail":    "tagged memql-wholesale on acme.myshopify.com",
		},
	}}
	writer := &fakeWriter{}
	p := &Provider{reader: approvedFixture(), caller: caller, writer: writer}
	nodes, err := p.provisionEntitlement(internalAsMerchant(),
		map[string]any{"applicationId": "app-1"}, 0)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	receipt := decodeOne(t, nodes, err)
	out := writer.only(t, "writeEntitlementRow")
	if out["state"] != EntitlementGranted {
		t.Fatalf("state = %v, want %v", out["state"], EntitlementGranted)
	}
	if !strings.Contains(asString(out["reference"]), "tag=memql-wholesale") {
		t.Fatalf("the reference must be kept so a revoke knows what to undo, got %v", out["reference"])
	}
	if asString(out["error"]) != "" {
		t.Fatalf("a granted entitlement must carry no error, got %v", out["error"])
	}
	// THE BUYER IS READ OFF THE APPLICATION, not supplied.
	if len(caller.args) != 1 || caller.args[0]["buyerEmail"] != "sam@acme.example" {
		t.Fatalf("the adapter must be handed the applicant's own email, got %v", caller.args)
	}
	// AND THE CONSTRAINT THE PACK CANNOT ENFORCE TRAVELS WITH THE RECEIPT.
	if !strings.Contains(asString(receipt["detail"]), "never a discount code") {
		t.Fatalf("the customerTag adapter must record that the tag entitles nobody on its "+
			"own and that a code would leak, got %v", out["detail"])
	}
}

// ---------------------------------------------------------------------------
// No adapter configured
// ---------------------------------------------------------------------------

// A NAMED REFUSAL RATHER THAN A SILENT NO-OP. A merchant who approves an
// application and sees no wholesale prices appear is owed the reason.
func TestNoAdapterConfiguredRefusesByName(t *testing.T) {
	registerShippedAdapters()
	reader := approvedFixture()
	reader.settings["store-live"] = []map[string]any{{
		"storeId": "store-live", "applicationsOpen": true, "entitlementAdapter": "",
	}}
	writer := &fakeWriter{}
	p := &Provider{reader: reader, caller: &fakeCaller{}, writer: writer}
	if _, err := p.provisionEntitlement(internalAsMerchant(),
		map[string]any{"applicationId": "app-1"}, 0); err != nil {
		t.Fatalf("a store with no adapter configured is recorded on the row, not returned: %v", err)
	}
	out := writer.only(t, "writeEntitlementRow")
	if out["errorCode"] != ErrorNoRoute {
		t.Fatalf("errorCode = %v, want %v", out["errorCode"], ErrorNoRoute)
	}
	if !strings.Contains(asString(out["error"]), "entitlementAdapter") {
		t.Fatalf("the row must name the setting to change, got: %v", out["error"])
	}
}

// ---------------------------------------------------------------------------
// Availability: the difference between the two adapters
// ---------------------------------------------------------------------------

// THIS PAIR IS WHAT "PLAN-INDEPENDENT" MEANS. The store row is
// @rowAuthz(clusterOwner) and D10 left that tier unchanged, so an
// unreadable plan is the ORDINARY case for a merchant. shopifyB2B cannot
// establish the catalog ceiling it has to respect and refuses; customerTag
// needs no plan at all and proceeds.
func TestAnUnknownPlanStopsOneAdapterAndNotTheOther(t *testing.T) {
	b2b := NewShopifyB2BAdapter()
	if err := b2b.Available(""); err == nil {
		t.Fatal("shopifyB2B must refuse when the plan is unreadable: it cannot establish " +
			"the catalog ceiling")
	} else if !isAdapterRefusal(err) {
		t.Fatal("that refusal must be final rather than retried -- the tier will not change " +
			"because we asked again")
	}
	if err := b2b.Available("Shopify"); err != nil {
		t.Fatalf("shopifyB2B must admit a non-Plus plan: since 2026-04-02 companies, payment "+
			"terms, volume pricing and three catalogs are available below Plus: %v", err)
	}

	tag := NewCustomerTagAdapter("")
	if err := tag.Available(""); err != nil {
		t.Fatalf("customerTag must not care about the plan -- that is the whole point of it: %v", err)
	}
	if err := tag.Available("Basic"); err != nil {
		t.Fatalf("customerTag must admit every plan: %v", err)
	}
}

// NO TIER GATE. The obvious implementation refuses below Plus and it would
// be WRONG since 2026-04-02. The real constraint is the catalog COUNT,
// which is checked where it can be measured.
func TestShopifyB2BDoesNotGateOnPlus(t *testing.T) {
	b2b := NewShopifyB2BAdapter()
	for _, plan := range []string{"Basic", "Shopify", "Advanced", "Shopify Plus", "Development"} {
		if err := b2b.Available(plan); err != nil {
			t.Errorf("plan %q must be admitted; the ceiling is a catalog count, not a tier: %v",
				plan, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Revoking through the adapter that granted
// ---------------------------------------------------------------------------

// A MERCHANT WHO SWITCHES ADAPTERS MUST STILL BE ABLE TO UNDO WHAT THE OLD
// ONE GAVE. The settings value has already moved on, so the adapter is read
// off the entitlement ROW.
func TestRevokeUsesTheAdapterThatGranted(t *testing.T) {
	registerShippedAdapters()
	reader := approvedFixture()
	reader.decisions["app-1"] = []map[string]any{{"transition": TransitionRevoke}}
	// Settings now name the B2B adapter...
	reader.settings["store-live"] = []map[string]any{{
		"storeId": "store-live", "applicationsOpen": true,
		"entitlementAdapter": AdapterShopifyB2B,
	}}
	// ...but the entitlement was granted by the tag adapter.
	reader.ents["app-1"] = []map[string]any{{
		"applicationId": "app-1", "storeId": "store-live",
		"adapter":   AdapterCustomerTag,
		"state":     EntitlementGranted,
		"reference": "customer=gid://shopify/Customer/1;tag=memql-wholesale",
	}}
	caller := &fakeCaller{}
	writer := &fakeWriter{}
	p := &Provider{reader: reader, caller: caller, writer: writer}

	if _, err := p.revokeEntitlement(internalAsMerchant(),
		map[string]any{"applicationId": "app-1"}, 0); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	out := writer.only(t, "writeEntitlementRow")
	if out["adapter"] != AdapterCustomerTag {
		t.Fatalf("adapter = %v, want %v -- the revoke must go through the adapter that "+
			"granted, not the one settings name today", out["adapter"], AdapterCustomerTag)
	}
	if len(caller.calls) != 1 || caller.calls[0] != "shopifyUntagWholesaleCustomer" {
		t.Fatalf("calls = %v, want the tag adapter's untag", caller.calls)
	}
	if out["state"] != EntitlementRevoked {
		t.Fatalf("state = %v, want %v", out["state"], EntitlementRevoked)
	}
}

// THE TAG THAT WAS GRANTED IS THE TAG THAT IS REMOVED, read back off the
// reference. An operator who re-registered the adapter with a different tag
// between the grant and the revoke would otherwise remove a tag the buyer
// never had and leave the one they do have in place -- a revoke that
// reports success and revokes nothing.
func TestRevokeRemovesTheTagThatWasActuallyGranted(t *testing.T) {
	if got := tagFromReference("customer=gid://shopify/Customer/1;tag=acme-trade", "memql-wholesale"); got != "acme-trade" {
		t.Fatalf("tagFromReference = %q, want the tag on the reference", got)
	}
	if got := tagFromReference("", "memql-wholesale"); got != "memql-wholesale" {
		t.Fatalf("an unreadable reference must fall back to the configured tag, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// The reply contract
// ---------------------------------------------------------------------------

// A REPLY WITH NO STATUS IS A FAILURE, not a success. Recording an
// entitlement as granted on the strength of a reply nobody could parse is
// how a buyer is told they have trade prices they do not have.
func TestAnUnreadableReplyIsNotASuccess(t *testing.T) {
	if _, err := outcomeFromReply(AdapterCustomerTag, map[string]any{}); err == nil {
		t.Fatal("a reply with no status must not be read as a grant")
	}
	if _, err := outcomeFromReply(AdapterCustomerTag,
		map[string]any{"status": "unconfigured"}); err == nil {
		t.Fatal("an unconfigured connector must refuse")
	} else if !isAdapterRefusal(err) {
		t.Fatal("an unconfigured connector is a refusal: no amount of retrying configures it")
	}
}

// ---------------------------------------------------------------------------
// The registry
// ---------------------------------------------------------------------------

func TestUnknownAdapterNamesWhatIsRegistered(t *testing.T) {
	registerShippedAdapters()
	_, err := AdapterByName("draftOrders")
	if err == nil {
		t.Fatal("draftOrders is not an entitlement adapter in this pack and must be refused")
	}
	for _, shipped := range []string{AdapterShopifyB2B, AdapterCustomerTag} {
		if !strings.Contains(err.Error(), shipped) {
			t.Errorf("the refusal must list %q so a merchant can fix their settings: %v",
				shipped, err)
		}
	}
}

func TestBothShippedAdaptersAreRegistered(t *testing.T) {
	registerShippedAdapters()
	names := AdapterNames()
	want := map[string]bool{AdapterShopifyB2B: false, AdapterCustomerTag: false}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("%q must be registered; AdapterNames() = %v", name, names)
		}
	}
}

// ---------------------------------------------------------------------------
// The rendered call
// ---------------------------------------------------------------------------

// THE `builtin ` KEYWORD IS THE ONE THING A FAKE CALLER CANNOT CHECK.
//
// Every other test in this package satisfies itself with a fakeCaller, which
// is right for the state machine and wrong for this: a bare `name(args)` is
// the MUTATION form, and rendering an adapter's call that way would leave
// every test here green while provisioning failed at runtime against a
// construct the engine could not resolve. That is exactly how it was written
// the first time.
func TestABuiltinCallCarriesTheBuiltinKeyword(t *testing.T) {
	got := renderBuiltinCall("shopifyProvisionWholesale", map[string]any{
		"storeId":       "store-live",
		"applicationId": "app-1",
	})
	const want = `builtin shopifyProvisionWholesale(applicationId: "app-1", storeId: "store-live")`
	if got != want {
		t.Fatalf("renderBuiltinCall:\n got %s\nwant %s", got, want)
	}
}

// ARGUMENTS ARE SORTED, so the rendered text is a function of the arguments
// rather than of Go's map iteration order -- which is what makes the
// assertion above possible at all.
func TestTheRenderedCallIsStable(t *testing.T) {
	args := map[string]any{"b": "2", "a": "1", "c": 3, "d": true}
	first := renderBuiltinCall("x", args)
	for i := 0; i < 20; i++ {
		if again := renderBuiltinCall("x", args); again != first {
			t.Fatalf("unstable rendering:\n %s\n %s", first, again)
		}
	}
	const want = `builtin x(a: "1", b: "2", c: 3, d: true)`
	if first != want {
		t.Fatalf("got %s, want %s", first, want)
	}
}

// CALLER-SUPPLIED TEXT IS QUOTED BY THE ENGINE'S OWN ESCAPER. A company name
// reaches this from a public form, so a quote or a backslash in it must not
// change what is being asked.
func TestCallerSuppliedTextIsEscaped(t *testing.T) {
	got := renderBuiltinCall("x", map[string]any{"companyName": `Acme "Trading" \ Co`})
	if strings.Contains(got, `Acme "Trading"`) {
		t.Fatalf("an unescaped quote reached the rendered call: %s", got)
	}
	if !strings.HasPrefix(got, "builtin x(companyName: ") {
		t.Fatalf("unexpected shape: %s", got)
	}
}

// AN UNAVAILABLE ADAPTER IS RECORDED, NOT RETURNED.
//
// This is the contract issue memql#5557 sets for the builtin -- "a
// provisioning failure leaves the application approved and the entitlement
// pending with its error" -- applied to the refusal that arrives BEFORE the
// push rather than from it. An earlier version returned this straight to
// the caller, which left no row: a merchant whose store's plan cannot be
// read would have seen an approved application, no wholesale prices, and
// nothing anywhere saying why.
func TestAnUnavailableAdapterIsRecordedOnTheRow(t *testing.T) {
	registerShippedAdapters()
	reader := approvedFixture()
	// shopifyB2B, and a store whose plan this caller cannot read -- which is
	// the ORDINARY case for a merchant, because v1:shopify:store is
	// cluster-owner tier and epic memql#5530's D10 left that unchanged.
	reader.settings["store-live"] = []map[string]any{{
		"storeId": "store-live", "applicationsOpen": true,
		"entitlementAdapter": AdapterShopifyB2B,
	}}
	reader.stores = map[string][]map[string]any{}
	caller := &fakeCaller{}
	writer := &fakeWriter{}
	p := &Provider{reader: reader, caller: caller, writer: writer}

	if _, err := p.provisionEntitlement(internalAsMerchant(),
		map[string]any{"applicationId": "app-1"}, 0); err != nil {
		t.Fatalf("an unavailable adapter must be recorded, not returned: %v", err)
	}
	out := writer.only(t, "writeEntitlementRow")
	if out["state"] != EntitlementFailed {
		t.Fatalf("state = %v, want %v -- an unreadable plan will not become readable "+
			"because we asked again", out["state"], EntitlementFailed)
	}
	if !strings.Contains(asString(out["error"]), "not readable") {
		t.Fatalf("the row must carry why, got %v", out["error"])
	}
	// AND NOTHING WAS PUSHED. The refusal is decided before any call.
	if len(caller.calls) != 0 {
		t.Fatalf("an unavailable adapter must not reach Shopify, got %v", caller.calls)
	}
}

// ---------------------------------------------------------------------------
// Who provisioning runs as
// ---------------------------------------------------------------------------

// refusingReader fails the test on any read: a refusal that has to come
// before the builtin reads anything is asserted by reaching none.
type refusingReader struct{ t *testing.T }

func (r refusingReader) Rows(_ context.Context, query string, _ map[string]string) ([]map[string]any, error) {
	r.t.Fatalf("a call refused for its origin still read %s", query)
	return nil, nil
}

func (r refusingReader) RowsWithList(_ context.Context, query string, _ map[string]string,
	_ string, _ []string) ([]map[string]any, error) {
	r.t.Fatalf("a call refused for its origin still read %s", query)
	return nil, nil
}

func (r refusingReader) RowsPage(_ context.Context, query string, _ map[string]string, _ string) ([]map[string]any, string, error) {
	r.t.Fatalf("a call refused for its origin still read %s", query)
	return nil, "", nil
}

// THE STORE'S AUTOMATION, AND NOTHING ELSE. Both builtins read as the
// deployment and act as the application's merchant, so a client that could
// call them would provision any application it can name; a call without
// internal origin is refused before anything is read.
func TestProvisionAndRevokeRefuseACallWithoutInternalOrigin(t *testing.T) {
	registerShippedAdapters()
	writer := &fakeWriter{}
	caller := &fakeCaller{}
	p := &Provider{reader: refusingReader{t: t}, caller: caller, writer: writer}
	client := auth.ContextWithClientOrigin(auth.ContextWithUserActor(context.Background(), "merchant-1"))

	for name, call := range map[string]func() error{
		"wholesaleProvisionEntitlement": func() error {
			_, err := p.provisionEntitlement(client, map[string]any{"applicationId": "app-1"}, 0)
			return err
		},
		"wholesaleRevokeEntitlement": func() error {
			_, err := p.revokeEntitlement(client, map[string]any{"applicationId": "app-1"}, 0)
			return err
		},
	} {
		err := call()
		if err == nil {
			t.Fatalf("%s ran for a client-origin call", name)
		}
		want := "wholesale: " + name + " is called by the store's automation, not by a client"
		if err.Error() != want {
			t.Fatalf("%s refused with %q, want %q", name, err.Error(), want)
		}
	}
	if len(writer.writes) != 0 || len(caller.calls) != 0 {
		t.Fatalf("a refused call still acted: writes %+v, calls %v", writer.writes, caller.calls)
	}
}

// READ AS THE SYSTEM, WRITE AS THE MERCHANT. The automation's own actor owns
// nothing, so every read runs as the named system actor; and a row written
// as a system actor is owned by nobody, so the adapter call and the
// entitlement write run as the application's merchant.
func TestProvisionReadsAsTheSystemAndWritesAsTheMerchant(t *testing.T) {
	registerShippedAdapters()
	reader := approvedFixture()
	reader.apps["app-1"][0]["ownerUserId"] = "merchant-1"
	caller := &fakeCaller{}
	writer := &fakeWriter{}
	p := &Provider{reader: reader, caller: caller, writer: writer}
	automation := auth.ContextWithInternalOrigin(
		auth.ContextWithSystemActor(context.Background(), "contosoGrantAfterChain"))

	if _, err := p.provisionEntitlement(automation, map[string]any{"applicationId": "app-1"}, 0); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if len(reader.readAs) == 0 {
		t.Fatal("provisioning read nothing")
	}
	for i, actor := range reader.readAs {
		if actor != "system:wholesale-provision" {
			t.Fatalf("read %s ran as %q, want the system actor system:wholesale-provision",
				reader.asked[i], actor)
		}
	}
	if len(caller.actors) != 1 || caller.actors[0] != "merchant-1" {
		t.Fatalf("the adapter's call ran as %v, want the merchant merchant-1", caller.actors)
	}
	out := writer.only(t, "writeEntitlementRow")
	if writer.writes[0].actor != "merchant-1" {
		t.Fatalf("the entitlement was written as %q, want the merchant merchant-1", writer.writes[0].actor)
	}
	if out["ownerUserId"] != "v1:identity:user:merchant-1" {
		t.Fatalf("the entitlement's ownerUserId = %v, want the application's merchant", out["ownerUserId"])
	}

	// AN APPLICATION WITH NO MERCHANT IS REFUSED, rather than written as
	// whoever the context happened to carry.
	delete(reader.apps["app-1"][0], "ownerUserId")
	before := len(writer.writes)
	if _, err := p.provisionEntitlement(automation, map[string]any{"applicationId": "app-1"}, 0); err == nil {
		t.Fatal("an application with no ownerUserId was provisioned")
	}
	if len(writer.writes) != before {
		t.Fatal("an application with no ownerUserId still wrote an entitlement")
	}
}

// originEngine records the call origin of every Execute it is handed.
type originEngine struct {
	memql.IntegrationEngineAccess
	origins []auth.CallOrigin
}

func (e *originEngine) Execute(ctx context.Context, _ string) (*memql.ExecuteResult, error) {
	e.origins = append(e.origins, auth.OriginFromContext(ctx))
	return &memql.ExecuteResult{}, nil
}

// THE WRITER STAMPS INTERNAL ORIGIN ON ITS ONE CALL, so the pack's
// @serverOnly mutations are reachable from its builtins and from nothing
// else -- and the stamp is inline, so the caller's own context is untouched.
func TestTheWriterStampsInternalOriginOnItsOneCall(t *testing.T) {
	eng := &originEngine{}
	w := &engineWriter{engine: eng}
	ctx := auth.ContextWithClientOrigin(auth.ContextWithUserActor(context.Background(), "merchant-1"))
	if err := w.Write(ctx, "appendApplicationDecision", map[string]any{"applicationId": "app-1"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(eng.origins) != 1 {
		t.Fatalf("the writer made %d engine calls, want 1", len(eng.origins))
	}
	if !eng.origins[0].IsInternal() {
		t.Fatalf("the write ran at origin %v, want internal", eng.origins[0])
	}
	if auth.OriginFromContext(ctx).IsInternal() {
		t.Fatal("the stamp leaked into the caller's own context")
	}
}

// ---------------------------------------------------------------------------
// An approval that grants nothing says why (U2)
// ---------------------------------------------------------------------------

// provisionWith runs one provision against a fixture and returns the single
// entitlement write and the caller, failing on a returned error.
func provisionWith(t *testing.T, reader *fixtureReader, caller *fakeCaller) (map[string]any, *fakeCaller) {
	t.Helper()
	registerShippedAdapters()
	writer := &fakeWriter{}
	p := &Provider{reader: reader, caller: caller, writer: writer}
	if _, err := p.provisionEntitlement(internalAsMerchant(),
		map[string]any{"applicationId": "app-1"}, 0); err != nil {
		t.Fatalf("a grant that cannot happen is recorded, not returned: %v", err)
	}
	return writer.only(t, "writeEntitlementRow"), caller
}

func requireFailedWith(t *testing.T, out map[string]any, state, code string) {
	t.Helper()
	if out["state"] != state || out["errorCode"] != code {
		t.Fatalf("state/errorCode = %v/%v, want %s/%s (error %v)",
			out["state"], out["errorCode"], state, code, out["error"])
	}
	if asString(out["error"]) == "" {
		t.Fatal("the row must carry a sentence saying why")
	}
}

func TestAnApprovalOnAStoreWithNoSettingsLeavesAFailedRowNamingNoSettings(t *testing.T) {
	reader := approvedFixture()
	reader.settings = map[string][]map[string]any{}
	out, caller := provisionWith(t, reader, &fakeCaller{})
	requireFailedWith(t, out, EntitlementFailed, ErrorNoSettings)
	if out["adapter"] != AdapterNone {
		t.Fatalf("adapter = %v, want %v", out["adapter"], AdapterNone)
	}
	if len(caller.calls) != 0 {
		t.Fatalf("no adapter may be called, got %v", caller.calls)
	}
}

func TestAnApprovalOnAStoreWithNoRouteLeavesAFailedRowNamingNoRoute(t *testing.T) {
	reader := approvedFixture()
	reader.settings["store-live"] = []map[string]any{{
		"storeId": "store-live", "applicationsOpen": true, "entitlementAdapter": "",
	}}
	out, caller := provisionWith(t, reader, &fakeCaller{})
	requireFailedWith(t, out, EntitlementFailed, ErrorNoRoute)
	if out["adapter"] != AdapterNone {
		t.Fatalf("adapter = %v, want %v", out["adapter"], AdapterNone)
	}
	if len(caller.calls) != 0 {
		t.Fatalf("no adapter may be called, got %v", caller.calls)
	}
}

func TestAPlanThatCannotBeReadIsPlanCeiling(t *testing.T) {
	reader := approvedFixture()
	reader.settings["store-live"] = []map[string]any{{
		"storeId": "store-live", "applicationsOpen": true, "entitlementAdapter": AdapterShopifyB2B,
	}}
	reader.stores = map[string][]map[string]any{}
	out, caller := provisionWith(t, reader, &fakeCaller{})
	requireFailedWith(t, out, EntitlementFailed, ErrorPlanCeiling)
	if len(caller.calls) != 0 {
		t.Fatalf("got calls %v", caller.calls)
	}
}

func TestAReplyNamingThePlanCeilingIsPlanCeiling(t *testing.T) {
	reader := approvedFixture()
	reader.settings["store-live"] = []map[string]any{{
		"storeId": "store-live", "applicationsOpen": true, "entitlementAdapter": AdapterShopifyB2B,
	}}
	out, _ := provisionWith(t, reader, &fakeCaller{replies: map[string]map[string]any{
		"shopifyProvisionWholesale": {"status": "refused", "reason": "three catalogs already", "code": ErrorPlanCeiling},
	}})
	requireFailedWith(t, out, EntitlementFailed, ErrorPlanCeiling)
}

func TestARefusalCarryingScopeMissingIsRecordedAsScopeMissing(t *testing.T) {
	out, _ := provisionWith(t, approvedFixture(), &fakeCaller{replies: map[string]map[string]any{
		"shopifyTagWholesaleCustomer": {"status": "refused", "reason": "write_customers is not granted", "code": "scope_missing"},
	}})
	requireFailedWith(t, out, EntitlementFailed, ErrorScopeMissing)
}

func TestARefusalWithNoCodeIsCustomerRefused(t *testing.T) {
	out, _ := provisionWith(t, approvedFixture(), &fakeCaller{replies: map[string]map[string]any{
		"shopifyTagWholesaleCustomer": {"status": "refused", "reason": "no customer has that email"},
	}})
	requireFailedWith(t, out, EntitlementFailed, ErrorCustomerRefused)
}

func TestAnUnconfiguredConnectorIsAFailedShopifyError(t *testing.T) {
	out, _ := provisionWith(t, approvedFixture(), &fakeCaller{replies: map[string]map[string]any{
		"shopifyTagWholesaleCustomer": {"status": "unconfigured"},
	}})
	requireFailedWith(t, out, EntitlementFailed, ErrorShopifyError)
	if !strings.Contains(asString(out["error"]), "not configured on this cluster") {
		t.Fatalf("the existing sentence must be kept, got %v", out["error"])
	}
}

func TestATransientErrorIsPendingWithShopifyError(t *testing.T) {
	out, _ := provisionWith(t, approvedFixture(), &fakeCaller{err: errors.New("shopify: 503 from the Admin API")})
	requireFailedWith(t, out, EntitlementPending, ErrorShopifyError)
	if !strings.Contains(asString(out["error"]), "503") {
		t.Fatalf("the error text must be on the row, got %v", out["error"])
	}
}

func TestASuccessRecordsTheCustomerAndClearsTheCode(t *testing.T) {
	out, _ := provisionWith(t, approvedFixture(), &fakeCaller{replies: map[string]map[string]any{
		"shopifyTagWholesaleCustomer": {
			"status": "ok", "reference": "customer=gid://shopify/Customer/1;tag=memql-wholesale",
			"customerId": "gid://shopify/Customer/1", "customerCreated": true,
		},
	}})
	if out["state"] != EntitlementGranted || out["errorCode"] != "" || out["error"] != "" {
		t.Fatalf("a success carries no code or error, got %v/%v/%v", out["state"], out["errorCode"], out["error"])
	}
	if out["shopifyCustomerId"] != "gid://shopify/Customer/1" || out["customerCreated"] != true {
		t.Fatalf("the customer must be recorded, got %v/%v", out["shopifyCustomerId"], out["customerCreated"])
	}
}

func TestAGrantAfterAFailureWritesEveryFieldExplicitly(t *testing.T) {
	out, _ := provisionWith(t, approvedFixture(), &fakeCaller{})
	for _, k := range []string{"errorCode", "error", "shopifyCustomerId", "customerCreated"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("the write omits %q, so a version would inherit the attempt before it", k)
		}
	}
	if out["errorCode"] != "" || out["error"] != "" || out["customerCreated"] != false {
		t.Fatalf("want empty/empty/false, got %v/%v/%v", out["errorCode"], out["error"], out["customerCreated"])
	}
}

func revokedFixture(prior []map[string]any) *fixtureReader {
	reader := approvedFixture()
	reader.decisions["app-1"] = []map[string]any{{"transition": TransitionRevoke}}
	if prior != nil {
		reader.ents["app-1"] = prior
	}
	return reader
}

func TestRevokingAnEntitlementThatNeverReachedAStoreCallsNoAdapter(t *testing.T) {
	registerShippedAdapters()
	reader := revokedFixture([]map[string]any{{
		"applicationId": "app-1", "storeId": "store-live", "adapter": AdapterNone, "state": EntitlementFailed,
	}})
	caller := &fakeCaller{}
	writer := &fakeWriter{}
	p := &Provider{reader: reader, caller: caller, writer: writer}
	if _, err := p.revokeEntitlement(internalAsMerchant(), map[string]any{"applicationId": "app-1"}, 0); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	out := writer.only(t, "writeEntitlementRow")
	if out["state"] != EntitlementRevoked || out["adapter"] != AdapterNone || out["errorCode"] != "" {
		t.Fatalf("got %v/%v/%v", out["state"], out["adapter"], out["errorCode"])
	}
	if len(caller.calls) != 0 {
		t.Fatalf("no adapter may be called, got %v", caller.calls)
	}
}

func TestRevokingWithNoEntitlementRowWritesRevoked(t *testing.T) {
	registerShippedAdapters()
	caller := &fakeCaller{}
	writer := &fakeWriter{}
	p := &Provider{reader: revokedFixture(nil), caller: caller, writer: writer}
	if _, err := p.revokeEntitlement(internalAsMerchant(), map[string]any{"applicationId": "app-1"}, 0); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	out := writer.only(t, "writeEntitlementRow")
	if out["state"] != EntitlementRevoked || out["adapter"] != AdapterNone {
		t.Fatalf("got %v/%v", out["state"], out["adapter"])
	}
	if len(caller.calls) != 0 {
		t.Fatalf("no adapter may be called, got %v", caller.calls)
	}
}

func TestSettingsNamingARouteThisEngineDoesNotHaveLeaveAFailedRowNamingNoRoute(t *testing.T) {
	reader := approvedFixture()
	reader.settings["store-live"] = []map[string]any{{
		"storeId": "store-live", "applicationsOpen": true, "entitlementAdapter": "draftOrders",
	}}
	out, caller := provisionWith(t, reader, &fakeCaller{})
	requireFailedWith(t, out, EntitlementFailed, ErrorNoRoute)
	if out["adapter"] != AdapterNone {
		t.Fatalf("adapter = %v, want %v", out["adapter"], AdapterNone)
	}
	if !strings.Contains(asString(out["error"]), `"draftOrders"`) {
		t.Fatalf("the sentence must name the route, got %v", out["error"])
	}
	if len(caller.calls) != 0 {
		t.Fatalf("no adapter may be called, got %v", caller.calls)
	}
}

func TestARevokeKeepsTheCustomerTheGrantLandedOn(t *testing.T) {
	registerShippedAdapters()
	reader := revokedFixture([]map[string]any{{
		"applicationId": "app-1", "storeId": "store-live", "adapter": AdapterCustomerTag,
		"state": EntitlementGranted, "reference": "customer=gid://shopify/Customer/1;tag=memql-wholesale",
		"shopifyCustomerId": "gid://shopify/Customer/1", "customerCreated": true,
	}})
	writer := &fakeWriter{}
	p := &Provider{reader: reader, caller: &fakeCaller{}, writer: writer}
	if _, err := p.revokeEntitlement(internalAsMerchant(), map[string]any{"applicationId": "app-1"}, 0); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	out := writer.only(t, "writeEntitlementRow")
	if out["state"] != EntitlementRevoked || out["shopifyCustomerId"] != "gid://shopify/Customer/1" || out["customerCreated"] != true {
		t.Fatalf("got %v/%v/%v", out["state"], out["shopifyCustomerId"], out["customerCreated"])
	}
}

// A GRANTED ROW WHOSE ADAPTER IS NO LONGER REGISTERED cannot be revoked by
// it, so the row must not claim revoked: it stays pending, naming no_route.
func TestARevokeThroughAnAdapterThatIsGoneIsNotClaimedRevoked(t *testing.T) {
	registerShippedAdapters()
	reader := revokedFixture([]map[string]any{{
		"applicationId": "app-1", "storeId": "store-live", "adapter": "draftOrders",
		"state": EntitlementGranted, "reference": "ref-1",
		"shopifyCustomerId": "gid://shopify/Customer/1", "customerCreated": true,
	}})
	caller := &fakeCaller{}
	writer := &fakeWriter{}
	p := &Provider{reader: reader, caller: caller, writer: writer}
	if _, err := p.revokeEntitlement(internalAsMerchant(), map[string]any{"applicationId": "app-1"}, 0); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	out := writer.only(t, "writeEntitlementRow")
	if out["state"] != EntitlementPending || out["errorCode"] != ErrorNoRoute || out["adapter"] != "draftOrders" ||
		out["reference"] != "ref-1" || out["shopifyCustomerId"] != "gid://shopify/Customer/1" {
		t.Fatalf("got %v", out)
	}
	if !strings.Contains(asString(out["error"]), `"draftOrders"`) || len(caller.calls) != 0 {
		t.Fatalf("error %v calls %v", out["error"], caller.calls)
	}
}

// ---------------------------------------------------------------------------
// A failed re-provision never loses what the grant recorded (fix round 1)
// ---------------------------------------------------------------------------

func grantedRow() map[string]any {
	return map[string]any{
		"applicationId": "app-1", "storeId": "store-live", "adapter": AdapterCustomerTag,
		"state": EntitlementGranted, "reference": "customer=gid://shopify/Customer/1;tag=memql-wholesale",
		"shopifyCustomerId": "gid://shopify/Customer/1", "customerCreated": true,
	}
}

func reprovisionFixture() *fixtureReader {
	reader := approvedFixture()
	reader.ents["app-1"] = []map[string]any{grantedRow()}
	return reader
}

// revokeAfter revokes over a row (as a later revoke would read it) and
// returns the revoke's write and the adapter calls it made.
func revokeAfter(t *testing.T, row map[string]any) (map[string]any, []string) {
	t.Helper()
	reader := revokedFixture([]map[string]any{row})
	caller := &fakeCaller{}
	writer := &fakeWriter{}
	p := &Provider{reader: reader, caller: caller, writer: writer}
	if _, err := p.revokeEntitlement(internalAsMerchant(), map[string]any{"applicationId": "app-1"}, 0); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	return writer.only(t, "writeEntitlementRow"), caller.calls
}

func TestAReprovisionThatReachesNoAdapterLeavesTheGrantedRowAlone(t *testing.T) {
	registerShippedAdapters()
	cases := map[string]func(r *fixtureReader){
		"no settings": func(r *fixtureReader) { r.settings = map[string][]map[string]any{} },
		"no route": func(r *fixtureReader) {
			r.settings["store-live"] = []map[string]any{{"storeId": "store-live", "entitlementAdapter": ""}}
		},
		"unknown route": func(r *fixtureReader) {
			r.settings["store-live"] = []map[string]any{{"storeId": "store-live", "entitlementAdapter": "draftOrders"}}
		},
	}
	for name, mutate := range cases {
		reader := reprovisionFixture()
		mutate(reader)
		caller := &fakeCaller{}
		writer := &fakeWriter{}
		p := &Provider{reader: reader, caller: caller, writer: writer}
		nodes, err := p.provisionEntitlement(internalAsMerchant(), map[string]any{"applicationId": "app-1"}, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(writer.writes) != 0 || len(caller.calls) != 0 {
			t.Fatalf("%s: wrote %+v, called %v; the granted row must be left alone", name, writer.writes, caller.calls)
		}
		if got := decodeOne(t, nodes, nil); got["state"] != EntitlementGranted || got["adapter"] != AdapterCustomerTag {
			t.Fatalf("%s: the answer must be the existing row, got %v", name, got)
		}
		// A later revoke still finds the adapter that put the access on.
		out, calls := revokeAfter(t, reader.ents["app-1"][0])
		if out["state"] != EntitlementRevoked || out["adapter"] != AdapterCustomerTag ||
			len(calls) != 1 || calls[0] != "shopifyUntagWholesaleCustomer" {
			t.Fatalf("%s: revoke wrote %v, calls %v", name, out, calls)
		}
	}
}

func TestAReprovisionThatErrorsKeepsTheAdapterReferenceAndCustomer(t *testing.T) {
	registerShippedAdapters()
	reader := reprovisionFixture()
	reader.settings["store-live"] = []map[string]any{{
		"storeId": "store-live", "entitlementAdapter": AdapterShopifyB2B,
	}}
	writer := &fakeWriter{}
	p := &Provider{reader: reader, caller: &fakeCaller{err: errors.New("shopify: 503")}, writer: writer}
	if _, err := p.provisionEntitlement(internalAsMerchant(), map[string]any{"applicationId": "app-1"}, 0); err != nil {
		t.Fatal(err)
	}
	out := writer.only(t, "writeEntitlementRow")
	if out["state"] != EntitlementPending || out["errorCode"] != ErrorShopifyError {
		t.Fatalf("got %v/%v", out["state"], out["errorCode"])
	}
	if out["adapter"] != AdapterCustomerTag || out["reference"] != grantedRow()["reference"] ||
		out["shopifyCustomerId"] != "gid://shopify/Customer/1" || out["customerCreated"] != true {
		t.Fatalf("the prior grant was lost: %v", out)
	}
	revoked, calls := revokeAfter(t, out)
	if revoked["state"] != EntitlementRevoked || len(calls) != 1 || calls[0] != "shopifyUntagWholesaleCustomer" {
		t.Fatalf("revoke wrote %v, calls %v", revoked, calls)
	}
}

func TestAReprovisionThatSucceedsWithNoCustomerKeepsThePriorOne(t *testing.T) {
	registerShippedAdapters()
	writer := &fakeWriter{}
	p := &Provider{reader: reprovisionFixture(), caller: &fakeCaller{}, writer: writer}
	if _, err := p.provisionEntitlement(internalAsMerchant(), map[string]any{"applicationId": "app-1"}, 0); err != nil {
		t.Fatal(err)
	}
	out := writer.only(t, "writeEntitlementRow")
	if out["state"] != EntitlementGranted || out["shopifyCustomerId"] != "gid://shopify/Customer/1" || out["customerCreated"] != true {
		t.Fatalf("got %v", out)
	}
}

func TestARevokeOverARowWithNoAdapterIsRevokedAndKeepsItsCustomer(t *testing.T) {
	row := grantedRow()
	row["adapter"] = ""
	out, calls := revokeAfter(t, row)
	if out["state"] != EntitlementRevoked || out["adapter"] != AdapterNone || len(calls) != 0 ||
		out["shopifyCustomerId"] != "gid://shopify/Customer/1" || out["errorCode"] != "" {
		t.Fatalf("got %v, calls %v", out, calls)
	}
}

func TestAnUnknownRefusalCodeIsCustomerRefusedAndKeepsTheReason(t *testing.T) {
	out, _ := provisionWith(t, approvedFixture(), &fakeCaller{replies: map[string]map[string]any{
		"shopifyTagWholesaleCustomer": {"status": "refused", "reason": "the store said no", "code": "weird_code"},
	}})
	requireFailedWith(t, out, EntitlementFailed, ErrorCustomerRefused)
	if !strings.Contains(asString(out["error"]), "the store said no") {
		t.Fatalf("the reason must stay in error, got %v", out["error"])
	}
}

func TestARevokeThatIsRefusedOrErrorsKeepsTheCustomer(t *testing.T) {
	registerShippedAdapters()
	for name, caller := range map[string]*fakeCaller{
		"refused": {replies: map[string]map[string]any{
			"shopifyUntagWholesaleCustomer": {"status": "refused", "reason": "no"}}},
		"errors": {err: errors.New("shopify: 503")},
	} {
		writer := &fakeWriter{}
		p := &Provider{reader: revokedFixture([]map[string]any{grantedRow()}), caller: caller, writer: writer}
		if _, err := p.revokeEntitlement(internalAsMerchant(), map[string]any{"applicationId": "app-1"}, 0); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out := writer.only(t, "writeEntitlementRow")
		if out["state"] == EntitlementRevoked || out["shopifyCustomerId"] != "gid://shopify/Customer/1" ||
			out["customerCreated"] != true || out["adapter"] != AdapterCustomerTag || out["reference"] != grantedRow()["reference"] {
			t.Fatalf("%s: got %v", name, out)
		}
	}
}

func TestARowWrittenBeforeTheCodeFieldsExistedGoesThroughARevoke(t *testing.T) {
	row := grantedRow()
	delete(row, "shopifyCustomerId")
	delete(row, "customerCreated")
	out, calls := revokeAfter(t, row)
	if out["state"] != EntitlementRevoked || out["shopifyCustomerId"] != "" || out["customerCreated"] != false ||
		out["errorCode"] != "" || len(calls) != 1 {
		t.Fatalf("got %v, calls %v", out, calls)
	}
}
