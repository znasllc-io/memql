package wholesalepack

import (
	"context"
	"errors"
	"strings"
	"testing"
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
}

func (f *fakeCaller) Call(_ context.Context, construct string, args map[string]any) (map[string]any, error) {
	f.calls = append(f.calls, construct)
	f.args = append(f.args, args)
	if f.err != nil {
		return nil, f.err
	}
	if reply, ok := f.replies[construct]; ok {
		return reply, nil
	}
	return map[string]any{"status": "ok"}, nil
}

func approvedFixture() *fixtureReader {
	return &fixtureReader{
		apps: map[string][]map[string]any{
			"app-1": {{
				"id": "app-1", "storeId": "store-live", "companyName": "Acme Trading",
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
	p := &Provider{reader: reader, caller: &fakeCaller{}}

	_, err := p.provisionEntitlement(context.Background(),
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
	p := &Provider{reader: approvedFixture(), caller: &fakeCaller{}}
	_, err := p.revokeEntitlement(context.Background(),
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
	p := &Provider{
		reader: approvedFixture(),
		caller: &fakeCaller{err: errors.New("shopify: 503 from the Admin API")},
	}
	out := decodedBy(t)(p.provisionEntitlement(context.Background(),
		map[string]any{"applicationId": "app-1"}, 0))
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
	p := &Provider{
		reader: approvedFixture(),
		caller: &fakeCaller{replies: map[string]map[string]any{
			"shopifyTagWholesaleCustomer": {
				"status": "refused",
				"reason": "no customer on this store has that email",
			},
		}},
	}
	out := decodedBy(t)(p.provisionEntitlement(context.Background(),
		map[string]any{"applicationId": "app-1"}, 0))
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
	p := &Provider{
		reader: approvedFixture(),
		caller: &fakeCaller{err: errors.New("network unreachable")},
	}
	nodes, err := p.provisionEntitlement(context.Background(),
		map[string]any{"applicationId": "app-1"}, 0)
	if err != nil {
		t.Fatalf("a push failure must not be returned as an error: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1 -- the attempt must leave a row", len(nodes))
	}
	if nodes[0].ID != EntitlementRowID("app-1") {
		t.Fatalf("row id = %q, want the derived one", nodes[0].ID)
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
	p := &Provider{reader: approvedFixture(), caller: caller}
	out := decodedBy(t)(p.provisionEntitlement(context.Background(),
		map[string]any{"applicationId": "app-1"}, 0))
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
	// AND THE CONSTRAINT THE PACK CANNOT ENFORCE TRAVELS WITH THE ROW.
	if !strings.Contains(asString(out["detail"]), "never a discount code") {
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
	p := &Provider{reader: reader, caller: &fakeCaller{}}
	_, err := p.provisionEntitlement(context.Background(),
		map[string]any{"applicationId": "app-1"}, 0)
	if err == nil {
		t.Fatal("a store with no adapter configured must refuse by name")
	}
	if !strings.Contains(err.Error(), "entitlementAdapter") {
		t.Fatalf("the refusal must name the setting to change, got: %v", err)
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
	p := &Provider{reader: reader, caller: caller}

	out := decodedBy(t)(p.revokeEntitlement(context.Background(),
		map[string]any{"applicationId": "app-1"}, 0))
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
