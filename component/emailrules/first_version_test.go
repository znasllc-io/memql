package emailrules

// first_version_test.go -- a created rule fires on a row's FIRST write, once
// (memql#5368).
//
// The store is append-only, so every write publishes graph.node.created. A rule
// on "created" that read the topic alone fired on every later write to the row
// as well: a welcome email when a user is created went out again each time the
// user's row changed. Three pieces make "created" true as written, and each is
// pinned here:
//
//   - the write path marks .created with whether the write materialised the
//     row's first version (firstVersion; its DB-gated test is in component/memql);
//   - a created rule's generated trigger admits only that write;
//   - its fire path claims (rule, row), so two writes RACING on a new row --
//     both of which honestly see no prior version -- send once.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

func ruleOn(eventKind, condition string) Rule {
	r := baseRule()
	r.EventKind = eventKind
	r.Condition = condition
	return r
}

// TestCreatedRuleTriggerIsTheFirstVersionGuard: a created rule's filter is its
// condition behind the guard, parenthesised whole so its own `||` cannot escape
// it; a changed rule's is the condition alone. Both load through every loader a
// rule meets.
func TestCreatedRuleTriggerIsTheFirstVersionGuard(t *testing.T) {
	for _, c := range []struct {
		kind, condition, filter string
		declares                bool
	}{
		{"created", "", `args.firstVersion == true`, true},
		{"created", `row.role == "admin"`, `args.firstVersion == true && (row.role == "admin")`, true},
		{"created", `row.plan != "free" || row.seats > 10`, `args.firstVersion == true && (row.plan != "free" || row.seats > 10)`, true},
		{"updated", `row.role == "admin"`, `row.role == "admin"`, false},
	} {
		src, err := GenerateAutomation(ruleOn(c.kind, c.condition))
		if err != nil {
			t.Fatalf("%s %q: %v", c.kind, c.condition, err)
		}
		// memqlmigrate:keep -- builds the expected v1 filter; not a fixture.
		if want := "@filter(row => " + c.filter + ")\n"; !strings.Contains(src, want) {
			t.Errorf("%s %q generated no %s:\n%s", c.kind, c.condition, strings.TrimSpace(want), src)
		}
		if got := strings.Contains(src, firstVersionArg); got != c.declares {
			t.Errorf("%s %q: declares %s = %v, want %v", c.kind, c.condition, firstVersionArg, got, c.declares)
		}
		loadsThroughTheRealCompiler(t, src)
	}
	if src, _ := GenerateAutomation(ruleOn("updated", "")); strings.Contains(src, "@filter") {
		t.Errorf("a changed rule with no condition carries a filter:\n%s", src)
	}
}

