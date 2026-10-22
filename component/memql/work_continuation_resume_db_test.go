package memql

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

func TestWorkContinuationRequiresTheExactOwnedAnsweredCheckpoint(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	ctx := askTestActor()
	ac, _ := auth.AccessFromContext(ctx)
	runID, goalID, approvalID := id.NewShortId(), id.NewShortId(), id.NewShortId()
	write := func(name string, args map[string]any) {
		t.Helper()
		call, err := parser.RenderCall(name, args)
		require.NoError(t, err)
		_, err = a.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
		require.NoError(t, err)
	}
	write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Continue saved work", "origin": "user"})
	write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "feedback", "templateFingerprint": "test", "triggeredBy": "manual", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	write("createWorkApproval", map[string]any{"approvalId": approvalID, "runId": runID, "stepKey": "reason", "kind": "feedback", "artifactHash": "question", "question": "Continue?", "requestedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	run := common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: ac.UserId, StepKey: "reason", Mode: common.RunModeLive}
	runCtx := common.ContextWithRun(ctx, run)
	request := CheckpointResumeRequest{RunID: runID, StepKey: "reason", ApprovalID: approvalID}
	_, err := b.PrepareWorkContinuation(runCtx, request)
	require.Error(t, err, "an unanswered question is not a continuation")
	write("decideWorkApproval", map[string]any{"approvalId": approvalID, "decision": "answered", "decidedBy": ac.UserId, "decidedAt": time.Now().UTC().Format(time.RFC3339Nano), "answer": map[string]any{"text": "Continue"}})
	write("updateWorkRun", map[string]any{"runId": runID, "status": "running", "humanResumeId": approvalID})
	_, err = b.PrepareWorkContinuation(runCtx, request)
	require.Error(t, err, "an answered question without a checkpoint cannot repeat a turn")
	// Receipts already stored as JSON arrays must remain resumable after a
	// deployment; they cannot be rewritten without changing their identity.
	legacy := []common.ChatMessage{{Role: "user", Content: "Existing saved request"}}
	legacyRaw, err := json.Marshal(legacy)
	require.NoError(t, err)
	write("createWorkObservation", map[string]any{
		"observationId": continuationID(run, legacyRaw), "runId": runID, "stepKey": run.StepKey,
		"kind": "note", "content": "Saved execution progress", "data": map[string]any{"continuation": true, "messages": legacy},
	})
	legacyCtx, err := b.PrepareWorkContinuation(runCtx, request)
	require.NoError(t, err)
	legacyRestored, err := b.RestoreWorkContinuation(legacyCtx, nil)
	require.NoError(t, err)
	require.Equal(t, legacy, legacyRestored)
	messages := []common.ChatMessage{{Role: "user", Content: "Perform work"}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: "saved", Name: "writeFile", Arguments: "{}"}}}, {Role: "tool", ToolCallId: "saved", Name: "writeFile", Content: "Written once; literal \u0000; escaped \\u0000; UTF-8 🌿"}}
	require.NoError(t, a.SaveWorkContinuation(runCtx, messages))
	prepared, err := b.PrepareWorkContinuation(runCtx, request)
	require.NoError(t, err)
	proof, _ := common.RunFromContext(prepared)
	require.NotNil(t, proof.Continuation)
	restored, err := b.RestoreWorkContinuation(prepared, nil)
	require.NoError(t, err)
	require.Equal(t, messages, restored)
	// A prior compaction may have lost the unmarked section instruction. A
	// fresh replica must restore the executing section while retaining effects
	// and the new human decision, without rewriting the original checkpoint.
	instruction := common.ChatMessage{Role: "user", Name: "memql_work_instruction", Content: "Recover prior evidence and return a handoff; later sections implement the change."}
	answer := common.ChatMessage{Role: "user", Content: "[Recorded answer for this suspended step]\nContinue"}
	restored, err = b.RestoreWorkContinuation(prepared, []common.ChatMessage{instruction, answer})
	require.NoError(t, err)
	require.Contains(t, restored, instruction)
	require.Equal(t, messages, restored[1:len(restored)-1], "completed effects retain their exact order")
	require.Equal(t, answer, restored[len(restored)-1])
	unchanged, err := b.RestoreWorkContinuation(prepared, nil)
	require.NoError(t, err)
	require.Equal(t, messages, unchanged, "rehydration must not mutate the saved receipt")
	_, err = b.PrepareWorkContinuation(common.ContextWithRun(askTestActor(), run), request)
	require.Error(t, err, "the checkpoint grants no cross-owner access")
	changed := request
	changed.StepKey = "other"
	_, err = b.PrepareWorkContinuation(runCtx, changed)
	require.Error(t, err, "the approval cannot resume another step")
	changed = request
	changed.ApprovalID = id.NewShortId()
	_, err = b.PrepareWorkContinuation(runCtx, changed)
	require.Error(t, err)
	// A later checkpoint is a different continuation. Never fall back to it,
	// or to a fresh turn, after admission selected the original receipt.
	newer := append([]common.ChatMessage(nil), messages...)
	newer = append(newer, common.ChatMessage{Role: "user", Content: "Different continuation"})
	require.NoError(t, a.SaveWorkContinuation(runCtx, newer))
	_, err = b.RestoreWorkContinuation(prepared, nil)
	require.Error(t, err, "changed checkpoint accepted")
	write("updateWorkRun", map[string]any{"runId": runID, "status": "cancelled"})
	_, err = b.PrepareWorkContinuation(runCtx, request)
	require.Error(t, err, "cancelled work resumed")
}

