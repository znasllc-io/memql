package wholesalepack

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// application_test.go -- the transitions and their invariants, exercised as
// decisions over rows (epic memql#5533, issues memql#5555 and memql#5556).
//
// Behind the rowReader seam the gate, the fold and the legality check are
// functions over values, so these tests build fixtures rather than an
// engine envelope. published_test.go in packs/reviewspack does the same for
// the same reason.

// fixtureReader answers the pack's own queries from fixtures.
type fixtureReader struct {
	settings  map[string][]map[string]any // storeId -> rows
	apps      map[string][]map[string]any // applicationId -> rows
	decisions map[string][]map[string]any // applicationId -> rows, NEWEST FIRST
	ents      map[string][]map[string]any // applicationId -> rows
	stores    map[string][]map[string]any // storeId -> rows
	asked     []string
	readAs    []string         // the actor each read ran as, in order
	sites     []map[string]any // sitesBoundToStore's answer
	walk      []map[string]any // applicationsForBackfill's answer, whatever the cursor
	docs      []map[string]any // every document version, matched by documentVersionsForStore as written
}

func (f *fixtureReader) Rows(ctx context.Context, query string, args map[string]string) ([]map[string]any, error) {
	f.asked = append(f.asked, query)
	f.readAs = append(f.readAs, actorOf(ctx))
	switch query {
	case "wholesaleSettingsForStore":
		return f.settings[args["storeId"]], nil
	case "applicationById":
		return f.apps[args["applicationId"]], nil
	case "decisionsForApplication":
		return f.decisions[args["applicationId"]], nil
	case "entitlementForApplication":
		return f.ents[args["applicationId"]], nil
	case "storePlanById":
		return f.stores[args["storeId"]], nil
	case "applicationsForBackfill":
		return f.walk, nil
	case "documentVersionsForStore":
		return docsMatching(f.docs, args["storeId"], args["kind"]), nil
	case "documentVersionBriefsForStore":
		return briefsOf(docsMatching(f.docs, args["storeId"], args["kind"])), nil
	case "wholesaleDocumentVersionById":
		return docByID(f.docs, args["documentVersionId"]), nil
	case "siteById":
		for _, site := range f.sites {
			if bareID(trimmed(site["id"])) == bareID(args["siteId"]) {
				return []map[string]any{site}, nil
			}
		}
	case "wholesaleSettingsById":
		// A row that names its id is found at it; one that does not is found
		// at the id its storeId derives, which is where every write puts it.
		for _, rows := range f.settings {
			for _, row := range rows {
				id := bareID(trimmed(row["id"]))
				if id == "" {
					id = SettingsRowID(trimmed(row["storeId"]))
				}
				if id == args["settingsId"] {
					return []map[string]any{row}, nil
				}
			}
		}
	}
	return nil, nil
}

func (f *fixtureReader) RowsWithList(ctx context.Context, query string, _ map[string]string,
	_ string, _ []string) ([]map[string]any, error) {
	f.asked = append(f.asked, query)
	f.readAs = append(f.readAs, actorOf(ctx))
	if query == "sitesBoundToStore" {
		return f.sites, nil
	}
	if query == "applicationsForBackfill" {
		return f.walk, nil
	}
	return nil, nil
}

func (f *fixtureReader) RowsPageWithList(ctx context.Context, query string, args map[string]string, _ string, _ []string, _ string) ([]map[string]any, string, error) {
	rows, err := f.Rows(ctx, query, args)
	return rows, "", err
}

func (f *fixtureReader) RowsPage(ctx context.Context, query string, args map[string]string, _ string) ([]map[string]any, string, error) {
	rows, err := f.Rows(ctx, query, args)
	return rows, "", err
}

// fakeWriter records what the pack PERSISTED.
//
// IT IS A SEPARATE FAKE FROM fixtureReader ON PURPOSE. The write path and
// the read path are different constructs with different call forms, and a
// test that asserted a write by reading the capability's RETURN VALUE would
// be asserting a receipt rather than a row -- which is exactly the mistake
// the live e2e suite caught in the first version of this pack, where the
// returned node was all there ever was.
type fakeWriter struct {
	writes []writeCall
	err    error
	// refuse fails one write rather than all of them.
	refuse func(mutation string, args map[string]any) error
	// inserted answers the id the engine generated for an Insert; nil answers
	// one numbered by the write.
	inserted func(mutation string, args map[string]any) string
}

type writeCall struct {
	mutation string
	args     map[string]any
	actor    string          // the user the write ran as
	ctx      context.Context // the context it ran under, for its fences
}

func (f *fakeWriter) Write(ctx context.Context, mutation string, args map[string]any) error {
	if f.err != nil {
		return f.err
	}
	if f.refuse != nil {
		if err := f.refuse(mutation, args); err != nil {
			return err
		}
	}
	f.writes = append(f.writes, writeCall{mutation: mutation, args: args, actor: actorOf(ctx), ctx: ctx})
	return nil
}

// Insert records the write and answers the id the engine would have generated.
func (f *fakeWriter) Insert(ctx context.Context, mutation string, args map[string]any) (string, error) {
	if err := f.Write(ctx, mutation, args); err != nil {
		return "", err
	}
	if f.inserted != nil {
		return f.inserted(mutation, args), nil
	}
	return "generated-" + strconv.Itoa(len(f.writes)), nil
}

// asShopperPath is the borrowed authority the shopper handler submits under:
// the site owner's actor.
func asShopperPath() context.Context {
	return auth.ContextWithUserActor(context.Background(), "site-owner-1")
}

// actorOf is the user id a context acts as, or "" when it carries none.
func actorOf(ctx context.Context) string {
	if ac, ok := auth.AccessFromContext(ctx); ok {
		return ac.UserId
	}
	return ""
}

