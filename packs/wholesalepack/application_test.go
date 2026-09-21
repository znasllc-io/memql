package wholesalepack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
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
}

func (f *fixtureReader) Rows(_ context.Context, query string, args map[string]string) ([]map[string]any, error) {
	f.asked = append(f.asked, query)
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
	}
	return nil, nil
}

func (f *fixtureReader) RowsWithList(_ context.Context, query string, _ map[string]string,
	_ string, _ []string) ([]map[string]any, error) {
	f.asked = append(f.asked, query)
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
}

type writeCall struct {
	mutation string
	args     map[string]any
}

func (f *fakeWriter) Write(_ context.Context, mutation string, args map[string]any) error {
	if f.err != nil {
		return f.err
	}
	f.writes = append(f.writes, writeCall{mutation: mutation, args: args})
	return nil
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
	_, err := p.submitApplication(context.Background(), map[string]any{
		"storeId": "store-live", "companyName": "Acme",
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
	if _, err := p.submitApplication(context.Background(), map[string]any{
		"storeId": "store-live", "companyName": "Acme",
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
	if _, err := p.submitApplication(context.Background(), map[string]any{
		"storeId": "store-live", "companyName": "Acme",
		"applicantName": "Sam", "applicantEmail": "sam@example.com",
	}, 0); err != nil {
		t.Fatalf("submitApplication: %v", err)
	}
	wrote := writer.only(t, "createApplicationRow")
	if wrote["storeId"] != "store-live" {
		t.Fatalf("storeId = %v", wrote["storeId"])
	}
	if _, err := p.submitApplication(context.Background(), map[string]any{
		"storeId": "store-dev", "companyName": "Acme",
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
			"storeId": "store-live", "companyName": "Acme",
			"applicantName": "Sam", "applicantEmail": "sam@example.com",
		}
		delete(args, missing)
		if _, err := p.submitApplication(context.Background(), args, 0); err == nil {
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
		if _, err := p.setWholesaleSettings(context.Background(),
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
	_, err := p.setWholesaleSettings(context.Background(), map[string]any{
		"storeId": "store-live", "applicationsOpen": true,
		"entitlementAdapter": "shopifyB2b", // wrong case; a plausible typo
	}, 0)
	if err == nil {
		t.Fatal("an unknown adapter name must be refused when settings are written")
	}
	if !strings.Contains(err.Error(), AdapterShopifyB2B) {
		t.Fatalf("the refusal must list what is registered, got: %v", err)
	}
	if _, err := p.setWholesaleSettings(context.Background(), map[string]any{
		"storeId": "store-live", "applicationsOpen": true,
		"entitlementAdapter": AdapterShopifyB2B,
	}, 0); err != nil {
		t.Fatalf("a registered adapter must be accepted: %v", err)
	}
}
