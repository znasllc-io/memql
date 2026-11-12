package automations

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// call_account_test.go -- the account a run acts in (design record
// docs/superpowers/specs/2026-11-12-account-row-ownership-design.md, section C:
// "An automation: the account of the row that triggered it, when that row's
// concept declares an account form. A scheduled automation has none").
//
// A run acts as the cluster, which belongs to no account, so the only thing that
// can place what it writes is the row that started it. These hold the run to
// that row, and to nothing a row did not say: a field of the same name on a
// cluster-wide concept, a payload a caller handed in, and an event that is not
// the write path's own account of a row each place nothing.

const (
	callAccountNoteConcept        = "v1:callaccounttest:note"
	callAccountEntitlementConcept = "v1:callaccounttest:entitlement"
	callAccountLegacyConcept      = "v1:callaccounttest:legacy"
	// A concept whose own mutations name the account, and which the
	// organization rules do not govern: nothing holds its `accountId` to the
	// accounts its writer may work in.
	callAccountStatedConcept = "v1:callaccounttest:stated"
	// One the organization rules do govern, by its real name: a campaign's
	// account is held to its writer's on every write.
	callAccountCampaignConcept = "v1:campaigns:campaign"
)

// registerCallAccountConcepts puts three concepts in the registry the executor
// reads: one whose rows belong to an account, one that is cluster-wide and
// happens to carry a field named accountId with another meaning, and one that
// declares nothing.
func registerCallAccountConcepts(t *testing.T) {
	t.Helper()
	concepts := map[string]*memoryNodes.Concept{
		callAccountNoteConcept: {Name: callAccountNoteConcept,
			BelongsTo: &languageParser.BelongsToDecl{Scope: languageParser.BelongsToAccount}},
		callAccountEntitlementConcept: {Name: callAccountEntitlementConcept,
			BelongsTo: &languageParser.BelongsToDecl{Scope: languageParser.BelongsToCluster}},
		callAccountLegacyConcept: {Name: callAccountLegacyConcept},
		callAccountStatedConcept: {Name: callAccountStatedConcept,
			BelongsTo: &languageParser.BelongsToDecl{Scope: languageParser.BelongsToAccount, Stated: true}},
	}
	// The campaign concept is registered only when the registry does not
	// already hold it, and only then removed again: another test in this
	// package may have loaded the real one.
	if existing, err := memoryNodes.Get(callAccountCampaignConcept); err != nil || existing == nil {
		concepts[callAccountCampaignConcept] = &memoryNodes.Concept{Name: callAccountCampaignConcept,
			BelongsTo: &languageParser.BelongsToDecl{Scope: languageParser.BelongsToAccount, Stated: true}}
	}
	memoryNodes.MergeAll(concepts)
	t.Cleanup(func() {
		registry := memoryNodes.DefaultRegistry().(*memoryNodes.MemoryRegistry)
		for name := range concepts {
			registry.Remove(name)
		}
	})
}

// callAccountProbe records the account each step's context states, by asking the
// context the way the engine does.
type callAccountProbe struct {
	accounts map[string]string
}

const noCallAccount = "(none)"

func (p *callAccountProbe) Execute(ctx context.Context, step *Step, _ *StepContext) (*StepResult, error) {
	if p.accounts == nil {
		p.accounts = map[string]string{}
	}
	account, stated := auth.CallAccountFromContext(ctx)
	if !stated {
		account = noCallAccount
	}
	p.accounts[step.ID] = account
	now := time.Now()
	return &StepResult{StepId: step.ID, Status: "completed", Result: map[string]any{"ok": true}, StartedAt: now, CompletedAt: now}, nil
}

// callAccountRowEvent is the event the write path publishes for a row: the
// concept on the topic, the row's fields flattened into the payload beside its
// id.
func callAccountRowEvent(base string, kind events.Kind, concept string, fields map[string]any) *events.Event {
	payload := map[string]any{"id": concept + ":row-1", "nodeId": concept + ":row-1", "concept": concept, "nodeType": "node"}
	for k, v := range fields {
		payload[k] = v
	}
	ev := events.NewEvent(events.BuildTopicWithConcept(base, concept), kind, payload)
	return &ev
}

