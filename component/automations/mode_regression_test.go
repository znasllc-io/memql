package automations

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

type modeRegressionRegistry struct {
	entered chan context.Context
	release chan struct{}
}

func (r *modeRegressionRegistry) Execute(ctx context.Context, s *Step, _ *StepContext) (*StepResult, error) {
	r.entered <- ctx
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.release:
		return &StepResult{StepId: s.ID, Status: "completed"}, nil
	}
}
func modeRegressionEvent(value int) *events.Event {
	event := events.NewEvent("mode.regression", events.KindMessage, map[string]any{"value": value})
	return &event
}
func TestModeRestartRedeliveryKeepsTheActiveRun(t *testing.T) {
	registry := &modeRegressionRegistry{make(chan context.Context, 2), make(chan struct{})}
	executor := NewExecutor(ExecutorOptions{StepRegistry: registry, ChainTrackingEnabled: true, DedupEnabled: true, MaxConcurrentExecutions: 1})
	defer executor.Close()
	automation := causeProbeAutomation(t.Name())
	automation.Mode = &ModeConfig{Kind: ModeRestart}
	event := modeRegressionEvent(1)
	done := make(chan *AutomationExecution, 1)
	go func() {
		run, _ := executor.ExecuteWithEvent(context.Background(), automation, event.Topic, event)
		done <- run
	}()
	active := <-registry.entered
	duplicate, err := executor.ExecuteWithEvent(context.Background(), automation, event.Topic, event)
	if err != nil || duplicate.Status != "skipped" || duplicate.Error != "duplicate execution detected" {
		t.Fatalf("redelivery: %+v, %v", duplicate, err)
	}
	if active.Err() != nil {
		t.Fatalf("duplicate cancelled the active run: %v", active.Err())
	}
	close(registry.release)
	if run := <-done; run.Status != "completed" {
		t.Fatalf("active run ended %s", run.Status)
	}
}
func TestModeRefusalReleasesItsDedupReservation(t *testing.T) {
	registry := &modeRegressionRegistry{make(chan context.Context, 2), make(chan struct{})}
	executor := NewExecutor(ExecutorOptions{StepRegistry: registry, ChainTrackingEnabled: true, DedupEnabled: true})
	defer executor.Close()
	automation := causeProbeAutomation(t.Name())
	automation.Mode = &ModeConfig{Kind: ModeSingle}
	first, second := modeRegressionEvent(1), modeRegressionEvent(2)
	done := make(chan *AutomationExecution, 1)
	go func() {
		run, _ := executor.ExecuteWithEvent(context.Background(), automation, first.Topic, first)
		done <- run
	}()
	<-registry.entered
	refused, err := executor.ExecuteWithEvent(context.Background(), automation, second.Topic, second)
	if err != nil || refused.Status != "skipped" {
		t.Fatalf("mode refusal: %+v %v", refused, err)
	}
	close(registry.release)
	<-done
	retry, err := executor.ExecuteWithEvent(context.Background(), automation, second.Topic, second)
	if err != nil || retry.Status != "completed" {
		t.Fatalf("refused fire retained reservation: %+v %v", retry, err)
	}
}

type modeRegistryFunc func(context.Context, *Step, *StepContext) (*StepResult, error)