// TestCreatedRuleFiresOncePerRowAndAChangedRuleOncePerUpdate drives the real
// authored scheduler with the generated constructs and the events the write
// path publishes: an insert, two later updates of the same row, and a second
// new row.
func TestCreatedRuleFiresOncePerRowAndAChangedRuleOncePerUpdate(t *testing.T) {
	bus := events.NewBus()
	var mu sync.Mutex
	fired := map[string]int{}
	scheduler, err := automations.NewAuthoredScheduler(automations.AuthoredSchedulerOptions{
		Loader:   automations.NewLoader(automations.LoaderOptions{Registry: concept.DefaultRegistry()}),
		EventBus: bus,
		Run: func(_ context.Context, a *automations.Automation, _ *events.Event) error {
			mu.Lock()
			defer mu.Unlock()
			fired[a.Name]++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.Stop()

	const owner = "v1:identity:user:owner1"
	arm := func(id, eventKind string) string {
		r := ruleOn(eventKind, `row.primaryEmail.includes("@acme.com")`)
		r.ID = id
		src, err := GenerateAutomation(r)
		if err != nil {
			t.Fatal(err)
		}
		name := ConstructNameFor(id)
		if err := scheduler.Activate(&memql.AuthoredConstruct{
			OwnerUserId: owner, Kind: "automation", Name: name,
			BundleId: "v1:authoring:bundle:" + name, Version: 1, Source: src,
		}); err != nil {
			t.Fatalf("activate %s: %v", id, err)
		}
		return name
	}
	createdRule := arm("v1:campaigns:emailRule:welcome", "created")
	changedRule := arm("v1:campaigns:emailRule:changes", "updated")

	// The events executeWrite and executeUpdate publish: the stored payload
	// flattened and again under `payload`, firstVersion on .created.
	publish := func(action, short, email string, firstVersion any) {
		id := "v1:identity:user:" + short
		stored := map[string]any{"primaryEmail": email}
		payload := map[string]any{
			"id": id, "nodeId": id, "concept": "v1:identity:user", "nodeType": "node",
			"primaryEmail": email, "payload": stored,
		}
		if firstVersion != nil {
			payload["firstVersion"] = firstVersion
		}
		kind := events.KindNodeCreated
		if action == "updated" {
			kind = events.KindNodeUpdated
		}
		bus.PublishSync(events.NewEvent("graph.node."+action+".v1:identity:user", kind, payload))
	}
	insert := func(short, email string) { publish("created", short, email, true) }
	update := func(short, email string) {
		publish("created", short, email, false) // every write publishes .created
		publish("updated", short, email, nil)
	}

	insert("eve", "eve@acme.com")
	update("eve", "eve@acme.com")
	update("eve", "eve@acme.com")
	insert("finn", "finn@acme.com")
	// The condition still decides: a new row outside it fires neither rule.
	insert("dana", "dana@example.org")
	// An event without the marker -- a replica on an older engine -- fires
	// no created rule.
	publish("created", "gus", "gus@acme.com", nil)

	mu.Lock()
	defer mu.Unlock()
	if got := fired[createdRule]; got != 2 {
		t.Errorf("the created rule fired %d times, want 2: once for each new row, never for their updates", got)
	}
	if got := fired[changedRule]; got != 2 {
		t.Errorf("the changed rule fired %d times, want 2: once per update", got)
	}
}

// fireEngine answers the reads a firing makes and records every call.
type fireEngine struct {
	mu    sync.Mutex
	rule  map[string]any
	calls []string
}

func (e *fireEngine) Execute(_ context.Context, q string) (any, error) {
	e.mu.Lock()
	e.calls = append(e.calls, q)
	e.mu.Unlock()
	switch {
	case strings.HasPrefix(q, "query emailRuleById"):
		return rowsEnvelope([]map[string]any{e.rule}), nil
	case strings.HasPrefix(q, "query templateById"):
		return rowsEnvelope([]map[string]any{{"id": "v1:campaigns:template:t1", "subject": "Welcome", "textBody": "Hello"}}), nil
	case strings.HasPrefix(q, "query activeUsers"):
		return rowsEnvelope([]map[string]any{{"id": "v1:identity:user:owner1", "role": "owner", "primaryEmail": "owner@acme.com"}}), nil
	}
	return rowsEnvelope(nil), nil
}

func (e *fireEngine) count(prefix string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// stagedIDs are the outbound request ids a firing staged.
func (e *fireEngine) stagedIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var ids []string
	for _, c := range e.calls {
		if strings.HasPrefix(c, "mutation stageOutboundRequest") {
			start := strings.Index(c, `requestId: "`) + len(`requestId: "`)
			ids = append(ids, c[start:start+strings.Index(c[start:], `"`)])
		}
	}
	return ids
}

func activeRule(eventKind string) map[string]any {
	row := ruleRow()
	row["eventKind"] = eventKind
	row["status"] = "active"
	row["recipientRoles"] = []any{"owner"}
	return row
}

// keyClaim is the claim's contract -- one winner per (rule, row) -- held in
// memory; the primary key it stands for is exercised by the real table.
func keyClaim() FirstFireClaim {
	var mu sync.Mutex
	won := map[string]bool{}
	return func(_ context.Context, ruleID, rowID string) bool {
		mu.Lock()
		defer mu.Unlock()
		if won[ruleID+"|"+rowID] {
			return false
		}
		won[ruleID+"|"+rowID] = true
		return true
	}
}

// TestRacingFirstWritesSendOnce: two firings of a created rule for one new row
// -- the race the marker cannot close -- send once, and the loser is not
// recorded as a firing. A second row sends again.
func TestRacingFirstWritesSendOnce(t *testing.T) {
	e := &fireEngine{rule: activeRule("created")}
	claim := keyClaim()
	var wg sync.WaitGroup
	outcomes := make([]FireOutcome, 2)
	for i := range outcomes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := NewFirer(e).WithFirstFireClaim(claim).Fire(context.Background(),
				"v1:campaigns:emailRule:ab12cd34", "v1:identity:user:eve", map[string]any{})
			if err != nil {
				t.Errorf("fire: %v", err)
			}
			outcomes[i] = out
		}(i)
	}
	wg.Wait()
	if got := e.count("mutation stageOutboundRequest"); got != 1 {
		t.Fatalf("two firings for one new row staged %d sends, want 1", got)
	}
	if got := e.count("mutation recordEmailRuleFiring"); got != 1 {
		t.Errorf("recorded %d firings, want 1: the duplicate is not a firing", got)
	}
	if outcomes[0].Duplicate == outcomes[1].Duplicate {
		t.Errorf("want exactly one duplicate, got %+v and %+v", outcomes[0], outcomes[1])
	}

	if _, err := NewFirer(e).WithFirstFireClaim(claim).Fire(context.Background(),
		"v1:campaigns:emailRule:ab12cd34", "v1:identity:user:finn", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if got := e.count("mutation stageOutboundRequest"); got != 2 {
		t.Errorf("a second new row staged %d sends in all, want 2", got)
	}
}

// TestCreatedRuleWithNoClaimSendsNothing: a created rule that cannot prove it
// is first does not send.
func TestCreatedRuleWithNoClaimSendsNothing(t *testing.T) {
	e := &fireEngine{rule: activeRule("created")}
	out, err := NewFirer(e).Fire(context.Background(), "v1:campaigns:emailRule:ab12cd34", "v1:identity:user:eve", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Duplicate || e.count("mutation stageOutboundRequest") != 0 {
		t.Fatalf("with no claim wired a created rule sent: %+v", out)
	}
}

// TestChangedRuleNotifiesOncePerChange: the operational lane's dedupe key
// names the triggering write for a changed rule, so two changes of one row are
// two notices, while a redelivered event is still one.
func TestChangedRuleNotifiesOncePerChange(t *testing.T) {
	e := &fireEngine{rule: activeRule("updated")}
	fire := func(stamp string) {
		t.Helper()
		if _, err := NewFirer(e).Fire(context.Background(), "v1:campaigns:emailRule:ab12cd34",
			"v1:identity:user:eve", map[string]any{"timestamp": stamp}); err != nil {
			t.Fatal(err)
		}
	}
	fire("2026-09-14T10:00:01Z")
	fire("2026-09-14T10:00:07Z")
	fire("2026-09-14T10:00:07Z") // the same event, redelivered
	ids := e.stagedIDs()
	if len(ids) != 3 || ids[0] == ids[1] || ids[1] != ids[2] {
		t.Fatalf("staged request ids %v: want two distinct changes, then the redelivery collapsing onto the second", ids)
	}
}