func callAccountNoteCreated(fields map[string]any) *events.Event {
	return callAccountRowEvent(events.TopicGraphNodeCreated, events.KindNodeCreated, callAccountNoteConcept, fields)
}

// firedByEvent is the triggeredBy the scheduler gives an event firing.
func firedByEvent(ev *events.Event) string { return TriggeredByEventPrefix + ev.Topic }

func TestARunActsInTheAccountOfTheRowThatTriggeredIt(t *testing.T) {
	registerCallAccountConcepts(t)
	probe := &callAccountProbe{}
	e := NewExecutor(ExecutorOptions{StepRegistry: probe})
	defer e.Close()

	for step, ev := range map[string]*events.Event{
		// Stored canonical on most concepts and bare on a few; one account.
		"created":   callAccountNoteCreated(map[string]any{"accountId": "v1:accounts:account:acme", "title": "Minutes"}),
		"bare":      callAccountNoteCreated(map[string]any{"accountId": "acme"}),
		"updated":   callAccountRowEvent(events.TopicGraphNodeUpdated, events.KindNodeUpdated, callAccountNoteConcept, map[string]any{"accountId": "v1:accounts:account:acme"}),
		"canonical": callAccountNoteCreated(map[string]any{"accountId": " v1:accounts:account:acme "}),
		// A stated concept the organization rules govern: its account was held
		// to its writer's on the write, so it places the run too.
		"governed": callAccountRowEvent(events.TopicGraphNodeCreated, events.KindNodeCreated, callAccountCampaignConcept, map[string]any{"accountId": "v1:accounts:account:acme"}),
	} {
		a := &Automation{Name: "onNote-" + step, Steps: scheduledSteps(step)}
		if _, err := e.ExecuteWithEvent(context.Background(), a, firedByEvent(ev), ev); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if got := probe.accounts[step]; got != "acme" {
			t.Errorf("%s: the run's steps act in %q, want acme -- a row this run writes would record "+
				"no account, because the run's own actor is the cluster", step, got)
		}
	}

	// The row's account is the run's, whatever the frame that fired it carried.
	ev := callAccountNoteCreated(map[string]any{"accountId": "acme"})
	inherited := auth.ContextWithCallAccount(context.Background(), "beta")
	if _, err := e.ExecuteWithEvent(inherited, &Automation{Name: "onNote-inherited", Steps: scheduledSteps("inherited")}, firedByEvent(ev), ev); err != nil {
		t.Fatal(err)
	}
	if got := probe.accounts["inherited"]; got != "acme" {
		t.Errorf("an inherited account outlived the triggering row's: %q", got)
	}
}

