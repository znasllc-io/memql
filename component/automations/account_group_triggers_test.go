package automations

import (
	"testing"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// The account group automations (dsl/accounts/automations.memql) fire on
// writes to v1:accounts:account, and every write to an account publishes
// node.created whatever version it is (executor_mutation.go): unfiltered,
// ensureAccountGroup ran on every version of every account. On production
// (2026-11-06) one account written 1,439 times in a day started 2,880
// journaled runs of this pair.
//
// Two properties, neither visible to the automation corpus, which answers
// every call and never evaluates a filter or opens a journal:
//
//   - ensureAccountGroup fires on an account's FIRST version only;
//   - both hold their journal until the builtin writes (@journalOnWrite), so a
//     run that found the group in place leaves no run or step rows. The
//     generic mechanism is TestEventJournalOnWrite; this pins that these two
//     declare it, and that the declaration is one the loader accepts.

func loadedAccountGroupAutomations(t *testing.T) (ensure, refresh *Automation) {
	t.Helper()
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	loaded, err := NewLoader(LoaderOptions{}).LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	for _, a := range loaded {
		switch {
		case a == nil:
		case a.Name == "ensureAccountGroup":
			ensure = a
		case a.Name == "refreshAccountGroup":
			refresh = a
		}
	}
	if ensure == nil || refresh == nil {
		t.Fatalf("the embedded tree loads ensureAccountGroup=%v refreshAccountGroup=%v; both are under test", ensure != nil, refresh != nil)
	}
	return ensure, refresh
}

// accountCreatedEvent is the event executeWrite publishes for one write to an
// account: the stored payload flattened onto the envelope and kept whole under
// `payload`, with firstVersion set after the flatten.
func accountCreatedEvent(id string, firstVersion bool) events.Event {
	payload := map[string]any{"name": "Acme", "status": "active", "domainStatus": "verifying"}
	flat := map[string]any{
		"id": id, "nodeId": id, "concept": "v1:accounts:account", "nodeType": "node",
		"actor": "system:customDomain", "payload": payload,
	}
	for k, v := range payload {
		flat[k] = v
	}
	flat["firstVersion"] = firstVersion
	ev := events.NewEvent("graph.node.created.v1:accounts:account", events.KindNodeCreated, flat)
	ev.Metadata = map[string]string{"actor": "system:customDomain"}
	return ev
}

func TestEnsureAccountGroupFiresOnTheAccountsFirstVersionOnly(t *testing.T) {
	ensure, _ := loadedAccountGroupAutomations(t)
	if !filterFires(t, ensure, accountCreatedEvent("v1:accounts:account:acme", true)) {
		t.Error("ensureAccountGroup did not fire for a new account: it would get no group until the next boot backfill")
	}
	if filterFires(t, ensure, accountCreatedEvent("v1:accounts:account:acme", false)) {
		t.Error("ensureAccountGroup fired for a later version of an account; every write publishes node.created, " +
			"so it would run on every version -- refreshAccountGroup already covers updates")
	}
}

func TestAccountGroupAutomationsJournalOnlyWhenTheyWrite(t *testing.T) {
	ensure, refresh := loadedAccountGroupAutomations(t)
	for _, a := range []*Automation{ensure, refresh} {
		if !a.JournalOnWrite {
			t.Errorf("%s does not declare @journalOnWrite: a run that finds its group in place would still "+
				"leave a v1:work:run and its v1:work:step rows", a.Name)
		}
		if err := prepareJournalContract(a); err != nil {
			t.Errorf("%s: the journal contract refuses the declaration: %v", a.Name, err)
		}
		if !holdsBackgroundJournal(a, "event:"+a.Trigger.Event, nil) {
			t.Errorf("%s: an event firing does not hold its journal until a write", a.Name)
		}
	}
}
