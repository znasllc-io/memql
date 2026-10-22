package work

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/agent"
)

func TestProgressDBWorkflowAcrossReplicas(t *testing.T) {
	a, b := openActsDB(t), openActsDB(t)
	cases := []struct {
		name string
		run  func(*testing.T, actsDB, actsDB)
	}{
		{"AutomaticWindows", testProgressDBAutomaticWindows},
		{"SavedChoiceRecovery", testProgressDBRecoversCheckpointAfterRunReleaseFailure},
		{"HardUsageObservation", testModelCallBudgetDBSeesPeerRaisesAndReceiptsBeforeHardExhaustion},
		{"GuidanceAndHardStop", testProgressDBGuidanceAndHardStop},
		{"InterruptedDecisions", testProgressDBInterruptedDecisions},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { first, peer := freshActsDBPair(t, a, b); tc.run(t, first, peer) })
	}
}
func testProgressDBAutomaticWindows(t *testing.T, a, b actsDB) {
	for _, peer := range []actsDB{a, b} {
		require.NoError(t, peer.eng.RegisterIntegration(peer.i))
	}
	runID := a.openRun(t, nil)
	goalID := rowString(a.runRow(t, runID), "goalId")
	a.write(t, "updateWorkGoal", map[string]any{"goalId": goalID, "ceilings": map[string]any{"maxModelCalls": 96, "wallClockMs": 7200000, "costCeiling": 0.5}})
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "classification": map[string]any{"workload": "lookup"}, "spent": map[string]any{"retries": 1}})
	ctx := actorCtx(a.owner)
	rc := common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: a.owner, StepKey: "reason", Mode: common.RunModeLive}
	before := a.query(t, a.owner, "query "+call("workGoalForOwner", map[string]any{"goalId": goalID}))[0]
	// Warm both replicas before their competing read/modify/write calls.
	for _, peer := range []actsDB{a, b} {
		_, err := peer.i.store().runForOwner(ctx, runID)
		require.NoError(t, err)
	}
	journal := NewModelJournal(a.eng)
	for n := 0; n < 12; n++ {
		require.NoError(t, journal.Record(ctx, a.owner, memql.JournaledCall{ReceiptID: fmt.Sprintf("%s-call-%d", memql.BareShortId(runID), n), RunId: runID, StepKey: "reason", RequestHash: fmt.Sprint(n), Served: "local"}))
	}
	guard := NewRunCeilings(b.eng, testLogger())
	require.NotNil(t, guard.Admit(ctx, rc, 0), "no tool progress must pause at the estimate")
	for n := 1; n <= 5; n++ {
		require.NoError(t, a.i.RecordToolInvocation(ctx, agent.ToolInvocation{RunId: runID, OwnerUserId: a.owner, ToolName: "readSource", Args: map[string]any{"id": fmt.Sprint(n)}}))
		if n == 1 {
			require.Nil(t, guard.Admit(ctx, rc, 0), "the installed admission seam must extend after new completed work")
		}
		var wg sync.WaitGroup
		results := make([]bool, 2)
		errs := make([]error, 2)
		for p, peer := range []*Integration{a.i, b.i} {
			wg.Add(1)
			go func(p int, peer *Integration) {
				defer wg.Done()
				results[p], errs[p] = peer.ContinueWorkProgress(ctx, rc, workstate.Spent{ModelCalls: 12 * n}, 0)
			}(p, peer)
		}
		wg.Wait()
		for p := range results {
			require.NoError(t, errs[p])
			require.True(t, results[p])
		}
		saved := b.runRow(t, runID)
		state := rowMap(rowMap(saved, "classification"), "progressContinuation")
		require.Equal(t, n, rowInt(state, "extensions"))
		require.Equal(t, 1, rowInt(rowMap(saved, "spent"), "retries"))
		// A replayed completed read (different observation id, identical arguments)
		// is not permission for another window.
		require.NoError(t, b.i.RecordToolInvocation(ctx, agent.ToolInvocation{RunId: runID, OwnerUserId: a.owner, ToolName: "readSource", Args: map[string]any{"id": fmt.Sprint(n)}}))
		continued, err := b.i.ContinueWorkProgress(ctx, rc, workstate.Spent{ModelCalls: 12 * (n + 1)}, 0)
		require.NoError(t, err)
		require.False(t, continued)
	}
	require.NoError(t, a.i.RecordToolInvocation(ctx, agent.ToolInvocation{RunId: runID, OwnerUserId: a.owner, ToolName: "readSource", Args: map[string]any{"id": "sixth"}}))
	continued, err := b.i.ContinueWorkProgress(ctx, rc, workstate.Spent{ModelCalls: 72}, 0)
	require.NoError(t, err)
	require.True(t, continued)
	// The actual admission seam on a new replica reads the durable allowance.
	for n := 12; n < 84; n++ {
		require.NoError(t, journal.Record(ctx, a.owner, memql.JournaledCall{ReceiptID: fmt.Sprintf("%s-call-%d", memql.BareShortId(runID), n), RunId: runID, StepKey: "reason", RequestHash: fmt.Sprint(n), Served: "local"}))
	}
	guard = NewRunCeilings(b.eng, testLogger())
	breach := guard.Admit(ctx, rc, 0)
	require.NotNil(t, breach)
	require.Equal(t, workstate.CeilingModelCalls, breach.Ceiling)
	require.Equal(t, 84, guard.Spent(ctx, rc).ModelCalls)
	require.Equal(t, before, b.query(t, a.owner, "query "+call("workGoalForOwner", map[string]any{"goalId": goalID}))[0])
	require.Empty(t, a.query(t, a.owner, "query "+call("workApprovalsForOwnedRun", map[string]any{"runId": runID})))
	// A foreign owner cannot acquire another person's progress or extension.
	rc.OwnerUserId = "different-owner"
	ok, err := b.i.ContinueWorkProgress(context.Background(), rc, workstate.Spent{ModelCalls: 48}, 0)
	require.False(t, ok)
	_ = err
}
func testProgressDBGuidanceAndHardStop(t *testing.T, a, b actsDB) {
	for _, hard := range []bool{false, true} {
		t.Run(fmt.Sprint(hard), func(t *testing.T) {
			ctx := actorCtx(a.owner)
			runID := a.openRun(t, nil)
			goalID := rowString(a.runRow(t, runID), "goalId")
			cap := 96
			if hard {
				cap = 11
			}
			a.write(t, "updateWorkGoal", map[string]any{"goalId": goalID, "ceilings": map[string]any{"maxModelCalls": 96, "wallClockMs": 7200000, "tokenBudget": cap}})
			a.writeVersion(t, runID, runID, "reason", 0, 1, nil, "", map[string]any{"status": "waiting"})
			a.write(t, "updateWorkRun", map[string]any{"runId": runID, "stepOrder": []string{"reason"}, "classification": map[string]any{"workload": "lookup"}, "spent": map[string]any{"modelCalls": 12, "tokens": 12, "retries": 1}})
			rc := common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: a.owner, StepKey: "reason", Mode: common.RunModeLive}
			runCtx := common.ContextWithRun(ctx, rc)
			require.NoError(t, a.eng.SaveWorkContinuation(runCtx, []common.ChatMessage{{Role: "user", Content: "Use the source"}, {Role: "assistant", Content: "Saved evidence"}}))
			checkpoint := a.query(t, a.owner, "query "+call("workContinuationForOwnerRun", map[string]any{"runId": runID, "stepKey": "reason"}))[0]
			approvalID, err := a.i.PauseWorkForBudget(runCtx, &memql.RunCeilingError{RunId: runID, StepKey: "reason", Breach: memql.RunCeilingBreach{Ceiling: "modelCalls", Limit: "12", Actual: "12"}, Spent: memql.RunSpend{ModelCalls: 12, Tokens: 12}}, rowString(checkpoint, "id"))
			require.NoError(t, err)
			rows := b.query(t, a.owner, "query "+call("workApprovalForOwner", map[string]any{"approvalId": approvalID}))
			require.Len(t, rows, 1)
			require.Equal(t, "feedback", rowString(rows[0], "kind"))
			require.Equal(t, hard, rowMap(rows[0], "subject")["continuationAllowed"] == false)
			answer := map[string]any{"text": "Use the saved source and focus on the conclusion."}
			if hard {
				answer = map[string]any{"value": "abandon"}
			}
			nodes, err := b.i.handleDecideApproval(ctx, map[string]any{"approvalId": approvalID, "decision": "answered", "answer": answer}, 0)
			require.NoError(t, err)
			after := a.runRow(t, runID)
			require.Equal(t, 12, rowInt(rowMap(after, "spent"), "modelCalls"))
			require.Equal(t, 1, rowInt(rowMap(after, "spent"), "retries"))
			if hard {
				require.Equal(t, runStatusFailed, rowString(after, "status"))
				require.Equal(t, true, decodeReply(t, nodes)["runStopped"])
			} else {
				require.Equal(t, runStatusRunning, rowString(after, "status"))
				require.Equal(t, 1, rowInt(rowMap(rowMap(after, "classification"), "progressContinuation"), "extensions"))
				_, err = b.eng.PrepareWorkContinuation(runCtx, memql.CheckpointResumeRequest{RunID: runID, StepKey: "reason", ApprovalID: approvalID})
				require.NoError(t, err)
			}
		})
	}
}

