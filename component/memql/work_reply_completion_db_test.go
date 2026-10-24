package memql

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/airoute"
	"strings"
	"testing"
	"time"
)

func TestReplyCompletionCurrentStepAndSourcesAcrossReplica(t *testing.T) {
	a, b, ctx, _, run, write := guidanceFixture(t)
	write("createWorkGoal", map[string]any{"goalId": "fixture-goal", "statement": "Publish the finished report after all preparation.", "origin": "user"})
	write("createWorkStep", map[string]any{"stepId": run + "-section", "runId": run, "key": "section", "seq": 0, "attempt": 1, "stepType": "function", "kind": "reasoning", "status": "running", "call": map[string]any{"name": "runAgentTurn"}})
	runtime := ContextWithWorkReplyTask(guidanceRunContext(ctx, run), "Save the prepared report.")
	model := &replyReviewModel{answer: `{"reply":"The save remains unperformed.","missingEvidence":"","completion":{"kind":"operation","disposition":"unfinished","gap":"Save the approved draft and verify the result.","sourceRefs":[]}}`}
	b.SetAIResolver(testAIResolver{fn: func(context.Context, airoute.ResolveRequest) (ResolvedProvider, error) {
		return ResolvedProvider{Client: model, Resolution: airoute.Resolution{ProviderName: "fleet:review", Model: "test"}}, nil
	}})
	rows, err := b.workReviewReplyBuiltin(runtime, map[string]any{"draft": map[string]any{"reply": "The save remains unperformed.", "reviewCompletion": true, "continueLookup": false}}, 0)
	require.NoError(t, err)
	require.Equal(t, 1, model.calls, "an operation with no outcome source cannot take the self-contained bypass")
	var result map[string]any
	require.NoError(t, json.Unmarshal(rows[0].Payload, &result))
	require.Equal(t, "unfinished", result["completion"].(map[string]any)["disposition"])
	prompt := flattenForSession(model.messages)
	require.Contains(t, prompt, "Save the prepared report.")
	require.Contains(t, prompt, "not every unfinished item")
	require.Contains(t, prompt, "even after a bounded attempt fails")
	require.NoError(t, a.RecordDelegatedToolSource(WithActingAgentId(runtime, "editor"), "saveReport", map[string]any{}, `{"saved":true,"verified":true}`))
	model.answer = `{"reply":"The report was saved.","missingEvidence":"","completion":{"kind":"operation","disposition":"satisfied","gap":"","sourceRefs":["source-1"]}}`
	_, err = b.workReviewReplyBuiltin(runtime, map[string]any{"draft": map[string]any{"reply": "Saved.", "reviewCompletion": true}}, 0)
	require.NoError(t, err)
	model.answer = strings.Replace(model.answer, "source-1", "invented-source", 1)
	_, err = b.workReviewReplyBuiltin(runtime, map[string]any{"draft": map[string]any{"reply": "Saved with invented citation.", "reviewCompletion": true}}, 0)
	require.ErrorContains(t, err, "unavailable source")
	// Explicit owner deferral changes current scope; the old goal-wide publish
	// request must not become an unfinished effect or another permission loop.
	actor, _ := auth.AccessFromContext(ctx)
	recordApprovalGuidance(write, actor.UserId, run, "Leave publication on hold; report the saved draft only.")
	model.answer = `{"reply":"The draft is saved; publication remains on hold.","missingEvidence":"","completion":{"kind":"information","disposition":"satisfied","gap":"","sourceRefs":[]}}`
	_, err = b.workReviewReplyBuiltin(runtime, map[string]any{"draft": map[string]any{"reply": "Publication on hold.", "reviewCompletion": true}}, 0)
	require.NoError(t, err)
	require.Contains(t, flattenForSession(model.messages), "Leave publication on hold")
	write("updateWorkStep", map[string]any{"stepId": run + "-section", "status": "done"})
	_, err = b.workReviewReplyBuiltin(runtime, map[string]any{"draft": map[string]any{"reply": "Saved.", "reviewCompletion": true}}, 0)
	require.ErrorContains(t, err, "current step unavailable")
}
func TestReplyCompletionKeepsDirectQuickReplyBypass(t *testing.T) {
	_, b, ctx, _, run, _ := guidanceFixture(t)
	model := &replyReviewModel{}
	b.SetAIResolver(testAIResolver{fn: func(context.Context, airoute.ResolveRequest) (ResolvedProvider, error) {
		return ResolvedProvider{Client: model}, nil
	}})
	rows, err := b.workReviewReplyBuiltin(guidanceRunContext(ctx, run), map[string]any{"draft": map[string]any{"reply": "Hello."}}, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Zero(t, model.calls)
}
func TestReplyCompletionTypedSchemaDoesNotAcceptMissingOrInvalidControl(t *testing.T) {
	for _, answer := range []string{`{"reply":"done","missingEvidence":""}`, `{"reply":"done","missingEvidence":"","completion":{"kind":"operation","disposition":"satisfied","gap":"","sourceRefs":[]}}`} {
		_, err := reviewReplyOnce(context.Background(), "Goal", "Draft", "{}", time.Now(), false, func(context.Context, string, map[string]any, string, json.RawMessage, bool) (string, error) {
			return answer, nil
		}, `{"task":"Save"}`)
		require.Error(t, err)
	}
}

func TestOperationSourcesBoundedOmissionsAndMissingStepRequireReconciliation(t *testing.T) {
	a, b, ctx, _, run, _ := guidanceFixture(t)
	runtime := WithActingAgentId(guidanceRunContext(ctx, run), "fixture-agent")
	for n := 0; n < 10; n++ {
		require.NoError(t, a.RecordDelegatedToolSource(runtime, "readFile", map[string]any{"index": n}, `{"fileId":"retained","verified":true}`))
	}
	sources, err := b.ReadWorkStepSources(ctx, run, "section")
	require.NoError(t, err)
	require.Equal(t, true, sources["sourcesOmitted"])
	require.Equal(t, true, sources["reconciliationRequired"])
	sources, err = b.ReadWorkStepSources(ctx, run, "unrecorded-step")
	require.NoError(t, err)
	require.Empty(t, sources["recordedSources"])
	require.Equal(t, true, sources["reconciliationRequired"], "absence of a receipt is not permission to replay an effect")
}

func TestReplyCompletionTaskUsesOwnedGoalAndKeepsLongNativeInstructions(t *testing.T) {
	_, b, ctx, _, run, write := guidanceFixture(t)
	write("createWorkGoal", map[string]any{"goalId": "fixture-goal", "statement": "Explain the supplied document; do not publish it.", "origin": "user"})
	write("createWorkStep", map[string]any{"stepId": run + "-section", "runId": run, "key": "section", "seq": 0, "attempt": 1, "stepType": "function", "kind": "reasoning", "status": "running", "call": map[string]any{"name": "runAgentTurn"}})
	runtime := ContextWithWorkReplyTask(guidanceRunContext(ctx, run), "")
	scope, err := b.replyCompletionScope(runtime, true)
	require.NoError(t, err)
	require.Contains(t, scope, "Explain the supplied document; do not publish it.")
	long := strings.Repeat("Retain the original supplied context. ", 2500) + "Only save the explicitly requested final output."
	scope, err = b.replyCompletionScope(ContextWithWorkReplyTask(runtime, long), true)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(scope), &decoded))
	require.Equal(t, long, decoded["task"])
	_, err = b.replyCompletionScope(ContextWithWorkReplyTask(guidanceRunContext(ctx, "unowned-run"), "Save"), true)
	require.Error(t, err)
}
