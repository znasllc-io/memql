package automations

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/metrics"
)

// chainOf is the cause a chain of runs of the named automations carries, built
// the way the executor builds one -- Next per run, so the depth, the chain and
// the causation agree exactly as they do on a real event. Run ids are run-1,
// run-2, ... in order.
func chainOf(correlation string, names ...string) events.Cause {
	var c events.Cause
	for i, name := range names {
		c = c.Next(name, fmt.Sprintf("run-%d", i+1), correlation)
	}
	return c
}

// repeatName is n copies of name, for a chain of one automation re-firing itself.
func repeatName(name string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = name
	}
	return out
}

// jsonRoundTrip is what a stored row hands back: the value through JSON, so a
// depth is a float64 and a chain a []any of maps -- the shape a resume or a
// recovery reads, not the one the writer held.
func jsonRoundTrip(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestRunCause_ARootEventStartsAChainAtDepthOne(t *testing.T) {
	auto := &Automation{Name: "onTicket"}
	ev := &events.Event{
		Topic:     "graph.node.created.v1:probe:ticket",
		Kind:      events.KindNodeCreated,
		Timestamp: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC),
		Payload:   map[string]any{"id": "t1", "status": "open"},
	}

	parent, run := runCause(context.Background(), auto, "run-1", ev)
	if !parent.IsZero() {
		t.Fatalf("a root event has no parent cause; got %+v", parent)
	}
	if run.Depth != 1 {
		t.Errorf("depth = %d, want 1: the first automation a person's write triggers runs at depth 1", run.Depth)
	}
	if run.CausationId != "run-1" {
		t.Errorf("causation = %q, want run-1: the run is the cause of what it publishes", run.CausationId)
	}
	if !strings.HasPrefix(run.CorrelationId, "evt-") || len(run.CorrelationId) <= len("evt-") {
		t.Errorf("correlation = %q, want evt-<the event's fingerprint>", run.CorrelationId)
	}
	if want := []events.Link{{Automation: "onTicket", RunId: "run-1"}}; !reflect.DeepEqual(run.Chain, want) {
		t.Errorf("chain = %+v, want %+v", run.Chain, want)
	}

	// DETERMINISTIC. Two replicas that receive the same event -- or one replica
	// asked twice -- derive one correlation, because the id is the event's own
	// identity rather than a fresh one.
	_, again := runCause(context.Background(), auto, "run-2", ev)
	if again.CorrelationId != run.CorrelationId {
		t.Fatalf("two calls on one event gave %q and %q: every replica must derive the same correlation", run.CorrelationId, again.CorrelationId)
	}

	// The Timestamp is a wall clock, not a discriminator (eventFingerprintData's
	// rule): the same event delivered later is the same root.
	later := *ev
	later.Timestamp = ev.Timestamp.Add(time.Hour)
	if _, r := runCause(context.Background(), auto, "run-3", &later); r.CorrelationId != run.CorrelationId {
		t.Errorf("the delivery time changed the correlation (%q vs %q): replicas would disagree", r.CorrelationId, run.CorrelationId)
	}

	// While a different write of the row is a different root: a person
	// toggling a status twice starts two chains, not one.
	toggled := *ev
	toggled.Payload = map[string]any{"id": "t1", "status": "closed"}
	if _, r := runCause(context.Background(), auto, "run-4", &toggled); r.CorrelationId == run.CorrelationId {
		t.Errorf("two different writes share correlation %q", r.CorrelationId)
	}
}

