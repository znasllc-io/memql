package automations

// loop_depth_chain_test.go -- the depth cap end to end, on a real Executor
// (epic memql#5380, D-A and D-C). The step registry is a fake and the journal
// a recording one, so the chain runs in microseconds with no database; what is
// real is everything the executor does between one fire and the next: the
// cause it reads off the event, the cause it stamps on the context its steps
// run under, the gates, the refusal, the journal rows and the counter.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/metrics"
	"github.com/znasllc-io/memql/component/work"
)

// loopProbeTopic is the topic the probe automation triggers on and writes.
const loopProbeTopic = "graph.node.updated.v1:probe:ticket"

// republishRegistry is the loop. The automation's one step publishes its own
// trigger topic again, carrying the step context's cause exactly as the event
// step does (steps/event.go), and keeps the event so the test can feed it back
// as the next fire -- the delivery the scheduler's bus subscription makes.
//
// Each publish carries a new count, so every fire is a different write of the
// row: a loop that rewrites the same value is an echo, which dedup (not the
// depth cap) is for.
type republishRegistry struct {
	count int
	last  *events.Event
}

func (r *republishRegistry) Execute(ctx context.Context, step *Step, stepCtx *StepContext) (*StepResult, error) {
	r.count++
	ev := events.NewEvent(loopProbeTopic, events.KindNodeUpdated, map[string]any{"id": "ticket-1", "count": r.count})
	if cause, ok := events.CauseFromContext(ctx); ok {
		ev = ev.WithCause(cause)
	}
	r.last = &ev
	if stepCtx != nil && stepCtx.EventBus != nil {
		stepCtx.EventBus.Publish(ev)
	}
	now := time.Now()
	return &StepResult{StepId: step.ID, Status: "completed", Result: map[string]any{"topic": loopProbeTopic}, StartedAt: now, CompletedAt: now}, nil
}

// loopProbeAutomation re-fires itself: triggered by the topic its step writes.
func loopProbeAutomation(name string) *Automation {
	return &Automation{
		Name:    name,
		Trigger: &TriggerConfig{Event: loopProbeTopic},
		Steps:   []*Step{{ID: "rewrite", Type: StepTypeEvent, Event: &EventStepConfig{Topic: loopProbeTopic}}},
	}
}

// lifecycleCapture keeps every automation.* event the executor publishes. The
// bus delivers on goroutines, so reads wait for what they expect.
type lifecycleCapture struct {
	mu  sync.Mutex
	evs []events.Event
}

func captureLifecycle(t *testing.T, bus *events.Bus) *lifecycleCapture {
	t.Helper()
	c := &lifecycleCapture{}
	unsub := bus.Subscribe("automation.#", func(ev events.Event) {
		c.mu.Lock()
		c.evs = append(c.evs, ev)
		c.mu.Unlock()
	})
	t.Cleanup(unsub)
	return c
}

func (c *lifecycleCapture) topic(topic string) []events.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []events.Event
	for _, ev := range c.evs {
		if ev.Topic == topic {
			out = append(out, ev)
		}
	}
	return out
}

func (c *lifecycleCapture) all() []events.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]events.Event(nil), c.evs...)
}

// waitFor returns the topic's events once n have arrived.
func (c *lifecycleCapture) waitFor(t *testing.T, topic string, n int) []events.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := c.topic(topic)
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d %s events arrived in 10s, want %d", len(got), topic, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// forExecution is the one event of the topic a run published.
func forExecution(t *testing.T, evs []events.Event, execId string) events.Event {
	t.Helper()
	for _, ev := range evs {
		if ev.Payload["executionId"] == execId {
			return ev
		}
	}
	t.Fatalf("no event for execution %s among %d", execId, len(evs))
	return events.Event{}
}

// loopHarness is one replica: an Executor configured like the scheduler's
// event executor (chain tracking and dedup on), a recording journal and a
// lifecycle capture on its bus.
type loopHarness struct {
	e         *Executor
	rec       *recordingJournalExecutor
	bus       *events.Bus
	lifecycle *lifecycleCapture
	logs      *bytes.Buffer
}

