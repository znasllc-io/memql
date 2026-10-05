package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

// work_continued_failure_replicas_db_test.go -- the cross-replica half of
// memql#5664's last finding: a goal run whose step failed under `on error
// continue` looked restartable by a second replica once its dispatch lease
// lapsed. Confirmed here against the real claim table, the real admission
// (workRunCanStart) and the real resume, and fixed in component/automations'
// statementResumePoint: a failure the body continued past is no resume point.

// replicaDispatch is one agent replica's Dispatch for a run with a journal:
// the stored row under the journal's actor, the agent's own admission, and
// the resume on this replica's executor. The template is handed in rather
// than loaded -- loading is not what is under test, and a goal run's template
// is the same on every replica.
type replicaDispatch struct {
	engine     *memql.MemQLEngine
	automation *automations.Automation
	executor   *automations.Executor
	mu         sync.Mutex
	claimed    bool
	admitted   bool
	resumeErr  error
	done       chan struct{}
}

func (d *replicaDispatch) Dispatch(ctx context.Context, req workspine.DispatchRequest) {
	defer func() { d.done <- struct{}{} }()
	d.mu.Lock()
	d.claimed = true
	d.mu.Unlock()
	journal, err := automations.LoadRunJournal(ctx, d.engine, req.RunId)
	if err != nil || !workRunCanStart(req, journal, time.Now()) {
		return
	}
	d.mu.Lock()
	d.admitted = true
	d.mu.Unlock()
	_, err = d.executor.ResumeFrom(ctx, journal, d.automation, &automations.ResumeOptions{AllowSideEffects: true})
	d.mu.Lock()
	d.resumeErr = err
	d.mu.Unlock()
}

// stepsRan records every step a replica executed.
type stepsRan struct {
	mu  sync.Mutex
	ran []string
}

func (s *stepsRan) Execute(_ context.Context, step *automations.Step, _ *automations.StepContext) (*automations.StepResult, error) {
	s.mu.Lock()
	s.ran = append(s.ran, step.ID)
	s.mu.Unlock()
	now := time.Now()
	return &automations.StepResult{StepId: step.ID, Status: "completed", Result: "ok", StartedAt: now, CompletedAt: now}, nil
}

func insertWorkRow(t *testing.T, db *bun.DB, rowId, concept string, at time.Time, fields map[string]any) {
	t.Helper()
	payload, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",type,schema,payload) VALUES (?, ?, ?, 'replica-test', 'object', '{}', ?::jsonb)`,
		rowId, concept, at, string(payload)); err != nil {
		t.Fatal(err)
	}
}