func (f modeRegistryFunc) Execute(ctx context.Context, s *Step, scope *StepContext) (*StepResult, error) {
	return f(ctx, s, scope)
}
func TestModeQueuedSynchronousSubcallDoesNotWaitForItsParent(t *testing.T) {
	automation := causeProbeAutomation(t.Name())
	automation.Mode = &ModeConfig{Kind: ModeQueued, Max: 3}
	var executor *Executor
	var calls atomic.Int32
	executor = NewExecutor(ExecutorOptions{MaxConcurrentExecutions: 1, StepRegistry: modeRegistryFunc(func(ctx context.Context, s *Step, _ *StepContext) (*StepResult, error) {
		if calls.Add(1) > 1 {
			return nil, errors.New("reentrant child reached a step")
		}
		child, err := executor.Execute(ctx, automation, "subautomation")
		if err != nil {
			return nil, err
		}
		if child.Status != "skipped" {
			return nil, errors.New("synchronous queued child was not refused")
		}
		return &StepResult{StepId: s.ID, Status: "completed"}, nil
	})})
	defer executor.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	parent, err := executor.Execute(ctx, automation, "manual")
	if err != nil || parent.Status != "completed" {
		t.Fatalf("parent waited for its own queue: %+v %v", parent, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("executed %d steps", calls.Load())
	}
}
func TestModeQueuedAsynchronousCausalChildCanWait(t *testing.T) {
	gate := &modeGate{}
	automation := &Automation{Name: t.Name(), Mode: &ModeConfig{Kind: ModeQueued, Max: 1}}
	_, release, err := gate.acquire(context.Background(), automation, "parent")
	if err != nil {
		t.Fatal(err)
	}
	// A bus event carries the causal run ID, but no synchronous waiting frame.
	ctx := events.ContextWithCause(context.Background(), events.Cause{CorrelationId: "root", Depth: 1, Chain: []events.Link{{Automation: automation.Name, RunId: "parent"}}})
	done := make(chan error, 1)
	go func() {
		_, finish, err := gate.acquire(ctx, automation, "child")
		if err == nil {
			finish()
		}
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		gate.mu.Lock()
		waiting := len(gate.byName[automation.Name].waiting)
		gate.mu.Unlock()
		if waiting == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("causal event failed to queue")
		}
		time.Sleep(time.Millisecond)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestModeAuthoredNamesAreIsolatedFromOwnersAndShipped(t *testing.T) {
	gate := &modeGate{}
	a := &Automation{Name: "same", Origin: "authored:authorA:same", Mode: &ModeConfig{Kind: ModeRestart}}
	b := &Automation{Name: "same", Origin: "authored:authorB:same", Mode: &ModeConfig{Kind: ModeRestart}}
	shipped := &Automation{Name: "same", Origin: "unified:pack/automations.memql:same", Mode: &ModeConfig{Kind: ModeRestart}}
	ctxA, releaseA, err := gate.acquire(context.Background(), a, "A")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseA()
	ctxB, releaseB, err := gate.acquire(context.Background(), b, "B")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseB()
	ctxShipped, releaseShipped, err := gate.acquire(context.Background(), shipped, "shipped")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseShipped()
	if ctxA.Err() != nil || ctxB.Err() != nil || ctxShipped.Err() != nil {
		t.Fatal("one owner cancelled another definition")
	}
	_, releaseNext, err := gate.acquire(context.Background(), a, "A2")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseNext()
	if ctxA.Err() != context.Canceled || ctxB.Err() != nil || ctxShipped.Err() != nil {
		t.Fatal("restart did not stay within one authored identity")
	}
}
func TestModeAuthoredDedupUsesOrigin(t *testing.T) {
	executor := NewExecutor(ExecutorOptions{StepRegistry: &causeProbeRegistry{}, ChainTrackingEnabled: true, DedupEnabled: true})
	defer executor.Close()
	event := modeRegressionEvent(1)
	for _, origin := range []string{"authored:authorA:same", "authored:authorB:same", "unified:pack/automations.memql:same"} {
		a := causeProbeAutomation("same")
		a.Origin = origin
		a.Mode = &ModeConfig{Kind: ModeRestart}
		run, err := executor.ExecuteWithEvent(context.Background(), a, event.Topic, event)
		if err != nil || run.Status != "completed" {
			t.Fatalf("%s was deduplicated across owners: %+v %v", origin, run, err)
		}
	}
}
func TestModeAuthoredActivationStampsIdentity(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	var origin string
	scheduler, err := NewAuthoredScheduler(AuthoredSchedulerOptions{Loader: NewLoader(LoaderOptions{}), EventBus: bus, Run: func(_ context.Context, a *Automation, _ *events.Event) error { origin = a.Origin; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.Stop()
	construct := &memql.AuthoredConstruct{Name: "same", Kind: "automation", OwnerUserId: "authorA", Source: "@mode(restart)\n@trigger(event=\"mode.authored.identity\")\nautomation same { return 1 }"}
	if err := scheduler.Activate(construct); err != nil {
		t.Fatal(err)
	}
	bus.PublishSync(events.NewEvent("mode.authored.identity", events.KindMessage, nil))
	if origin != "authored:authorA:same" {
		t.Fatalf("authored origin=%q", origin)
	}
}

func TestModeRestartDepthRefusalKeepsTheActiveRun(t *testing.T) {
	t.Setenv("MEMQL_AUTOMATION_MAX_CHAIN_DEPTH", "16")
	registry := &modeRegressionRegistry{make(chan context.Context, 2), make(chan struct{})}
	executor := NewExecutor(ExecutorOptions{StepRegistry: registry, ChainTrackingEnabled: true, DedupEnabled: true, MaxConcurrentExecutions: 1})
	defer executor.Close()
	automation := causeProbeAutomation(t.Name())
	automation.Mode = &ModeConfig{Kind: ModeRestart}
	first := modeRegressionEvent(1)
	done := make(chan *AutomationExecution, 1)
	go func() {
		run, _ := executor.ExecuteWithEvent(context.Background(), automation, first.Topic, first)
		done <- run
	}()
	active := <-registry.entered
	refused := modeRegressionEvent(2)
	refused.Cause = events.Cause{CorrelationId: "depth-limit", Depth: 16}
	run, err := executor.ExecuteWithEvent(context.Background(), automation, refused.Topic, refused)
	var loopRefusal *LoopRefusal
	if run.Status != "failed" || !errors.As(err, &loopRefusal) {
		t.Fatalf("depth refusal: %+v %v", run, err)
	}
	if active.Err() != nil {
		t.Fatal("depth-refused fire cancelled active restart run")
	}
	close(registry.release)
	if firstRun := <-done; firstRun.Status != "completed" {
		t.Fatal(firstRun.Status)
	}
}