func newLoopHarness(t *testing.T, reg StepExecutorRegistry) *loopHarness {
	t.Helper()
	// The budget is process-wide and this package runs many executions in one
	// process; a chain test tripping it would read as a depth bug.
	sharedAutomationBudget.reset()
	bus := events.NewBus(events.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(bus.Close)
	var logs bytes.Buffer
	e := NewExecutor(ExecutorOptions{
		Logger:               capturingLogger(&logs),
		EventBus:             bus,
		StepRegistry:         reg,
		ChainTrackingEnabled: true,
		DedupEnabled:         true,
	})
	t.Cleanup(e.Close)
	rec := &recordingJournalExecutor{}
	e.journal = newWorkJournal(rec, nil)
	return &loopHarness{e: e, rec: rec, bus: bus, lifecycle: captureLifecycle(t, bus), logs: &logs}
}

// driveChain fires the automation from a root write, then keeps feeding it
// the event its own step published, up to fires times or until a fire
// publishes nothing.
func driveChain(t *testing.T, h *loopHarness, auto *Automation, reg *republishRegistry, fires int) ([]*AutomationExecution, []error) {
	t.Helper()
	ev := events.NewEvent(loopProbeTopic, events.KindNodeUpdated, map[string]any{"id": "ticket-1", "count": 0})
	next := &ev
	var execs []*AutomationExecution
	var errs []error
	for i := 1; i <= fires; i++ {
		reg.last = nil
		exec, err := h.e.ExecuteWithEvent(context.Background(), auto, "event:"+loopProbeTopic, next)
		execs = append(execs, exec)
		errs = append(errs, err)
		if reg.last == nil {
			break
		}
		next = reg.last
	}
	return execs, errs
}

// callsFor returns the journal calls that name the run, parsed.
func callsFor(t *testing.T, rec *recordingJournalExecutor, runId string) (names []string, args []map[string]any) {
	t.Helper()
	for _, c := range rec.calls {
		n, a := argsOf(t, c)
		if a["runId"] == runId {
			names = append(names, n)
			args = append(args, a)
		}
	}
	return names, args
}

// TestLoopDepth_AChainOf17FiresStopsAt16 is the record's acceptance case: a
// chain of 17 fires is stopped at 16, as a loop_depth_exceeded run failure
// that names the chain.
func TestLoopDepth_AChainOf17FiresStopsAt16(t *testing.T) {
	t.Setenv(maxChainDepthEnv, "16")
	reg := &republishRegistry{}
	h := newLoopHarness(t, reg)
	auto := loopProbeAutomation("loopChainProbe")
	beforeDepth := metrics.AutomationLoopsStoppedValue(auto.Name, metrics.LoopStopDepth)
	beforeBound := metrics.AutomationLoopsStoppedValue(auto.Name, metrics.LoopStopLoopBound)

	execs, errs := driveChain(t, h, auto, reg, 17)
	if len(execs) != 17 {
		t.Fatalf("the chain ran %d fires, want 17", len(execs))
	}
	for i := 0; i < 16; i++ {
		if errs[i] != nil || execs[i].Status != "completed" {
			t.Fatalf("run %d = %q (%v), want completed: runs 1-16 are inside the cap", i+1, execs[i].Status, errs[i])
		}
	}

	// Run 17 is refused, as a *LoopRefusal naming the chain.
	refused := execs[16]
	var r *LoopRefusal
	if !errors.As(errs[16], &r) {
		t.Fatalf("run 17 returned %v (status %q), want a *LoopRefusal", errs[16], refused.Status)
	}
	if r.Reason != metrics.LoopStopDepth || r.Depth != 17 || r.Cap != 16 || r.Automation != auto.Name {
		t.Fatalf("refusal = %+v, want depth 17 past the cap of 16", r)
	}
	if refused.Status != "failed" || refused.Error != r.Error() || refused.ErrorValue != error(r) || refused.CompletedAt.IsZero() {
		t.Fatalf("refused execution = status %q error %q value %v completed %v", refused.Status, refused.Error, refused.ErrorValue, refused.CompletedAt)
	}
	if len(refused.Steps) != 0 {
		t.Fatalf("the refused run executed steps: %v", refused.Steps)
	}

	// The chain: the root write's correlation, and the 16 runs in order.
	correlation := r.Cause.CorrelationId
	if !strings.HasPrefix(correlation, "evt-") {
		t.Fatalf("correlation = %q, want the root write's evt- fingerprint", correlation)
	}

	// ONE row for the refused run: opened and closed at failed, never
	// classified. No step rows, no cancel read -- nothing ran.
	names, args := callsFor(t, h.rec, refused.ID)
	if strings.Join(names, ",") != "createWorkRun,updateWorkRun" {
		t.Fatalf("journal calls for the refused run = %v, want createWorkRun then updateWorkRun", names)
	}
	open, closed := args[0], args[1]
	trigger, _ := open["triggerEvent"].(map[string]any)
	parentCause := events.CauseFromMap(mapOf(trigger["cause"]))
	if parentCause.Depth != 16 || parentCause.CausationId != execs[15].ID || parentCause.CorrelationId != correlation {
		t.Errorf("triggerEvent.cause = %+v, want the parent: depth 16, caused by run 16 (%s)", parentCause, execs[15].ID)
	}
	if closed["status"] != "failed" || closed["errorCode"] != work.TerminalLoopDepthExceeded || closed["errorMessage"] != r.Error() || closed["finishedAt"] == nil {
		t.Fatalf("the refused run's close = %v", closed)
	}
	outcome, _ := closed["outcome"].(map[string]any)
	loop, _ := outcome["loop"].(map[string]any)
	if outcome["executorStatus"] != "failed" || loop == nil {
		t.Fatalf("outcome = %v, want executorStatus failed and the loop record -- a close through the classifier writes neither", outcome)
	}
	if loop["reason"] != metrics.LoopStopDepth || loop["depth"] != float64(17) || loop["cap"] != float64(16) || loop["correlationId"] != correlation {
		t.Errorf("outcome.loop = %v", loop)
	}
	chain, _ := loop["chain"].([]any)
	if len(chain) != 16 {
		t.Fatalf("outcome.loop.chain has %d links, want the 16 runs that ran: %v", len(chain), chain)
	}
	for i, link := range chain {
		l, _ := link.(map[string]any)
		if l["runId"] != execs[i].ID || l["automation"] != auto.Name {
			t.Errorf("chain[%d] = %v, want %s run %s", i, l, auto.Name, execs[i].ID)
		}
	}
	for _, c := range h.rec.calls {
		if n, a := argsOf(t, c); n == "updateWorkRun" && a["runId"] != refused.ID && a["errorCode"] != nil {
			t.Errorf("a run inside the cap was closed with an error code: %v", a)
		}
	}

	// The counter moved once, under depth and nothing else.
	if got := metrics.AutomationLoopsStoppedValue(auto.Name, metrics.LoopStopDepth) - beforeDepth; got != 1 {
		t.Errorf("loops_stopped_total{reason=depth} rose by %v, want exactly 1", got)
	}
	if got := metrics.AutomationLoopsStoppedValue(auto.Name, metrics.LoopStopLoopBound) - beforeBound; got != 0 {
		t.Errorf("loops_stopped_total{reason=loop_bound} rose by %v, want 0", got)
	}

	// The WARN an operator reads names the automation, the depth, the cap and
	// the chain.
	logs := h.logs.String()
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "depth=17") || !strings.Contains(logs, "cap=16") || !strings.Contains(logs, execs[15].ID) {
		t.Errorf("no WARN naming depth 17, cap 16 and the chain in:\n%s", logs)
	}

	// THE LIFECYCLE EVENTS CARRY THE RUN'S CAUSE (Task 2's stamping, and its
	// negative control): each run's automation.started and automation.completed
	// name that run as their causation at that run's depth.
	started := h.lifecycle.waitFor(t, events.TopicAutomationStarted, 16)
	completed := h.lifecycle.waitFor(t, events.TopicAutomationCompleted, 16)
	for i := 0; i < 16; i++ {
		for _, ev := range []events.Event{forExecution(t, started, execs[i].ID), forExecution(t, completed, execs[i].ID)} {
			if ev.Cause.Depth != i+1 || ev.Cause.CausationId != execs[i].ID || ev.Cause.CorrelationId != correlation {
				t.Errorf("run %d's %s carries %+v, want depth %d caused by %s in %s", i+1, ev.Topic, ev.Cause, i+1, execs[i].ID, correlation)
			}
		}
	}
	// A refused run never acquires its mode or starts. It records the
	// terminal journal row without publishing a misleading started event.
	for _, ev := range started {
		if ev.Payload["executionId"] == refused.ID {
			t.Error("depth-refused run published automation.started")
		}
	}
	for _, ev := range h.lifecycle.all() {
		if ev.Cause.Depth > 16 || len(ev.Cause.Chain) > 16 {
			t.Errorf("%s carries depth %d with %d links: past the cap of 16", ev.Topic, ev.Cause.Depth, len(ev.Cause.Chain))
		}
	}
}