func testProgressDBInterruptedDecisions(t *testing.T, a, b actsDB) {
	for _, scenario := range []string{"guidance", "stop", "hard-before-answer", "hard-after-answer"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := actorCtx(a.owner)
			runID := a.openRun(t, nil)
			goalID := rowString(a.runRow(t, runID), "goalId")
			setCap := func(cap int) {
				a.write(t, "updateWorkGoal", map[string]any{"goalId": goalID, "ceilings": map[string]any{"maxModelCalls": 96, "wallClockMs": 7200000, "tokenBudget": cap}})
			}
			setCap(96)
			a.writeVersion(t, runID, runID, "reason", 0, 1, nil, "", map[string]any{"status": "waiting"})
			a.write(t, "updateWorkRun", map[string]any{"runId": runID, "stepOrder": []string{"reason"}, "classification": map[string]any{"workload": "lookup"}, "spent": map[string]any{"modelCalls": 12, "tokens": 12}})
			rc := common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: a.owner, StepKey: "reason", Mode: common.RunModeLive}
			runCtx := common.ContextWithRun(ctx, rc)
			require.NoError(t, a.eng.SaveWorkContinuation(runCtx, []common.ChatMessage{{Role: "user", Content: "Use the saved evidence."}}))
			checkpoint := a.query(t, a.owner, "query "+call("workContinuationForOwnerRun", map[string]any{"runId": runID, "stepKey": "reason"}))[0]
			approvalID, err := a.i.PauseWorkForBudget(runCtx, &memql.RunCeilingError{RunId: runID, StepKey: "reason", Breach: memql.RunCeilingBreach{Ceiling: "modelCalls", Limit: "12", Actual: "12"}, Spent: memql.RunSpend{ModelCalls: 12, Tokens: 12}}, rowString(checkpoint, "id"))
			require.NoError(t, err)
			answer := map[string]any{"value": "retry"}
			if scenario == "guidance" {
				answer = map[string]any{"text": "Use the completed source and finish the conclusion."}
			}
			if scenario == "stop" {
				answer = map[string]any{"value": "abandon"}
			}
			if scenario == "hard-before-answer" {
				setCap(11)
			}
			wrapper := &budgetResumeFailureEngine{Engine: a.eng}
			wrapper.fail.Store(true)
			a.i.engine = wrapper
			args := map[string]any{"approvalId": approvalID, "decision": "answered", "answer": answer}
			nodes, err := a.i.handleDecideApproval(ctx, args, 0)
			require.NoError(t, err)
			require.Contains(t, decodeReply(t, nodes)["resumeError"], "synthetic run release failure")
			before := a.runRow(t, runID)
			if scenario == "hard-after-answer" {
				setCap(11)
			}
			for n := 0; n < 2; n++ {
				nodes, err = b.i.handleDecideApproval(ctx, args, 0)
				require.NoError(t, err)
				result := decodeReply(t, nodes)
				require.Empty(t, result["resumeError"])
				if scenario == "guidance" {
					require.Equal(t, true, result["runResumed"])
				} else {
					require.Equal(t, true, result["runStopped"])
					require.Equal(t, false, result["runResumed"])
				}
			}
			after := b.runRow(t, runID)
			require.Equal(t, before["spent"], after["spent"])
			require.Equal(t, before["classification"], after["classification"], "recovery must not extend twice")
			a.i.engine = a.eng
		})
	}
}