// TestWorkDB_ASecondReplicaDoesNotRestartARunPastAContinuedFailure.
//
// Replica A is executing a goal's run and is between two statements: `a`
// finished, `flaky` failed and was continued past, `b` finished, and `three`
// has not begun -- so the run is `running`, its heartbeat fresh, nothing in
// flight. A dispatched it longer ago than the claim's lease. Replica B hears
// one of A's receipt events, wins the lapsed claim, and the agent's admission
// lets it through: it fences only a step in flight, and none is. The one
// thing that kept B from starting an ordinary run again at this point -- a
// journal with no failure has nothing to resume -- did not hold, because a
// continued failure IS a failed row: B resumed from `flaky`, running it, the
// finished `two`, and `three` beside the replica still executing the run.
func TestWorkDB_ASecondReplicaDoesNotRestartARunPastAContinuedFailure(t *testing.T) {
	engine, db := workTemplateDBEngineAndDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	stamp := strings.ReplaceAll(id.NewShortId(), "-", "")
	runId := "v1:work:run:continued-" + stamp
	now := time.Now().UTC()

	auto, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(`@trigger(event="probe.fired")
automation continuesPast`+stamp+` {
  a := builtin one()
  builtin flaky() on error continue
  b := builtin two(x: a)
  builtin three(y: b)
}`, "continued.memql")
	if err != nil {
		t.Fatal(err)
	}

	insertWorkRow(t, db, runId, "v1:work:run", now.Add(-10*time.Minute), map[string]any{
		"automationName": auto.Name, "status": "running", "goalId": "v1:work:goal:continued-" + stamp,
		"ownerUserId": "owner-" + stamp, "mode": "live", "triggeredBy": "api",
		"startedAt": now.Add(-10 * time.Minute).Format(time.RFC3339Nano), "heartbeatAt": now.Add(-5 * time.Second).Format(time.RFC3339Nano),
		"stepOrder": []string{"a", "flaky", "b"},
	})
	for seq, s := range []struct {
		key, status string
		result      map[string]any
	}{
		{"a", "done", map[string]any{"stepId": "a", "status": "completed", "value": "A"}},
		{"flaky", "failed", nil},
		{"b", "done", map[string]any{"stepId": "b", "status": "completed", "value": "B"}},
	} {
		row := map[string]any{"runId": runId, "key": s.key, "seq": seq, "stepType": "function", "status": s.status, "attempt": 1, "version": 1}
		if s.result != nil {
			row["result"] = s.result
		}
		if s.status == "failed" {
			row["errorMessage"] = "the flaky call failed, and the body carried on"
		}
		insertWorkRow(t, db, "v1:work:step:continued-"+stamp+"-"+s.key, "v1:work:step", now.Add(-time.Duration(4-seq)*time.Minute), row)
	}

	dbGetter := func() *bun.DB { return db }
	// Replica A claimed the run when it dispatched it, and the lease has
	// since lapsed, as it does for any run that outlives it.
	if !automations.NewClusterExecutionGuard(dbGetter, logger).StrictClaimer().ClaimWithTTL(context.Background(), "work.run.dispatch", runId, 4*time.Minute) {
		t.Fatal("replica A could not claim a fresh run")
	}
	if _, err := db.ExecContext(context.Background(),
		`UPDATE automation_execution_claims SET claimed_at = now() - interval '10 minutes' WHERE automation_name = 'work.run.dispatch' AND dedup_key = ?`, runId); err != nil {
		t.Fatal(err)
	}

	ran := &stepsRan{}
	b := &replicaDispatch{
		engine:     engine,
		automation: auto,
		executor:   automations.NewExecutor(automations.ExecutorOptions{Engine: engine, StepRegistry: ran}),
		done:       make(chan struct{}, 1),
	}
	defer b.executor.Close()
	replicaB := workspine.New(engine, logger, dbGetter)
	replicaB.SetDispatcher(b)
	replicaB.SetRunClaimer(automations.NewClusterExecutionGuard(dbGetter, logger).StrictClaimer())

	replicaB.HandleRunEvent(events.Event{Topic: "graph.node.updated.v1:work:run", Payload: map[string]any{
		"id": runId,
		"payload": map[string]any{
			"status": "running", "automationName": auto.Name, "goalId": "v1:work:goal:continued-" + stamp,
			"ownerUserId": "owner-" + stamp, "triggeredBy": "api",
		},
	}})
	select {
	case <-b.done:
	case <-time.After(30 * time.Second):
		t.Fatal("replica B never dispatched the run: the test did not reach the window it is about")
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.claimed || !b.admitted {
		t.Fatalf("claimed=%v admitted=%v: the precondition -- a second replica that wins the lapsed claim and passes the agent's admission -- did not hold, so this test asserts nothing", b.claimed, b.admitted)
	}
	ran.mu.Lock()
	defer ran.mu.Unlock()
	if len(ran.ran) != 0 {
		t.Fatalf("replica B ran %v beside replica A, which is still executing this run: a failure the body continued past is no resume point", ran.ran)
	}
	if b.resumeErr == nil || !strings.Contains(b.resumeErr.Error(), "continued past") {
		t.Errorf("resume err = %v, want the refusal that names the continued failure", b.resumeErr)
	}
}
