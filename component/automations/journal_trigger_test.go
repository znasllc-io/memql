package automations

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// journal_trigger_test.go -- an event firing records only the payload keys
// its run can read (journal_trigger.go), and a resume of the recorded trigger
// binds the same args and continues the same cause as the first attempt.

func triggerProbe(t *testing.T, args []string, steps ...*Step) *Automation {
	t.Helper()
	a := &Automation{Name: "probe", Steps: steps}
	if len(args) > 0 {
		a.Args = &ArgsSchema{}
		for _, name := range args {
			a.Args.Fields = append(a.Args.Fields, &ArgsField{Name: name, Type: "any", Optional: true})
		}
	}
	if err := PrepareExpressions(a); err != nil {
		t.Fatal(err)
	}
	return a
}

func callStep(id, name, kind string, args map[string]any) *Step {
	return &Step{ID: id, Type: StepTypeFunction, Function: &FunctionStepConfig{Name: name, Kind: kind, Args: args}}
}

func expr(source string) map[string]any { return map[string]any{"$expr": source} }

func TestJournalTriggerReadsAreWhatTheRunCanRead(t *testing.T) {
	logicBody := func(source string) []map[string]any {
		return []map[string]any{{"id": "x", "type": "expression", "expression": source}}
	}
	source := registrySource{
		reg: work.Registry{
			"pipelinesTrigger": {ConstructKind: work.ConstructBuiltin},
			"createArtifact":   {ConstructKind: work.ConstructMutation},
			"pureLogic":        {ConstructKind: work.ConstructLogic},
			"ambientLogic":     {ConstructKind: work.ConstructLogic},
		},
		functions: map[string]*memql.Function{
			"pureLogic":    {Name: "pureLogic", LogicBody: logicBody(`args.value + 1`)},
			"ambientLogic": {Name: "ambientLogic", LogicBody: logicBody(`event.payload.secret`)},
		},
	}
	for _, tc := range []struct {
		name string
		a    *Automation
		want []string // nil: the whole payload is recorded
	}{
		{"a builtin handed the row id, as the inbound automations are",
			triggerProbe(t, []string{"id"}, callStep("opened", "pipelinesTrigger", "builtin", map[string]any{"inboundRequestId": expr("args.id")})),
			[]string{"id", "nodeId"}},
		{"a payload path the statements name",
			triggerProbe(t, []string{"id"}, callStep("write", "createArtifact", "mutation", map[string]any{"title": expr("event.payload.name"), "topic": expr("event.topic")})),
			[]string{"id", "name", "nodeId"}},
		{"a logic handed args only",
			triggerProbe(t, []string{"value"}, callStep("decide", "pureLogic", "logic", map[string]any{"value": expr("args.value")})),
			[]string{"id", "nodeId", "value"}},
		{"the whole event handed on",
			triggerProbe(t, nil, callStep("decide", "pureLogic", "logic", map[string]any{"event": expr("event")})),
			nil},
		{"the payload read whole",
			triggerProbe(t, nil, callStep("write", "createArtifact", "mutation", map[string]any{"row": expr("event.payload")})),
			nil},
		{"a logic whose body names the event",
			triggerProbe(t, []string{"id"}, callStep("decide", "ambientLogic", "logic", map[string]any{"value": expr("args.id")})),
			nil},
		{"a call nothing resolves",
			triggerProbe(t, []string{"id"}, callStep("mystery", "notRegistered", "builtin", map[string]any{"id": expr("args.id")})),
			nil},
		{"a call nested in a block",
			triggerProbe(t, []string{"id"}, &Step{ID: "block", Type: StepTypeBlock, Block: &BlockStepConfig{Steps: []*Step{
				callStep("decide", "ambientLogic", "logic", map[string]any{"value": expr("args.id")}),
			}}}),
			nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := computeJournalTriggerReads(tc.a, source); !slices.Equal(got, tc.want) {
				t.Fatalf("recorded keys = %v, want %v", got, tc.want)
			}
		})
	}

	withPrecondition := triggerProbe(t, []string{"id"}, callStep("opened", "pipelinesTrigger", "builtin", map[string]any{"inboundRequestId": expr("args.id")}))
	withPrecondition.Preconditions = []*Precondition{{ID: "p"}}
	if got := computeJournalTriggerReads(withPrecondition, source); got != nil {
		t.Fatalf("an automation with a precondition recorded %v, want the whole payload", got)
	}
	// The dedup key's projection joins it, so an adopted run's chain head
	// reads every key it kept.
	keyed := triggerProbe(t, []string{"id"}, callStep("opened", "pipelinesTrigger", "builtin", map[string]any{"inboundRequestId": expr("args.id")}))
	keyed.Reads = []string{"id", "nodeId", "status"}
	if got := computeJournalTriggerReads(keyed, source); !slices.Equal(got, []string{"id", "nodeId", "status"}) {
		t.Fatalf("with a dedup projection the recorded keys are %v", got)
	}
}

