package work

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

func dispatchDBEngine(t *testing.T, db *bun.DB) *memqlengine.MemQLEngine {
	t.Helper()
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	eng, err := memqlengine.New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	return eng
}

func TestDispatchDB_LatestPersistedGoalAndStatusGate(t *testing.T) {
	db, _ := sweepDB(t)
	eng := dispatchDBEngine(t, db)
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, goal, status string
		want               bool
	}{
		{"scheduler", "", "running", false},
		{"compiled", "v1:work:goal:g1", "running", true},
		{"finished", "v1:work:goal:g1", "succeeded", false},
		{"failed", "v1:work:goal:g1", "failed", false},
		{"parked", "v1:work:goal:g1", "waiting", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := runConcept + ":" + tc.name
			// Every event could have carried this older running version.
			insertSweepRow(t, db, id, runConcept, now.Add(-time.Hour), map[string]any{"automationName": "probe", "status": "running", "goalId": tc.goal, "ownerUserId": ""})
			insertSweepRow(t, db, id, runConcept, now, map[string]any{"automationName": "probe", "status": tc.status, "goalId": tc.goal, "ownerUserId": ""})
			j, err := automations.LoadRunJournal(context.Background(), eng, id)
			if err != nil {
				t.Fatal(err)
			}
			if j.Status != tc.status || j.GoalId != tc.goal || (DispatchRequest{Status: "running"}).CanDispatchStoredRun(j.GoalId, j.Status, j.WaitingOn, now) != tc.want {
				t.Fatalf("latest dispatch metadata: %+v; want dispatch=%v", j, tc.want)
			}
		})
	}
}

type pausedSchedulerStep struct{ entered, release chan struct{} }

func (p *pausedSchedulerStep) Execute(ctx context.Context, step *automations.Step, _ *automations.StepContext) (*automations.StepResult, error) {
	close(p.entered)
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &automations.StepResult{StepId: step.ID, Status: "completed", StartedAt: time.Now(), CompletedAt: time.Now()}, nil
}

type notifiedWorkDispatch struct{ called chan DispatchRequest }

func (d *notifiedWorkDispatch) Dispatch(_ context.Context, r DispatchRequest) { d.called <- r }

func TestDispatchDB_SchedulerJournalDoesNotStartACompetingExecution(t *testing.T) {
	db, i := sweepDB(t)
	eng := dispatchDBEngine(t, db)
	bus := events.NewBus()
	eng.SetEventBus(bus)
	d := &notifiedWorkDispatch{called: make(chan DispatchRequest, 4)}
	i.SetDispatcher(d)
	i.SetRunClaimer(&stubClaimer{grant: true})
	observed := make(chan struct{}, 4)
	unsub := bus.Subscribe("graph.node.created.v1:work:run", func(ev events.Event) { i.HandleRunEvent(ev); observed <- struct{}{} })
	defer unsub()
	pause := &pausedSchedulerStep{entered: make(chan struct{}), release: make(chan struct{})}
	exec := automations.NewExecutor(automations.ExecutorOptions{Engine: eng, StepRegistry: pause, EventBus: bus})
	defer exec.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	auto := &automations.Automation{Name: "schedulerJournalProbe", Trusted: true, Steps: []*automations.Step{{ID: "effect", Type: automations.StepTypeFunction, Function: &automations.FunctionStepConfig{Name: "probe", Kind: "query"}}}}
	go func() {
		_, err := exec.ExecuteWithEvent(ctx, auto, "event:system.startup", &events.Event{Topic: "system.startup", Payload: map[string]any{"node": map[string]any{"type": "bff"}}})
		done <- err
	}()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(pause.release) }) }
	defer release()
	select {
	case <-pause.entered:
	case err := <-done:
		t.Fatalf("scheduler stopped early: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal("no real journal-created event observed")
	}
	select {
	case req := <-d.called:
		t.Fatalf("ordinary scheduler journal was dispatched again: %+v", req)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestDispatchDB_SweepRecoversDueTimersAndInferenceWaits(t *testing.T) {
	db, i := sweepDB(t)
	eng := dispatchDBEngine(t, db)
	i.engine = eng
	now := time.Now().UTC()
	d := &capturingDispatcher{}
	i.SetDispatcher(d)
	i.SetRunClaimer(&stubClaimer{grant: true})
	for _, kind := range []string{"timer", "inferenceUnavailable"} {
		waiting := map[string]any{"kind": "timer", "resumeAt": now.Add(-time.Minute).Format(time.RFC3339Nano)}
		if kind == "inferenceUnavailable" {
			waiting["kind"] = "approval"
			waiting["approvalKind"] = kind
		}
		owner := actorCtx("recovery-owner")
		if err := i.store().createRunRow(owner, runSeed{RunId: runConcept + ":" + kind, AutomationName: "probe", TemplateFingerprint: "probe", Status: "waiting", Mode: "live", ReplayPolicy: "strict", StartedAt: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if err := i.store().updateRun(owner, runConcept+":"+kind, map[string]any{"waitingOn": waiting}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := i.SweepWaiting(auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("sweepWaitingWorkRuns")), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if result.Resumed != 1 || result.Redispatched != 1 {
		t.Fatalf("sweep %+v", result)
	}
	requests := d.seen()
	if len(requests) != 2 {
		t.Fatalf("requests %+v", requests)
	}
	for _, req := range requests {
		j, err := automations.LoadRunJournal(context.Background(), eng, req.RunId)
		if err != nil {
			t.Fatal(err)
		}
		if !req.Recovery || !req.CanDispatchStoredRun(j.GoalId, j.Status, j.WaitingOn, time.Now()) {
			t.Fatalf("recovery rejected: request %+v journal %+v", req, j)
		}
	}
}