// TestLoopDepth_ACapOf32LetsTheSameChainRun is the negative control: the same
// 17 fires under a cap of 32 all complete, and the counter does not move. It is
// what makes the stop above a fact about the cap rather than about the harness.
func TestLoopDepth_ACapOf32LetsTheSameChainRun(t *testing.T) {
	t.Setenv(maxChainDepthEnv, "32")
	reg := &republishRegistry{}
	h := newLoopHarness(t, reg)
	auto := loopProbeAutomation("loopChainControl")
	before := metrics.AutomationLoopsStoppedValue(auto.Name, metrics.LoopStopDepth)

	execs, errs := driveChain(t, h, auto, reg, 17)
	if len(execs) != 17 {
		t.Fatalf("the chain ran %d fires, want 17", len(execs))
	}
	for i := range execs {
		if errs[i] != nil || execs[i].Status != "completed" {
			t.Fatalf("run %d = %q (%v), want completed under a cap of 32", i+1, execs[i].Status, errs[i])
		}
	}
	if got := metrics.AutomationLoopsStoppedValue(auto.Name, metrics.LoopStopDepth) - before; got != 0 {
		t.Errorf("loops_stopped_total{reason=depth} rose by %v under a cap the chain never reached", got)
	}
	for _, c := range h.rec.calls {
		if strings.Contains(c, work.TerminalLoopDepthExceeded) {
			t.Errorf("a journal write names %s under a cap of 32: %s", work.TerminalLoopDepthExceeded, c)
		}
	}
	// Run 17 ran at depth 17: the cap moved, the counting did not.
	completed := h.lifecycle.waitFor(t, events.TopicAutomationCompleted, 17)
	if ev := forExecution(t, completed, execs[16].ID); ev.Cause.Depth != 17 {
		t.Errorf("run 17 ran at depth %d, want 17", ev.Cause.Depth)
	}
}