// TestARecordedTriggerResumesAsTheFirstAttemptRan: what a resume reads back
// from a projected trigger -- through the JSON the row stores -- binds the same
// args and continues the same cause, at the root and in a chain; and only an
// event firing is projected.
func TestARecordedTriggerResumesAsTheFirstAttemptRan(t *testing.T) {
	a := triggerProbe(t, []string{"id", "title"})
	a.JournalTriggerReads = []string{"id", "nodeId", "title"}
	body := strings.Repeat("a staged delivery's headers and body ", 600)
	ev := events.NewEvent("graph.node.created.v1:platform:inboundRequest", events.KindNodeCreated,
		map[string]any{"id": "v1:platform:inboundRequest:r1", "title": "push", "bodyText": body, "headersJson": body, "createdAt": "2026-11-08T00:00:00Z"})
	stored := func(m map[string]any) map[string]any {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var back map[string]any
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		return back
	}
	wantArgs, _, err := bindEventArgs(a, &ev)
	if err != nil {
		t.Fatal(err)
	}
	for _, parent := range []events.Cause{{}, {CausationId: "run-0", CorrelationId: "chain-1", Depth: 3, Chain: []events.Link{{Automation: "earlier", RunId: "run-0"}}}} {
		event := ev
		if !parent.IsZero() {
			event = ev.WithCause(parent)
		}
		_, firstAttempt := runCause(context.Background(), a, "run-1", &event)
		recorded := stored(recordedTrigger(a, TriggeredByEventPrefix+ev.Topic, &event, parent))
		payload, _ := recorded["payload"].(map[string]any)
		if _, kept := payload["bodyText"]; kept || len(payload) != 2 {
			t.Fatalf("an event firing recorded %v of its payload", payload)
		}
		if got := journalRunCause(context.Background(), a, "run-1", recorded); !reflect.DeepEqual(got, firstAttempt) {
			t.Fatalf("parent %+v: a resume continues cause %+v, the first attempt ran under %+v", parent, got, firstAttempt)
		}
		gotArgs, _, err := bindEventArgs(a, &events.Event{Payload: payload})
		if err != nil || !reflect.DeepEqual(gotArgs, wantArgs) {
			t.Fatalf("a resume binds %v (%v), the first attempt bound %v", gotArgs, err, wantArgs)
		}
		whole, _ := json.Marshal(stored(recordedTrigger(&Automation{Name: "probe"}, TriggeredByEventPrefix+ev.Topic, &event, parent)))
		projected, _ := json.Marshal(recorded)
		t.Logf("recorded trigger: %d bytes, %d whole", len(projected), len(whole))
	}
	for _, by := range []string{"sub-automation", "manual", "http-trigger", "run:client", "event:"} {
		recorded := recordedTrigger(a, by, &ev, events.Cause{})
		if payload, _ := recorded["payload"].(map[string]any); len(payload) != len(ev.Payload) || recorded["cause"] != nil {
			t.Fatalf("triggeredBy %q recorded %v keys of its payload and cause %v; only an event firing is projected", by, len(payload), recorded["cause"])
		}
	}
}

// TestShippedEventAutomationsRecordWhatTheyRead pins the projection of the
// shipped automations whose trigger rows were the large ones on production:
// the inbound deliveries' automations record the row id alone, and the
// Library's file promotion its declared args.
func TestShippedEventAutomationsRecordWhatTheyRead(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := memql.LoadUnifiedConcepts(logger); err != nil {
		t.Fatal(err)
	}
	eng, err := NewOfflineEngine(logger, memoryNodes.DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := NewLoader(LoaderOptions{Logger: logger, Registry: memoryNodes.DefaultRegistry(), Functions: eng.Functions()}).LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*Automation{}
	projected, eventDriven := 0, 0
	var whole []string
	for _, a := range loaded {
		byName[a.Name] = a
		if a.IsEventTriggered() {
			eventDriven++
			if a.JournalTriggerReads != nil {
				projected++
			} else {
				whole = append(whole, a.Name)
			}
		}
	}
	slices.Sort(whole)
	for name, want := range map[string][]string{
		"triggerPipelinesOnGitHubDelivery": {"id", "nodeId"},
		"dispatchInboundToConnector":       {"id", "nodeId"},
	} {
		a := byName[name]
		if a == nil {
			t.Fatalf("%s is not a shipped automation any more", name)
		}
		if !slices.Equal(a.JournalTriggerReads, want) {
			t.Errorf("%s records %v of its trigger, want %v", name, a.JournalTriggerReads, want)
		}
	}
	file := byName["indexFileOnCreate"]
	if file == nil || file.JournalTriggerReads == nil {
		t.Fatalf("indexFileOnCreate records its whole trigger")
	}
	for _, f := range file.Args.Fields {
		if !slices.Contains(file.JournalTriggerReads, f.Name) {
			t.Errorf("indexFileOnCreate does not record its declared arg %s: %v", f.Name, file.JournalTriggerReads)
		}
	}
	t.Logf("%d of %d shipped event automations record a projected trigger; the whole payload: %v", projected, eventDriven, whole)
	if projected == 0 {
		t.Fatal(fmt.Sprintf("no shipped event automation is projected out of %d", eventDriven))
	}
}