// only returns the single write of a named mutation, failing when there is
// not exactly one.
func (f *fakeWriter) only(t *testing.T, mutation string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, w := range f.writes {
		if w.mutation == mutation {
			found = append(found, w.args)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s was written %d times, want 1 (writes: %+v)", mutation, len(found), f.writes)
	}
	return found[0]
}

func (f *fakeWriter) count(mutation string) int {
	n := 0
	for _, w := range f.writes {
		if w.mutation == mutation {
			n++
		}
	}
	return n
}

// decodedBy curries the test handle so a capability's TWO return values can
// be spliced straight in: decodedBy(t)(p.provisionEntitlement(...)). Go
// allows f(g()) only when g's returns match f's params exactly, and a
// three-parameter helper taking *testing.T first does not.
func decodedBy(t *testing.T) func([]memorynodes.MemoryNode, error) map[string]any {
	t.Helper()
	return func(nodes []memorynodes.MemoryNode, err error) map[string]any {
		return decodeOne(t, nodes, err)
	}
}

// decodeOne unwraps the single node a builtin replies with.
func decodeOne(t *testing.T, nodes []memorynodes.MemoryNode, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatalf("capability: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	var out map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatalf("payload: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------------
// The state fold
// ---------------------------------------------------------------------------

// A FRESH APPLICATION IS submitted, and that is derived rather than stored:
// there is no state field to have been set.
func TestNoDecisionsMeansSubmitted(t *testing.T) {
	if got := FoldState(nil); got != StateSubmitted {
		t.Fatalf("FoldState(nil) = %q, want %q", got, StateSubmitted)
	}
	if got := FoldState([]map[string]any{}); got != StateSubmitted {
		t.Fatalf("FoldState(empty) = %q, want %q", got, StateSubmitted)
	}
}

// THE NEWEST DECISION WINS, and the older ones are still there. This is the
// whole argument for append-only storage: the reason an application was
// rejected survives the approval that followed it.
func TestNewestDecisionWinsAndTheOldOneSurvives(t *testing.T) {
	// Newest first, which is how the queries sort.
	log := []map[string]any{
		{"transition": TransitionApprove, "note": "vouched for by the rep"},
		{"transition": TransitionReject, "note": "no trading history"},
	}
	if got := FoldState(log); got != StateApproved {
		t.Fatalf("FoldState = %q, want %q", got, StateApproved)
	}
	if log[1]["note"] != "no trading history" {
		t.Fatal("the earlier decision's reason must survive the later decision")
	}
}

// A TRANSITION THIS PACK DOES NOT RECOGNISE IS SKIPPED, not treated as
// final. The alternative is a payload nobody can parse silently pinning an
// application into a state no transition leads out of.
func TestUnreadableTransitionIsSkipped(t *testing.T) {
	log := []map[string]any{
		{"transition": "escalate"},
		{"transition": TransitionApprove},
	}
	if got := FoldState(log); got != StateApproved {
		t.Fatalf("FoldState = %q, want %q -- an unreadable transition must be skipped, "+
			"not allowed to pin the application", got, StateApproved)
	}
}

// ---------------------------------------------------------------------------
// Legality
// ---------------------------------------------------------------------------

func TestLegalTransitions(t *testing.T) {
	for _, tc := range []struct {
		state, transition string
		legal             bool
	}{
		{StateSubmitted, TransitionApprove, true},
		{StateSubmitted, TransitionReject, true},
		{StateSubmitted, TransitionRevoke, false},
		{StateApproved, TransitionRevoke, true},
		{StateApproved, TransitionApprove, false},
		{StateApproved, TransitionReject, false},
		{StateRejected, TransitionApprove, false},
		{StateRejected, TransitionReject, false},
		{StateRevoked, TransitionApprove, false},
	} {
		err := LegalTransition(tc.state, tc.transition)
		if tc.legal && err != nil {
			t.Errorf("%s from %s must be legal: %v", tc.transition, tc.state, err)
		}
		if !tc.legal && err == nil {
			t.Errorf("%s from %s must be refused", tc.transition, tc.state)
		}
	}
}

// A FINAL STATE IS FINAL, and the refusal SAYS what to do instead. A
// rejected applicant applies again -- a new application with its own log --
// rather than having their old refusal quietly re-decided.
func TestFinalStatesRefuseWithARemedy(t *testing.T) {
	err := LegalTransition(StateRejected, TransitionApprove)
	if err == nil {
		t.Fatal("approving a rejected application must be refused")
	}
	if !strings.Contains(err.Error(), "new application") {
		t.Fatalf("the refusal must name the remedy, got: %v", err)
	}
}

func TestUnknownTransitionIsRefused(t *testing.T) {
	if err := ValidTransition("escalate"); err == nil {
		t.Fatal("escalate is not in the closed set and must be refused")
	}
	for _, ok := range Transitions {
		if err := ValidTransition(ok); err != nil {
			t.Fatalf("%s must be valid: %v", ok, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Who may decide
// ---------------------------------------------------------------------------

// staffCtx is a person on the store's side, signed in: no cluster rank.
func staffCtx(userID string) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userID, Role: auth.Role("user")})
}

// THE CLUSTER'S OWN OPERATORS READ EVERY STORE'S APPLICATIONS AND DECIDE NONE
// (X4), and a call with no signed-in person decides nothing either. Both are
// refused before anything is read or written.
func TestTheClustersOwnOperatorsCannotDecide(t *testing.T) {
	for name, c := range map[string]struct {
		ctx  context.Context
		want string
	}{
		"a cluster operator": {operatorCtx("operator-1"), "wholesale: the cluster's operators host the store's " +
			"applications and do not decide them; a person on the store's side decides"},
		"nobody signed in": {context.Background(), "wholesale: a decision needs a signed-in person"},
	} {
		writer := &fakeWriter{}
		p := &Provider{reader: refusingReader{t: t}, writer: writer}
		_, err := p.recordDecision(c.ctx, map[string]any{
			"applicationId": "app-1", "transition": TransitionApprove,
			"principalKind": PrincipalClient, "decidedBy": "op-1",
		}, 0)
		if err == nil || err.Error() != c.want {
			t.Fatalf("%s: err = %v, want %q", name, err, c.want)
		}
		if len(writer.writes) != 0 {
			t.Fatalf("%s: a refused decision wrote: %+v", name, writer.writes)
		}
	}
}

// decidedBy NAMING SOMEONE ELSE IS IGNORED: the decider is the caller, which is
// the only one of the two that cannot be forged. The argument and principalKind
// stay declared for old callers and change nothing.
func TestACallerSuppliedDecidedByNamingSomeoneElseIsIgnored(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{writer: writer, reader: &fixtureReader{
		apps: map[string][]map[string]any{
			"app-1": {{"id": "app-1", "storeId": "store-live", "ownerUserId": "merchant-1"}},
		},
	}}
	got := decodedBy(t)(p.recordDecision(staffCtx("staff-1"), map[string]any{
		"applicationId": "app-1", "transition": TransitionApprove, "decidedBy": "someone-else",
	}, 0))
	wrote := writer.only(t, "appendApplicationDecision")
	if _, present := wrote["decidedBy"]; present {
		t.Fatalf("the write carries a decidedBy argument: %+v", wrote)
	}
	if writer.writes[0].actor != "staff-1" {
		t.Fatalf("the decision was written as %q, want the caller staff-1, whose id the row stamps", writer.writes[0].actor)
	}
	if got["decidedBy"] != "v1:identity:user:staff-1" {
		t.Fatalf("the receipt names decider %v, want the caller in the canonical form the row holds", got["decidedBy"])
	}
	if _, echoed := got["claimedBy"]; echoed {
		t.Fatalf("the receipt still echoes the caller's claim: %+v", got)
	}
}

// A SUBMITTED APPLICATION IS WAITING FOR ITS TERMS WHILE THE STORE REQUIRES
// THEM AND THEY ARE NOT ACCEPTED, AND READY TO REVIEW OTHERWISE; every other
// state is its own stage.
func TestStageOfASubmittedApplication(t *testing.T) {
	for _, c := range []struct {
		state           string
		terms, accepted bool
		want            string
	}{
		{StateSubmitted, true, false, StageWaitingForTerms},
		{StateSubmitted, true, true, StageReadyToReview},
		{StateSubmitted, false, false, StageReadyToReview},
		{StateApproved, true, false, StateApproved},
	} {
		if got := StageOf(c.state, c.terms, c.accepted); got != c.want {
			t.Errorf("StageOf(%s, %v, %v) = %q, want %q", c.state, c.terms, c.accepted, got, c.want)
		}
	}
	if StageWaitingForTerms != "waitingForTerms" || StageReadyToReview != "readyToReview" {
		t.Fatal("a stage drifted from its name")
	}
}

// RETRYING ACCESS ON AN APPLICATION THAT HAS NONE TO RETRY IS REFUSED BY NAME,
// and nothing is called or written.
func TestRetryAccessIsRefusedByNameForARejectedApplication(t *testing.T) {
	registerShippedAdapters()
	reader := approvedFixture()
	reader.decisions["app-1"] = []map[string]any{{"transition": TransitionReject}}
	writer, caller := &fakeWriter{}, &fakeCaller{}
	p := &Provider{reader: reader, writer: writer, caller: caller}
	_, err := p.retryAccess(staffCtx("staff-1"), map[string]any{"applicationId": "app-1"}, 0)
	if err == nil || err.Error() != "wholesale: there is no access to retry for an application that is rejected" {
		t.Fatalf("err = %v", err)
	}
	if len(writer.writes) != 0 || len(caller.calls) != 0 {
		t.Fatalf("a refused retry wrote %+v and called %v", writer.writes, caller.calls)
	}
}

// A REVOKED APPLICATION WHOSE TAG IS STILL ON IS RETRIED AS A REVOKE, through
// the adapter that granted it, as the application's merchant.
func TestRetryAccessRevokesWhenTheTagIsStillOn(t *testing.T) {
	registerShippedAdapters()
	reader := approvedFixture()
	reader.decisions["app-1"] = []map[string]any{{"transition": TransitionRevoke}, {"transition": TransitionApprove}}
	reader.ents["app-1"] = []map[string]any{{"applicationId": "app-1", "storeId": "store-live",
		"adapter": AdapterCustomerTag, "state": EntitlementPending, "reference": "customer=gid://shopify/Customer/1;tag=acme-trade",
		"errorCode": ErrorShopifyError}}
	writer, caller := &fakeWriter{}, &fakeCaller{}
	p := &Provider{reader: reader, writer: writer, caller: caller}
	got := decodedBy(t)(p.retryAccess(staffCtx("staff-1"), map[string]any{"applicationId": "app-1"}, 0))
	if len(caller.calls) != 1 || caller.calls[0] != "shopifyUntagWholesaleCustomer" {
		t.Fatalf("the retry called %v, want the tag taken off once", caller.calls)
	}
	if caller.actors[0] != "merchant-1" {
		t.Fatalf("the adapter ran as %q, want the application's merchant", caller.actors[0])
	}
	if w := writer.only(t, "writeEntitlementRow"); w["state"] != EntitlementRevoked || got["state"] != EntitlementRevoked {
		t.Fatalf("the entitlement was written %v and answered %v, want revoked", w["state"], got["state"])
	}

	// A revoked application whose entitlement is already revoked has nothing to retry.
	reader.ents["app-1"][0]["state"] = EntitlementRevoked
	if _, err := p.retryAccess(staffCtx("staff-1"), map[string]any{"applicationId": "app-1"}, 0); err == nil ||
		err.Error() != "wholesale: there is no access to retry for an application that is revoked" {
		t.Fatalf("a retry over a revoked entitlement: %v", err)
	}
}

// settingsSite is one site of organization north serving store-live.
func settingsSite() map[string]any {
	return map[string]any{"id": "site-1", "ownerUserId": "v1:identity:user:merchant-1", "accountId": "acct-north",
		"binding": map[string]any{"storeId": "store-live"}}
}

// AN UPDATE MERGES INTO THE ROW THE OPERATORS SET UP, and a store they never set
// up is refused with the sentence the OS shows.
func TestUpdateSettingsMergesAndRefusesAStoreWithNoSettings(t *testing.T) {
	registerShippedAdapters()
	writer := &fakeWriter{}
	reader := &fixtureReader{sites: []map[string]any{settingsSite()}}
	p := &Provider{reader: reader, writer: writer}
	_, err := p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"applicationsOpen": true}, 0)
	if err == nil || err.Error() != "This store's wholesale program is not set up yet. The cluster's operators set it up." {
		t.Fatalf("err = %v", err)
	}
	if len(writer.writes) != 0 {
		t.Fatalf("a store with no settings was written: %+v", writer.writes)
	}

	reader.settings = map[string][]map[string]any{"store-live": {{"storeId": "store-live", "ownerUserId": "merchant-1",
		"applicationsOpen": true, "entitlementAdapter": AdapterCustomerTag, "introText": "Apply here.",
		"accountId": "acct-north", "siteId": "site-1", "createdAt": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}}
	if _, err := p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"staffEmail": "desk@example.com"}, 0); err != nil {
		t.Fatalf("updateSettings: %v", err)
	}
	want := map[string]any{"settingsId": SettingsRowID("store-live"), "ownerUserId": "v1:identity:user:merchant-1",
		"storeId": "store-live", "applicationsOpen": true, "entitlementAdapter": AdapterCustomerTag,
		"introText": "Apply here.", "staffEmail": "desk@example.com", "accountId": "acct-north", "siteId": "site-1",
		"termsRequired": false, "linkLifetimeDays": 0, "acceptanceText": ""}
	wrote := writer.only(t, "putWholesaleSettingsRow")
	for k, v := range want {
		if wrote[k] != v {
			t.Errorf("%s = %v, want %v", k, wrote[k], v)
		}
	}
	if len(wrote) != len(want) {
		t.Errorf("the write names %d fields, want every settings field (%d): %+v", len(wrote), len(want), wrote)
	}

	// none means none, an unknown route is refused by name.
	writer.writes = nil
	if _, err := p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"entitlementAdapter": "none"}, 0); err != nil {
		t.Fatalf("updateSettings(none): %v", err)
	}
	if got := writer.only(t, "putWholesaleSettingsRow")["entitlementAdapter"]; got != "" {
		t.Fatalf("none was written as %q, want no route", got)
	}
	if _, err := p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"entitlementAdapter": "draftOrders"}, 0); err == nil || !strings.Contains(err.Error(), "draftOrders") {
		t.Fatalf("an unknown route: %v", err)
	}
	// A site that does not serve the store is refused before anything is written.
	writer.writes = nil
	reader.sites = append(reader.sites, map[string]any{"id": "site-x", "accountId": "acct-north",
		"binding": map[string]any{"storeId": "store-other"}})
	if _, err := p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-x", "storeId": "store-live",
		"applicationsOpen": false}, 0); err == nil || len(writer.writes) != 0 {
		t.Fatalf("an update through a site that does not serve the store: err %v, writes %d", err, len(writer.writes))
	}
}

// SETTING A STORE UP IS THE CLUSTER'S OPERATORS' (OR TRUSTED SERVER CODE'S):
// a person holding the wholesale part is refused before anything is read. With
// a siteId the row is that site's: its owner, its organization and its id.
func TestOnlyAnOperatorOrInternalOriginSetsUpAStore(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{reader: refusingReader{t: t}, writer: writer}
	if _, err := p.setWholesaleSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1",
		"storeId": "store-live", "applicationsOpen": true}, 0); err == nil || len(writer.writes) != 0 {
		t.Fatalf("a holder of the part set a store up: err %v, writes %d", err, len(writer.writes))
	}

	two := map[string]any{"id": "site-2", "ownerUserId": "merchant-2", "accountId": "acct-south",
		"binding": map[string]any{"storeId": "store-live"}}
	elsewhere := map[string]any{"id": "site-x", "ownerUserId": "merchant-1", "accountId": "acct-north",
		"binding": map[string]any{"storeId": "store-other"}}
	reader := &fixtureReader{sites: []map[string]any{settingsSite(), two, elsewhere}}
	p = &Provider{reader: reader, writer: writer}
	got := decodedBy(t)(p.setWholesaleSettings(operatorCtx("operator-1"), map[string]any{"siteId": "site-2",
		"storeId": "store-live", "applicationsOpen": false, "entitlementAdapter": ""}, 0))
	wrote := writer.only(t, "setWholesaleSettingsRow")
	if wrote["ownerUserId"] != "v1:identity:user:merchant-2" || wrote["accountId"] != "acct-south" || wrote["siteId"] != "site-2" {
		t.Fatalf("the set-up through site-2 wrote %+v, want its owner, organization and id", wrote)
	}
	if got["ownerUserId"] != "v1:identity:user:merchant-2" || got["siteId"] != "site-2" {
		t.Fatalf("the receipt %+v does not name whose store it now is", got)
	}
	writer.writes = nil
	if _, err := p.setWholesaleSettings(operatorCtx("operator-1"), map[string]any{"siteId": "site-x",
		"storeId": "store-live", "applicationsOpen": true}, 0); err == nil || len(writer.writes) != 0 {
		t.Fatalf("a set-up through a site that does not serve the store: err %v, writes %d", err, len(writer.writes))
	}
}

