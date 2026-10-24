//go:build agent

package app

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	work "github.com/znasllc-io/memql/integrations/work"
)

type inferenceRecoveryDispatcher struct {
	*workRunDispatcher
	auto     *automations.Automation
	executor *automations.Executor
	done     chan error
}

func (d *inferenceRecoveryDispatcher) Dispatch(ctx context.Context, req work.DispatchRequest) {
	j, err := automations.LoadRunJournal(memql.ContextWithFreshRead(ctx), d.app.engine, req.RunId)
	if err == nil && (!workRunCanStart(req, j, time.Now()) || !workRerunServable(req, j)) {
		err = fmt.Errorf("claimed availability wait refused")
	}
	if err == nil {
		resume, opts, _, e := workRerunResumption(j, nil, d.auto)
		err = e
		if resume == nil {
			resume = j
		}
		if err == nil {
			_, err = d.executor.ResumeFrom(ctx, resume, d.auto, opts)
		}
	}
	d.done <- err
}

// A non-agent leader reannounces a wait written before the agents started.
// Two fresh engines race on the real Postgres claim and executor fence. The
// old once-only rerun claim and bare-run lease are both still held; recovery
// must preserve that saved request and execute only the unfinished effect.
func TestWorkDBInferenceWaitRecoversAfterRestartWithoutReplayingPrefix(t *testing.T) {
	writer, db := workTemplateDBEngineAndDB(t)
	reader := workTemplateDBEngine(t)
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	require.NoError(t, err)
	defer gates.Close()
	newPeer := func(engine *memql.MemQLEngine) *work.Integration {
		for _, plugin := range memql.RegisteredPlugins() {
			if plugin.Name == "work" {
				provider, e := plugin.Factory(memql.PluginContext{Engine: engine, Logger: engine.Logger, BunDB: func() *bun.DB { return db }, GateDB: func() *sql.DB { return gates }, AdmitSourceRow: memql.AdmitSourceRow})
				require.NoError(t, e)
				return provider.(*work.Integration)
			}
		}
		t.Fatal("no work integration")
		return nil
	}
	for _, rerun := range []bool{false, true} {
		t.Run(fmt.Sprintf("savedRerun=%v", rerun), func(t *testing.T) {
			key := id.NewShortId()
			owner, runID, goalID, approval := key+"owner", key+"run", key+"goal", key+"approval"
			person := auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), owner), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
			write := func(name string, args map[string]any) {
				templateMutation(t, writer, auth.ContextWithInternalOrigin(person), name, args)
			}
			auto, e := automations.NewLoader(automations.LoaderOptions{}).CompileSource(`@template automation inferenceFixture {
  prefix := builtin savedEffect()
  deliver := builtin deliverFile(input: prefix)
  return deliver
}`, "inference-recovery.memql")
			require.NoError(t, e)
			now := time.Now().UTC()
			stamp := now.Add(-10 * time.Minute).Format(time.RFC3339Nano)
			wait := map[string]any{"kind": "approval", "approvalKind": "inferenceUnavailable", "subject": approval, "since": stamp, "resumeAt": now.Add(-2 * time.Minute).Format(time.RFC3339Nano)}
			write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Synthetic availability recovery", "origin": "user"})
			write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": auto.Name, "templateFingerprint": auto.DefinitionFingerprint(id.New()), "status": "waiting", "mode": "live", "startedAt": stamp, "waitingOn": wait})
			write("createWorkStep", map[string]any{"stepId": runID + "-prefix", "runId": runID, "key": "prefix", "seq": 0, "stepType": "function", "status": "done", "attempt": 1, "version": 1, "startedAt": stamp, "result": map[string]any{"stepId": "prefix", "status": "completed", "value": "saved-effect"}})
			write("createWorkStep", map[string]any{"stepId": runID + "-deliver", "runId": runID, "key": "deliver", "seq": 1, "stepType": "function", "status": "failed", "attempt": 1, "version": 1, "startedAt": stamp, "errorMessage": "every_door_shut"})
			update := map[string]any{"runId": runID, "stepOrder": []string{"prefix", "deliver"}, "spent": map[string]any{"retries": 2, "modelCalls": 4}, "classification": map[string]any{"progressContinuation": map[string]any{"extensions": 3}}}
			requestID := key + "request"
			if rerun {
				update["rerun"] = map[string]any{"requestId": requestID, "reason": "rerun", "stepKey": "deliver", "fromStepKey": "deliver", "requestedAt": stamp, "versions": map[string]any{"deliver": 2}}
			}
			write("updateWorkRun", update)
			write("createWorkApproval", map[string]any{"approvalId": approval, "runId": runID, "stepKey": "deliver", "kind": "inferenceUnavailable", "subject": map[string]any{"code": "every_door_shut"}, "artifactHash": "fixture", "question": "Use paid provider?", "requestedAt": stamp, "expiresAt": now.Add(time.Hour).Format(time.RFC3339Nano)})
			leader := newPeer(writer)
			result, e := leader.SweepWaiting(auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("sweepWaitingWorkRuns")), time.Minute)
			require.NoError(t, e)
			require.GreaterOrEqual(t, result.Redispatched, 1)
			j, e := automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, runID)
			require.NoError(t, e)
			require.Equal(t, "waiting", j.Status)
			require.True(t, j.HeartbeatAt.After(now.Add(-time.Second)))
			claimer := automations.NewClusterExecutionGuard(func() *bun.DB { return db }, writer.Logger).StrictClaimer()
			require.True(t, claimer.ClaimWithTTL(person, "work.run.dispatch", runID, 4*time.Minute))
			require.True(t, claimer.ClaimWithTTL(person, "work.run.dispatch", runID+"#rerun:"+requestID, 0))
			done := make(chan error, 4)
			ran := &stepsRan{}
			var peers []*work.Integration
			for _, engine := range []*memql.MemQLEngine{writer, reader} {
				peer := newPeer(engine)
				executor := automations.NewExecutor(automations.ExecutorOptions{Engine: engine, StepRegistry: ran})
				t.Cleanup(executor.Close)
				peer.SetDispatcher(&inferenceRecoveryDispatcher{workRunDispatcher: &workRunDispatcher{app: &App{engine: engine, Logger: engine.Logger}}, auto: auto, executor: executor, done: done})
				peer.SetRunClaimer(automations.NewClusterExecutionGuard(func() *bun.DB { return db }, engine.Logger).StrictClaimer())
				peers = append(peers, peer)
			}
			ev := events.Event{Payload: map[string]any{"id": runID, "payload": map[string]any{"status": "waiting", "ownerUserId": owner, "goalId": goalID, "automationName": auto.Name, "waitingOn": wait, "rerun": map[string]any{"requestId": requestID}}}}
			foreign := events.Event{Payload: map[string]any{"id": runID, "payload": map[string]any{"status": "waiting", "ownerUserId": "other-owner", "goalId": goalID, "automationName": auto.Name, "waitingOn": wait}}}
			peers[0].HandleRunEvent(foreign)
			select {
			case e := <-done:
				t.Fatalf("foreign owner dispatched: %v", e)
			case <-time.After(100 * time.Millisecond):
			}
			for range 2 {
				for _, peer := range peers {
					peer.HandleRunEvent(ev)
				}
			}
			select {
			case e := <-done:
				if rerun {
					require.NoError(t, e)
				} else {
					// Availability alone cannot authorize repeating an
					// unclassified effect with an uncertain prior outcome.
					require.ErrorIs(t, e, automations.ErrNonRetryableStep)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("restored subscription never resumed")
			}
			for _, peer := range peers {
				peer.HandleRunEvent(ev)
			}
			select {
			case e := <-done:
				t.Fatalf("duplicate execution: %v", e)
			case <-time.After(100 * time.Millisecond):
			}
			ran.mu.Lock()
			if rerun {
				require.Equal(t, []string{"deliver"}, ran.ran)
			} else {
				require.Empty(t, ran.ran)
			}
			ran.mu.Unlock()
			j, e = automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, runID)
			require.NoError(t, e)
			require.Equal(t, "saved-effect", j.Steps["prefix"].Value)
			require.Equal(t, 1, j.StepStates["prefix"].Version)
			if rerun {
				require.Equal(t, "done", j.StepStates["deliver"].Status)
			}
			var saved struct{ Payload map[string]any }
			require.NoError(t, db.NewRaw(`SELECT payload FROM "MemoryNodes" WHERE id = ? ORDER BY "createdAt" DESC LIMIT 1`, "v1:work:run:"+runID).Scan(context.Background(), &saved))
			require.EqualValues(t, 4, saved.Payload["spent"].(map[string]any)["modelCalls"])
			require.EqualValues(t, 3, saved.Payload["classification"].(map[string]any)["progressContinuation"].(map[string]any)["extensions"])
		})
	}
}
