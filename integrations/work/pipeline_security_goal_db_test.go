package work

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

func TestPipelineSecurityGoalLostEventRecoversThroughExistingCompiler(t *testing.T) {
	_, intake, planners, probe := compileDB(t)
	intake.SetGateDB(testGates(t))
	intake.SetNow(func() time.Time { return time.Now().Add(-2 * time.Minute) })
	receipt, err := intake.OpenPipelineSecurityGoal(actorCtx("compile-bob"), "lost-security-event", DirectGoal{
		OwnerUserId: "compile-bob", Statement: "Resolve the verified security finding", Input: map[string]any{"repository": "acme/repo", "sha": "first"},
		BeforeRun: func(context.Context, string, string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	// No event bus is attached. Only the normal cross-replica sweep can find it.
	result, err := planners[0].SweepWaiting(auth.ContextWithAccess(context.Background(), auth.MaintenanceActor(auth.AutomationOriginEngine, "sweepWaitingWorkRuns")), time.Minute)
	if err != nil || result.Redispatched != 1 || result.Abandoned != 0 {
		t.Fatal("security compile was stranded", result, err)
	}
	got := awaitCompile(t, probe)
	if memql.BareShortId(got.request.RunId) != receipt.RunID || got.request.Input["sha"] != "first" || got.actor != canonicalUser("compile-bob") || !got.budget || !got.hasAuthority {
		t.Fatalf("compiler lost owner, evidence or budget: %+v", got)
	}
	finishCompile(t, probe)
}

func TestPipelineSecurityGoalsDeduplicateAcrossReplicasAndPreserveEvidence(t *testing.T) {
	a, b := openActsDB(t), openActsDB(t)
	a.i.EnableCompileViaEvent()
	b.i.EnableCompileViaEvent()
	ctx := actorCtx(a.owner)
	enabled := true
	g := DirectGoal{OwnerUserId: a.owner, Statement: "Resolve the verified unsafe SQL finding", Input: map[string]any{"sha": "original", "finding": "stable"},
		BeforeRun: func(context.Context, string, string) error {
			if !enabled {
				return errors.New("disabled")
			}
			return nil
		}}
	var got [2]ReviewGoalReceipt
	var failures [2]error
	var wg sync.WaitGroup
	for n, i := range []*Integration{a.i, b.i} {
		wg.Add(1)
		go func(n int, i *Integration) {
			defer wg.Done()
			got[n], failures[n] = i.OpenPipelineSecurityGoal(ctx, "finding-1", g)
		}(n, i)
	}
	wg.Wait()
	if failures[0] != nil || failures[1] != nil || got[0] != got[1] {
		t.Fatalf("concurrent admission: %v %v", got, failures)
	}
	run, err := b.i.store().runForOwner(memql.ContextWithFreshRead(ctx), got[0].RunID)
	if err != nil || rowString(run, "status") != runStatusCompiling || rowString(run, "triggeredBy") != "pipeline-security" || len(rowMap(run, "spine")) == 0 {
		t.Fatalf("goal did not enter the existing planning workflow: %v %v", run, err)
	}
	g.Input = map[string]any{"sha": "next-week", "finding": "stable"}
	again, err := b.i.OpenPipelineSecurityGoal(ctx, "finding-1", g)
	if err != nil || again != got[0] {
		t.Fatalf("weekly repeat duplicated goal: %v %v", again, err)
	}
	goal, err := a.i.store().goalForOwner(memql.ContextWithFreshRead(ctx), got[0].GoalID)
	if err != nil || rowMap(goal, "input")["sha"] != "original" {
		t.Fatalf("original evidence changed: %v %v", goal, err)
	}
	for concept, id := range map[string]string{goalConcept: got[0].GoalID, runConcept: got[0].RunID} {
		count, err := a.db.NewSelect().TableExpr(`"MemoryNodes"`).Where("concept = ?", concept).Where("id IN (?, ?)", id, concept+":"+id).Count(ctx)
		if err != nil || count != 1 {
			t.Fatalf("%s duplicate writes: %d %v", concept, count, err)
		}
	}
	enabled = false
	if _, err := a.i.OpenPipelineSecurityGoal(ctx, "new-finding-off", g); err == nil {
		t.Fatal("disabled scan created work")
	}
	if _, err := a.i.OpenPipelineSecurityGoal(actorCtx("another-owner"), "other", g); err == nil {
		t.Fatal("another owner created the goal")
	}
	enabled = true
	if err := a.i.store().writeInternal(ctx, "mutation "+call("updateWorkGoal", map[string]any{"goalId": got[0].GoalID, "status": "closed"})); err != nil {
		t.Fatal(err)
	}
	if _, err := b.i.OpenPipelineSecurityGoal(ctx, "finding-1", g); err != nil {
		t.Fatal(err)
	}
	goal, err = b.i.store().goalForOwner(memql.ContextWithFreshRead(ctx), got[0].GoalID)
	if err != nil || rowString(goal, "status") != "closed" {
		t.Fatal("repeat scan reopened closed work", goal, err)
	}
}

func TestPipelineSecurityGoalRecoversPartialBootstrapWithOriginalInput(t *testing.T) {
	a, b := openActsDB(t), openActsDB(t)
	a.i.EnableCompileViaEvent()
	b.i.EnableCompileViaEvent()
	ctx := actorCtx(a.owner)
	g := DirectGoal{OwnerUserId: a.owner, Statement: "Resolve the verified finding", Input: map[string]any{"sha": "first"}, BeforeRun: func(context.Context, string, string) error { return nil }}
	original := a.i.engine
	a.i.engine = reviewGoalWriteFailure{original, "createWorkRun"}
	first, err := a.i.OpenPipelineSecurityGoal(ctx, "partial", g)
	a.i.engine = original
	if err == nil {
		t.Fatal("injected run failure was ignored")
	}
	g.Input = map[string]any{"sha": "later"}
	second, err := b.i.OpenPipelineSecurityGoal(ctx, "partial", g)
	if err != nil || second != first {
		t.Fatal("partial request did not recover", first, second, err)
	}
	run, err := b.i.store().runForOwner(memql.ContextWithFreshRead(ctx), second.RunID)
	if err != nil || rowMap(run, "input")["sha"] != "first" {
		t.Fatal("recovery replaced original evidence", run, err)
	}
}

func TestPipelineSecurityGoalRechecksGateAfterSourceValidation(t *testing.T) {
	a := openActsDB(t)
	a.i.EnableCompileViaEvent()
	ctx := actorCtx(a.owner)
	receipt, err := a.i.OpenPipelineSecurityGoal(ctx, "gate-lost-during-source-read", DirectGoal{
		OwnerUserId: a.owner, Statement: "Resolve the verified finding",
		BeforeRun: func(c context.Context, goalID, _ string) error {
			// Terminate only this test request's exact advisory-lock session,
			// reproducing provider latency across a gate connection loss.
			var terminated bool
			err := a.db.DB.QueryRowContext(c, `SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype = 'advisory' AND granted
				AND objsubid = 2 AND classid = 1296387409::oid AND objid = hashtext($1)::oid`, "work-security-goal:"+goalID).Scan(&terminated)
			if err != nil {
				return err
			}
			if !terminated {
				t.Fatal("test request's gate session was not terminated")
			}
			return nil
		},
	})
	if !errors.Is(err, memql.ErrWriteGateLost) {
		t.Fatalf("lost gate answered %v", err)
	}
	goal, readErr := a.i.store().goalForOwner(memql.ContextWithFreshRead(ctx), receipt.GoalID)
	if readErr != nil || goal != nil {
		t.Fatal("goal written after serialization authority ended", goal, readErr)
	}
}
