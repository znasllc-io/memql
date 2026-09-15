package automations

import (
	"context"
	"fmt"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/metrics"
	"github.com/znasllc-io/memql/component/work"
	"testing"
	"time"
)

func TestJournalStartsRootAfterDepthCap(t *testing.T) {
	ctx := events.ContextWithCause(context.Background(), events.Cause{Depth: 16, CorrelationId: "chain", CausationId: "run16"})
	cause, _ := events.CauseFromContext(journalContext(ctx))
	event := events.NewEvent("graph.node.updated.v1:work:run", events.KindNodeUpdated, map[string]any{"id": "run16"}).WithCause(cause)
	_, run := runCause(context.Background(), &Automation{Name: "releaseWorkspace"}, "release", &event)
	if run.Depth != 1 {
		t.Fatalf("journal reactor depth = %d", run.Depth)
	}
}
func TestRowBudgetLimitsAndPrunes(t *testing.T) {
	now := time.Unix(100, 0)
	b := newTestBudget(0, 0, time.Minute, &now)
	b.perRowMax = 30
	for i := 0; i < 30; i++ {
		if ok, _, _ := b.admitRow("a", "row"); !ok {
			t.Fatal(i)
		}
	}
	if ok, dim, alert := b.admitRow("a", "row"); ok || dim != "per-row" || !alert {
		t.Fatalf("%v %s %v", ok, dim, alert)
	}
	if ok, dim, alert := b.admitRow("a", "row"); ok || dim != "per-row" || alert {
		t.Fatalf("repeat: %v %s %v", ok, dim, alert)
	}
	for i := 0; i < 100; i++ {
		b.admitRow("a", fmt.Sprint(i))
		if ok, _, _ := b.admitRow("a", ""); !ok {
			t.Fatal("non-row budget")
		}
	}
	now = now.Add(time.Minute)
	b.admitRow("a", "fresh")
	if len(b.perRow) != 1 {
		t.Fatal(len(b.perRow))
	}
}
func TestDedupClaimsInflightUntilReleased(t *testing.T) {
	d := newExecutionDedup(time.Hour)
	defer d.stop()
	if dup, _ := d.claimOrDuplicate("a", "key", "event1", "run1"); dup {
		t.Fatal("first")
	}
	if dup, echo := d.claimOrDuplicate("a", "key", "event1", "run2"); !dup || echo {
		t.Fatal("redelivery")
	}
	if dup, echo := d.claimOrDuplicate("a", "key", "event2", "run2"); !dup || !echo {
		t.Fatal("echo")
	}
	d.release("a", "key", "run2")
	if !d.isDuplicate("a", "key") {
		t.Fatal("non-owner released")
	}
	d.release("a", "key", "run1")
	if dup, _ := d.claimOrDuplicate("a", "key", "event1", "retry"); dup {
		t.Fatal("retry blocked")
	}
}
func TestChainProjectionExecution(t *testing.T) {
	t.Setenv("MEMQL_AUTOMATION_MAX_CHAIN_DEPTH", "16")
	probe := &causeProbeRegistry{}
	e := NewExecutor(ExecutorOptions{StepRegistry: probe, ChainTrackingEnabled: true, DedupEnabled: true})
	defer e.Close()
	a := causeProbeAutomation(t.Name())
	a.Reads = []string{"id", "status"}
	fire := func(clock, status, chain string) *AutomationExecution {
		ev := events.NewEvent("graph.node.updated.v1:t:x", events.KindNodeUpdated, map[string]any{"id": "row", "createdAt": clock, "status": status})
		if chain != "" {
			ev = ev.WithCause(events.Cause{CorrelationId: chain, Depth: 1})
		}
		run, err := e.ExecuteWithEvent(context.Background(), a, ev.Topic, &ev)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	before := metrics.AutomationLoopsStoppedValue(a.Name, metrics.LoopStopEcho)
	if fire("1", "open", "chain").Status != "completed" {
		t.Fatal("first")
	}
	if fire("2", "open", "chain").Status != "skipped" {
		t.Fatal("clock echo not skipped")
	}
	if metrics.AutomationLoopsStoppedValue(a.Name, metrics.LoopStopEcho) != before+1 {
		t.Fatal("echo count")
	}
	if fire("3", "closed", "chain").Status != "completed" {
		t.Fatal("changed read suppressed")
	}
	if fire("4", "open", "").Status != "completed" || fire("5", "open", "").Status != "completed" {
		t.Fatal("roots suppressed")
	}
}
func TestReadsProjectionIsConservative(t *testing.T) {
	source := registrySource{reg: work.Registry{"write": {ConstructKind: work.ConstructMutation}, "read": {ConstructKind: work.ConstructQuery}}}
	a := &Automation{Name: "projection", Trigger: &TriggerConfig{Filter: "row => row.status != \"done\""}, Steps: []*Step{{ID: "write", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "write", Kind: "mutation", Args: map[string]any{"x": map[string]any{"$expr": "event.payload.value"}}}}}}
	if err := PrepareExpressions(a); err != nil {
		t.Fatal(err)
	}
	fields := computeReads(a, source)
	if fmt.Sprint(fields) != "[id nodeId status value]" {
		t.Fatal(fields)
	}
	a.Steps[0].Function.Name = "read"
	if computeReads(a, source) != nil {
		t.Fatal("storage query narrowed")
	}
}