func TestRunCause_AChainedEventRunsOneDeeper(t *testing.T) {
	ev := &events.Event{
		Topic:   "graph.node.updated.v1:probe:ticket",
		Kind:    events.KindNodeUpdated,
		Payload: map[string]any{"id": "t1"},
		Cause:   chainOf("evt-root", "a", "b", "c"),
	}

	parent, run := runCause(context.Background(), &Automation{Name: "d"}, "run-4", ev)
	if !reflect.DeepEqual(parent, ev.Cause) {
		t.Fatalf("parent = %+v, want the triggering event's cause %+v", parent, ev.Cause)
	}
	if run.Depth != ev.Cause.Depth+1 {
		t.Errorf("depth = %d, want %d: a run is one deeper than the event that triggered it", run.Depth, ev.Cause.Depth+1)
	}
	if run.CorrelationId != "evt-root" {
		t.Errorf("correlation = %q, want the chain's own evt-root", run.CorrelationId)
	}
	if run.CausationId != "run-4" {
		t.Errorf("causation = %q, want run-4", run.CausationId)
	}
	want := append(append([]events.Link(nil), ev.Cause.Chain...), events.Link{Automation: "d", RunId: "run-4"})
	if !reflect.DeepEqual(run.Chain, want) {
		t.Errorf("chain = %+v, want the parent's chain extended by this run: %+v", run.Chain, want)
	}

	// The run's chain is its own: extending or editing it must not reach the
	// event, which the bus may be delivering to other subscribers.
	run.Chain[0].Automation = "mutated"
	parent.Chain[0].Automation = "mutated"
	if ev.Cause.Chain[0].Automation != "a" {
		t.Fatal("runCause aliased the triggering event's chain")
	}

	// The EVENT decides when it has a cause, even inside a run that has one:
	// the event is what fired this automation, the context is only where the
	// call happened to be made from.
	ctx := events.ContextWithCause(context.Background(), chainOf("evt-other", "x"))
	if p, _ := runCause(ctx, &Automation{Name: "d"}, "run-5", ev); p.CorrelationId != "evt-root" {
		t.Errorf("parent correlation = %q, want the event's evt-root over the context's", p.CorrelationId)
	}
}

func TestRunCause_ASubAutomationRunsInsideItsCallersChain(t *testing.T) {
	caller := chainOf("evt-root", "parentAuto")
	ctx := events.ContextWithCause(context.Background(), caller)

	// A sub-automation called with no args: no event at all.
	parent, run := runCause(ctx, &Automation{Name: "child"}, "run-2", nil)
	if !reflect.DeepEqual(parent, caller) {
		t.Fatalf("parent = %+v, want the caller's cause %+v", parent, caller)
	}
	if run.Depth != 2 || run.CorrelationId != "evt-root" || run.CausationId != "run-2" {
		t.Fatalf("run = %+v, want depth 2 in correlation evt-root caused by run-2", run)
	}
	want := []events.Link{{Automation: "parentAuto", RunId: "run-1"}, {Automation: "child", RunId: "run-2"}}
	if !reflect.DeepEqual(run.Chain, want) {
		t.Errorf("chain = %+v, want %+v: a sub-automation counts as a run", run.Chain, want)
	}

	// A sub-automation called WITH args runs on a synthetic invocation event
	// that carries no cause of its own (scheduler.TriggerAutomationWithArgs).
	// The caller's context still decides.
	syn := &events.Event{Topic: "automation.invocation.child", Payload: map[string]any{"x": 1}}
	if _, r := runCause(ctx, &Automation{Name: "child"}, "run-3", syn); r.Depth != 2 || r.CorrelationId != "evt-root" {
		t.Errorf("a sub-automation with args ran at depth %d in %q, want depth 2 in the caller's evt-root", r.Depth, r.CorrelationId)
	}
}

func TestRunCause_ACronRunIsTheRootOfItsOwnChain(t *testing.T) {
	parent, run := runCause(context.Background(), &Automation{Name: "sweep"}, "run-cron", nil)
	if !parent.IsZero() {
		t.Fatalf("a cron run has no parent; got %+v", parent)
	}
	if run.Depth != 1 || run.CorrelationId != "run-cron" {
		t.Fatalf("run = %+v, want depth 1 with its own run id as the correlation", run)
	}
}

func TestLoopBound_TheCapIsTheDeepestChainAllowed(t *testing.T) {
	auto := &Automation{Name: "p"}

	at16 := chainOf("evt-root", repeatName("p", 16)...)
	if r := loopBound(auto, at16, 16); r != nil {
		t.Fatalf("a run at depth 16 under a cap of 16 was refused: %v", r)
	}

	prior := chainOf("evt-root", repeatName("p", 16)...)
	at17 := prior.Next("p", "run-17", "evt-root")
	r := loopBound(auto, at17, 16)
	if r == nil {
		t.Fatal("a run at depth 17 under a cap of 16 was admitted: a chain of 17 fires must stop at 16")
	}
	if r.Reason != metrics.LoopStopDepth || r.Depth != 17 || r.Cap != 16 || r.Automation != "p" {
		t.Fatalf("refusal = %+v, want reason %q, depth 17, cap 16, automation p", r, metrics.LoopStopDepth)
	}
	if r.Cause.Depth != 17 || r.Cause.CausationId != "run-17" || len(r.Cause.Chain) != 17 {
		t.Fatalf("refusal cause = %+v, want the refused run's own cause (depth 17, chain of 17)", r.Cause)
	}

	msg := r.Error()
	if !strings.HasPrefix(msg, "loop_depth_exceeded: p would run at depth 17, past the cap of 16; chain: p (run-1) -> p (run-2) -> ") {
		t.Errorf("message = %q", msg)
	}
	if !strings.HasSuffix(msg, "p (run-16) [loop_depth_exceeded]") {
		t.Errorf("message = %q: the chain it names ends at the last run that RAN, then the rule id", msg)
	}
	if strings.Contains(msg, "run-17") {
		t.Errorf("message = %q names the refused run in its chain; that run never ran", msg)
	}
}