// TestLoopDepth_ALoopAutomationStopsAtItsMaxDepth: a @loop(maxDepth=2)
// automation re-firing itself runs twice in one chain, and the third fire is
// refused as loop_bound long before the cap.
func TestLoopDepth_ALoopAutomationStopsAtItsMaxDepth(t *testing.T) {
	t.Setenv(maxChainDepthEnv, "16")
	reg := &republishRegistry{}
	h := newLoopHarness(t, reg)
	auto := loopProbeAutomation("loopBoundProbe")
	auto.Trigger.Filter = `row => row.status != "done"`
	auto.Loop = &LoopConfig{MaxDepth: 2, Until: `row => row.status == "done"`}
	beforeBound := metrics.AutomationLoopsStoppedValue(auto.Name, metrics.LoopStopLoopBound)
	beforeDepth := metrics.AutomationLoopsStoppedValue(auto.Name, metrics.LoopStopDepth)

	execs, errs := driveChain(t, h, auto, reg, 3)
	if len(execs) != 3 || errs[0] != nil || errs[1] != nil {
		t.Fatalf("fires = %d, errs = %v: the first two runs are inside maxDepth=2", len(execs), errs)
	}
	var r *LoopRefusal
	if !errors.As(errs[2], &r) || r.Reason != metrics.LoopStopLoopBound || r.Cap != 2 || r.Depth != 3 {
		t.Fatalf("the third fire returned %v (%+v), want a loop_bound refusal at depth 3 with cap 2", errs[2], r)
	}
	_, args := callsFor(t, h.rec, execs[2].ID)
	if len(args) != 2 {
		t.Fatalf("the refused run wrote %d journal calls, want 2", len(args))
	}
	loop, _ := mapOf(args[1]["outcome"])["loop"].(map[string]any)
	if loop["reason"] != metrics.LoopStopLoopBound || loop["cap"] != float64(2) || args[1]["errorCode"] != work.TerminalLoopDepthExceeded {
		t.Errorf("the refused run's close = %v", args[1])
	}
	if got := metrics.AutomationLoopsStoppedValue(auto.Name, metrics.LoopStopLoopBound) - beforeBound; got != 1 {
		t.Errorf("loops_stopped_total{reason=loop_bound} rose by %v, want 1", got)
	}
	if got := metrics.AutomationLoopsStoppedValue(auto.Name, metrics.LoopStopDepth) - beforeDepth; got != 0 {
		t.Errorf("loops_stopped_total{reason=depth} rose by %v, want 0", got)
	}
}