// ONLY AN OPERATOR IS TOLD THEY MAY SET A STORE UP, and only while it has no
// settings; once it has, its people and the operators may update it.
func TestStoreSettingsTellsOnlyAnOperatorTheyMaySetUpAStore(t *testing.T) {
	registerShippedAdapters()
	reader := &fixtureReader{sites: []map[string]any{settingsSite()}}
	p := &Provider{reader: reader}
	ask := func(ctx context.Context) (map[string]any, map[string]any) {
		got := decodedBy(t)(p.storeSettings(ctx, map[string]any{"siteId": "site-1", "storeId": "store-live"}, 0))
		may, _ := got["may"].(map[string]any)
		return got, may
	}
	if got, may := ask(operatorCtx("operator-1")); got["exists"] != false || may["setUp"] != true || may["update"] != false {
		t.Fatalf("an operator on a store with no settings: %+v", got)
	}
	if got, may := ask(staffCtx("staff-1")); may["setUp"] != false || may["update"] != false {
		t.Fatalf("the store's person on a store with no settings: %+v", got)
	}
	reader.settings = map[string][]map[string]any{"store-live": {{"storeId": "store-live", "ownerUserId": "merchant-1",
		"applicationsOpen": true, "entitlementAdapter": AdapterCustomerTag, "staffEmail": "desk@example.com",
		"accountId": "acct-north", "siteId": "site-1"}}}
	for name, ctx := range map[string]context.Context{"an operator": operatorCtx("operator-1"), "staff": staffCtx("staff-1")} {
		got, may := ask(ctx)
		if got["exists"] != true || may["setUp"] != false || may["update"] != true || got["staffEmail"] != "desk@example.com" ||
			got["entitlementAdapter"] != AdapterCustomerTag || got["applicationsOpen"] != true {
			t.Fatalf("%s on a store with settings: %+v", name, got)
		}
		if adapters, _ := got["adapters"].([]any); len(adapters) < 2 {
			t.Fatalf("%s is not told the registered routes: %+v", name, got["adapters"])
		}
	}
}

// A STAFF ADDRESS IS ONE ADDRESS OR NONE: without an @, with two, or longer than
// an address can be, it is refused and nothing is written.
func TestAStaffAddressWithoutAnAtIsRefused(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{writer: writer, reader: &fixtureReader{sites: []map[string]any{settingsSite()},
		settings: map[string][]map[string]any{"store-live": {{"storeId": "store-live", "ownerUserId": "merchant-1",
			"applicationsOpen": true, "accountId": "acct-north", "createdAt": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}}}}
	for _, bad := range []string{"desk.example.com", "desk@@example.com", "a@b@example.com", strings.Repeat("a", 310) + "@example.com"} {
		if _, err := p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
			"staffEmail": bad}, 0); err == nil {
			t.Errorf("the staff address %q was accepted", bad)
		}
	}
	if len(writer.writes) != 0 {
		t.Fatalf("a refused address was written: %+v", writer.writes)
	}
	if _, err := p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"staffEmail": ""}, 0); err != nil {
		t.Fatalf("an empty staff address (none) was refused: %v", err)
	}
}

// A STAFF ADDRESS IS AN ADDRESS BY THE PACK'S ONE RULE (ruling 54), the
// applicant's: nothing before or after the @, or whitespace inside, is refused
// and nothing is written; a well-formed address is written as before, trimmed.
func TestAStaffAddressIsHeldToTheApplicantsRule(t *testing.T) {
	newProvider := func() (*Provider, *fakeWriter) {
		writer := &fakeWriter{}
		return &Provider{writer: writer, reader: &fixtureReader{sites: []map[string]any{settingsSite()},
			settings: map[string][]map[string]any{"store-live": {{"storeId": "store-live", "ownerUserId": "merchant-1",
				"applicationsOpen": true, "accountId": "acct-north", "createdAt": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}}}}, writer
	}
	for _, bad := range []string{"desk@", "@example.com", "desk @example.com", "desk@exam ple.com", "desk@example.com\tdesk"} {
		p, writer := newProvider()
		if _, err := p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
			"staffEmail": bad}, 0); err == nil {
			t.Errorf("the staff address %q was accepted", bad)
		}
		if len(writer.writes) != 0 {
			t.Errorf("the refused address %q was written: %+v", bad, writer.writes)
		}
	}
	for given, stored := range map[string]string{
		"desk@example.com": "desk@example.com", "first.last+tag@example.com": "first.last+tag@example.com",
		"  desk@example.com ": "desk@example.com",
	} {
		p, writer := newProvider()
		if _, err := p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
			"staffEmail": given}, 0); err != nil {
			t.Fatalf("the staff address %q was refused: %v", given, err)
		}
		if got := writer.only(t, "putWholesaleSettingsRow")["staffEmail"]; got != stored {
			t.Fatalf("the staff address %q was written as %v, want %q", given, got, stored)
		}
	}
}