type blockingRuntimeRegistry struct {
	entered chan string
	finish  chan struct{}
}

func (p *blockingRuntimeRegistry) Execute(ctx context.Context, s *Step, _ *StepContext) (*StepResult, error) {
	p.entered <- s.ID
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.finish:
		return &StepResult{StepId: s.ID, Status: "completed"}, nil
	}
}
func TestModeExecutorSingleAndRestart(t *testing.T) {
	for _, kind := range []string{"single", "restart"} {
		t.Run(kind, func(t *testing.T) {
			p := &blockingRuntimeRegistry{make(chan string, 2), make(chan struct{})}
			e := NewExecutor(ExecutorOptions{StepRegistry: p})
			defer e.Close()
			a := causeProbeAutomation(t.Name())
			a.Mode = &ModeConfig{Kind: kind}
			done := make(chan *AutomationExecution, 2)
			go func() { run, _ := e.Execute(context.Background(), a, "manual"); done <- run }()
			<-p.entered
			before := metrics.AutomationLoopsStoppedValue(a.Name, metrics.LoopStopMode)
			if kind == "single" {
				run, err := e.Execute(context.Background(), a, "manual")
				if err != nil || run.Status != "skipped" {
					t.Fatalf("%+v %v", run, err)
				}
				if metrics.AutomationLoopsStoppedValue(a.Name, metrics.LoopStopMode) != before+1 {
					t.Fatal("mode counter")
				}
				close(p.finish)
				<-done
			} else {
				go func() { run, _ := e.Execute(context.Background(), a, "manual"); done <- run }()
				<-p.entered
				first := <-done
				if first.Status != "cancelled" {
					t.Fatal(first.Status)
				}
				close(p.finish)
				if second := <-done; second.Status != "completed" {
					t.Fatal(second.Status)
				}
			}
		})
	}
}

func TestInflightEchoAndFailedRetry(t *testing.T) {
	p := &blockingRuntimeRegistry{make(chan string, 2), make(chan struct{})}
	e := NewExecutor(ExecutorOptions{StepRegistry: p, ChainTrackingEnabled: true, DedupEnabled: true})
	defer e.Close()
	a := causeProbeAutomation(t.Name())
	a.Reads = []string{"id"}
	event := events.NewEvent("graph.node.updated.v1:t:x", events.KindNodeUpdated, map[string]any{"id": "row", "createdAt": "1"}).WithCause(events.Cause{CorrelationId: "inflight", Depth: 1})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *AutomationExecution, 1)
	go func() { run, _ := e.ExecuteWithEvent(ctx, a, event.Topic, &event); done <- run }()
	<-p.entered
	echo := event
	echo.Payload = map[string]any{"id": "row", "createdAt": "2"}
	run, err := e.ExecuteWithEvent(context.Background(), a, event.Topic, &echo)
	if err != nil || run.Status != "skipped" {
		t.Fatalf("inflight echo: %+v %v", run, err)
	}
	cancel()
	if first := <-done; first.Status != "cancelled" {
		t.Fatal(first.Status)
	}
	close(p.finish)
	retry, err := e.ExecuteWithEvent(context.Background(), a, event.Topic, &event)
	if err != nil || retry.Status != "completed" {
		t.Fatalf("retry: %+v %v", retry, err)
	}
}
func TestRowBudgetExecutorCountsEveryRefusal(t *testing.T) {
	original := sharedAutomationBudget
	defer func() { sharedAutomationBudget = original }()
	now := time.Now()
	b := newTestBudget(0, 0, time.Minute, &now)
	b.perRowMax = 1
	sharedAutomationBudget = b
	p := &causeProbeRegistry{}
	e := NewExecutor(ExecutorOptions{StepRegistry: p})
	defer e.Close()
	a := causeProbeAutomation(t.Name())
	event := events.NewEvent("graph.node.updated.v1:t:x", events.KindNodeUpdated, map[string]any{"nodeId": "row"})
	before := metrics.AutomationLoopsStoppedValue(a.Name, metrics.LoopStopRowBudget)
	for i := 0; i < 3; i++ {
		run, err := e.ExecuteWithEvent(context.Background(), a, event.Topic, &event)
		if err != nil {
			t.Fatal(err)
		}
		want := "skipped"
		if i == 0 {
			want = "completed"
		}
		if run.Status != want {
			t.Fatal(run.Status)
		}
	}
	if metrics.AutomationLoopsStoppedValue(a.Name, metrics.LoopStopRowBudget) != before+2 {
		t.Fatal("row refusal count")
	}
	event.Topic = "custom.event"
	for i := 0; i < 3; i++ {
		run, err := e.ExecuteWithEvent(context.Background(), a, event.Topic, &event)
		if err != nil || run.Status != "completed" {
			t.Fatalf("non graph row: %+v %v", run, err)
		}
	}
}

func TestBeforeWriteCannotExecuteAsIndependentRun(t *testing.T) {
	e := NewExecutor(ExecutorOptions{})
	defer e.Close()
	a := &Automation{Name: "hook", BeforeWrite: &BeforeWriteConfig{}}
	if run, err := e.Execute(context.Background(), a, "manual"); run != nil || err == nil {
		t.Fatalf("before-write created independent run: %+v %v", run, err)
	}
}
