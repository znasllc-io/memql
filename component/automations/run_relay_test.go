package automations

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// run_relay_test.go covers the invoke path's local half (memql#3310). The
// cross-node half -- where the interesting bug lives -- is covered by
// test/clustere2e/automation_run_routing_test.go, which drives two relays
// through the real routing decision.

func relayTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// captureSink records every frame in arrival order.
type captureSink struct {
	accepted []RunAccepted
	steps    []RunStep
	complete []RunComplete
}

func (c *captureSink) Accepted(a RunAccepted) { c.accepted = append(c.accepted, a) }
func (c *captureSink) Step(s RunStep)         { c.steps = append(c.steps, s) }
func (c *captureSink) Complete(x RunComplete) { c.complete = append(c.complete, x) }

func (c *captureSink) onlyComplete(t *testing.T) RunComplete {
	t.Helper()
	if len(c.complete) != 1 {
		t.Fatalf("a run must emit EXACTLY one complete frame -- a caller parks on it; got %d", len(c.complete))
	}
	return c.complete[0]
}

// fakeRunner stands in for the scheduler. It records what it was asked to run
// so the event-synthesis assertions can read it back, and drives the observer
// with canned step results.
type fakeRunner struct {
	registry map[string]*Automation

	gotName  string
	gotEvent *events.Event
	steps    []*StepResult
	exec     *AutomationExecution
	err      error
}

func (f *fakeRunner) LookupAutomation(name string) *Automation { return f.registry[name] }

func (f *fakeRunner) TriggerAutomationWithClientEvent(ctx context.Context, name string, event *events.Event) (*AutomationExecution, error) {
	f.gotName = name
	f.gotEvent = event
	for _, s := range f.steps {
		notifyStepObserver(ctx, s)
	}
	if f.exec != nil {
		return f.exec, f.err
	}
	e := NewExecution(name, "run:client")
	e.Complete()
	return e, f.err
}

func newTestRelay(t *testing.T, runner LocalAutomationRunner) *RunRelay {
	t.Helper()
	r, err := NewRunRelay(RunRelayOptions{
		Logger:   relayTestLogger(),
		Runner:   runner,
		NodeId:   "node-a",
		NodeType: "bff",
	})
	if err != nil {
		t.Fatalf("NewRunRelay: %v", err)
	}
	return r
}

// A run on an event-triggered automation synthesizes an event on the
// automation's own trigger topic and reports every step in order.
func TestRunRelay_LocalEventTriggeredRun(t *testing.T) {
	auto := &Automation{
		Name:    "onParticipantCreated",
		Trigger: &TriggerConfig{Event: "graph.node.created.v1:cognition:participant"},
	}
	runner := &fakeRunner{
		registry: map[string]*Automation{auto.Name: auto},
		steps: []*StepResult{
			{StepId: "load", Status: "success", Duration: 3 * time.Millisecond, Result: map[string]any{"rows": 1}},
			{StepId: "notify", Status: "skipped"},
		},
	}
	relay := newTestRelay(t, runner)

	sink := &captureSink{}
	relay.Run(context.Background(), RunRequest{
		Automation:        auto.Name,
		Payload:           map[string]any{"id": "v1:cognition:participant:abc"},
		IncludeStepOutput: true,
	}, sink)

	if len(sink.accepted) != 1 {
		t.Fatalf("want exactly one accepted frame, got %d", len(sink.accepted))
	}
	acc := sink.accepted[0]
	if !acc.RanDeployedDefinition || acc.DefinitionNote == "" {
		t.Errorf("the accepted frame must carry the deployed-definition banner AS DATA, got %+v", acc)
	}
	if acc.TriggerKind != "event" {
		t.Errorf("trigger kind = %q, want event", acc.TriggerKind)
	}
	if acc.TriggerTopic != auto.Trigger.Event {
		t.Errorf("trigger topic = %q, want the automation's own concrete trigger %q",
			acc.TriggerTopic, auto.Trigger.Event)
	}
	if acc.RequestedOnNodeId != "node-a" || acc.RequestedOnNodeType != "bff" {
		t.Errorf("accepted frame must name the node that took the request, got %+v", acc)
	}

	if runner.gotEvent == nil {
		t.Fatal("an event-triggered automation must be dispatched WITH the synthesized event")
	}
	if got := runner.gotEvent.Payload["id"]; got != "v1:cognition:participant:abc" {
		t.Errorf("the caller's payload must reach the automation as the event payload, got %v", got)
	}

	if len(sink.steps) != 2 {
		t.Fatalf("want 2 step frames, got %d", len(sink.steps))
	}
	if sink.steps[0].StepId != "load" || sink.steps[0].Sequence != 0 {
		t.Errorf("first step frame = %+v", sink.steps[0])
	}
	if sink.steps[0].Output == nil {
		t.Error("include_step_output was set, so the step's result must ride along")
	}
	if sink.steps[1].StepId != "notify" || sink.steps[1].Status != "skipped" || sink.steps[1].Sequence != 1 {
		t.Errorf("second step frame = %+v", sink.steps[1])
	}

	done := sink.onlyComplete(t)
	if done.Status != "completed" || done.ErrorCode != RunCodeOK {
		t.Errorf("complete = %+v, want a completed run with code OK", done)
	}
	if done.StepCount != 2 {
		t.Errorf("step count = %d, want 2", done.StepCount)
	}
	if done.ExecutedOnNodeId != "node-a" {
		t.Errorf("complete frame must name the executing node, got %q", done.ExecutedOnNodeId)
	}
}