// TestLoopRefusal_TheMessage pins the refusal's exact sentence: it is the
// errorMessage a person reads on the refused run, and it ends with the rule id
// the corpus reads.
func TestLoopRefusal_TheMessage(t *testing.T) {
	prior := chainOf("evt-root", "a", "b")
	r := loopBound(&Automation{Name: "c"}, prior.Next("c", "run-3", "evt-root"), 2)
	want := "loop_depth_exceeded: c would run at depth 3, past the cap of 2; chain: a (run-1) -> b (run-2) [loop_depth_exceeded]"
	if r == nil || r.Error() != want {
		t.Fatalf("refusal = %v, want %q", r, want)
	}
	var err error = r
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("the refusal spans lines: %q", err)
	}
}

func TestLoopBound_ALoopAutomationStopsAtItsMaxDepth(t *testing.T) {
	tick := &Automation{Name: "tick", Loop: &LoopConfig{MaxDepth: 2}}

	// The chain already holds two runs of tick, with another automation
	// between them: the bound counts THIS automation's runs, not the depth.
	holdsTwice := chainOf("evt-root", "tick", "other", "tick")
	run := holdsTwice.Next("tick", "run-4", "evt-root")
	r := loopBound(tick, run, 16)
	if r == nil {
		t.Fatal("a third run of a @loop(maxDepth=2) automation in one chain was admitted")
	}
	if r.Reason != metrics.LoopStopLoopBound || r.Cap != 2 || r.Depth != 4 || r.Automation != "tick" {
		t.Fatalf("refusal = %+v, want reason %q, cap 2 (the maxDepth), depth 4", r, metrics.LoopStopLoopBound)
	}
	msg := r.Error()
	if !strings.HasPrefix(msg, "loop_depth_exceeded: tick would run at depth 4, past its @loop maxDepth of 2 runs in one chain; chain: tick (run-1) -> other (run-2) -> tick (run-3) [loop_depth_exceeded]") {
		t.Errorf("message = %q", msg)
	}

	// Holding it once is inside the bound.
	holdsOnce := chainOf("evt-root", "tick", "other")
	if r := loopBound(tick, holdsOnce.Next("tick", "run-3", "evt-root"), 16); r != nil {
		t.Errorf("a second run of a maxDepth=2 automation was refused: %v", r)
	}

	// The same chain without the @loop is bounded by the cap alone.
	if r := loopBound(&Automation{Name: "tick"}, run, 16); r != nil {
		t.Errorf("an automation with no @loop was refused below the cap: %v", r)
	}
}

// causeProbeRegistry runs every step as a success and records the cause the
// step's context carried -- what every write and publish of the step stamps.
type causeProbeRegistry struct {
	causes []events.Cause
}

func (p *causeProbeRegistry) Execute(ctx context.Context, step *Step, _ *StepContext) (*StepResult, error) {
	c, _ := events.CauseFromContext(ctx)
	p.causes = append(p.causes, c)
	now := time.Now()
	return &StepResult{StepId: step.ID, Status: "completed", Result: map[string]any{"ok": true}, StartedAt: now, CompletedAt: now}, nil
}

func causeProbeAutomation(name string) *Automation {
	return &Automation{Name: name, Steps: []*Step{{ID: "a", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}}}}
}

