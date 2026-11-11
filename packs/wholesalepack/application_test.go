package wholesalepack

import (
	"context"
	"encoding/json"
	"errors"
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
	case "siteById":
		for _, site := range f.sites {
			if bareID(trimmed(site["id"])) == bareID(args["siteId"]) {
				return []map[string]any{site}, nil
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

// A PROVIDER OPERATOR IS INEXPRESSIBLE on the write path, exactly as
// reviewspack's ClientMayModerate refuses one. Reading every merchant's
// applications is what makes an operator surface possible; deciding one is
// not the operator's to do.
func TestProviderOperatorCannotDecide(t *testing.T) {
	if err := ClientMayDecide("provider"); err == nil {
		t.Fatal("a Provider operator must be refused")
	}
	if err := ClientMayDecide(PrincipalClient); err != nil {
		t.Fatal(err)
	}

	p := &Provider{reader: &fixtureReader{}, writer: &fakeWriter{}}
	if _, err := p.recordDecision(context.Background(), map[string]any{
		"applicationId": "app-1", "transition": TransitionApprove,
		"principalKind": "provider", "decidedBy": "op-1",
	}, 0); err == nil {
		t.Fatal("expected a provider refusal from the capability")
	}
}

// A DECISION AGAINST AN APPLICATION THE CALLER CANNOT READ IS REFUSED, not
// recorded against no store. The read runs under the caller's own actor, so
// zero rows means "not yours" and appending anyway would write a row every
// storefront read would then miss -- hiding nothing while looking like it had.
func TestDecisionOnUnreadableApplicationIsRefused(t *testing.T) {
	p := &Provider{reader: &fixtureReader{}, writer: &fakeWriter{}}
	_, err := p.recordDecision(context.Background(), map[string]any{
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
	_, err := p.recordDecision(context.Background(), map[string]any{
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
	_, err := p.recordDecision(context.Background(), map[string]any{
		"applicationId": "app-1", "transition": TransitionApprove,
		"principalKind": PrincipalClient, "decidedBy": "user-1",
	}, 0)
	if err == nil {
		t.Fatal("approving an already-rejected application must be refused")
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
	p := &Provider{reader: &fixtureReader{}, writer: writer}
	for _, tc := range []struct {
		store string
		open  bool
	}{{"store-live", true}, {"store-live", false}, {"store-dev", true}} {
		if _, err := p.setWholesaleSettings(internalAsMerchant(),
			map[string]any{"storeId": tc.store, "applicationsOpen": tc.open}, 0); err != nil {
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
	p := &Provider{reader: &fixtureReader{}, writer: &fakeWriter{}}
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
	if wrote["ownerUserId"] != "site-owner-1" {
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
		"store-live": {{"storeId": "store-live", "ownerUserId": "merchant-1", "applicationsOpen": true}},
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
// the deployables bound to it -- so the merchant's own reads (the shopper
// path's applicationsOpen among them) see them. A store whose bound sites
// name no owner, or more than one, is refused rather than guessed at.
func TestANewStoresSettingsAreOwnedByItsSitesOwner(t *testing.T) {
	site := func(owner string) map[string]any {
		return map[string]any{"ownerUserId": owner, "binding": map[string]any{"storeId": "store-live"}}
	}
	for _, tc := range []struct {
		name   string
		sites  []map[string]any
		ctx    context.Context
		owner  string
		refuse bool
	}{
		{"one owner", []map[string]any{site("merchant-1"), site("v1:identity:user:merchant-1")}, operatorCtx("operator-1"), "v1:identity:user:merchant-1", false},
		{"two owners", []map[string]any{site("merchant-1"), site("merchant-2")}, operatorCtx("operator-1"), "", true},
		{"no site", nil, operatorCtx("operator-1"), "", true},
		{"internal code, no site", nil, internalAsMerchant(), "v1:identity:user:merchant-1", false},
	} {
		w := &fakeWriter{}
		p := &Provider{reader: &fixtureReader{sites: tc.sites}, writer: w}
		_, err := p.setWholesaleSettings(tc.ctx, map[string]any{"storeId": "store-live", "applicationsOpen": true}, 0)
		if tc.refuse {
			if err == nil {
				t.Fatalf("%s: a new settings row was written with no single merchant to own it: %+v", tc.name, w.writes)
			}
			if len(w.writes) != 0 {
				t.Fatalf("%s: a refused call wrote", tc.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := w.only(t, "setWholesaleSettingsRow")["ownerUserId"]; got != tc.owner {
			t.Fatalf("%s: ownerUserId = %v, want %s", tc.name, got, tc.owner)
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
		sites: []map[string]any{{"id": "site-1", "ownerUserId": "merchant-2", "binding": map[string]any{"storeId": "store-new"}}},
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