// Step outputs are omitted unless asked for: a timeline only needs name,
// status and duration until a row is expanded, and step results can be large.
func TestRunRelay_StepOutputOmittedByDefault(t *testing.T) {
	auto := &Automation{Name: "cron", Schedule: "0 * * * * *"}
	runner := &fakeRunner{
		registry: map[string]*Automation{auto.Name: auto},
		steps:    []*StepResult{{StepId: "sweep", Status: "success", Result: map[string]any{"deleted": 12}}},
	}
	sink := &captureSink{}
	newTestRelay(t, runner).Run(context.Background(), RunRequest{Automation: auto.Name}, sink)

	if len(sink.steps) != 1 {
		t.Fatalf("want 1 step frame, got %d", len(sink.steps))
	}
	if sink.steps[0].Output != nil {
		t.Errorf("step output must be omitted unless include_step_output is set, got %v", sink.steps[0].Output)
	}
}

// A @trigger(schedule=...) automation has no concept and no event: it is
// fire-now with an EMPTY event, which is exactly what a cron firing hands the
// executor. This is an explicit acceptance criterion of memql#3310.
func TestRunRelay_ScheduledAutomationFiresWithEmptyEvent(t *testing.T) {
	auto := &Automation{Name: "accountDeletionSweep", Schedule: "0 */10 * * * *"}
	runner := &fakeRunner{registry: map[string]*Automation{auto.Name: auto}}
	sink := &captureSink{}

	newTestRelay(t, runner).Run(context.Background(), RunRequest{Automation: auto.Name}, sink)

	if runner.gotEvent != nil {
		t.Fatalf("a scheduled automation must fire with an EMPTY (nil) event, got %+v", runner.gotEvent)
	}
	if len(sink.accepted) != 1 || sink.accepted[0].TriggerKind != "schedule" {
		t.Fatalf("accepted frame must report trigger kind schedule, got %+v", sink.accepted)
	}
	if sink.accepted[0].TriggerTopic != "" {
		t.Errorf("a schedule-kind run has no topic, got %q", sink.accepted[0].TriggerTopic)
	}
	if done := sink.onlyComplete(t); done.Status != "completed" {
		t.Errorf("complete = %+v", done)
	}
}

// An unknown name is a refusal with NOT_FOUND, delivered as accepted+complete
// so a client's state machine is uniform.
func TestRunRelay_UnknownAutomationRefused(t *testing.T) {
	runner := &fakeRunner{registry: map[string]*Automation{}}
	sink := &captureSink{}
	newTestRelay(t, runner).Run(context.Background(), RunRequest{Automation: "noSuchThing"}, sink)

	if len(sink.accepted) != 1 {
		t.Fatalf("even a refused run opens with accepted; got %d", len(sink.accepted))
	}
	done := sink.onlyComplete(t)
	if done.Status != "refused" || done.ErrorCode != RunCodeNotFound {
		t.Fatalf("complete = %+v, want refused/NOT_FOUND", done)
	}
	if !strings.Contains(done.ErrorMessage, "noSuchThing") {
		t.Errorf("the refusal must name the automation, got %q", done.ErrorMessage)
	}
}

