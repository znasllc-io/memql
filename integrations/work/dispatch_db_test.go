package work

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/workjournal"
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

// replicaAdmission is one agent replica's dispatcher as far as its admission:
// it re-reads the run it claimed behind the journal's privileged read and
// admits it only where the agent does -- CanDispatchStoredRun, which the
// agent's fence (app/work_run_fence.go) ends at, and for a waiting run decides
// alone. An admitted dispatch is the run executing on this replica.
type replicaAdmission struct {
	engine   *memqlengine.MemQLEngine
	mu       sync.Mutex
	claimed  []string
	admitted []string
}

func (d *replicaAdmission) Dispatch(ctx context.Context, req DispatchRequest) {
	d.mu.Lock()
	d.claimed = append(d.claimed, req.RunId)
	d.mu.Unlock()
	j, err := automations.LoadRunJournal(ctx, d.engine, req.RunId)
	if err != nil || IsDriverOwnedRun(j.TriggeredBy) || !req.CanDispatchStoredRun(j.GoalId, j.Status, j.WaitingOn, time.Now()) {
		return
	}
	d.mu.Lock()
	d.admitted = append(d.admitted, req.RunId)
	d.mu.Unlock()
}

// TestDispatchDB_ADueRetryIsServedByExactlyOneOfTwoReplicas (memql#5664). Two
// agent replicas share one Postgres claim table, as two pods do, and each runs
// the waiting sweep -- the cron lease moves, and two passes can overlap. A run
// the failure path parked on a DUE `retry` must execute on exactly one of
// them. It executed on neither: the replica that won the claim refused the
// waiting run at its admission, then held the claim for its lease, so the
// other replica could not take it either -- and when the lease lapsed the
// next pass claimed it and refused it again.
func TestDispatchDB_ADueRetryIsServedByExactlyOneOfTwoReplicas(t *testing.T) {
	db, a := sweepDB(t)
	eng := dispatchDBEngine(t, db)
	b := New(eng, slog.New(slog.NewTextHandler(io.Discard, nil)), func() *bun.DB { return db })
	b.admitRow = a.admitRow
	a.engine = eng
	now := time.Now().UTC()
	// The claim table is the database's own, not the test schema's, so the run
	// id is unique to this run of the test.
	runId := runConcept + ":retry-" + bareRunId(newRowId(runConcept))
	owner := actorCtx("retry-owner")
	if err := a.store().createRunRow(owner, runSeed{RunId: runId, AutomationName: "probe", TemplateFingerprint: "probe", Status: runStatusWaiting, Mode: modeLive, ReplayPolicy: "strict", StartedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := a.store().updateRun(owner, runId, map[string]any{"waitingOn": map[string]any{
		"kind": waitKindRetry, "subject": "fetch", "since": now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
		"resumeAt": now.Add(-time.Minute).Format(time.RFC3339Nano), "reason": "the far side reported itself temporarily unavailable",
	}}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	replicas := map[string]*replicaAdmission{}
	for name, r := range map[string]*Integration{"a": a, "b": b} {
		d := &replicaAdmission{engine: eng}
		replicas[name] = d
		r.SetDispatcher(d)
		r.SetRunClaimer(automations.NewClusterExecutionGuard(func() *bun.DB { return db }, logger).StrictClaimer())
	}
	maintenance := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("sweepWaitingWorkRuns"))
	for _, r := range []*Integration{a, b} {
		if _, err := r.SweepWaiting(maintenance, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	var claimed, admitted []string
	for _, d := range replicas {
		claimed = append(claimed, d.claimed...)
		admitted = append(admitted, d.admitted...)
	}
	if len(claimed) != 1 {
		t.Fatalf("the run was claimed %d times across two replicas sharing one claim table, want once: %v", len(claimed), claimed)
	}
	if len(admitted) != 1 {
		t.Fatalf("the due retry executed on %d replicas, want exactly one: the replica that claimed it refused it at its admission and holds the claim for its lease", len(admitted))
	}
}

// TestDispatchDB_SweepLeavesAProcedureReplayRunToItsRunner is the unit test's
// finding against the REAL recovery read: runsInFlight projects the payload
// into the row the sweep judges, and the replay runner's trigger has to survive
// that projection for the sweep to see it. Both runs are written through the
// real createWorkRun, both went silent an hour ago, neither names a goal; only
// the ordinary one -- the control -- is handed back, and the replay run is
// closed by its heartbeat rather than run a second time.
func TestDispatchDB_SweepLeavesAProcedureReplayRunToItsRunner(t *testing.T) {
	db, i := sweepDB(t)
	eng := dispatchDBEngine(t, db)
	i.engine = eng
	d, c := &capturingDispatcher{}, &stubClaimer{grant: true}
	i.SetDispatcher(d)
	i.SetRunClaimer(c)
	silentSince := time.Now().UTC().Add(-time.Hour)
	replay, ordinary := runConcept+":replay", runConcept+":ordinary"
	for _, seed := range []runSeed{
		{
			RunId: replay, AutomationName: "learnedProcedure_abc_l1", TemplateFingerprint: "sha256:abc",
			TemplateConstructId: "v1:authoring:construct:p1", TriggeredBy: "procedure:trusted",
			Status: "running", Mode: "live", StartedAt: silentSince,
		},
		{
			RunId: ordinary, AutomationName: "probe", TemplateFingerprint: "probe", TriggeredBy: "schedule",
			Status: "running", Mode: "live", StartedAt: silentSince,
		},
	} {
		if err := i.store().createRunRow(actorCtx("replay-sweep-owner"), seed); err != nil {
			t.Fatal(err)
		}
	}

	result, err := i.SweepWaiting(auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("sweepWaitingWorkRuns")), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.seen(); len(got) != 1 || got[0].RunId != ordinary {
		t.Fatalf("dispatched %+v, want only the ordinary run (sweep %+v)", got, result)
	}
	if len(c.keys) != 1 || c.keys[0] != ordinary {
		t.Errorf("claimed %v, want only the ordinary run's claim", c.keys)
	}
	if result.Redispatched != 1 || result.Abandoned != 1 {
		t.Errorf("sweep %+v, want the ordinary run handed back and the silent replay run closed", result)
	}
	j, err := automations.LoadRunJournal(context.Background(), eng, replay)
	if err != nil {
		t.Fatal(err)
	}
	if j.TriggeredBy != "procedure:trusted" || j.Status != runStatusAbandoned {
		t.Fatalf("the replay run reads back triggeredBy %q status %q, want procedure:trusted and abandoned", j.TriggeredBy, j.Status)
	}
}

// TestDispatchDB_SweepLeavesAPipelineRunToItsRunner is the pipeline half of
// the replay test above (epic memql#5477), against the REAL recovery read and
// the REAL journal. The pipeline's run is opened as the pipelines driver opens
// it -- component/workjournal's Begin with the driver's trigger and every step
// queued -- so the same test shows the journal's new writes are ones the work
// DSL accepts, and that the trigger survives runsInFlight's projection for the
// sweep to read past. Both runs are silent; only the ordinary one -- the
// control -- is handed back, and the pipeline run reads back exactly as its
// driver left it: running, every step pending, nothing abandoned.
func TestDispatchDB_SweepLeavesAPipelineRunToItsRunner(t *testing.T) {
	db, i := sweepDB(t)
	eng := dispatchDBEngine(t, db)
	i.engine = eng
	d, c := &capturingDispatcher{}, &stubClaimer{grant: true}
	i.SetDispatcher(d)
	i.SetRunClaimer(c)
	ctx := context.Background()

	journal := workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) {
		return eng.Execute(ctx, q)
	}), slog.New(slog.NewTextHandler(io.Discard, nil)), "agent-1")
	steps := []workjournal.StepDecl{
		{
			Key: "checks/build-vet", Kind: workjournal.KindDeterministic, StepType: "exec",
			Call: map[string]any{"construct": "pipeline", "name": "build-vet", "stage": "checks"},
		},
		{
			Key: "tests/go-tests#1", Kind: workjournal.KindDeterministic, StepType: "exec",
			DependsOn: []string{"checks/build-vet"},
			Call:      map[string]any{"construct": "pipeline", "name": "go-tests", "stage": "tests"},
		},
	}
	run, err := journal.Begin(ctx, workjournal.Work{
		OwnerUserID: "pipeline-sweep-owner",
		Template:    "pipeline",
		Statement:   "Run the checks on 1a2b3c4",
		GoalKey:     "v1:pipelines:run:sweep-db",
		TriggeredBy: "pipeline:full",
		QueueSteps:  true,
		Steps:       steps,
	})
	if err != nil {
		t.Fatalf("the journal could not open the pipeline's run: %v", err)
	}
	pipelineRun := runConcept + ":" + run.RunID()
	ordinary := runConcept + ":ordinary-beside-a-pipeline"
	if err := i.store().createRunRow(actorCtx("pipeline-sweep-owner"), runSeed{
		RunId: ordinary, AutomationName: "probe", TemplateFingerprint: "probe", TriggeredBy: "schedule",
		Status: "running", Mode: "live", StartedAt: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// The journal stamps the run's start as it opens it; a one-nanosecond
	// window makes it as silent as the control, so only its trigger can keep
	// the sweep off it.
	result, err := i.SweepWaiting(auth.ContextWithAccess(ctx, auth.MaintenanceActor("sweepWaitingWorkRuns")), time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.seen(); len(got) != 1 || got[0].RunId != ordinary {
		t.Fatalf("dispatched %+v, want only the ordinary run (sweep %+v)", got, result)
	}
	if len(c.keys) != 1 || c.keys[0] != ordinary {
		t.Errorf("claimed %v, want only the ordinary run's claim", c.keys)
	}
	if result.Redispatched != 1 || result.Abandoned != 0 || result.Resumed != 0 {
		t.Errorf("sweep %+v, want the ordinary run handed back and nothing else touched", result)
	}
	j, err := automations.LoadRunJournal(ctx, eng, pipelineRun)
	if err != nil {
		t.Fatal(err)
	}
	if j.TriggeredBy != "pipeline:full" || j.Status != runStatusRunning {
		t.Fatalf("the pipeline run reads back triggeredBy %q status %q, want pipeline:full and running", j.TriggeredBy, j.Status)
	}
	versions, err := i.StepVersions(actorCtx("pipeline-sweep-owner"), pipelineRun)
	if err != nil {
		t.Fatal(err)
	}
	pending := map[string]bool{}
	for _, v := range versions {
		if rowString(v, "status") == "pending" && rowString(v, "stepType") == "exec" {
			pending[rowString(v, "key")] = true
		}
	}
	for _, s := range steps {
		if !pending[s.Key] {
			t.Errorf("step %s did not read back pending: the journal's queued write was refused or the sweep moved it (versions %v)", s.Key, versions)
		}
	}
}