// What must NOT place a run. Each case is a payload carrying `accountId: acme`
// that is not a statement by a row of an account-scoped concept.
func TestARunTakesNoAccountFromAnythingButAnAccountsRow(t *testing.T) {
	registerCallAccountConcepts(t)
	acme := map[string]any{"accountId": "v1:accounts:account:acme"}

	for name, ev := range map[string]*events.Event{
		// The field means something else here (on the identity file's
		// accountEntitlement it is a user id). Readers go through the
		// declaration, never through the name of a field.
		"a cluster-wide concept with a field named accountId": callAccountRowEvent(events.TopicGraphNodeCreated, events.KindNodeCreated, callAccountEntitlementConcept, acme),
		"a concept that declares nothing":                     callAccountRowEvent(events.TopicGraphNodeCreated, events.KindNodeCreated, callAccountLegacyConcept, acme),
		// Its own mutations wrote the value and nothing held it to the
		// writer's accounts, so it places nothing (memql.TriggerAccountTrusted).
		"a stated concept the organization rules do not govern": callAccountRowEvent(events.TopicGraphNodeCreated, events.KindNodeCreated, callAccountStatedConcept, acme),
		"a concept the registry does not hold":                callAccountRowEvent(events.TopicGraphNodeCreated, events.KindNodeCreated, "v1:callaccounttest:unknown", acme),
		"a row with no account yet":                           callAccountNoteCreated(map[string]any{"title": "Unassigned"}),
		"a row with an empty account":                         callAccountNoteCreated(map[string]any{"accountId": "  "}),
		"an account that is not a string":                     callAccountNoteCreated(map[string]any{"accountId": []any{"acme", "beta"}}),
		"an event that is not a row's":                        func() *events.Event { ev := events.NewEvent("probe.fired", events.KindMessage, acme); return &ev }(),
		// A row's event is its topic AND one of the node kinds, which only the
		// write path gives an event. Either without the other is not one.
		"a row's topic under the message kind":      callAccountRowEvent(events.TopicGraphNodeCreated, events.KindMessage, callAccountNoteConcept, acme),
		"a row's topic under the graph_update kind": callAccountRowEvent(events.TopicGraphNodeCreated, events.KindGraphUpdate, callAccountNoteConcept, acme),
		"a row's kind on some other topic": func() *events.Event {
			ev := events.NewEvent("notes.imported", events.KindNodeCreated, acme)
			return &ev
		}(),
		"the bare topic, naming no concept": func() *events.Event {
			ev := events.NewEvent(events.TopicGraphNodeCreated, events.KindNodeCreated, acme)
			return &ev
		}(),
		"a sub-automation's call arguments": func() *events.Event {
			return &events.Event{Topic: "automation.invocation.child", Kind: events.KindUnspecified, Payload: acme}
		}(),
		"a compiled goal's dispatch variables": func() *events.Event {
			return &events.Event{Topic: "work.run.dispatched", Kind: events.KindTelemetry, Payload: acme}
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			probe := &callAccountProbe{}
			e := NewExecutor(ExecutorOptions{StepRegistry: probe})
			defer e.Close()
			if _, err := e.ExecuteWithEvent(context.Background(), &Automation{Name: "probe: " + name, Steps: scheduledSteps("a")}, firedByEvent(ev), ev); err != nil {
				t.Fatal(err)
			}
			if got := probe.accounts["a"]; got != noCallAccount {
				t.Fatalf("the run acts in %q; nothing here is an account-scoped row saying so", got)
			}

			// And it leaves alone what the frame around it stated: a
			// sub-automation acts in its caller's account.
			inherited := &callAccountProbe{}
			child := NewExecutor(ExecutorOptions{StepRegistry: inherited})
			defer child.Close()
			ctx := auth.ContextWithCallAccount(context.Background(), "beta")
			if _, err := child.ExecuteWithEvent(ctx, &Automation{Name: "probe, inherited: " + name, Steps: scheduledSteps("a")}, firedByEvent(ev), ev); err != nil {
				t.Fatal(err)
			}
			if got := inherited.accounts["a"]; got != "beta" {
				t.Fatalf("the run acts in %q, want the account its caller was acting in (beta)", got)
			}
		})
	}

	t.Run("a scheduled run has none", func(t *testing.T) {
		probe := &callAccountProbe{}
		e := NewExecutor(ExecutorOptions{StepRegistry: probe})
		defer e.Close()
		if _, err := e.Execute(context.Background(), &Automation{Name: "sweep", Steps: scheduledSteps("a")}, TriggeredBySchedule); err != nil {
			t.Fatal(err)
		}
		if got := probe.accounts["a"]; got != noCallAccount {
			t.Fatalf("a scheduled run acts in %q", got)
		}
	})
}

// A PAYLOAD A CALLER SUPPLIED PLACES NOTHING, however much it looks like a row's
// event. The run-automation paths hand the executor an event the caller wrote,
// topic and kind included, and the same rule that keeps such a run at client
// origin (ExecuteWithClientEvent) keeps it from choosing the account its rows
// are recorded in. The caller's own account is resolved from who they are.
func TestACallerSuppliedTriggerPlacesNothing(t *testing.T) {
	registerCallAccountConcepts(t)
	probe := &callAccountProbe{}
	e := NewExecutor(ExecutorOptions{StepRegistry: probe})
	defer e.Close()

	ev := callAccountNoteCreated(map[string]any{"accountId": "v1:accounts:account:acme"})
	a := &Automation{Name: "onNote", Trusted: true, Steps: scheduledSteps("a")}
	if _, err := e.ExecuteWithClientEvent(context.Background(), a, "run:client", ev); err != nil {
		t.Fatal(err)
	}
	if got := probe.accounts["a"]; got != noCallAccount {
		t.Fatalf("a caller-supplied payload placed the run in %q", got)
	}

	// The control: the same automation and the same event, fired by the bus.
	if _, err := e.ExecuteWithEvent(context.Background(), &Automation{Name: "onNote-bus", Trusted: true, Steps: scheduledSteps("b")}, firedByEvent(ev), ev); err != nil {
		t.Fatal(err)
	}
	if got := probe.accounts["b"]; got != "acme" {
		t.Fatalf("control: the bus-fired run acts in %q, want acme", got)
	}
}