// onceClaimer is the cross-replica claim (ClusterExecutionGuard) without its
// table: the first replica to claim an (automation, key) runs it.
type onceClaimer struct {
	mu      sync.Mutex
	claimed map[string]bool
}

func (c *onceClaimer) Claim(_ context.Context, automationName, dedupKey string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.claimed[automationName+"\x00"+dedupKey] {
		return false
	}
	c.claimed[automationName+"\x00"+dedupKey] = true
	return true
}

// TestLoopDepth_OneEventRefusedOnTwoReplicasWritesOneRow: the refusal sits
// AFTER the cluster guard's claim (D-C), so the one event that reaches both
// replicas is refused once, recorded once and counted once.
func TestLoopDepth_OneEventRefusedOnTwoReplicasWritesOneRow(t *testing.T) {
	t.Setenv(maxChainDepthEnv, "16")
	sharedAutomationBudget.reset()
	guard := &onceClaimer{claimed: map[string]bool{}}
	replica := func() (*Executor, *recordingJournalExecutor) {
		e := NewExecutor(ExecutorOptions{StepRegistry: &causeProbeRegistry{}, ChainTrackingEnabled: true, DedupEnabled: true, ClusterGuard: guard})
		t.Cleanup(e.Close)
		rec := &recordingJournalExecutor{}
		e.journal = newWorkJournal(rec, nil)
		return e, rec
	}
	a, recA := replica()
	b, recB := replica()
	auto := causeProbeAutomation("loopTwoReplicas")
	auto.Trigger = &TriggerConfig{Event: loopProbeTopic}
	ev := &events.Event{Topic: loopProbeTopic, Kind: events.KindNodeUpdated, Payload: map[string]any{"id": "ticket-1"}, Cause: chainOf("evt-root", repeatName("upstream", 16)...)}
	before := metrics.AutomationLoopsStoppedValue(auto.Name, metrics.LoopStopDepth)

	_, errA := a.ExecuteWithEvent(context.Background(), auto, "event:"+loopProbeTopic, ev)
	execB, errB := b.ExecuteWithEvent(context.Background(), auto, "event:"+loopProbeTopic, ev)

	var r *LoopRefusal
	if !errors.As(errA, &r) {
		t.Fatalf("replica A returned %v, want the refusal", errA)
	}
	if errB != nil || execB.Status != "skipped" {
		t.Fatalf("replica B = %q (%v), want skipped by the claim replica A holds", execB.Status, errB)
	}
	if len(recA.calls) != 2 || len(recB.calls) != 0 {
		t.Fatalf("journal writes: A %d, B %d; want the refusal's two on A and none on B", len(recA.calls), len(recB.calls))
	}
	if got := metrics.AutomationLoopsStoppedValue(auto.Name, metrics.LoopStopDepth) - before; got != 1 {
		t.Errorf("the stop was counted %v times across two replicas, want once", got)
	}
}

