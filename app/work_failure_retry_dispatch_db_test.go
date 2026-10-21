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
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/id"
	work "github.com/znasllc-io/memql/integrations/work"
)

type failureRetryDispatcher struct {
	*workRunDispatcher
	auto     *automations.Automation
	executor *automations.Executor
	done     chan error
}

func (d *failureRetryDispatcher) Dispatch(ctx context.Context, req work.DispatchRequest) {
	j, err := automations.LoadRunJournal(memql.ContextWithFreshRead(ctx), d.app.engine, req.RunId)
	if err == nil && (!workRunCanStart(req, j, time.Now()) || !workRerunServable(req, j)) {
		err = fmt.Errorf("claimed retry was not admitted")
	}
	if err == nil {
		var resumed *automations.RunJournal
		var options *automations.ResumeOptions
		resumed, options, _, err = workRerunResumption(j, nil, d.auto)
		if err == nil {
			_, err = d.executor.ResumeFrom(ctx, resumed, d.auto, options)
		}
	}
	d.done <- err
}

// Use the real public decision, peer readiness/claim, journal and executor.
// The old readiness check consumed neither dispatch path: it waited for a
// waiting step even though the failure question deliberately leaves it failed.
func TestWorkDBFailureRetryDispatchesOnceAndKeepsCompletedPrefix(t *testing.T) {
	writer, db := workTemplateDBEngineAndDB(t)
	reader := workTemplateDBEngine(t)
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	require.NoError(t, err)
	defer gates.Close()
	for _, recovery := range []bool{false, true} {
		t.Run(fmt.Sprintf("missedEvent=%v", recovery), func(t *testing.T) {
			key := id.NewShortId()
			owner, runID, goalID, approval := key+"owner", key+"run", key+"goal", key+"approval"
			person := auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), owner), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
			write := func(name string, args map[string]any) {
				templateMutation(t, writer, auth.ContextWithInternalOrigin(person), name, args)
			}
			auto, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(`@template automation retryFixture {
  prefix := builtin savedEffect()
  deliver := builtin deliverFile(input: prefix)
  return deliver
}`, "failure-retry.memql")
			require.NoError(t, err)
			now := time.Now().UTC()
			stamp := now.Format(time.RFC3339Nano)
			write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Synthetic failed-run Retry", "origin": "user"})
			write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": auto.Name, "templateFingerprint": auto.DefinitionFingerprint(id.New()), "status": "waiting", "mode": "live", "startedAt": stamp, "waitingOn": map[string]any{"kind": "approval", "subject": approval}})
			write("createWorkStep", map[string]any{"stepId": runID + "-prefix", "runId": runID, "key": "prefix", "seq": 0, "stepType": "function", "status": "done", "attempt": 1, "version": 1, "startedAt": stamp, "result": map[string]any{"stepId": "prefix", "status": "completed", "value": "already-saved-file"}})
			write("createWorkStep", map[string]any{"stepId": runID + "-deliver", "runId": runID, "key": "deliver", "seq": 1, "stepType": "function", "status": "failed", "attempt": 1, "version": 1, "startedAt": stamp, "errorMessage": "fixture delivery unavailable"})
			write("updateWorkRun", map[string]any{"runId": runID, "stepOrder": []string{"prefix", "deliver"}, "head": workstate.Head{"prefix": {Version: 1}, "deliver": {Version: 1}}.Object(), "spent": map[string]any{"retries": 2, "modelCalls": 4}})
			question := workstate.FailureApproval(workstate.ApprovalKindFeedback, runID, "deliver", workstate.SymptomHuman, "fixture delivery unavailable", "Retry delivery?", workstate.Evidence{}, now, time.Hour)
			write("createWorkApproval", map[string]any{"approvalId": approval, "runId": runID, "stepKey": "deliver", "kind": question.Kind, "subject": question.Subject, "artifactHash": question.ArtifactHash, "question": question.Question, "options": question.Options, "requestedAt": stamp, "expiresAt": now.Add(time.Hour).Format(time.RFC3339Nano)})
			done := make(chan error, 4)
			ran := &stepsRan{}
			var peers []*work.Integration
			for _, engine := range []*memql.MemQLEngine{writer, reader} {
				var peer *work.Integration
				for _, plugin := range memql.RegisteredPlugins() {
					if plugin.Name == "work" {
						provider, err := plugin.Factory(memql.PluginContext{Engine: engine, Logger: engine.Logger, BunDB: func() *bun.DB { return db }, GateDB: func() *sql.DB { return gates }, AdmitSourceRow: memql.AdmitSourceRow})
						require.NoError(t, err)
						peer = provider.(*work.Integration)
					}
				}
				require.NotNil(t, peer)
				executor := automations.NewExecutor(automations.ExecutorOptions{Engine: engine, StepRegistry: ran})
				t.Cleanup(executor.Close)
				peer.SetDispatcher(&failureRetryDispatcher{workRunDispatcher: &workRunDispatcher{app: &App{engine: engine, Logger: engine.Logger}}, auto: auto, executor: executor, done: done})
				peer.SetRunClaimer(automations.NewClusterExecutionGuard(func() *bun.DB { return db }, engine.Logger).StrictClaimer())
				peers = append(peers, peer)
			}
			claimer := automations.NewClusterExecutionGuard(func() *bun.DB { return db }, writer.Logger).StrictClaimer()
			require.True(t, claimer.ClaimWithTTL(person, "work.run.dispatch", runID, 4*time.Minute), "the old run lease is still held")
			decided := false
			for _, capability := range peers[1].Capabilities() {
				if capability.Name == "decideApproval" {
					_, err := capability.Handler(person, map[string]any{"approvalId": approval, "decision": "answered", "answer": map[string]any{"value": workstate.FailureAnswerRetry}}, 0)
					require.NoError(t, err)
					decided = true
				}
			}
			require.True(t, decided)
			j, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, runID)
			require.NoError(t, err)
			require.Equal(t, "failed", j.StepStates["deliver"].Status)
			require.Equal(t, approval, j.HumanResumeId)
			require.NotNil(t, j.Rerun)
			require.Equal(t, 2, j.Rerun.Versions["deliver"])
			event := events.Event{Payload: map[string]any{"id": runID, "payload": map[string]any{"status": "running", "goalId": goalID, "ownerUserId": owner, "automationName": auto.Name, "humanResumeId": approval, "rerun": map[string]any{"requestId": j.Rerun.RequestId}}}}
			if recovery {
				require.True(t, peers[0].DispatchRun(person, runID, owner), "the sweep must recover the unspent answer while the old run lease remains held")
			} else {
				for range 2 {
					for _, peer := range peers {
						peer.HandleRunEvent(event)
					}
				}
			}
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(4 * time.Second):
				t.Fatal("Retry was never dispatched from the failed step")
			}
			for _, peer := range peers {
				peer.HandleRunEvent(event)
			}
			select {
			case err := <-done:
				t.Fatalf("duplicate retry dispatched: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			ran.mu.Lock()
			require.Equal(t, []string{"deliver"}, ran.ran)
			ran.mu.Unlock()
			j, err = automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, runID)
			require.NoError(t, err)
			require.Equal(t, "already-saved-file", j.Steps["prefix"].Value)
			require.Equal(t, 1, j.StepStates["prefix"].Version)
			require.Equal(t, 2, j.StepStates["deliver"].Version)
			require.Equal(t, "done", j.StepStates["deliver"].Status)
		})
	}
}