// The journal writes a run's own rows under an actor of its own (journalContext),
// which is one more rebuild of the actor beneath the run. The run's account has
// to survive it, or the run's own row would be the one write of the run made
// under a context that states none.
func TestTheJournalsContextKeepsTheRunsAccount(t *testing.T) {
	ctx := journalContext(auth.ContextWithCallAccount(context.Background(), "acme"))
	if got, ok := auth.CallAccountFromContext(ctx); !ok || got != "acme" {
		t.Fatalf("the journal's context states (%q, %v), want acme", got, ok)
	}
	if ac, ok := auth.AccessFromContext(ctx); !ok || ac.UserId != workJournalActor {
		t.Fatalf("fixture: this is not the journal's own actor: %+v", ac)
	}
}

// The recorded trigger keeps the account when it keeps nothing else the run did
// not ask for. A run records only the payload keys its body can read
// (journal_trigger.go), and the account is read by the executor, not by the
// body: projected away, a resumed run would act in no account where its first
// attempt acted in one.
func TestTheRecordedTriggerKeepsTheRowsAccount(t *testing.T) {
	ev := callAccountNoteCreated(map[string]any{"accountId": "v1:accounts:account:acme", "title": "Minutes", "body": "long text"})
	a := &Automation{Name: "onNote", JournalTriggerReads: []string{"id", "nodeId", "title"}}

	recorded := recordedTrigger(a, firedByEvent(ev), ev, events.Cause{})
	payload, _ := recorded["payload"].(map[string]any)
	if payload["accountId"] != "v1:accounts:account:acme" {
		t.Fatalf("the projected trigger dropped the row's account: %v", payload)
	}
	if _, kept := payload["body"]; kept {
		t.Fatalf("the projection kept a field nothing reads: %v", payload)
	}
	if payload["title"] != "Minutes" || payload["id"] == nil {
		t.Fatalf("the projection lost a field the run reads: %v", payload)
	}

	// A row with no account records no key at all, not an empty one.
	bare := callAccountNoteCreated(map[string]any{"title": "Unassigned"})
	payload, _ = recordedTrigger(a, firedByEvent(bare), bare, events.Cause{})["payload"].(map[string]any)
	if _, kept := payload["accountId"]; kept {
		t.Fatalf("a row with no account recorded one: %v", payload)
	}

	// A trigger that is not projected is recorded whole, as before.
	whole, _ := recordedTrigger(&Automation{Name: "onNote"}, firedByEvent(ev), ev, events.Cause{})["payload"].(map[string]any)
	if whole["body"] != "long text" || whole["accountId"] != "v1:accounts:account:acme" {
		t.Fatalf("an unprojected trigger was not recorded whole: %v", whole)
	}
}

