package work

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

type budgetResumeFailureEngine struct {
	Engine
	fail atomic.Bool
}

func (e *budgetResumeFailureEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(query, "mutation updateWorkRun(") && e.fail.CompareAndSwap(true, false) {
		return nil, fmt.Errorf("synthetic run release failure after budget decision")
	}
	return e.Engine.Execute(ctx, query)
}

func TestBudgetGuidanceDBRecoversApprovedCheckpointAfterRunReleaseFailure(t *testing.T) {
	a, b := openActsDB(t), openActsDB(t)
	b.owner = a.owner
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	person := actorCtx(a.owner)
	runID := a.openRun(t, nil)
	goalID := rowString(a.runRow(t, runID), "goalId")
	a.writeVersion(t, runID, runID, "fetch", 0, 1, nil, "completed evidence", nil)
	a.writeVersion(t, runID, runID, "draft", 1, 1, nil, "", map[string]any{"status": "waiting"})
	a.write(t, "updateWorkGoal", map[string]any{"goalId": goalID, "ceilings": map[string]any{"maxModelCalls": 12, "maxRetries": 2}})
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "stepOrder": []string{"fetch", "draft"}, "spent": map[string]any{"modelCalls": 12, "retries": 1}, "rerun": map[string]any{"requestId": "existing-generation"}})
	rc := common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: a.owner, StepKey: "draft", Mode: common.RunModeLive, RerunRequestId: "existing-generation"}
	runCtx := common.ContextWithRun(person, rc)
	messages := []common.ChatMessage{{Role: "system", Content: "Existing authority"}, {Role: "user", Content: "Original request"},
		{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "read-once", Name: "readSource", Arguments: `{}`}}},
		{Role: "tool", ToolCallId: "read-once", Name: "readSource", Content: "Completed evidence"}}
	require.NoError(t, a.eng.SaveWorkContinuation(runCtx, messages))
	checkpoint := a.query(t, a.owner, "query "+call("workContinuationForOwnerRun", map[string]any{"runId": runID, "stepKey": "draft"}))[0]
	subject := map[string]any{"ceiling": "modelCalls", "limit": "12", "actual": "12", "resumeKind": "checkpoint", "continuationId": rowString(checkpoint, "id")}
	approvalID := newRowId(approvalConcept)
	a.write(t, "createWorkApproval", map[string]any{"approvalId": approvalID, "runId": runID, "kind": "budget", "stepKey": "draft", "subject": subject, "artifactHash": artifactHashOf(subject), "requestedAt": rfc(time.Now())})
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": approvalID}})
	before := a.runRow(t, runID)
	_, err := a.i.handleDecideApproval(person, map[string]any{"approvalId": approvalID, "decision": "answered", "answer": map[string]any{"feedback": "Use the completed evidence without repeating it."}}, 0)
	require.NoError(t, err)
	wrapper := &budgetResumeFailureEngine{Engine: a.eng}
	wrapper.fail.Store(true)
	a.i.engine = wrapper
	args := map[string]any{"approvalId": approvalID, "decision": "approved", "answer": map[string]any{"newLimit": 15}}
	nodes, err := a.i.handleDecideApproval(person, args, 0)
	require.NoError(t, err)
	require.Equal(t, false, decodeReply(t, nodes)["runResumed"])
	require.Contains(t, decodeReply(t, nodes)["resumeError"], "synthetic run release failure")

	readApproval := func() map[string]any {
		t.Helper()
		rows, err := b.i.store().query(memql.ContextWithFreshRead(person), "query "+call("workApprovalForOwner", map[string]any{"approvalId": approvalID}))
		require.NoError(t, err)
		require.Len(t, rows, 1)
		return rows[0]
	}
	accepted := readApproval()
	require.Equal(t, "approved", rowString(accepted, "decision"))
	require.Equal(t, "Use the completed evidence without repeating it.", rowString(rowMap(accepted, "answer"), "feedback"))
	goal, err := b.i.store().goalForOwner(memql.ContextWithFreshRead(person), goalID)
	require.NoError(t, err)
	require.Equal(t, 15, rowInt(rowMap(goal, "ceilings"), "maxModelCalls"))
	rowCount := func(id string) int {
		t.Helper()
		var count int
		require.NoError(t, a.db.NewRaw(`SELECT count(*) FROM "MemoryNodes" WHERE id = ?`, id).Scan(person, &count))
		return count
	}
	approvalVersions, goalVersions := rowCount(approvalID), rowCount(goalID)
	refuses := func(ctx context.Context, attempt map[string]any) {
		t.Helper()
		versions := rowCount(runID)
		nodes, err := b.i.handleDecideApproval(ctx, attempt, 0)
		if err == nil {
			require.Equal(t, false, decodeReply(t, nodes)["runResumed"])
			require.NotEmpty(t, decodeReply(t, nodes)["resumeError"])
		}
		require.Equal(t, versions, rowCount(runID), "an invalid replay must not release or rewrite the run")
	}
	refuses(actorCtx("different-person"), args)
	refuses(person, map[string]any{"approvalId": approvalID, "decision": "approved", "answer": map[string]any{"newLimit": 16}})
	refuses(person, map[string]any{"approvalId": approvalID, "decision": "approved", "answer": map[string]any{"newLimit": 15, "feedback": "Changed browser text"}})
	// A different latest native checkpoint cannot borrow the accepted answer.
	require.NoError(t, a.eng.SaveWorkContinuation(runCtx, append(messages, common.ChatMessage{Role: "user", Content: "Changed checkpoint"})))
	refuses(person, args)
	require.NoError(t, a.eng.SaveWorkContinuation(runCtx, messages))
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "cancelRequested": true})
	refuses(person, args)
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "cancelRequested": false})
	gate := b.i.computerDecisionGate
	b.i.computerDecisionGate = func(context.Context, string) (func(), func(context.Context) error, error) {
		return func() {}, func(context.Context) error { return fmt.Errorf("receipt gate lost") }, nil
	}
	refuses(person, args)
	b.i.computerDecisionGate = gate

	// Both replicas retry the same durable decision, with no repeat raise or
	// decision write. One releases it; the other acknowledges the same receipt.
	runVersions := rowCount(runID)
	var wg sync.WaitGroup
	results := make([]map[string]any, 2)
	errs := make([]error, 2)
	for n, peer := range []*Integration{a.i, b.i} {
		wg.Add(1)
		go func(n int, peer *Integration) {
			defer wg.Done()
			nodes, err := peer.handleDecideApproval(person, args, 0)
			errs[n] = err
			if err == nil {
				results[n] = decodeReply(t, nodes)
			}
		}(n, peer)
	}
	wg.Wait()
	for n := range results {
		require.NoError(t, errs[n])
		require.Equal(t, true, results[n]["runResumed"])
	}
	require.Equal(t, runVersions+1, rowCount(runID))
	require.Equal(t, approvalVersions, rowCount(approvalID))
	require.Equal(t, goalVersions, rowCount(goalID))
	require.Equal(t, accepted, readApproval(), "recovery preserves the exact durable decision and guidance")
	run, err := b.i.store().runForOwner(memql.ContextWithFreshRead(person), runID)
	require.NoError(t, err)
	require.Equal(t, "running", rowString(run, "status"))
	for _, field := range []string{"spent", "rerun", "head", "stepOrder"} {
		require.Equal(t, before[field], run[field], field)
	}
	// The native executor on the other replica accepts precisely that receipt.
	_, err = b.eng.PrepareWorkContinuation(runCtx, memql.CheckpointResumeRequest{RunID: runID, StepKey: rc.StepKey, ApprovalID: approvalID, RerunRequestID: rc.RerunRequestId})
	require.NoError(t, err)
}