// TestLoopDepth_AJournalSkippedAutomationCountsAndWarnsWithNoRow: an
// automation that reacts to work rows writes no journal (its own rows would
// re-fire it), so its refusal records no row -- and is still counted and
// logged with the chain, which is then the only record of it.
func TestLoopDepth_AJournalSkippedAutomationCountsAndWarnsWithNoRow(t *testing.T) {
	t.Setenv(maxChainDepthEnv, "16")
	sharedAutomationBudget.reset()
	var logs bytes.Buffer
	e := NewExecutor(ExecutorOptions{Logger: capturingLogger(&logs), StepRegistry: &causeProbeRegistry{}})
	t.Cleanup(e.Close)
	rec := &recordingJournalExecutor{}
	e.journal = newWorkJournal(rec, nil)
	auto := causeProbeAutomation("loopOnWorkRows")
	auto.Trigger = &TriggerConfig{Event: "graph.node.updated.v1:work:run"}
	if !journalSkipsAutomation(auto) {
		t.Fatal("the probe must be journal-skipped for this test to mean anything")
	}
	ev := &events.Event{Topic: "graph.node.updated.v1:work:run", Payload: map[string]any{"id": "run-x"}, Cause: chainOf("evt-root", repeatName("upstream", 16)...)}
	before := metrics.AutomationLoopsStoppedValue(auto.Name, metrics.LoopStopDepth)

	exec, err := e.ExecuteWithEvent(context.Background(), auto, "event:"+ev.Topic, ev)
	var r *LoopRefusal
	if !errors.As(err, &r) || exec.Status != "failed" {
		t.Fatalf("got %q (%v), want the refusal", exec.Status, err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("a journal-skipped automation wrote %v", rec.calls)
	}
	if got := metrics.AutomationLoopsStoppedValue(auto.Name, metrics.LoopStopDepth) - before; got != 1 {
		t.Errorf("the stop was counted %v times, want 1", got)
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "upstream (run-16)") || !strings.Contains(out, "automation="+auto.Name) {
		t.Errorf("the WARN is the only record of this stop and must name the chain:\n%s", out)
	}
}

// subAutomationTrigger is the scheduler's sub-automation dispatch
// (TriggerAutomationWithArgs): the child runs on the SAME executor, under the
// calling step's context, on a synthetic invocation event with no cause of
// its own.
type subAutomationTrigger struct {
	e     *Executor
	autos map[string]*Automation
}

func (s *subAutomationTrigger) TriggerAutomation(ctx context.Context, name string) (*AutomationExecution, error) {
	return s.e.Execute(ctx, s.autos[name], "manual")
}

func (s *subAutomationTrigger) TriggerAutomationWithArgs(ctx context.Context, name string, args map[string]any) (*AutomationExecution, error) {
	return s.e.ExecuteWithEvent(ctx, s.autos[name], "sub-automation", &events.Event{Topic: "automation.invocation." + name, Payload: args})
}

// subAutomationRegistry runs an automation step the way steps.AutomationExecutor
// does -- through the trigger, wrapping a child's failure -- and records the
// cause every step ran under, by automation.
type subAutomationRegistry struct {
	mu     sync.Mutex
	causes map[string][]events.Cause
}

func (r *subAutomationRegistry) Execute(ctx context.Context, step *Step, stepCtx *StepContext) (*StepResult, error) {
	c, _ := events.CauseFromContext(ctx)
	r.mu.Lock()
	r.causes[stepCtx.Execution.AutomationName] = append(r.causes[stepCtx.Execution.AutomationName], c)
	r.mu.Unlock()
	now := time.Now()
	if step.Automation != nil {
		if _, err := stepCtx.AutomationTrigger.TriggerAutomationWithArgs(ctx, step.Automation.Name, map[string]any{"from": stepCtx.Execution.ID}); err != nil {
			msg := fmt.Sprintf("automation %q execution failed: %v", step.Automation.Name, err)
			return &StepResult{StepId: step.ID, Status: "failed", Error: msg, StartedAt: now, CompletedAt: now}, fmt.Errorf("automation %q execution failed: %w", step.Automation.Name, err)
		}
	}
	return &StepResult{StepId: step.ID, Status: "completed", StartedAt: now, CompletedAt: now}, nil
}

func subAutomationHarness(t *testing.T) (*Executor, *recordingJournalExecutor, *subAutomationRegistry, *Automation, *Automation) {
	t.Helper()
	sharedAutomationBudget.reset()
	reg := &subAutomationRegistry{causes: map[string][]events.Cause{}}
	e := NewExecutor(ExecutorOptions{StepRegistry: reg, ChainTrackingEnabled: true})
	t.Cleanup(e.Close)
	rec := &recordingJournalExecutor{}
	e.journal = newWorkJournal(rec, nil)
	parent := &Automation{Name: "loopSubParent", Trigger: &TriggerConfig{Event: loopProbeTopic}, Steps: []*Step{
		{ID: "call", Type: StepTypeAutomation, Automation: &AutomationStepConfig{Name: "loopSubChild"}},
	}}
	child := causeProbeAutomation("loopSubChild")
	e.automationTrigger = &subAutomationTrigger{e: e, autos: map[string]*Automation{parent.Name: parent, child.Name: child}}
	return e, rec, reg, parent, child
}

// TestLoopDepth_ASubAutomationCountsAsARun: a sub-automation runs inside its
// caller's chain, one deeper -- a logic call is a statement of its run, a
// sub-automation is a run.
func TestLoopDepth_ASubAutomationCountsAsARun(t *testing.T) {
	t.Setenv(maxChainDepthEnv, "16")
	e, _, reg, parent, child := subAutomationHarness(t)
	ev := events.NewEvent(loopProbeTopic, events.KindNodeUpdated, map[string]any{"id": "ticket-1"})

	exec, err := e.ExecuteWithEvent(context.Background(), parent, "event:"+loopProbeTopic, &ev)
	if err != nil || exec.Status != "completed" {
		t.Fatalf("parent = %q (%v)", exec.Status, err)
	}
	p, c := reg.causes[parent.Name], reg.causes[child.Name]
	if len(p) != 1 || len(c) != 1 {
		t.Fatalf("steps ran: parent %d, child %d; want one each", len(p), len(c))
	}
	if p[0].Depth != 1 || c[0].Depth != 2 {
		t.Fatalf("parent ran at depth %d, child at %d; want 1 and 2", p[0].Depth, c[0].Depth)
	}
	if c[0].CorrelationId != p[0].CorrelationId || len(c[0].Chain) != 2 || c[0].Chain[0] != p[0].Chain[0] || c[0].Chain[1].Automation != child.Name {
		t.Fatalf("child cause %+v does not extend the parent's %+v", c[0], p[0])
	}
}

// TestLoopDepth_AParentWhoseSubAutomationWasRefusedFailsTerminally: the child
// one past the cap is refused, and the PARENT's step failure carries the code,
// so the parent's run lands terminal with loop_depth_exceeded rather than
// parked on a retry that would run the same chain into the same bound.
func TestLoopDepth_AParentWhoseSubAutomationWasRefusedFailsTerminally(t *testing.T) {
	t.Setenv(maxChainDepthEnv, "16")
	e, rec, _, parent, child := subAutomationHarness(t)
	ev := &events.Event{Topic: loopProbeTopic, Kind: events.KindNodeUpdated, Payload: map[string]any{"id": "ticket-1"}, Cause: chainOf("evt-root", repeatName("upstream", 15)...)}
	before := metrics.AutomationLoopsStoppedValue(child.Name, metrics.LoopStopDepth)

	exec, err := e.ExecuteWithEvent(context.Background(), parent, "event:"+loopProbeTopic, ev)
	if err == nil || exec.Status != "failed" {
		t.Fatalf("parent = %q (%v), want failed: its sub-automation was refused", exec.Status, err)
	}
	var r *LoopRefusal
	if !errors.As(err, &r) || r.Automation != child.Name || r.Depth != 17 {
		t.Fatalf("parent error %v does not carry the child's refusal at depth 17", err)
	}

	var parentClose, childClose map[string]any
	for _, c := range rec.calls {
		n, a := argsOf(t, c)
		if n != "updateWorkRun" || a["status"] != "failed" {
			continue
		}
		switch a["runId"] {
		case exec.ID:
			parentClose = a
		default:
			childClose = a
		}
	}
	if parentClose == nil || parentClose["errorCode"] != work.TerminalLoopDepthExceeded {
		t.Fatalf("the parent's run closed as %v, want errorCode %s", parentClose, work.TerminalLoopDepthExceeded)
	}
	if childClose == nil {
		t.Fatal("the refused child wrote no run row")
	}
	loop, _ := mapOf(childClose["outcome"])["loop"].(map[string]any)
	chain, _ := loop["chain"].([]any)
	if len(chain) != 16 || mapOf(chain[15])["runId"] != exec.ID || mapOf(chain[15])["automation"] != parent.Name {
		t.Fatalf("the child's recorded chain = %v, want 16 links ending at the parent's run %s", chain, exec.ID)
	}
	if got := metrics.AutomationLoopsStoppedValue(child.Name, metrics.LoopStopDepth) - before; got != 1 {
		t.Errorf("the child's stop was counted %v times, want 1", got)
	}
}

// mapOf is v as an object, or nil.
func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