// A @disabled automation is not runnable on the invoke path either -- the
// same rule the MCP run path applies (memql#2681).
func TestRunRelay_DisabledAutomationRefused(t *testing.T) {
	off := false
	auto := &Automation{Name: "retired", Schedule: "0 * * * * *", Enabled: &off}
	runner := &fakeRunner{registry: map[string]*Automation{auto.Name: auto}}
	sink := &captureSink{}
	newTestRelay(t, runner).Run(context.Background(), RunRequest{Automation: auto.Name}, sink)

	done := sink.onlyComplete(t)
	if done.Status != "refused" || done.ErrorCode != RunCodeFailedPrecond {
		t.Fatalf("complete = %+v, want refused/FAILED_PRECONDITION", done)
	}
}

// A trigger whose pattern is a glob cannot be made concrete without the
// caller's concept. Refusing beats guessing: a guessed topic would run the
// automation against an event shape that can never occur.
func TestRunRelay_WildcardTriggerNeedsConcept(t *testing.T) {
	auto := &Automation{Name: "anyNode", Trigger: &TriggerConfig{Event: "graph.node.created.*"}}
	runner := &fakeRunner{registry: map[string]*Automation{auto.Name: auto}}

	sink := &captureSink{}
	newTestRelay(t, runner).Run(context.Background(), RunRequest{Automation: auto.Name}, sink)
	if done := sink.onlyComplete(t); done.ErrorCode != RunCodeInvalidArgument {
		t.Fatalf("a wildcard trigger with no concept must refuse with INVALID_ARGUMENT, got %+v", done)
	}

	sink = &captureSink{}
	newTestRelay(t, runner).Run(context.Background(), RunRequest{
		Automation: auto.Name,
		Concept:    "v1:cognition:participant",
	}, sink)
	if done := sink.onlyComplete(t); done.Status != "completed" {
		t.Fatalf("supplying the concept must make the topic concrete, got %+v", done)
	}
	if got := sink.accepted[0].TriggerTopic; got != "graph.node.created.v1:cognition:participant" {
		t.Errorf("topic = %q", got)
	}
}

// A trigger @filter that the synthesized event does not satisfy is a refusal
// with the filter quoted, not a run that silently does nothing. "Would a real
// trigger carrying this payload have fired?" is half the question the invoke
// path exists to answer.
func TestRunRelay_TriggerFilterMissRefused(t *testing.T) {
	auto := &Automation{
		Name: "onlyCompleted",
		Trigger: &TriggerConfig{
			Event:  "graph.node.created.v1:planner:plan",
			Filter: `event.payload.status == "completed"`,
		},
	}
	runner := &fakeRunner{registry: map[string]*Automation{auto.Name: auto}}
	sink := &captureSink{}
	newTestRelay(t, runner).Run(context.Background(), RunRequest{
		Automation: auto.Name,
		Payload:    map[string]any{"status": "queued"},
	}, sink)

	done := sink.onlyComplete(t)
	if done.Status != "refused" || done.ErrorCode != RunCodeFailedPrecond {
		t.Fatalf("complete = %+v, want refused/FAILED_PRECONDITION", done)
	}
	if !strings.Contains(done.ErrorMessage, "@filter") {
		t.Errorf("the refusal must quote the filter, got %q", done.ErrorMessage)
	}
	if runner.gotName != "" {
		t.Error("a filter miss must not reach the executor at all")
	}
}

// Admission control: operator runs beyond the per-node cap are REFUSED rather
// than queued, so a jammed Run button degrades into a visible error instead of
// starving the shared automation lane.
func TestRunRelay_ConcurrentRunCapRefuses(t *testing.T) {
	auto := &Automation{Name: "slow", Schedule: "0 * * * * *"}
	release := make(chan struct{})
	blocking := &blockingRunner{
		registry: map[string]*Automation{auto.Name: auto},
		release:  release,
		entered:  make(chan struct{}, 1),
	}
	relay, err := NewRunRelay(RunRelayOptions{
		Logger:            relayTestLogger(),
		Runner:            blocking,
		NodeId:            "node-a",
		NodeType:          "bff",
		MaxConcurrentRuns: 1,
	})
	if err != nil {
		t.Fatalf("NewRunRelay: %v", err)
	}

	started := make(chan struct{})
	go func() {
		close(started)
		relay.Run(context.Background(), RunRequest{Automation: auto.Name}, &captureSink{})
	}()
	<-started
	// Wait for the first run to actually be inside the runner and holding the
	// only slot.
	select {
	case <-blocking.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first run never entered the runner")
	}

	sink := &captureSink{}
	relay.Run(context.Background(), RunRequest{Automation: auto.Name}, sink)
	done := sink.onlyComplete(t)
	if done.ErrorCode != RunCodeResourceExhausted {
		t.Fatalf("second concurrent run must be refused with RESOURCE_EXHAUSTED, got %+v", done)
	}
	close(release)
}