// A new replica must bind the immutable transcript to the exact rerun request,
// even when the messages are identical to a prior attempt. Missing legacy proof
// is a refusal, not permission to restart the effectful turn.
func TestWorkContinuationBindsTheFreshRerunRequestAcrossReplicas(t *testing.T) {
	for _, variant := range []string{"ordinary", "repair", "model", "inputs"} {
		t.Run(variant, func(t *testing.T) { testWorkContinuationRerunAcrossReplicas(t, variant) })
	}
}

func testWorkContinuationRerunAcrossReplicas(t *testing.T, variant string) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	ctx := askTestActor()
	ac, _ := auth.AccessFromContext(ctx)
	runID, goalID, approvalID := id.NewShortId(), id.NewShortId(), id.NewShortId()
	write := func(name string, args map[string]any) {
		t.Helper()
		call, err := parser.RenderCall(name, args)
		require.NoError(t, err)
		_, err = a.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
		require.NoError(t, err)
	}
	write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Continue one explicit rerun", "origin": "user"})
	write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "feedback", "templateFingerprint": "test", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	write("createWorkApproval", map[string]any{"approvalId": approvalID, "runId": runID, "stepKey": "reason", "kind": "feedback", "artifactHash": "question", "question": "Continue?", "requestedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	write("decideWorkApproval", map[string]any{"approvalId": approvalID, "decision": "answered", "decidedBy": ac.UserId, "decidedAt": time.Now().UTC().Format(time.RFC3339Nano), "answer": map[string]any{"text": "Continue"}})
	rerun := map[string]any{"requestId": id.NewShortId(), "stepKey": "reason", "reason": "rerun"}
	setRerun := func(value map[string]any) {
		write("updateWorkRun", map[string]any{"runId": runID, "status": "running", "humanResumeId": approvalID, "rerun": value})
	}
	override := map[string]any{}
	switch variant {
	case "repair":
		override["guidance"] = map[string]any{"reason": "The prior turn did not produce a valid completion or durable question."}
	case "model":
		override["level"] = "strong"
	case "inputs":
		override["inputs"] = map[string]any{"path": "/home/person"}
	}
	rerun["override"] = override
	setRerun(rerun)
	run := common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: ac.UserId, StepKey: "reason", Mode: common.RunModeLive, RerunRequestId: rerun["requestId"].(string), Override: common.StepOverrideFromRecord(override)}
	request := CheckpointResumeRequest{RunID: runID, StepKey: run.StepKey, ApprovalID: approvalID, RerunRequestID: run.RerunRequestId}
	messages := []common.ChatMessage{{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "effect", Name: "writeFile", Arguments: "{}"}}}, {Role: "tool", ToolCallId: "effect", Name: "writeFile", Content: "Committed once"}}
	prepare := func() (context.Context, error) {
		return b.PrepareWorkContinuation(common.ContextWithRun(ctx, run), request)
	}
	raw, err := json.Marshal(messages)
	require.NoError(t, err)
	old := run
	old.RerunRequestId = ""
	require.NotEqual(t, continuationID(old, raw), continuationID(run, raw))
	require.NoError(t, a.SaveWorkContinuation(common.ContextWithRun(ctx, old), messages))
	_, err = prepare()
	require.Error(t, err, "ordinary transcript cannot authorize a rerun")
	freshInput := []common.ChatMessage{{Role: "user", Content: "Explicit new version"}}
	restored, err := b.RestoreWorkContinuation(common.ContextWithRun(ctx, run), freshInput)
	require.NoError(t, err)
	require.Equal(t, freshInput, restored, "a new explicit rerun must not inherit the previous attempt's transcript")
	require.NoError(t, a.SaveWorkContinuation(common.ContextWithRun(ctx, run), messages))
	prepared, err := prepare()
	require.NoError(t, err)
	restored, err = b.RestoreWorkContinuation(prepared, nil)
	require.NoError(t, err)
	require.Equal(t, messages, restored)
	changedExecution := run
	changedExecution.Override = &common.StepOverride{Prompt: "Different execution"}
	_, err = b.PrepareWorkContinuation(common.ContextWithRun(ctx, changedExecution), request)
	require.Error(t, err, "an override must not borrow the prior execution's answer")
	changedRecord := map[string]any{}
	for k, v := range rerun {
		changedRecord[k] = v
	}
	changedRecord["override"] = map[string]any{"prompt": "Changed after admission"}
	setRerun(changedRecord)
	_, err = b.RestoreWorkContinuation(prepared, nil)
	require.Error(t, err, "a fresh peer change invalidates prepared proof")
	setRerun(rerun)
	for _, next := range []map[string]any{{}, {"requestId": "another", "stepKey": "reason", "reason": "rerun"}} {
		setRerun(next)
		_, err = prepare()
		require.Error(t, err, "fresh peer mutation must invalidate warmed admission")
		_, err = b.RestoreWorkContinuation(prepared, nil)
		require.Error(t, err, "already prepared context is not an authority token")
	}
	setRerun(rerun)
	for _, missing := range []bool{true, false} {
		changed := run
		changed.RerunRequestId = "different"
		if missing {
			changed.RerunRequestId = ""
		}
		_, err = b.PrepareWorkContinuation(common.ContextWithRun(ctx, changed), request)
		require.Error(t, err)
		badRequest := request
		badRequest.RerunRequestID = changed.RerunRequestId
		_, err = b.PrepareWorkContinuation(common.ContextWithRun(ctx, run), badRequest)
		require.Error(t, err)
		// Preserve the original row ID but strip/change its proof: neither a
		// legacy record nor a mutable metadata field can authorize the content.
		write("createWorkObservation", map[string]any{"observationId": continuationID(run, raw), "runId": runID, "stepKey": run.StepKey, "kind": "note", "content": "Saved execution progress", "data": map[string]any{"continuation": true, "messages": messages, "rerunRequestId": changed.RerunRequestId}})
		_, err = prepare()
		require.Error(t, err)
		_, err = b.RestoreWorkContinuation(prepared, nil)
		require.Error(t, err)
	}
}