// TestResumeKeepsTheRunsPlaceInItsChain: a resumed run is the same run
// continuing, so its steps run at the depth its first attempt did -- the
// parent cause openRun recorded on triggerEvent, one deeper, under the same
// run id. Read back through JSON, the way a stored row arrives.
func TestResumeKeepsTheRunsPlaceInItsChain(t *testing.T) {
	parent := chainOf("evt-root", "a", "b", "c")
	trigger := jsonRoundTrip(t, map[string]any{
		"topic":   "graph.node.updated.v1:probe:ticket",
		"kind":    events.KindNodeUpdated.String(),
		"payload": map[string]any{"id": "t1"},
		"cause":   parent,
	})

	probe := &causeProbeRegistry{}
	e := NewExecutor(ExecutorOptions{StepRegistry: probe})
	defer e.Close()
	auto := causeProbeAutomation("resumeProbe")
	journal := &RunJournal{RunId: "run-resumed", AutomationName: auto.Name, FailedStep: "a", TriggerEvent: trigger}
	if _, err := e.ResumeFrom(context.Background(), journal, auto, &ResumeOptions{}); err != nil {
		t.Fatalf("ResumeFrom: %v", err)
	}
	if len(probe.causes) != 1 {
		t.Fatalf("the resumed step ran %d times, want 1", len(probe.causes))
	}
	got := probe.causes[0]
	want := parent.Next(auto.Name, "run-resumed", "evt-root")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the resumed step ran under cause %+v, want %+v: a resume that restarts the chain lets a loop that fails and resumes run forever", got, want)
	}

	// The control: a run whose row recorded no cause was a root, and resumes
	// as one. Without it the assertion above could pass on a resume that
	// invents depth.
	probe.causes = nil
	rootTrigger := jsonRoundTrip(t, map[string]any{"topic": "graph.node.updated.v1:probe:ticket", "kind": events.KindNodeUpdated.String(), "payload": map[string]any{"id": "t1"}})
	root := &RunJournal{RunId: "run-root", AutomationName: auto.Name, FailedStep: "a", TriggerEvent: rootTrigger}
	if _, err := e.ResumeFrom(context.Background(), root, auto, &ResumeOptions{}); err != nil {
		t.Fatalf("ResumeFrom: %v", err)
	}
	if len(probe.causes) != 1 || probe.causes[0].Depth != 1 {
		t.Fatalf("a root-triggered run resumed at %+v, want depth 1", probe.causes)
	}
}

// TestAResumedRootRunKeepsItsCorrelation: a root run's correlation is derived
// from its event, not recorded, so a resume must derive the SAME one from what
// openRun wrote -- the kind included, which the row stores as a word.
func TestAResumedRootRunKeepsItsCorrelation(t *testing.T) {
	auto := causeProbeAutomation("rootProbe")
	ev := events.NewEvent("graph.node.created.v1:probe:ticket", events.KindNodeCreated, map[string]any{"id": "t1", "n": 3})
	_, first := runCause(context.Background(), auto, "run-x", &ev)

	rec := &recordingJournalExecutor{}
	exec := NewExecution(auto.Name, "event:"+ev.Topic)
	exec.ID = "run-x"
	newWorkJournal(rec, nil).openRun(context.Background(), auto, exec, &ev, events.Cause{})
	_, args := argsOf(t, rec.calls[0])
	recorded, _ := args["triggerEvent"].(map[string]any)
	if _, has := recorded["cause"]; has {
		t.Fatalf("a root run recorded a cause: %v -- a root has none to record", recorded)
	}

	if got := journalRunCause(context.Background(), auto, "run-x", recorded); !reflect.DeepEqual(got, first) {
		t.Fatalf("resumed as %+v, first attempt ran as %+v: the resumed run left its own chain", got, first)
	}
}

// TestAnAdoptedRecoveryKeepsItsChain: a scheduler run recovered onto its own
// row (ExecuteAdopted with its journal) runs at its recorded depth, as itself.
func TestAnAdoptedRecoveryKeepsItsChain(t *testing.T) {
	parent := chainOf("evt-root", "a", "b")
	trigger := jsonRoundTrip(t, map[string]any{
		"topic":   "graph.node.updated.v1:probe:ticket",
		"kind":    events.KindNodeUpdated.String(),
		"payload": map[string]any{"id": "t1"},
		"cause":   parent,
	})
	probe := &causeProbeRegistry{}
	e := NewExecutor(ExecutorOptions{StepRegistry: probe})
	defer e.Close()
	auto := causeProbeAutomation("adoptProbe")
	journal := &RunJournal{TriggeredBy: "event:graph.node.updated.v1:probe:ticket", TriggerEvent: trigger}
	exec, err := e.ExecuteAdopted(context.Background(), auto, RunAdoption{RunId: "run-adopted", Journal: journal})
	if err != nil || exec.Status != "completed" {
		t.Fatalf("ExecuteAdopted: %+v %v", exec, err)
	}
	if len(probe.causes) != 1 {
		t.Fatalf("the step ran %d times, want 1", len(probe.causes))
	}
	if want := parent.Next(auto.Name, "run-adopted", "evt-root"); !reflect.DeepEqual(probe.causes[0], want) {
		t.Fatalf("the adopted run ran under %+v, want %+v", probe.causes[0], want)
	}
}