func TestProgressDBCapabilityReads(t *testing.T) {
	a, b := freshActsDBPair(t, openActsDB(t), openActsDB(t))
	for _, peer := range []actsDB{a, b} {
		require.NoError(t, peer.eng.RegisterIntegration(peer.i))
	}
	runID := a.openRun(t, nil)
	goalID := rowString(a.runRow(t, runID), "goalId")
	a.write(t, "updateWorkGoal", map[string]any{"goalId": goalID, "ceilings": map[string]any{"maxModelCalls": 96, "wallClockMs": 7200000}})
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "classification": map[string]any{"workload": "lookup"}})
	rc := common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: a.owner, StepKey: "reason", Mode: common.RunModeLive}
	ctx := common.ContextWithRun(actorCtx(a.owner), rc)
	// Use the real native producer. A second replica must read this persisted
	// capability evidence through the same owner-scoped query as tool results.
	_, err := a.eng.Execute(ctx, "query "+call("workCapabilities", map[string]any{"search": "library.libraryGenerateImage"}))
	require.NoError(t, err)
	rows := b.query(t, a.owner, "query "+call("workProgressReceiptsForOwnerRun", map[string]any{"runId": runID}))
	require.NotEmpty(t, rows)
	var keys []string
	for _, row := range rows {
		keys = append(keys, progressReceiptKeys(row)...)
	}
	require.NotEmpty(t, keys)
	continued, err := b.i.ContinueWorkProgress(ctx, rc, workstate.Spent{WallClockMs: 650000}, 0)
	require.NoError(t, err)
	require.True(t, continued)
	// Repeating the discovery has a new checkedAt timestamp and observation ID,
	// but cannot earn another window, even when the engines have separate caches.
	_, err = a.eng.Execute(ctx, "query "+call("workCapabilities", map[string]any{"search": "library.libraryGenerateImage"}))
	require.NoError(t, err)
	continued, err = a.i.ContinueWorkProgress(ctx, rc, workstate.Spent{WallClockMs: 1250000}, 0)
	require.NoError(t, err)
	require.False(t, continued)
	require.Empty(t, b.query(t, "different-owner", "query "+call("workProgressReceiptsForOwnerRun", map[string]any{"runId": runID})))
	require.Empty(t, b.query(t, a.owner, "query "+call("workApprovalsForOwnedRun", map[string]any{"runId": runID})))
}