// THE SETTINGS OWNER IS READ AT THE ID THE WRITE GOES TO. Two store ids that slug
// alike share one row id, so a lookup by the storeId field would miss the row
// the write then replaces, and hand it to somebody else. Since ruling 36 the row
// is another store's and not this one's: the set-up is refused, and nothing is
// written over it.
func TestTheSettingsOwnerIsReadAtTheIdTheWriteGoesTo(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{writer: writer, reader: &fixtureReader{
		settings: map[string][]map[string]any{"store.live": {{"id": SettingsRowID("store-live"), "storeId": "store.live",
			"ownerUserId": "merchant-1", "applicationsOpen": true, "accountId": "acct-north", "siteId": "site-1", "createdAt": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}},
		sites: []map[string]any{{"id": "site-9", "ownerUserId": "merchant-9", "accountId": "acct-nine", "binding": map[string]any{"storeId": "store-live"}}},
	}}
	_, err := p.setWholesaleSettings(operatorCtx("operator-1"), map[string]any{"storeId": "store-live",
		"applicationsOpen": false}, 0)
	if err == nil || !strings.Contains(err.Error(), "slug alike") {
		t.Fatalf("a set-up at an id another store's row holds: %v, want refused", err)
	}
	if len(writer.writes) != 0 {
		t.Fatalf("the other store's row was written over: %+v", writer.writes)
	}
}

// A PERSON'S ID IS CANONICALIZED, AND NOTHING ELSE IS TURNED INTO ONE.
func TestUserRefLeavesAnIdOfAnotherKindAlone(t *testing.T) {
	for in, want := range map[string]string{
		"merchant-1":                    "v1:identity:user:merchant-1",
		" merchant-1 ":                  "v1:identity:user:merchant-1",
		"v1:identity:user:merchant-1":   "v1:identity:user:merchant-1",
		"system:wholesale-provision":    "system:wholesale-provision",
		"v1:wholesale:application:app1": "v1:wholesale:application:app1",
		"":                              "",
	} {
		if got := userRef(in); got != want {
			t.Errorf("userRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// A SUBMISSION WRITES ITS OWNER IN CANONICAL FORM, like every other owner write.
func TestASubmissionWritesItsOwnerInCanonicalForm(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{writer: writer, reader: &fixtureReader{
		settings: map[string][]map[string]any{"store-live": {{"storeId": "store-live", "applicationsOpen": true}}},
	}}
	if _, err := p.submitApplication(asShopperPath(), map[string]any{
		"submissionId": "sub-canon", "storeId": "store-live", "companyName": "Acme",
		"applicantName": "Sam", "applicantEmail": "sam@example.com",
	}, 0); err != nil {
		t.Fatalf("submitApplication: %v", err)
	}
	if got := writer.only(t, "createApplicationRow")["ownerUserId"]; got != "v1:identity:user:site-owner-1" {
		t.Fatalf("ownerUserId = %v, want the canonical v1:identity:user:site-owner-1", got)
	}
}

// THE BACKFILL TIES THE SETTINGS ROW IT READ: at that row's own id and with its
// own storeId, not at an id rebuilt from the application's store id.
func TestTheBackfillTiesTheSettingsRowItRead(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reader := backfillFixture(map[string]any{"id": "app-1", "ownerUserId": "merchant-1", "storeId": "store-1",
		"companyName": "C", "applicantName": "N", "applicantEmail": "e@example.com", "createdAt": at})
	reader.settings = map[string][]map[string]any{"store-1": {{"id": "v1:wholesale:wholesaleSettings:wholesale-settings-legacy-1",
		"storeId": "v1:shopify:store:store-1", "ownerUserId": "merchant-1", "applicationsOpen": true, "createdAt": at}}}
	writer := &fakeWriter{}
	backfillReport(t, &Provider{writer: writer, reader: reader})
	got := writer.only(t, "backfillSettingsRow")
	if got["settingsId"] != "wholesale-settings-legacy-1" || got["storeId"] != "v1:shopify:store:store-1" {
		t.Fatalf("the settings tie wrote at %v with storeId %v, want the row it read", got["settingsId"], got["storeId"])
	}
	if !memql.HasRowVersionFence(writer.writes[len(writer.writes)-1].ctx, "v1:wholesale:wholesaleSettings", "wholesale-settings-legacy-1") {
		t.Fatal("the settings tie was not fenced at the row it read")
	}
}

// A DECISION AGAINST AN APPLICATION THE CALLER CANNOT READ IS REFUSED, not
// recorded against no store. The read runs under the caller's own actor, so
// zero rows means "not yours" and appending anyway would write a row every
// storefront read would then miss -- hiding nothing while looking like it had.
func TestDecisionOnUnreadableApplicationIsRefused(t *testing.T) {
	p := &Provider{reader: &fixtureReader{}, writer: &fakeWriter{}}
	_, err := p.recordDecision(staffCtx("user-1"), map[string]any{
		"applicationId": "app-nobody-can-read", "transition": TransitionApprove,
		"principalKind": PrincipalClient, "decidedBy": "user-1",
	}, 0)
	if err == nil {
		t.Fatal("a decision on an unreadable application must be refused")
	}
	if !strings.Contains(err.Error(), "not readable") {
		t.Fatalf("the refusal must say why, got: %v", err)
	}
}

// THE STORE IS COPIED OFF THE APPLICATION, never supplied. An argument for
// it would be a way to record a decision scoped to somebody else's store.
func TestDecisionCopiesStoreFromItsSubject(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{writer: writer, reader: &fixtureReader{
		apps: map[string][]map[string]any{
			"app-1": {{"id": "app-1", "storeId": "store-live", "companyName": "Acme"}},
		},
	}}
	_, err := p.recordDecision(staffCtx("user-1"), map[string]any{
		"applicationId": "app-1", "transition": TransitionApprove,
		"principalKind": PrincipalClient, "decidedBy": "user-1",
		// A SUPPLIED storeId IS IGNORED. It is not in the capability's
		// arg schema at all, and this asserts that passing one anyway
		// changes nothing.
		"storeId": "store-somebody-elses",
	}, 0)
	if err != nil {
		t.Fatalf("recordDecision: %v", err)
	}
	// WHAT WAS PERSISTED, not what came back.
	wrote := writer.only(t, "appendApplicationDecision")
	if wrote["storeId"] != "store-live" {
		t.Fatalf("storeId = %v, want store-live -- it must be copied off the application", wrote["storeId"])
	}
	// decidedBy IS NOT PASSED ON: the mutation stamps the ACTOR, because a
	// caller-supplied decider on an append-only log is a way to attribute
	// somebody else's approval to them permanently.
	if _, present := wrote["decidedBy"]; present {
		t.Fatal("decidedBy must not be written from the caller's argument")
	}
}

// A DECISION RECORDS THE APPLICATION'S MERCHANT, copied off the application
// rather than taken from the caller, so the merchant and their staff read one
// log whoever decided.
func TestADecisionRecordsTheApplicationsMerchant(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{writer: writer, reader: &fixtureReader{
		apps: map[string][]map[string]any{
			"app-1": {{"id": "app-1", "storeId": "store-live", "ownerUserId": "merchant-1"}},
		},
	}}
	ctx := auth.ContextWithUserActor(context.Background(), "staff-1")
	if _, err := p.recordDecision(ctx, map[string]any{
		"applicationId": "app-1", "transition": TransitionApprove,
		"principalKind": PrincipalClient, "decidedBy": "staff-1",
	}, 0); err != nil {
		t.Fatalf("recordDecision: %v", err)
	}
	wrote := writer.only(t, "appendApplicationDecision")
	if wrote["ownerUserId"] != "v1:identity:user:merchant-1" {
		t.Fatalf("ownerUserId = %v, want the application's merchant merchant-1", wrote["ownerUserId"])
	}
}

// THE LOG DECIDES LEGALITY, not an argument. A caller that could say what
// state it believed the application was in would be the one deciding.
func TestIllegalTransitionIsRefusedAgainstTheLog(t *testing.T) {
	p := &Provider{reader: &fixtureReader{
		apps: map[string][]map[string]any{
			"app-1": {{"id": "app-1", "storeId": "store-live"}},
		},
		decisions: map[string][]map[string]any{
			"app-1": {{"transition": TransitionReject}},
		},
	}}
	_, err := p.recordDecision(staffCtx("user-1"), map[string]any{
		"applicationId": "app-1", "transition": TransitionApprove,
		"principalKind": PrincipalClient, "decidedBy": "user-1",
	}, 0)
	if err == nil || !strings.Contains(err.Error(), "final") {
		t.Fatalf("approving an already-rejected application must be refused against the log, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The applications-open gate
// ---------------------------------------------------------------------------

// ABSENT SETTINGS ARE CLOSED. A merchant who has never touched their
// wholesale settings has not asked the internet for their customers'
// business details.
func TestAbsentSettingsRefuseASubmission(t *testing.T) {
	p := &Provider{reader: &fixtureReader{}, writer: &fakeWriter{}}
	_, err := p.submitApplication(asShopperPath(), map[string]any{
		"submissionId": "sub-1", "storeId": "store-live", "companyName": "Acme",
		"applicantName": "Sam", "applicantEmail": "sam@example.com",
	}, 0)
	if err == nil {
		t.Fatal("a store with no settings must not accept applications")
	}
}

func TestClosedApplicationsRefuseASubmission(t *testing.T) {
	p := &Provider{reader: &fixtureReader{
		settings: map[string][]map[string]any{
			"store-live": {{"storeId": "store-live", "applicationsOpen": false}},
		},
	}}
	if _, err := p.submitApplication(asShopperPath(), map[string]any{
		"submissionId": "sub-1", "storeId": "store-live", "companyName": "Acme",
		"applicantName": "Sam", "applicantEmail": "sam@example.com",
	}, 0); err == nil {
		t.Fatal("a closed store must refuse a submission")
	}
}

// THE GATE IS PER STORE, which is the whole of what previewing a storefront
// means: a merchant takes applications on their live store while they are
// closed on the development store a candidate is being exercised against.
func TestTheGateIsPerStore(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{writer: writer, reader: &fixtureReader{
		settings: map[string][]map[string]any{
			"store-live": {{"storeId": "store-live", "applicationsOpen": true}},
			"store-dev":  {{"storeId": "store-dev", "applicationsOpen": false}},
		},
	}}
	if _, err := p.submitApplication(asShopperPath(), map[string]any{
		"submissionId": "sub-1", "storeId": "store-live", "companyName": "Acme",
		"applicantName": "Sam", "applicantEmail": "sam@example.com",
	}, 0); err != nil {
		t.Fatalf("submitApplication: %v", err)
	}
	wrote := writer.only(t, "createApplicationRow")
	if wrote["storeId"] != "store-live" {
		t.Fatalf("storeId = %v", wrote["storeId"])
	}
	if _, err := p.submitApplication(asShopperPath(), map[string]any{
		"submissionId": "sub-2", "storeId": "store-dev", "companyName": "Acme",
		"applicantName": "Sam", "applicantEmail": "sam@example.com",
	}, 0); err == nil {
		t.Fatal("the development store is closed and must refuse")
	}
	// AND NOTHING WAS WRITTEN FOR THE CLOSED STORE.
	if writer.count("createApplicationRow") != 1 {
		t.Fatalf("createApplicationRow written %d times; the closed store must write nothing",
			writer.count("createApplicationRow"))
	}
}

// A STORE THAT REQUIRES ITS TERMS TAKES NO APPLICATION UNTIL ALL THREE
// DOCUMENTS EXIST, judged when the application is written, and the shopper is
// refused exactly as a closed store refuses them: the same words, so the same
// redirect. Nothing is written.
func TestASubmissionIsRefusedWhenTermsAreRequiredAndADocumentIsMissing(t *testing.T) {
	writer := &fakeWriter{}
	reader := &fixtureReader{
		settings: map[string][]map[string]any{
			"store-live":   {{"storeId": "store-live", "applicationsOpen": true, "termsRequired": true}},
			"store-closed": {{"storeId": "store-closed", "applicationsOpen": false}},
		},
		docs: docsFor(DocumentTerms, DocumentPriceSheet),
	}
	p := &Provider{writer: writer, reader: reader}
	submit := func(store, id string) error {
		_, err := p.submitApplication(asShopperPath(), map[string]any{
			"submissionId": id, "storeId": store, "companyName": "Spoke and Chain Cycles",
			"applicantName": "Sam", "applicantEmail": "sam@example.com",
		}, 0)
		return err
	}
	closed := submit("store-closed", "sub-0")
	missing := submit("store-live", "sub-1")
	if closed == nil || missing == nil || missing.Error() != closed.Error() {
		t.Fatalf("a submission with the MAP policy missing answered %v; a closed store answers %v", missing, closed)
	}
	if strings.Contains(missing.Error(), "Publish the terms") {
		t.Fatalf("the shopper is told the store's own instruction: %v", missing)
	}
	if len(writer.writes) != 0 {
		t.Fatalf("a refused submission was written: %+v", writer.writes)
	}
	reader.docs = docsFor(DocumentTerms, DocumentMap, DocumentPriceSheet)
	if err := submit("store-live", "sub-2"); err != nil {
		t.Fatalf("a submission with all three documents: %v", err)
	}
	if writer.count("createApplicationRow") != 1 {
		t.Fatalf("createApplicationRow written %d times, want 1", writer.count("createApplicationRow"))
	}
}

// THE MINIMUM IS REQUIRED AND IT IS THE ONLY THING REQUIRED.
func TestTheMinimumAnApplicationNeedsToBeOne(t *testing.T) {
	p := &Provider{writer: &fakeWriter{}, reader: &fixtureReader{
		settings: map[string][]map[string]any{
			"store-live": {{"storeId": "store-live", "applicationsOpen": true}},
		},
	}}
	for _, missing := range []string{"companyName", "applicantName", "applicantEmail"} {
		args := map[string]any{
			"submissionId": "sub-1", "storeId": "store-live", "companyName": "Acme",
			"applicantName": "Sam", "applicantEmail": "sam@example.com",
		}
		delete(args, missing)
		if _, err := p.submitApplication(asShopperPath(), args, 0); err == nil {
			t.Errorf("an application with no %s is not an application", missing)
		}
	}
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// ONE ROW PER STORE at a derived id, so a second write is a new VERSION of
// one logical row rather than a second row a read would have to choose
// between.
func TestSettingsAreOneRowPerStore(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{reader: &fixtureReader{sites: []map[string]any{settingsSite(),
		{"id": "site-dev", "ownerUserId": "merchant-1", "accountId": "acct-north", "binding": map[string]any{"storeId": "store-dev"}}}}, writer: writer}
	for _, tc := range []struct {
		store, site string
		open        bool
	}{{"store-live", "site-1", true}, {"store-live", "site-1", false}, {"store-dev", "site-dev", true}} {
		if _, err := p.setWholesaleSettings(internalAsMerchant(),
			map[string]any{"storeId": tc.store, "siteId": tc.site, "applicationsOpen": tc.open}, 0); err != nil {
			t.Fatal(err)
		}
	}
	if len(writer.writes) != 3 {
		t.Fatalf("writes = %d, want 3", len(writer.writes))
	}
	first, second, dev := writer.writes[0].args, writer.writes[1].args, writer.writes[2].args
	if first["settingsId"] != second["settingsId"] {
		t.Fatal("two writes for one store must be two versions of ONE row")
	}
	if dev["settingsId"] == first["settingsId"] {
		t.Fatal("two stores must not share one settings row")
	}
	// A DERIVED ID MUST BE A BARE SLUG. The engine refuses a row id with
	// colons in it, which no fixture would ever have noticed.
	for _, id := range []any{first["settingsId"], dev["settingsId"]} {
		if strings.Contains(asString(id), ":") {
			t.Fatalf("derived settings id %q carries a colon; the engine refuses it "+
				"(docs/public/concepts/identifiers.md)", id)
		}
	}
}

// AN UNKNOWN ADAPTER IS REFUSED AT SETTINGS TIME, so a merchant who typed
// it wrong learns now rather than when their first approved application
// fails to provision -- the moment they are least able to act on it.
func TestUnknownAdapterIsRefusedWhenSettingsAreWritten(t *testing.T) {
	registerShippedAdapters()
	p := &Provider{reader: &fixtureReader{sites: []map[string]any{settingsSite()}}, writer: &fakeWriter{}}
	_, err := p.setWholesaleSettings(internalAsMerchant(), map[string]any{
		"storeId": "store-live", "applicationsOpen": true,
		"entitlementAdapter": "shopifyB2b", // wrong case; a plausible typo
	}, 0)
	if err == nil {
		t.Fatal("an unknown adapter name must be refused when settings are written")
	}
	if !strings.Contains(err.Error(), AdapterShopifyB2B) {
		t.Fatalf("the refusal must list what is registered, got: %v", err)
	}
	if _, err := p.setWholesaleSettings(internalAsMerchant(), map[string]any{
		"storeId": "store-live", "applicationsOpen": true,
		"entitlementAdapter": AdapterShopifyB2B,
	}, 0); err != nil {
		t.Fatalf("a registered adapter must be accepted: %v", err)
	}
}

// THE ROW IS WRITTEN AT THE SUBMISSION ID (design record 2026-09-21, D4).
//
// It was engine-derived before this seam, which left the new application's
// id known to nobody -- and a client extension has to relate its own row to
// THIS application. The receipt still carries no row id, deliberately: the
// id travels DOWN from the bff into both constructs rather than back out of
// this one.
func TestTheApplicationIsWrittenAtTheSubmissionId(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{writer: writer, reader: &fixtureReader{
		settings: map[string][]map[string]any{
			"store-live": {{"storeId": "store-live", "applicationsOpen": true}},
		},
	}}
	if _, err := p.submitApplication(asShopperPath(), map[string]any{
		"submissionId": "sub-abc", "storeId": "store-live", "companyName": "Acme",
		"applicantName": "Sam", "applicantEmail": "sam@example.com",
	}, 0); err != nil {
		t.Fatalf("submitApplication: %v", err)
	}
	wrote := writer.only(t, "createApplicationRow")
	if wrote["applicationId"] != "sub-abc" {
		t.Fatalf("applicationId = %v, want sub-abc -- a client extension relates to this id",
			wrote["applicationId"])
	}
	// AND IT IS OWNED BY THE SITE OWNER whose authority the handler borrowed,
	// passed explicitly now that the mutation no longer stamps it.
	if wrote["ownerUserId"] != "v1:identity:user:site-owner-1" {
		t.Fatalf("ownerUserId = %v, want the borrowed site owner site-owner-1", wrote["ownerUserId"])
	}
}

// A submission that reaches the construct without one is a caller that is
// not the bff, and it is refused rather than written at a derived id
// nothing can point at.
func TestASubmissionWithNoSubmissionIdIsRefused(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{writer: writer, reader: &fixtureReader{
		settings: map[string][]map[string]any{
			"store-live": {{"storeId": "store-live", "applicationsOpen": true}},
		},
	}}
	if _, err := p.submitApplication(asShopperPath(), map[string]any{
		"storeId": "store-live", "companyName": "Acme",
		"applicantName": "Sam", "applicantEmail": "sam@example.com",
	}, 0); err == nil {
		t.Fatal("an application with no submission id was written")
	}
	if writer.count("createApplicationRow") != 0 {
		t.Fatal("a row was written for a submission carrying no id")
	}
}

// ---------------------------------------------------------------------------
// Who may write: the writer's stamp escapes the row gate
// ---------------------------------------------------------------------------

// operatorCtx is a cluster operator's own signed-in authority.
func operatorCtx(userID string) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userID, Role: auth.RoleAdmin})
}

// SETTINGS ARE SET BY THE CLUSTER'S OPERATORS OR TRUSTED SERVER CODE. Anyone
// else -- the store's own merchant included -- is refused before anything is
// read, because the write at internal origin is not owner-checked.
func TestSettingsAreSetByAnOperatorOrInternalOriginOnly(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{reader: refusingReader{t: t}, writer: writer}
	_, err := p.setWholesaleSettings(auth.ContextWithUserActor(context.Background(), "merchant-1"),
		map[string]any{"storeId": "store-live", "applicationsOpen": true}, 0)
	if err == nil {
		t.Fatal("a merchant who is not an operator set a store's wholesale settings")
	}
	if !strings.Contains(err.Error(), "cluster's operators") {
		t.Fatalf("the refusal must say who sets them, got: %v", err)
	}
	if len(writer.writes) != 0 {
		t.Fatalf("a refused call wrote: %+v", writer.writes)
	}

	existing := &fixtureReader{settings: map[string][]map[string]any{
		"store-live": {{"storeId": "store-live", "ownerUserId": "merchant-1", "applicationsOpen": true, "accountId": "acct-north", "createdAt": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}},
	}}
	for name, ctx := range map[string]context.Context{
		"an operator":   operatorCtx("operator-1"),
		"internal code": internalAsMerchant(),
	} {
		w := &fakeWriter{}
		p := &Provider{reader: existing, writer: w}
		if _, err := p.setWholesaleSettings(ctx,
			map[string]any{"storeId": "store-live", "applicationsOpen": false}, 0); err != nil {
			t.Fatalf("%s was refused: %v", name, err)
		}
		// THE EXISTING OWNER IS KEPT, never moved to whoever wrote.
		if got := w.only(t, "setWholesaleSettingsRow")["ownerUserId"]; got != "v1:identity:user:merchant-1" {
			t.Fatalf("%s's write moved the settings to %v; the existing owner merchant-1 stays", name, got)
		}
	}
}

// A NEW STORE'S SETTINGS ARE OWNED BY THE STORE'S MERCHANT -- the owner of
// the site the program belongs to -- and TIED TO THAT SITE'S ORGANIZATION
// (ruling 55): the named site, or the one site bound to the store. With no site
// named and not exactly one bound, it is refused with NameTheSiteSentence --
// one owner's two sites and trusted code with no bound site included, which
// were accepted before and left the row tied to nobody -- and so is a site of
// no organization.
func TestANewStoresSettingsAreOwnedByItsSitesOwner(t *testing.T) {
	site := func(id, owner, account string) map[string]any {
		return map[string]any{"id": id, "ownerUserId": owner, "accountId": account, "binding": map[string]any{"storeId": "store-live"}}
	}
	for _, tc := range []struct {
		name    string
		sites   []map[string]any
		ctx     context.Context
		siteID  string
		owner   string
		account string
		refusal string
	}{
		{"one site", []map[string]any{site("site-1", "merchant-1", "acct-north")}, operatorCtx("operator-1"), "", "v1:identity:user:merchant-1", "acct-north", ""},
		{"one owner's two sites", []map[string]any{site("site-1", "merchant-1", "acct-north"), site("site-2", "v1:identity:user:merchant-1", "acct-north")},
			operatorCtx("operator-1"), "", "", "", NameTheSiteSentence},
		{"one owner's two sites, one named", []map[string]any{site("site-1", "merchant-1", "acct-north"), site("site-2", "merchant-1", "acct-south")},
			operatorCtx("operator-1"), "site-2", "v1:identity:user:merchant-1", "acct-south", ""},
		{"two owners", []map[string]any{site("site-1", "merchant-1", "acct-north"), site("site-2", "merchant-2", "acct-south")}, operatorCtx("operator-1"), "", "", "", NameTheSiteSentence},
		{"no site", nil, operatorCtx("operator-1"), "", "", "", NameTheSiteSentence},
		{"internal code, no site", nil, internalAsMerchant(), "", "", "", NameTheSiteSentence},
		{"a site of no organization", []map[string]any{site("site-1", "merchant-1", "")}, operatorCtx("operator-1"), "", "", "", "belongs to no organization"},
	} {
		w := &fakeWriter{}
		p := &Provider{reader: &fixtureReader{sites: tc.sites}, writer: w}
		args := map[string]any{"storeId": "store-live", "applicationsOpen": true}
		if tc.siteID != "" {
			args["siteId"] = tc.siteID
		}
		_, err := p.setWholesaleSettings(tc.ctx, args, 0)
		if tc.refusal != "" {
			if err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("%s: %v, want %q", tc.name, err, tc.refusal)
			}
			if len(w.writes) != 0 {
				t.Fatalf("%s: a refused call wrote", tc.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		row := w.only(t, "setWholesaleSettingsRow")
		if row["ownerUserId"] != tc.owner || row["accountId"] != tc.account {
			t.Fatalf("%s: the row is %v's of %v, want %s's of %s", tc.name, row["ownerUserId"], row["accountId"], tc.owner, tc.account)
		}
	}
}

// A REPEATED SUBMISSION ANSWERS ITS ID AND WRITES NOTHING. The id is the
// one the bff minted for one POST, so seeing it again is the same POST.
func TestARepeatedSubmissionAnswersItsIdAndWritesNothing(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{writer: writer, reader: &fixtureReader{
		settings: map[string][]map[string]any{
			"store-live": {{"storeId": "store-live", "applicationsOpen": true}},
		},
		apps: map[string][]map[string]any{
			"sub-abc": {{"id": "sub-abc", "storeId": "store-live", "ownerUserId": "site-owner-1"}},
		},
	}}
	got := decodedBy(t)(p.submitApplication(asShopperPath(), map[string]any{
		"submissionId": "sub-abc", "storeId": "store-live", "companyName": "Acme",
		"applicantName": "Sam", "applicantEmail": "sam@example.com",
	}, 0))
	if got["applicationId"] != "sub-abc" {
		t.Fatalf("a repeated submission answered %v, want its id sub-abc", got)
	}
	if writer.count("createApplicationRow") != 0 {
		t.Fatal("a repeated submission wrote a second version")
	}
}

// A NEW APPLICATION IS WRITTEN FENCED ON "ROW ABSENT", so two submissions at
// one id at once cannot both land.
func TestANewApplicationIsFencedOnRowAbsent(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{writer: writer, reader: &fixtureReader{
		settings: map[string][]map[string]any{
			"store-live": {{"storeId": "store-live", "applicationsOpen": true}},
		},
	}}
	if _, err := p.submitApplication(asShopperPath(), map[string]any{
		"submissionId": "sub-new", "storeId": "store-live", "companyName": "Acme",
		"applicantName": "Sam", "applicantEmail": "sam@example.com",
	}, 0); err != nil {
		t.Fatalf("submitApplication: %v", err)
	}
	writer.only(t, "createApplicationRow")
	if !memql.HasRowVersionFence(writer.writes[0].ctx, "v1:wholesale:application", "sub-new") {
		t.Fatal("the new application was written with no row-absent fence")
	}
}

// A DECISION AND AN ENTITLEMENT COPY THE APPLICATION'S ORGANIZATION, never the
// caller's, so the people who may read the application read what was decided
// about it and what was granted.
func TestADecisionAndAnEntitlementCopyTheApplicationsAccount(t *testing.T) {
	registerShippedAdapters()
	writer := &fakeWriter{}
	reader := approvedFixture()
	reader.apps["app-1"][0]["accountId"] = "acct-north"
	p := &Provider{writer: writer, reader: reader, caller: &fakeCaller{}}

	reader.decisions["app-1"] = nil // submitted, so approve is legal
	if _, err := p.recordDecision(auth.ContextWithUserActor(context.Background(), "staff-1"), map[string]any{
		"applicationId": "app-1", "transition": TransitionApprove,
		"principalKind": PrincipalClient, "decidedBy": "staff-1",
		"accountId": "acct-somebody-elses",
	}, 0); err != nil {
		t.Fatalf("recordDecision: %v", err)
	}
	if got := writer.only(t, "appendApplicationDecision")["accountId"]; got != "acct-north" {
		t.Fatalf("the decision's accountId = %v, want the application's acct-north", got)
	}

	reader.decisions["app-1"] = []map[string]any{{"transition": TransitionApprove}}
	if _, err := p.provisionEntitlement(internalAsMerchant(), map[string]any{"applicationId": "app-1"}, 0); err != nil {
		t.Fatalf("provisionEntitlement: %v", err)
	}
	if got := writer.only(t, "writeEntitlementRow")["accountId"]; got != "acct-north" {
		t.Fatalf("the entitlement's accountId = %v, want the application's acct-north", got)
	}
}

// A DIRECT SUBMISSION MAY CARRY AN ACCOUNT ONLY FROM THE SITE'S OWN OWNER, ON
// A STORE THAT SITE SERVES. Any signed-in caller who reaches the builtin can
// name an accountId, and an application tied to an organization is readable by
// its people; the shopper handler stamps exactly the site, its store, its
// account and its owner, so the shopper path passes.
func TestASubmittedAccountMustBeTheSites(t *testing.T) {
	served := func(store string) map[string]any { return map[string]any{"storeId": store} }
	reader := &fixtureReader{
		settings: map[string][]map[string]any{
			"store-live": {{"storeId": "store-live", "applicationsOpen": true}},
			"store-b":    {{"storeId": "store-b", "applicationsOpen": true}},
		},
		sites: []map[string]any{
			{"id": "v1:platform:site:site-1", "ownerUserId": "v1:identity:user:site-owner-1",
				"accountId": "v1:accounts:account:acct-north", "binding": served("store-live")},
			{"id": "site-b", "ownerUserId": "v1:identity:user:owner-b",
				"accountId": "acct-b", "binding": served("store-b")},
			{"id": "site-x", "ownerUserId": "site-owner-1",
				"accountId": "acct-north", "binding": served("store-other")},
		},
	}
	submitAs := func(store, siteID, account string) (*fakeWriter, error) {
		writer := &fakeWriter{}
		p := &Provider{writer: writer, reader: reader}
		_, err := p.submitApplication(asShopperPath(), map[string]any{
			"storeId": store, "siteId": siteID, "accountId": account, "submissionId": "sub-" + store + siteID + account,
			"companyName": "Acme", "applicantName": "Sam", "applicantEmail": "sam@example.com",
		}, 0)
		return writer, err
	}
	for name, c := range map[string][3]string{
		"another organization": {"store-live", "site-1", "acct-south"},
		"no site":              {"store-live", "", "acct-north"},
		"an unknown site":      {"store-live", "site-9", "acct-north"},
		"B's own site and account, not B's owner": {"store-b", "site-b", "acct-b"},
		"a site that does not serve the store":    {"store-live", "site-x", "acct-north"},
	} {
		writer, err := submitAs(c[0], c[1], c[2])
		if err == nil || len(writer.writes) != 0 {
			t.Fatalf("%s: the account was written (err %v, writes %d)", name, err, len(writer.writes))
		}
	}
	writer, err := submitAs("store-live", "site-1", "acct-north")
	if err != nil {
		t.Fatalf("the shopper path's own site, store, account and owner were refused: %v", err)
	}
	if got := writer.only(t, "createApplicationRow"); got["accountId"] != "acct-north" || got["source"] != SourceStorefront {
		t.Fatalf("the application was written with accountId %v and source %v", got["accountId"], got["source"])
	}
}

// THE BACKFILL'S ACCOUNT IS THE APPLICATION'S SITE'S, or, with no siteId, the
// one site serving its store -- and when it cannot be told, the reason is one
// of three sentences the operator acts on.
func TestTheAccountOfAnApplicationIsItsSites(t *testing.T) {
	north := map[string]any{"id": "v1:platform:site:site-n", "accountId": "acct-north",
		"binding": map[string]any{"storeId": "v1:shopify:store:store-1"}}
	south := map[string]any{"id": "site-s", "accountId": "acct-south",
		"previewBinding": map[string]any{"storeId": "store-1"}}
	bare := map[string]any{"id": "site-b", "binding": map[string]any{"storeId": "store-2"}}
	for name, c := range map[string]struct {
		app             map[string]any
		sites           []map[string]any
		account, reason string
	}{
		"its own site":                 {map[string]any{"storeId": "store-1", "siteId": "site-s"}, []map[string]any{north, south}, "acct-south", ""},
		"the one site serving it":      {map[string]any{"storeId": "store-1"}, []map[string]any{north}, "acct-north", ""},
		"a preview binding serves too": {map[string]any{"storeId": "store-1"}, []map[string]any{south}, "acct-south", ""},
		"no site":                      {map[string]any{"storeId": "store-9"}, []map[string]any{north}, "", UntiedNoSite},
		"two sites":                    {map[string]any{"storeId": "store-1"}, []map[string]any{north, south}, "", UntiedTwoSites},
		"a site with no organization":  {map[string]any{"storeId": "store-2"}, []map[string]any{bare}, "", UntiedNoAccount},
		"its own site serves another":  {map[string]any{"storeId": "store-2", "siteId": "site-n"}, []map[string]any{north, bare}, "", UntiedForeignSite},
	} {
		account, reason := accountForApplication(c.app, c.sites)
		if account != c.account || reason != c.reason {
			t.Errorf("%s: (%q, %q), want (%q, %q)", name, account, reason, c.account, c.reason)
		}
	}
	if UntiedForeignSite != "its site does not serve this store" || UntiedNoSite != "no site serves this store" || UntiedTwoSites != "two sites serve this store" ||
		UntiedNoAccount != "its site has no organization" {
		t.Fatal("an untied reason drifted from its sentence")
	}
}

// A COPIED OWNER IS STORED IN THE FORM THE READ PREDICATE MATCHES: the read
// compares ownerUserId with actor.userId as written, and a real actor's userId
// is the canonical v1:identity:user:<id>. The decision's and the entitlement's
// owner from the application, and a new settings row's from its site, are
// stored that way whatever form the source row used.
func TestACopiedOwnerIsStoredInTheFormTheReadPredicateMatches(t *testing.T) {
	registerShippedAdapters()
	writer := &fakeWriter{}
	reader := approvedFixture() // its application is owned by the bare "merchant-1"
	p := &Provider{writer: writer, reader: reader, caller: &fakeCaller{}}
	reader.decisions["app-1"] = nil
	if _, err := p.recordDecision(auth.ContextWithUserActor(context.Background(), "staff-1"), map[string]any{
		"applicationId": "app-1", "transition": TransitionApprove, "principalKind": PrincipalClient, "decidedBy": "staff-1",
	}, 0); err != nil {
		t.Fatalf("recordDecision: %v", err)
	}
	reader.decisions["app-1"] = []map[string]any{{"transition": TransitionApprove}}
	if _, err := p.provisionEntitlement(internalAsMerchant(), map[string]any{"applicationId": "app-1"}, 0); err != nil {
		t.Fatalf("provisionEntitlement: %v", err)
	}
	for _, m := range []string{"appendApplicationDecision", "writeEntitlementRow"} {
		if got := writer.only(t, m)["ownerUserId"]; got != "v1:identity:user:merchant-1" {
			t.Fatalf("%s's ownerUserId = %v, want v1:identity:user:merchant-1", m, got)
		}
	}

	settingsWriter := &fakeWriter{}
	sp := &Provider{writer: settingsWriter, reader: &fixtureReader{
		sites: []map[string]any{{"id": "site-1", "ownerUserId": "merchant-2", "accountId": "acct-new", "binding": map[string]any{"storeId": "store-new"}}},
	}}
	if _, err := sp.setWholesaleSettings(operatorCtx("operator-1"), map[string]any{
		"storeId": "store-new", "applicationsOpen": true,
	}, 0); err != nil {
		t.Fatalf("setWholesaleSettings: %v", err)
	}
	if got := settingsWriter.only(t, "setWholesaleSettingsRow")["ownerUserId"]; got != "v1:identity:user:merchant-2" {
		t.Fatalf("the new settings' ownerUserId = %v, want v1:identity:user:merchant-2", got)
	}
}

// backfillFixture is one store served by one site of organization north.
func backfillFixture(walk ...map[string]any) *fixtureReader {
	return &fixtureReader{
		walk:  walk,
		sites: []map[string]any{{"id": "site-1", "accountId": "acct-north", "binding": map[string]any{"storeId": "store-1"}}},
	}
}

func backfillReport(t *testing.T, p *Provider) map[string]any {
	t.Helper()
	return decodedBy(t)(p.backfillAccounts(operatorCtx("operator-1"), map[string]any{"storeId": "store-1"}, 0))
}

// ONE BAD ROW NEVER ABORTS THE RUN. An application with no owner, and one whose
// write fails, are reported with why; the next is tied; the counts come back.
func TestOneBadRowNeverAbortsTheBackfill(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	app := func(id, owner string, i int) map[string]any {
		return map[string]any{"id": id, "ownerUserId": owner, "storeId": "store-1", "companyName": "C",
			"applicantName": "N", "applicantEmail": "e@example.com", "createdAt": at.Add(time.Duration(i) * time.Second)}
	}
	writer := &fakeWriter{refuse: func(mutation string, args map[string]any) error {
		if args["applicationId"] == "app-broken" {
			return errors.New("the write was refused")
		}
		return nil
	}}
	p := &Provider{writer: writer, reader: backfillFixture(
		app("app-ownerless", "", 0), app("app-broken", "merchant-1", 1), app("app-good", "merchant-1", 2))}
	report := backfillReport(t, p)
	if report["tied"] != float64(1) {
		t.Fatalf("report = %+v, want the good application tied", report)
	}
	untied, _ := report["untied"].([]any)
	if len(untied) != 2 {
		t.Fatalf("report = %+v, want the ownerless and the broken application reported", report)
	}
	reasons := map[string]string{}
	for _, u := range untied {
		m, _ := u.(map[string]any)
		reasons[asString(m["id"])] = asString(m["reason"])
	}
	if reasons["app-ownerless"] != UntiedNoOwner || !strings.Contains(reasons["app-broken"], "the write was refused") {
		t.Fatalf("untied reasons = %v", reasons)
	}
}

// A FULL WINDOW WHOSE ROWS ALL SHARE ONE CREATION TIME cannot be walked past by
// a createdAt cursor, and the backfill says so rather than stopping quietly.
func TestTheBackfillWalkRefusesAWindowThatSharesOneCreationTime(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var walk []map[string]any
	for _, id := range []string{"app-a", "app-b", "app-c"} {
		walk = append(walk, map[string]any{"id": id, "ownerUserId": "merchant-1", "storeId": "store-1",
			"accountId": "acct-north", "createdAt": at})
	}
	p := &Provider{writer: &fakeWriter{}, reader: backfillFixture(walk...), backfillWindow: 2}
	_, err := p.backfillAccounts(operatorCtx("operator-1"), map[string]any{"storeId": "store-1"}, 0)
	if err == nil || !strings.Contains(err.Error(), "one creation time") {
		t.Fatalf("a window sharing one creation time was walked or refused for another reason: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Fix round 1
// ---------------------------------------------------------------------------

// systemLog answers decisions and entitlements only to the pack's own system
// actor, as the real tier does for a row written before the account tie (no
// ownerUserId, no accountId): the store's side reads none of it.
type systemLog struct {
	*fixtureReader
	t *testing.T
	// forbidSystem fails a decision read as the system.
	forbidSystem bool
}

func (r systemLog) Rows(ctx context.Context, query string, args map[string]string) ([]map[string]any, error) {
	if query == "decisionsForApplication" || query == "entitlementForApplication" {
		if !strings.Contains(actorOf(ctx), provisionActor) {
			return nil, nil
		}
		if r.forbidSystem {
			r.t.Fatalf("%s was read as the system for a caller who cannot read the application", query)
		}
	}
	return r.fixtureReader.Rows(ctx, query, args)
}

// A DECISION'S LEGALITY IS JUDGED AGAINST THE WHOLE LOG (X27): a rejection the
// caller cannot read, from before the account tie, still makes an approve
// illegal, and the refusal is the one an illegal transition always gets.
func TestALegalityFoldReadsTheWholeLog(t *testing.T) {
	reader := &fixtureReader{
		apps:      map[string][]map[string]any{"app-1": {{"id": "app-1", "storeId": "store-live", "ownerUserId": "merchant-1"}}},
		decisions: map[string][]map[string]any{"app-1": {{"id": "d-1", "transition": TransitionReject}}},
	}
	writer := &fakeWriter{}
	p := &Provider{reader: systemLog{fixtureReader: reader, t: t}, writer: writer}
	_, err := p.recordDecision(staffCtx("merchant-1"), map[string]any{"applicationId": "app-1", "transition": TransitionApprove}, 0)
	if err == nil || err.Error() != LegalTransition(StateRejected, TransitionApprove).Error() {
		t.Fatalf("an approve over an unreadable rejection: %v", err)
	}
	if len(writer.writes) != 0 {
		t.Fatalf("an illegal decision was written: %+v", writer.writes)
	}
}

// THE STATE IS FOLDED FROM THE WHOLE LOG FOR A CALLER WHO READS THE
// APPLICATION, and a caller who cannot read it is told submitted -- what any
// id they cannot see answers -- with no decision read as the system for them.
func TestTheStateIsFoldedFromTheWholeLogOnlyForAReaderOfTheApplication(t *testing.T) {
	reader := &fixtureReader{
		apps:      map[string][]map[string]any{"app-1": {{"id": "app-1", "storeId": "store-live", "ownerUserId": "merchant-1"}}},
		decisions: map[string][]map[string]any{"app-1": {{"id": "d-1", "transition": TransitionApprove}}},
	}
	p := &Provider{reader: systemLog{fixtureReader: reader, t: t}}
	if got := decodedBy(t)(p.applicationState(staffCtx("merchant-1"), map[string]any{"applicationId": "app-1"}, 0)); got["state"] != StateApproved {
		t.Fatalf("the merchant is told %v, want approved", got["state"])
	}
	stranger := &fixtureReader{decisions: reader.decisions} // reads no application
	p = &Provider{reader: systemLog{fixtureReader: stranger, t: t, forbidSystem: true}}
	got := decodedBy(t)(p.applicationState(staffCtx("stranger-1"), map[string]any{"applicationId": "app-1"}, 0))
	if got["state"] != StateSubmitted || got["stage"] != StageReadyToReview {
		t.Fatalf("a caller who cannot read the application is told %+v, want submitted", got)
	}
}

// A DECISION ROW NAMES ITS APPLICATION IN CANONICAL FORM, and every read keyed
// by an application id asks both forms, so a log written under either is whole.
func TestReadsKeyedByAnApplicationIdAskBothForms(t *testing.T) {
	registerShippedAdapters()
	canon := "v1:wholesale:application:app-1"
	writer := &fakeWriter{}
	reader := &fixtureReader{apps: map[string][]map[string]any{"app-1": {{"id": "app-1", "storeId": "store-live", "ownerUserId": "merchant-1"}}}}
	p := &Provider{reader: reader, writer: writer}
	if _, err := p.recordDecision(staffCtx("staff-1"), map[string]any{"applicationId": "app-1", "transition": TransitionApprove}, 0); err != nil {
		t.Fatalf("recordDecision: %v", err)
	}
	if got := writer.only(t, "appendApplicationDecision")["applicationId"]; got != canon {
		t.Fatalf("the decision names its application %v, want %s", got, canon)
	}
	// A log kept under the canonical form is folded for a caller naming the bare id.
	reader.decisions = map[string][]map[string]any{canon: {{"id": "d-1", "transition": TransitionReject}}}
	if _, err := p.recordDecision(staffCtx("staff-1"), map[string]any{"applicationId": "app-1", "transition": TransitionApprove}, 0); err == nil {
		t.Fatal("a rejection kept under the canonical id was not folded for the bare id")
	}
	// And an entitlement kept under one form is found by the other.
	fx := approvedFixture()
	fx.decisions["app-1"] = []map[string]any{{"transition": TransitionRevoke}}
	fx.ents[canon] = []map[string]any{{"applicationId": canon, "storeId": "store-live", "adapter": AdapterCustomerTag,
		"state": EntitlementGranted, "reference": "customer=gid://shopify/Customer/1;tag=acme-trade"}}
	caller := &fakeCaller{}
	if _, err := (&Provider{reader: fx, writer: &fakeWriter{}, caller: caller}).retryAccess(staffCtx("staff-1"),
		map[string]any{"applicationId": "app-1"}, 0); err != nil || len(caller.calls) != 1 {
		t.Fatalf("an entitlement kept under the canonical id was not found for the bare id: %v, calls %v", err, caller.calls)
	}
}

// A RETRY OVER AN APPROVED APPLICATION PROVISIONS, AS ITS MERCHANT -- and a
// granted row survives a retry that finds no route.
func TestRetryAccessProvisionsAnApprovedApplicationAndKeepsAGrant(t *testing.T) {
	registerShippedAdapters()
	reader := approvedFixture()
	writer, caller := &fakeWriter{}, &fakeCaller{}
	p := &Provider{reader: reader, writer: writer, caller: caller}
	got := decodedBy(t)(p.retryAccess(staffCtx("staff-1"), map[string]any{"applicationId": "app-1"}, 0))
	if len(caller.calls) != 1 || caller.calls[0] != "shopifyTagWholesaleCustomer" || caller.actors[0] != "merchant-1" {
		t.Fatalf("the retry called %v as %v, want the tag put on once as the merchant", caller.calls, caller.actors)
	}
	if w := writer.only(t, "writeEntitlementRow"); w["state"] != EntitlementGranted || got["state"] != EntitlementGranted {
		t.Fatalf("the retry wrote %v and answered %v, want granted", w["state"], got["state"])
	}
	reader.ents["app-1"] = []map[string]any{{"applicationId": "app-1", "storeId": "store-live", "adapter": AdapterCustomerTag,
		"state": EntitlementGranted, "reference": "customer=gid://shopify/Customer/1;tag=acme-trade"}}
	reader.settings["store-live"][0]["entitlementAdapter"] = ""
	writer.writes, caller.calls = nil, nil
	got = decodedBy(t)(p.retryAccess(staffCtx("staff-1"), map[string]any{"applicationId": "app-1"}, 0))
	if len(writer.writes) != 0 || len(caller.calls) != 0 || got["state"] != EntitlementGranted {
		t.Fatalf("a retry with no route touched the grant: writes %+v, calls %v, answer %+v", writer.writes, caller.calls, got)
	}
}

// AN UPDATE IS FENCED ON THE VERSION IT READ, and a cleared staff address is
// written as the empty value.
func TestAnUpdateIsFencedAndClearsTheStaffAddress(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writer := &fakeWriter{}
	p := &Provider{writer: writer, reader: &fixtureReader{sites: []map[string]any{settingsSite()},
		settings: map[string][]map[string]any{"store-live": {{"storeId": "store-live", "ownerUserId": "merchant-1",
			"applicationsOpen": true, "accountId": "acct-north", "staffEmail": "desk@example.com", "createdAt": at}}}}}
	if _, err := p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"staffEmail": ""}, 0); err != nil {
		t.Fatalf("updateSettings: %v", err)
	}
	if got := writer.only(t, "putWholesaleSettingsRow")["staffEmail"]; got != "" {
		t.Fatalf("a cleared staff address was written as %v", got)
	}
	if !memql.HasRowVersionFence(writer.writes[0].ctx, "v1:wholesale:wholesaleSettings", SettingsRowID("store-live")) {
		t.Fatal("the update was not fenced on the version it read")
	}
	// A save that loses the fence is refused with the sentence the OS shows.
	lost := &Provider{reader: p.reader, writer: &fakeWriter{err: memql.ErrRowVersionChanged}}
	if _, err := lost.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"introText": "x"}, 0); err == nil || err.Error() != "These settings changed while you were editing. Reload and try again." {
		t.Fatalf("a save that lost its fence: %v", err)
	}
}

// A RE-SET-UP BY THE OPERATORS WRITES EVERY FIELD, the staff address the row
// already held included.
func TestASetUpKeepsTheStaffAddress(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{writer: writer, reader: &fixtureReader{settings: map[string][]map[string]any{"store-live": {{
		"storeId": "store-live", "ownerUserId": "merchant-1", "applicationsOpen": true, "accountId": "acct-north", "staffEmail": "desk@example.com", "createdAt": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}}}}
	if _, err := p.setWholesaleSettings(operatorCtx("operator-1"), map[string]any{"storeId": "store-live", "applicationsOpen": false}, 0); err != nil {
		t.Fatalf("setWholesaleSettings: %v", err)
	}
	if got, named := writer.only(t, "setWholesaleSettingsRow")["staffEmail"]; !named || got != "desk@example.com" {
		t.Fatalf("the re-set-up wrote staffEmail %v (named %v), want the address it held", got, named)
	}
}

// A SITE OF ANOTHER ORGANIZATION THAT BINDS THE STORE CANNOT ACT ON IT once its
// settings are tied to one: not to update them, read them, or set them up.
func TestASiteOfAnotherOrganizationIsRefusedOnTheStoresSettings(t *testing.T) {
	registerShippedAdapters()
	// THE CHECK DOES NOT DEPEND ON HOW THE STORE IS SPELLED (ruling 34): the
	// row is at the bare id's settings id, and the store is named both ways.
	for _, store := range []string{"store-live", "v1:shopify:store:store-live"} {
		south := map[string]any{"id": "site-s", "ownerUserId": "merchant-2", "accountId": "acct-south",
			"binding": map[string]any{"storeId": "store-live"}}
		reader := &fixtureReader{sites: []map[string]any{settingsSite(), south},
			settings: map[string][]map[string]any{"store-live": {{"storeId": "store-live", "ownerUserId": "merchant-1",
				"applicationsOpen": true, "accountId": "acct-north", "siteId": "site-1"}}}}
		writer := &fakeWriter{}
		p := &Provider{reader: reader, writer: writer}
		args := map[string]any{"siteId": "site-s", "storeId": store, "applicationsOpen": false}
		for name, call := range map[string]func() error{
			"wholesaleUpdateSettings": func() error { _, err := p.updateSettings(staffCtx("staff-s"), args, 0); return err },
			"wholesaleStoreSettings":  func() error { _, err := p.storeSettings(staffCtx("staff-s"), args, 0); return err },
			"wholesaleSetSettings":    func() error { _, err := p.setWholesaleSettings(operatorCtx("operator-1"), args, 0); return err },
		} {
			if err := call(); err == nil || !strings.Contains(err.Error(), "another organization") {
				t.Errorf("store named %q: %s through a site of another organization: %v", store, name, err)
			}
		}
		if len(writer.writes) != 0 {
			t.Fatalf("store named %q: a refused call wrote: %+v", store, writer.writes)
		}
		// Nothing but the site and the store's settings is read to refuse.
		for _, q := range reader.asked {
			if q != "siteById" && q != "wholesaleSettingsById" && q != "wholesaleSettingsForStore" {
				t.Errorf("store named %q: a refused call read %s", store, q)
			}
		}
	}
}

// A STORE'S OWN STAFF NAMING THEIR STORE IN THE CANONICAL FORM read and save
// their settings, and the save lands on the row a bare-id save would (ruling
// 34). A store set up under the canonical form is set up at the bare id's row.
func TestTheCanonicalFormOfAStoreIdReachesItsOneSettingsRow(t *testing.T) {
	registerShippedAdapters()
	canonical := "v1:shopify:store:store-live"
	reader := &fixtureReader{sites: []map[string]any{settingsSite()},
		settings: map[string][]map[string]any{"store-live": {{"storeId": "store-live", "ownerUserId": "merchant-1",
			"applicationsOpen": true, "accountId": "acct-north", "siteId": "site-1",
			"createdAt": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}}}
	writer := &fakeWriter{}
	p := &Provider{reader: reader, writer: writer}
	got := decodedBy(t)(p.storeSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": canonical}, 0))
	if got["exists"] != true || got["applicationsOpen"] != true {
		t.Fatalf("named canonically, the settings read %+v", got)
	}
	if _, err := p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": canonical,
		"introText": "Hello."}, 0); err != nil {
		t.Fatalf("named canonically, the save: %v", err)
	}
	wrote := writer.only(t, "putWholesaleSettingsRow")
	if wrote["settingsId"] != SettingsRowID("store-live") || wrote["storeId"] != "store-live" {
		t.Fatalf("the canonical save wrote %+v, want the bare id's row", wrote)
	}
	if !memql.HasRowVersionFence(writer.writes[0].ctx, settingsConcept, SettingsRowID("store-live")) {
		t.Fatal("the canonical save is not fenced on the row it read")
	}

	fresh := &fixtureReader{sites: []map[string]any{settingsSite()}}
	freshWriter := &fakeWriter{}
	p = &Provider{reader: fresh, writer: freshWriter}
	if _, err := p.setWholesaleSettings(operatorCtx("operator-1"), map[string]any{"siteId": "site-1", "storeId": canonical,
		"applicationsOpen": false}, 0); err != nil {
		t.Fatalf("a set-up named canonically: %v", err)
	}
	set := freshWriter.only(t, "setWholesaleSettingsRow")
	if set["settingsId"] != SettingsRowID("store-live") || set["storeId"] != "store-live" {
		t.Fatalf("a new row named canonically was written as %+v, want the bare id's row and form", set)
	}
}

// ---------------------------------------------------------------------------
// Fix round 2
// ---------------------------------------------------------------------------

// grantRecorder is an adapter that records the Grant it is handed.
type grantRecorder struct{ ids *[]string }

const grantRecorderName = "grantRecorderForTest"

var recordedGrantIDs []string

func (grantRecorder) Name() string           { return grantRecorderName }
func (grantRecorder) Available(string) error { return nil }
func (r grantRecorder) Provision(_ context.Context, _ Caller, g Grant) (Outcome, error) {
	*r.ids = append(*r.ids, "provision "+g.ApplicationID)
	return Outcome{Reference: "ref-1"}, nil
}
func (r grantRecorder) Revoke(_ context.Context, _ Caller, g Grant) (Outcome, error) {
	*r.ids = append(*r.ids, "revoke "+g.ApplicationID)
	return Outcome{Reference: "ref-1"}, nil
}

// bothForms answers the application at its bare and its canonical id, as the
// engine's applicationById does.
func bothForms(fx *fixtureReader) *fixtureReader {
	fx.apps["v1:wholesale:application:app-1"] = fx.apps["app-1"]
	return fx
}

// AN ADAPTER IS HANDED THE CANONICAL APPLICATION ID, whatever form the caller
// named it by (ruling 28): the B2B route makes it the company's idempotency key,
// so two forms would be two companies.
func TestTheAdapterIsHandedTheCanonicalApplicationId(t *testing.T) {
	registerShippedAdapters()
	if _, err := AdapterByName(grantRecorderName); err != nil {
		RegisterAdapter(grantRecorder{ids: &recordedGrantIDs})
	}
	const canon = "v1:wholesale:application:app-1"
	// The B2B route's own call.
	for _, asked := range []string{"app-1", canon} {
		fx := bothForms(approvedFixture())
		fx.settings["store-live"][0]["entitlementAdapter"] = AdapterShopifyB2B
		caller := &fakeCaller{}
		p := &Provider{reader: fx, writer: &fakeWriter{}, caller: caller}
		if _, err := p.provisionEntitlement(internalAsMerchant(), map[string]any{"applicationId": asked}, 0); err != nil {
			t.Fatalf("provision asked %q: %v", asked, err)
		}
		if len(caller.args) != 1 || caller.args[0]["applicationId"] != canon {
			t.Fatalf("provision asked %q handed shopifyProvisionWholesale %v, want %s", asked, caller.args, canon)
		}
	}
	// Every Grant, provision and revoke.
	for _, asked := range []string{"app-1", canon} {
		recordedGrantIDs = nil
		fx := bothForms(approvedFixture())
		fx.settings["store-live"][0]["entitlementAdapter"] = grantRecorderName
		p := &Provider{reader: fx, writer: &fakeWriter{}, caller: &fakeCaller{}}
		if _, err := p.provisionEntitlement(internalAsMerchant(), map[string]any{"applicationId": asked}, 0); err != nil {
			t.Fatalf("provision: %v", err)
		}
		fx.decisions["app-1"] = []map[string]any{{"transition": TransitionRevoke}}
		fx.ents["app-1"] = []map[string]any{{"applicationId": canon, "storeId": "store-live", "adapter": grantRecorderName,
			"state": EntitlementGranted, "reference": "ref-1"}}
		if _, err := p.revokeEntitlement(internalAsMerchant(), map[string]any{"applicationId": asked}, 0); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if len(recordedGrantIDs) != 2 || recordedGrantIDs[0] != "provision "+canon || recordedGrantIDs[1] != "revoke "+canon {
			t.Fatalf("asked %q, the adapter was handed %v, want the canonical id both times", asked, recordedGrantIDs)
		}
	}
	// A retry over a granted row hands the B2B route the id the first provision did.
	fx := bothForms(approvedFixture())
	fx.settings["store-live"][0]["entitlementAdapter"] = AdapterShopifyB2B
	caller := &fakeCaller{}
	p := &Provider{reader: fx, writer: &fakeWriter{}, caller: caller}
	if _, err := p.provisionEntitlement(internalAsMerchant(), map[string]any{"applicationId": canon}, 0); err != nil {
		t.Fatalf("provision: %v", err)
	}
	fx.ents["app-1"] = []map[string]any{{"applicationId": canon, "storeId": "store-live", "adapter": AdapterShopifyB2B,
		"state": EntitlementGranted, "reference": "gid://shopify/Company/1"}}
	if _, err := p.retryAccess(staffCtx("staff-1"), map[string]any{"applicationId": "app-1"}, 0); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(caller.args) != 2 || caller.args[1]["applicationId"] != caller.args[0]["applicationId"] {
		t.Fatalf("the retry handed the route %v, the first provision %v", caller.args[len(caller.args)-1], caller.args[0])
	}
}

// A SET-UP IS FENCED ON WHAT IT READ: an existing row on its version, an absent
// row on absent; one that loses the fence is refused with the update's sentence.
func TestASetUpIsFencedOnWhatItRead(t *testing.T) {
	writer := &fakeWriter{}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reader := &fixtureReader{settings: map[string][]map[string]any{"store-live": {{"storeId": "store-live",
		"ownerUserId": "merchant-1", "applicationsOpen": true, "accountId": "acct-north", "createdAt": at}}},
		sites: []map[string]any{{"id": "site-n", "ownerUserId": "merchant-2", "accountId": "acct-new", "binding": map[string]any{"storeId": "store-new"}}}}
	p := &Provider{reader: reader, writer: writer}
	for _, store := range []string{"store-live", "store-new"} {
		writer.writes = nil
		if _, err := p.setWholesaleSettings(operatorCtx("operator-1"), map[string]any{"storeId": store, "applicationsOpen": false}, 0); err != nil {
			t.Fatalf("%s: %v", store, err)
		}
		if !memql.HasRowVersionFence(writer.writes[0].ctx, "v1:wholesale:wholesaleSettings", SettingsRowID(store)) {
			t.Fatalf("the set-up of %s was not fenced on what it read", store)
		}
	}
	lost := &Provider{reader: reader, writer: &fakeWriter{err: memql.ErrRowVersionChanged}}
	if _, err := lost.setWholesaleSettings(operatorCtx("operator-1"), map[string]any{"storeId": "store-live", "applicationsOpen": true}, 0); err == nil ||
		err.Error() != ChangedWhileEditingSentence {
		t.Fatalf("a set-up that lost its fence: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Task 6, fix round 3: a store with two settings rows
// ---------------------------------------------------------------------------

// A ROW FOUND AT A STORE'S SETTINGS ID IS COUNTED ONLY WHEN IT IS THAT STORE'S:
// two store ids that slug alike derive one id, and must never share a row.
func TestASettingsRowOfAStoreThatSlugsAlikeIsNotTheStoresRow(t *testing.T) {
	if SettingsRowID("store.live") != SettingsRowID("store-live") {
		t.Fatal("the case needs two store ids that slug alike")
	}
	reader := &fixtureReader{settings: map[string][]map[string]any{"store-live": {{"storeId": "store-live",
		"ownerUserId": "merchant-1", "applicationsOpen": true, "accountId": "acct-north"}}}}
	p := &Provider{reader: reader}
	row, err := p.settingsRowAt(context.Background(), "store.live")
	if err != nil || row != nil {
		t.Fatalf("store.live resolves %+v (%v), want no row: the one at its id is store-live's", row, err)
	}
	if row, _ := p.settingsRowAt(context.Background(), "v1:shopify:store:store-live"); row == nil {
		t.Fatal("the canonical form of store-live does not resolve its row")
	}
}

// twoSettingsRows is a store holding a row under each spelling of its id.
func twoSettingsRows(bareAccount, canonicalAccount string) *fixtureReader {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	south := map[string]any{"id": "site-s", "ownerUserId": "merchant-2", "accountId": "acct-south",
		"binding": map[string]any{"storeId": "store-live"}}
	return &fixtureReader{sites: []map[string]any{settingsSite(), south}, settings: map[string][]map[string]any{
		"store-live": {{"storeId": "store-live", "ownerUserId": "merchant-1", "applicationsOpen": true,
			"accountId": bareAccount, "introText": "the bare row", "createdAt": at}},
		"v1:shopify:store:store-live": {{"storeId": "v1:shopify:store:store-live", "ownerUserId": "merchant-2",
			"applicationsOpen": false, "accountId": canonicalAccount, "introText": "the canonical row", "createdAt": at}},
	}}
}

// TWO ROWS TIED TO TWO ORGANIZATIONS: a site of either is refused, because the
// store's settings name the other too. The bare row is the one read.
func TestTwoSettingsRowsTiedToTwoOrganizationsRefuseBoth(t *testing.T) {
	registerShippedAdapters()
	p := &Provider{reader: twoSettingsRows("acct-north", "acct-south"), writer: &fakeWriter{}}
	for _, site := range []string{"site-1", "site-s"} {
		if _, err := p.siteForStore(staffCtx("staff"), site, "store-live"); err == nil || !strings.Contains(err.Error(), "another organization") {
			t.Errorf("through %s: %v, want refused for another organization", site, err)
		}
	}
	if row, _ := p.settingsRowAt(context.Background(), "v1:shopify:store:store-live"); row == nil || row["introText"] != "the bare row" {
		t.Fatalf("the store resolves %+v, want the bare row", row)
	}
}

// THE BARE ROW TIED, THE CANONICAL ONE UNTIED: the bare row's organization is
// admitted, the other refused, and the save lands on the bare row.
func TestTwoSettingsRowsWithOneUntiedAdmitTheTiedOrganization(t *testing.T) {
	registerShippedAdapters()
	writer := &fakeWriter{}
	p := &Provider{reader: twoSettingsRows("acct-north", ""), writer: writer}
	if _, err := p.siteForStore(staffCtx("staff"), "site-s", "store-live"); err == nil || !strings.Contains(err.Error(), "another organization") {
		t.Errorf("the other organization's site: %v, want refused", err)
	}
	if _, err := p.siteForStore(staffCtx("staff"), "site-1", "v1:shopify:store:store-live"); err != nil {
		t.Fatalf("the tied organization's site: %v", err)
	}
	if _, err := p.updateSettings(staffCtx("staff"), map[string]any{"siteId": "site-1", "storeId": "v1:shopify:store:store-live",
		"introText": "saved"}, 0); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := writer.only(t, "putWholesaleSettingsRow"); got["settingsId"] != SettingsRowID("store-live") || got["storeId"] != "store-live" {
		t.Fatalf("the save wrote %+v, want the bare row", got)
	}
}

// THE BACKFILL TIES THE STORE'S ROW AND NAMES THE SECOND ONE, tying nothing of
// it; a store whose applications use both spellings is handled once.
func TestTheBackfillNamesASecondSettingsRowAndHandlesTheStoreOnce(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	app := func(id, store string, i int) map[string]any {
		return map[string]any{"id": id, "ownerUserId": "merchant-1", "storeId": store, "siteId": "site-1", "companyName": "C",
			"applicantName": "N", "applicantEmail": "e@example.com", "createdAt": at.Add(time.Duration(i) * time.Second)}
	}
	reader := backfillFixture(app("app-bare", "store-1", 0), app("app-canon", "v1:shopify:store:store-1", 1))
	reader.settings = map[string][]map[string]any{
		"store-1": {{"storeId": "store-1", "ownerUserId": "merchant-1", "applicationsOpen": true, "createdAt": at}},
		"v1:shopify:store:store-1": {{"storeId": "v1:shopify:store:store-1", "ownerUserId": "merchant-1",
			"applicationsOpen": false, "createdAt": at}},
	}
	writer := &fakeWriter{}
	report := backfillReport(t, &Provider{writer: writer, reader: reader})
	var ties []map[string]any
	for _, w := range writer.writes {
		if w.mutation == "backfillSettingsRow" {
			ties = append(ties, w.args)
		}
	}
	if len(ties) != 1 || ties[0]["settingsId"] != SettingsRowID("store-1") {
		t.Fatalf("the settings ties are %+v, want the bare row's alone", ties)
	}
	untied, _ := report["untied"].([]any)
	second := 0
	for _, u := range untied {
		m := u.(map[string]any)
		if m["reason"] == UntiedSecondSettings {
			second++
			if m["id"] != SettingsRowID("v1:shopify:store:store-1") {
				t.Errorf("the second row is reported as %v", m["id"])
			}
		}
	}
	// Two applications, their settings row once: three rows tied, none twice.
	if second != 1 || report["tied"] != float64(3) || report["alreadyTied"] != float64(0) {
		t.Fatalf("the report is %+v, want three tied, none already tied, and the second row named once", report)
	}
}