type blockingRunner struct {
	registry map[string]*Automation
	release  chan struct{}
	entered  chan struct{}
}

func (b *blockingRunner) LookupAutomation(name string) *Automation { return b.registry[name] }

func (b *blockingRunner) TriggerAutomationWithClientEvent(ctx context.Context, name string, event *events.Event) (*AutomationExecution, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	e := NewExecution(name, "run:client")
	e.Complete()
	return e, nil
}

// Relaying to another node type with no event bus is refused with UNAVAILABLE
// rather than hanging out the caller's whole deadline.
func TestRunRelay_RemoteTargetWithoutBusRefused(t *testing.T) {
	runner := &fakeRunner{registry: map[string]*Automation{}}
	sink := &captureSink{}
	newTestRelay(t, runner).Run(context.Background(), RunRequest{
		Automation:     "whatever",
		TargetNodeType: "cognition",
	}, sink)

	if done := sink.onlyComplete(t); done.ErrorCode != RunCodeUnavailable {
		t.Fatalf("complete = %+v, want UNAVAILABLE", done)
	}
}

// Targeting THIS node's own type is a local run, not a mesh round-trip.
func TestRunRelay_SelfTargetRunsLocally(t *testing.T) {
	auto := &Automation{Name: "here", Schedule: "0 * * * * *"}
	runner := &fakeRunner{registry: map[string]*Automation{auto.Name: auto}}
	sink := &captureSink{}
	newTestRelay(t, runner).Run(context.Background(), RunRequest{
		Automation:     auto.Name,
		TargetNodeType: "bff",
	}, sink)

	if done := sink.onlyComplete(t); done.Status != "completed" {
		t.Fatalf("complete = %+v", done)
	}
	if runner.gotName != auto.Name {
		t.Error("a self-targeted run must reach the local runner")
	}
}

// The relay's runner seam is satisfied by the real *Scheduler. If this stops
// compiling the invoke path has silently grown a parallel dispatch, which is
// the one thing memql#3310 rules out.
func TestSchedulerSatisfiesLocalAutomationRunner(t *testing.T) {
	var _ LocalAutomationRunner = (*Scheduler)(nil)
}

// The step observer must fire from the REAL Executor, not just from the fake
// above -- the trace is only worth anything if it observes the execution the
// automation actually gets.
func TestStepObserverFiresFromRealExecutor(t *testing.T) {
	logger := relayTestLogger()
	auto, err := NewLoader(LoaderOptions{Logger: logger}).CompileSource(
		`@description("Two trivial steps.")
automation traced {
  step first  { automation subOne { } }
  step second { automation subTwo { } }
}`, "test:step-observer")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	reg := &recordingRegistry{}
	e := NewExecutor(ExecutorOptions{Logger: logger, StepRegistry: reg})
	defer e.Close()

	var seen []string
	ctx := ContextWithStepObserver(context.Background(), func(r *StepResult) {
		seen = append(seen, r.StepId+":"+r.Status)
	})
	if _, err := e.ExecuteWithClientEvent(ctx, auto, "test", nil); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if len(seen) != 2 || seen[0] != "first:success" || seen[1] != "second:success" {
		t.Fatalf("observer must see every step in order, got %v", seen)
	}
}

// A run with NO observer on the context must behave exactly as before -- the
// observer is a diagnostic bolted onto the existing path, not a change to it.
func TestExecutorUnaffectedWithoutObserver(t *testing.T) {
	logger := relayTestLogger()
	auto, err := NewLoader(LoaderOptions{Logger: logger}).CompileSource(
		`@description("One trivial step.")
automation untraced {
  step only { automation sub { } }
}`, "test:no-observer")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	reg := &recordingRegistry{}
	e := NewExecutor(ExecutorOptions{Logger: logger, StepRegistry: reg})
	defer e.Close()

	exec, err := e.ExecuteWithClientEvent(context.Background(), auto, "test", nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if exec.Status != "completed" || len(exec.Steps) != 1 {
		t.Fatalf("execution = %+v", exec)
	}
}

// recordingRegistry succeeds every step so the observer assertions are about
// the observer rather than about step semantics.
type recordingRegistry struct{}

func (r *recordingRegistry) Execute(ctx context.Context, step *Step, stepCtx *StepContext) (*StepResult, error) {
	now := time.Now()
	return &StepResult{
		StepId:      step.ID,
		Status:      "success",
		StartedAt:   now,
		CompletedAt: now,
	}, nil
}