// storedTrigger is a recorded trigger as a later read hands it back: through
// JSON, the way the run row stores it.
func storedTrigger(t *testing.T, a *Automation, triggeredBy string, ev *events.Event) map[string]any {
	t.Helper()
	raw, err := json.Marshal(recordedTrigger(a, triggeredBy, ev, events.Cause{}))
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

// A RESUMED RUN ACTS WHERE ITS FIRST ATTEMPT DID. Resume does not go through the
// trigger path: it rebuilds the run from its row, on whichever replica picks it
// up, and that replica holds nothing of the first attempt but what was recorded.
func TestAResumedRunActsInTheAccountItsTriggerRecorded(t *testing.T) {
	registerCallAccountConcepts(t)
	probe := &callAccountProbe{}
	e := NewExecutor(ExecutorOptions{StepRegistry: probe})
	defer e.Close()

	resume := func(step string, a *Automation, trigger map[string]any, callerSupplied bool) {
		t.Helper()
		a.Steps = scheduledSteps(step)
		journal := &RunJournal{
			RunId:                 "run-" + step,
			AutomationName:        a.Name,
			FailedStep:            step,
			StepStates:            states(step, StepState{Status: "failed", Attempt: 1}),
			TriggerEvent:          trigger,
			CallerSuppliedPayload: callerSupplied,
		}
		if _, err := e.ResumeFrom(context.Background(), journal, a, &ResumeOptions{AllowSideEffects: true}); err != nil {
			t.Fatal(err)
		}
	}

	ev := callAccountNoteCreated(map[string]any{"accountId": "v1:accounts:account:acme", "title": "Minutes"})

	// Projected to what its body reads, which names neither the concept nor the
	// account: the topic and the kept key are all a resume has.
	projected := &Automation{Name: "projected", JournalTriggerReads: []string{"id", "nodeId", "title"}}
	resume("projected", projected, storedTrigger(t, projected, firedByEvent(ev), ev), false)
	if got := probe.accounts["projected"]; got != "acme" {
		t.Errorf("a resumed run acts in %q, want acme: its first attempt acted in the triggering row's account", got)
	}

	whole := &Automation{Name: "whole"}
	resume("whole", whole, storedTrigger(t, whole, firedByEvent(ev), ev), false)
	if got := probe.accounts["whole"]; got != "acme" {
		t.Errorf("a resumed run with its trigger recorded whole acts in %q, want acme", got)
	}

	// The recorded flag is what a resume has of "the caller supplied this", and
	// it gates the account as it gates internal origin: otherwise a run a
	// caller started, failed and resumed would be placed by its own payload.
	supplied := &Automation{Name: "supplied"}
	resume("supplied", supplied, storedTrigger(t, supplied, "run:client", ev), true)
	if got := probe.accounts["supplied"]; got != noCallAccount {
		t.Errorf("resuming a run whose payload a caller supplied placed it in %q", got)
	}

	elsewhere := callAccountRowEvent(events.TopicGraphNodeCreated, events.KindNodeCreated, callAccountEntitlementConcept, map[string]any{"accountId": "v1:accounts:account:acme"})
	cluster := &Automation{Name: "cluster"}
	resume("cluster", cluster, storedTrigger(t, cluster, firedByEvent(elsewhere), elsewhere), false)
	if got := probe.accounts["cluster"]; got != noCallAccount {
		t.Errorf("resuming a run a cluster-wide row triggered placed it in %q", got)
	}

	cron := &Automation{Name: "cron"}
	resume("cron", cron, nil, false)
	if got := probe.accounts["cron"]; got != noCallAccount {
		t.Errorf("resuming a scheduled run placed it in %q", got)
	}
}

// A run recovered by adoption re-executes from its recorded trigger
// (ExecuteAdopted), so it goes through the trigger path with the recorded event
// and the recorded caller flag, and acts where it did.
func TestAnAdoptedSchedulerRunActsInTheAccountItsTriggerRecorded(t *testing.T) {
	registerCallAccountConcepts(t)
	ev := callAccountNoteCreated(map[string]any{"accountId": "v1:accounts:account:acme"})

	for name, tc := range map[string]struct {
		journal *RunJournal
		want    string
	}{
		"a recovered event run": {&RunJournal{TriggeredBy: firedByEvent(ev), TriggerEvent: storedTrigger(t, &Automation{Name: "a"}, firedByEvent(ev), ev)}, "acme"},
		"a recovered run a caller started": {&RunJournal{TriggeredBy: "run:client", CallerSuppliedPayload: true,
			TriggerEvent: storedTrigger(t, &Automation{Name: "a"}, "run:client", ev)}, noCallAccount},
		// A goal's run is dispatched with its compiled variables, never with a
		// recorded trigger, whatever its row happens to hold.
		"a compiled goal": {&RunJournal{GoalId: "goal-1", TriggerEvent: storedTrigger(t, &Automation{Name: "a"}, firedByEvent(ev), ev)}, noCallAccount},
	} {
		t.Run(name, func(t *testing.T) {
			probe := &callAccountProbe{}
			e := NewExecutor(ExecutorOptions{StepRegistry: probe})
			defer e.Close()
			a := &Automation{Name: "adopted", Steps: scheduledSteps("a")}
			adoption := RunAdoption{RunId: "adopt-" + name, Variables: map[string]any{"accountId": "v1:accounts:account:acme"}, Journal: tc.journal}
			if exec, err := e.ExecuteAdopted(context.Background(), a, adoption); err != nil || exec.Status != "completed" {
				t.Fatalf("execution %+v error %v", exec, err)
			}
			if got := probe.accounts["a"]; got != tc.want {
				t.Fatalf("the adopted run acts in %q, want %q", got, tc.want)
			}
		})
	}
}

// accountCapturingRunner is fakeRunner, noting the account the context it is
// called under states.
type accountCapturingRunner struct {
	fakeRunner
	account string
	stated  bool
}

func (r *accountCapturingRunner) TriggerAutomationWithClientEvent(ctx context.Context, name string, event *events.Event) (*AutomationExecution, error) {
	r.account, r.stated = auth.CallAccountFromContext(ctx)
	return r.fakeRunner.TriggerAutomationWithClientEvent(ctx, name, event)
}

// relayedOverTheBus runs the origin half of a relayed run and returns the
// request it put on the bus, exactly as it was marshalled. The origin waits for
// a node to claim the run; the context is cancelled once the request is in
// hand, which is how the wait ends.
func relayedOverTheBus(t *testing.T, origin context.Context) relayedRequest {
	t.Helper()
	bus := events.NewBus()
	defer bus.Close()
	requests := make(chan events.Event, 1)
	unsub := bus.Subscribe(TopicRunRequest, func(evt events.Event) { requests <- evt })
	defer unsub()

	sender := newTestRelay(t, &fakeRunner{})
	sender.bus = bus
	ctx, cancel := context.WithCancel(origin)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sender.runRemote(ctx, "run-1", "release", RunRequest{TargetNodeType: "planner"}, &captureSink{})
	}()

	var request relayedRequest
	select {
	case evt := <-requests:
		raw, _ := evt.Payload["request"].(string)
		if err := json.Unmarshal([]byte(raw), &request); err != nil {
			t.Fatalf("the relayed request does not decode: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the origin relayed nothing; its caller's authority was refused before the hop")
	}
	cancel()
	<-done
	return request
}

// THE RUN RELAY IS A SECOND WIRE FORM OF THE SAME ASSERTION. An operator's run
// of an automation on another node type travels as JSON on the event bus, not
// through the node proto, so the mapping the proto test holds does not cover
// it. The account a relayed call acts in has to survive this carrier too: the
// node that executes the run holds none of the caller's context, and binds what
// the request says.
func TestARelayedRunActsInTheAccountItsCallerStated(t *testing.T) {
	authority := runTestAuthority(t)
	access, err := auth.VerifyForwardedAuthority(authority, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	caller := auth.BindForwardedContext(context.Background(), authority.Principal().Claims, access, authority)

	execute := func(t *testing.T, request relayedRequest) *accountCapturingRunner {
		t.Helper()
		runner := &accountCapturingRunner{fakeRunner: fakeRunner{registry: map[string]*Automation{"release": {Name: "release", Schedule: "0 * * * * *"}}}}
		receiver := newTestRelay(t, runner)
		bus := events.NewBus()
		defer bus.Close()
		receiver.bus = bus
		receiver.executeRelayed(request.RunId, "node-origin", request)
		if runner.gotName != "release" {
			t.Fatalf("the relayed run was not executed (ran %q)", runner.gotName)
		}
		return runner
	}

	// Stated after the caller's actor was bound, further down the same call.
	request := relayedOverTheBus(t, auth.ContextWithCallAccount(caller, "v1:accounts:account:acme"))
	if request.Authority.AccountId != "acme" {
		t.Fatalf("the relayed request names account %q, want acme: the origin did not read the account the "+
			"call is acting in", request.Authority.AccountId)
	}
	if runner := execute(t, request); !runner.stated || runner.account != "acme" {
		t.Fatalf("the executing node acts in (%q, %v), want acme", runner.account, runner.stated)
	}

	// A caller that stated none relays none, and the executing node binds none.
	request = relayedOverTheBus(t, caller)
	if request.Authority.AccountId != "" {
		t.Fatalf("a caller with no stated account relayed %q", request.Authority.AccountId)
	}
	if runner := execute(t, request); runner.stated {
		t.Fatalf("the executing node bound %q from a request that named no account", runner.account)
	}
}
