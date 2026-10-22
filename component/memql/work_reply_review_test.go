package memql

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

func TestReplyReviewCallsOnceAndNeverFallsBackToUncheckedDraft(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		err          error
		wantError    bool
	}{
		{"corrected", `{"reply":"The status reports degraded service; it does not establish why."}`, nil, false},
		{"malformed", `broken`, nil, true},
		{"empty", `{"reply":""}`, nil, true},
		{"oversize", `{"reply":"` + strings.Repeat("x", replyReviewMaxBytes+1) + `"}`, nil, true},
		{"cancelled", "", context.Canceled, true},
		{"unavailable", "", errors.New("no provider available"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := common.ContextWithRun(context.Background(), common.RunContext{RunId: "original-run", OwnerUserId: "owner"})
			calls := 0
			reply, err := reviewReplyOnce(ctx, "Why is the service slow?", "It is slow because the provider is down.", `{"status":"degraded","cause":null}`, time.Now(), false, func(callCtx context.Context, prompt string, data map[string]any, _ string, schema json.RawMessage, strict bool) (string, error) {
				calls++
				run, _ := common.RunFromContext(callCtx)
				require.Equal(t, "original-run", run.RunId)
				require.Equal(t, "workReplyReview", prompt)
				require.True(t, strict)
				require.True(t, json.Valid(schema))
				require.Contains(t, data["sources"], `"cause":null`)
				return tc.answer, tc.err
			})
			require.Equal(t, 1, calls)
			if tc.wantError {
				require.Error(t, err)
				require.Empty(t, reply)
			} else {
				require.NoError(t, err)
				require.NotContains(t, reply.Reply, "provider is down")
			}
		})
	}
}

type replyReviewModel struct {
	answer   string
	calls    int
	messages []common.ChatMessage
	run      common.RunContext
}

func TestReplyReviewPriorRequestsDoNotPromoteAssistantClaims(t *testing.T) {
	requests := reviewPriorRequests([]any{
		map[string]any{"role": "user", "content": "What is happening in location A?"},
		map[string]any{"role": "assistant", "content": "It is definitely safe there."},
		map[string]any{"role": "user", "content": strings.Repeat("x", 6001)},
		map[string]any{"role": "user", "content": "Can I go outside?"},
	})
	require.Equal(t, []string{"What is happening in location A?", "Can I go outside?"}, requests)
}

func (m *replyReviewModel) CallChatStructured(ctx context.Context, messages []common.ChatMessage, _ common.StructuredSchema) (string, error) {
	m.calls++
	m.messages = messages
	m.run, _ = common.RunFromContext(ctx)
	if m.answer != "" {
		return m.answer, nil
	}
	return `{"reply":"The source reports degraded service. It does not identify a cause.","missingEvidence":""}`, nil
}

func TestReplyReviewReadsRecordedEvidenceOnAnotherReplica(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, a)
	run, _ := common.RunFromContext(ctx)
	model := &replyReviewModel{}
	b.SetAIResolver(testAIResolver{fn: func(context.Context, airoute.ResolveRequest) (ResolvedProvider, error) {
		return ResolvedProvider{Client: model, Resolution: airoute.Resolution{ProviderName: "fleet:review", Model: "test"}}, nil
	}})
	draft := map[string]any{"draft": map[string]any{"reply": "The provider is down, causing the slowness."}}
	// No tool receipts: an ordinary self-contained reply adds no model call.
	_, err := b.workReviewReplyBuiltin(ctx, draft, 0)
	require.NoError(t, err)
	require.Zero(t, model.calls)
	read := []common.ChatMessage{
		{Role: "system", Content: "PRIVATE_SYSTEM_NOT_EVIDENCE"},
		{Role: "assistant", Content: "PRIVATE_REASONING_NOT_EVIDENCE", ToolCalls: []common.ToolCall{{ID: "status", Name: "readStatus", Arguments: `{}`}}},
		{Role: "tool", Name: "readStatus", ToolCallId: "status", Content: `{"status":"degraded","cause":null,"timezone":"America/Phoenix","source":"https://status.example.test"}`},
	}
	require.NoError(t, a.SaveWorkContinuation(ctx, read))
	require.NoError(t, a.RecordDelegatedToolSource(WithActingAgentId(ctx, "coding-agent"), "workbenchHost", map[string]any{"action": "exec", "args": map[string]any{"cmd": strings.Repeat("LARGE_SCRIPT_INPUT", 900)}}, `{"stdout":"Executed 12 tests; all passed","exitCode":0}`))
	rows, err := b.workReviewReplyBuiltin(ctx, draft, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Contains(t, string(rows[0].Payload), "does not identify a cause")
	require.Equal(t, 1, model.calls)
	require.Equal(t, run.RunId, model.run.RunId)
	raw, _ := json.Marshal(model.messages)
	require.Contains(t, string(raw), "status.example.test")
	require.Contains(t, string(raw), "sourceZoneClocks")
	require.Contains(t, string(raw), "Executed 12 tests; all passed")
	require.Contains(t, string(raw), "currentRunId")
	require.Contains(t, string(raw), "argumentsOmitted")
	require.NotContains(t, string(raw), "LARGE_SCRIPT_INPUT")
	require.NotContains(t, string(raw), "PRIVATE_SYSTEM_NOT_EVIDENCE")
	require.NotContains(t, string(raw), "PRIVATE_REASONING_NOT_EVIDENCE")
	foreign := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "someone-else"), run)
	_, err = b.workReviewReplyBuiltin(foreign, draft, 0)
	require.Error(t, err)
	require.Equal(t, 1, model.calls, "an unowned run cannot spend a review call or read receipts")
	guard := &contextReserveGuard{limit: 3, used: 3}
	b.SetRunCeilingGuard(guard)
	rows, err = b.workReviewReplyBuiltin(ctx, draft, 0)
	var ceiling *RunCeilingError
	require.ErrorAs(t, err, &ceiling)
	require.Empty(t, rows, "budget exhaustion must not release the unchecked draft")
	require.Equal(t, 1, model.calls, "review cannot bypass the existing call ceiling")
	require.Equal(t, 3, guard.used)
}

func TestLookupReplyWithNoReceiptCannotBypassEvidenceReview(t *testing.T) {
	e, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, e)
	run, _ := common.RunFromContext(ctx)
	call, err := parser.RenderCall("updateWorkRun", map[string]any{"runId": run.RunId, "classification": map[string]any{"workload": "lookup"}})
	require.NoError(t, err)
	_, err = e.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
	require.NoError(t, err)
	model := &replyReviewModel{}
	e.SetAIResolver(testAIResolver{fn: func(context.Context, airoute.ResolveRequest) (ResolvedProvider, error) {
		return ResolvedProvider{Client: model, Resolution: airoute.Resolution{ProviderName: "fleet:review", Model: "test"}}, nil
	}})
	_, err = e.workReviewReplyBuiltin(ctx, map[string]any{"draft": map[string]any{"reply": "The live service is definitely unavailable."}}, 0)
	require.NoError(t, err)
	require.Equal(t, 1, model.calls)
	raw, _ := json.Marshal(model.messages)
	require.Contains(t, string(raw), "no successful source receipt was recorded")
	// Reading a profile cannot satisfy a lookup's need for source evidence.
	require.NoError(t, e.RecordDelegatedToolSource(WithActingAgentId(ctx, "chat-agent"), "workViewerContext", map[string]any{}, `{"name":"Test user"}`))
	_, err = e.workReviewReplyBuiltin(ctx, map[string]any{"draft": map[string]any{"reply": "The live service is definitely unavailable."}}, 0)
	require.NoError(t, err)
	require.Equal(t, 2, model.calls)
	raw, _ = json.Marshal(model.messages)
	require.Contains(t, string(raw), "no successful source receipt was recorded")
	model.answer = `{"reply":"The requested entity has not been checked.","missingEvidence":"Resolve the named entity from a source."}`
	for _, allowed := range []bool{false, true} {
		// Exercise the same parsed builtin boundary used by the agent, not just
		// a direct Go call that could conceal losing a nested boolean argument.
		call, err := parser.RenderCall("work.workReviewReply", map[string]any{"draft": map[string]any{"reply": "A draft", "continueLookup": allowed}})
		require.NoError(t, err)
		result, err := e.Execute(ctx, "builtin "+call)
		require.NoError(t, err)
		rows := MaterializeRows(result.OutputPayload())
		require.Len(t, rows, 1)
		raw, err := json.Marshal(rows[0])
		require.NoError(t, err)
		var decision replyReviewDecision
		require.NoError(t, json.Unmarshal(raw, &decision))
		if allowed {
			require.NotEmpty(t, decision.MissingEvidence)
		} else {
			require.Empty(t, decision.MissingEvidence)
		}
		observations, err := e.workRows(ContextWithFreshRead(ctx), "workObservationsForOwnerRun", run.RunId)
		require.NoError(t, err)
		found := false
		for _, observation := range observations {
			data, _ := observation["data"].(map[string]any)
			receipt, _ := data["replyReview"].(map[string]any)
			if receipt["continuationAvailable"] == allowed && receipt["missingEvidence"] == decision.MissingEvidence {
				found = true
			}
		}
		require.True(t, found, "the durable decision must distinguish an unavailable continuation from a model choosing not to use it")
		sources, err := e.workRows(ContextWithFreshRead(ctx), "workSourceCheckpointsForOwnerRun", run.RunId)
		require.NoError(t, err)
		require.Empty(t, sources, "a review decision must never become a factual source")
	}

}

func TestReplyReviewEvidenceGapIsBoundedAndDisabledForDirectAdvice(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		decision, err := reviewReplyOnce(context.Background(), "Is item A available?", "Item B is available.", `{"item":"B"}`, time.Now(), allowed, func(_ context.Context, _ string, data map[string]any, _ string, _ json.RawMessage, _ bool) (string, error) {
			require.Equal(t, allowed, data["canContinue"])
			return `{"reply":"The receipt describes another item.","missingEvidence":"Resolve item A's source identifier before checking its status."}`, nil
		})
		require.NoError(t, err)
		if allowed {
			require.Contains(t, decision.MissingEvidence, "item A")
		} else {
			require.Empty(t, decision.MissingEvidence, "direct advice must not reopen research")
		}
	}
	_, err := reviewReplyOnce(context.Background(), "Question", "Draft", "{}", time.Now(), true, func(context.Context, string, map[string]any, string, json.RawMessage, bool) (string, error) {
		raw, _ := json.Marshal(map[string]any{"reply": "A supported limit.", "missingEvidence": strings.Repeat("x", 1201)})
		return string(raw), nil
	})
	require.Error(t, err)
}

func TestReplyReviewKeepsRecoveryUnderFilePreviewPressure(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := WithActingAgentId(contextTestRun(t, a), "coding-agent")
	for n := range 9 {
		message := common.ChatMessage{Role: "tool", Name: "workbenchHost", ToolCallId: fmt.Sprint(n), Content: "original source"}
		raw, _ := json.Marshal(message)
		page, ok := workHistoryPage(message, "", 0, 4000, true)
		require.True(t, ok)
		page["sourceRunId"], page["checkpoint"], page["index"] = "prior", fmt.Sprint("original-", n), 1
		page["sourceEvidence"] = map[string]any{"sha256": fmt.Sprintf("%x", sha256.Sum256(raw)), "operation": "workbenchHost", "path": fmt.Sprint("history/source-", n)}
		pairs, _ := workCheckpointPairsWithSourceReads(recoveryTestCheckpoint(t, fmt.Sprint("read-", n), page, true), true)
		require.Len(t, pairs, 1)
		var args map[string]any
		require.NoError(t, json.Unmarshal([]byte(pairs[0].call.Arguments), &args))
		require.NoError(t, a.RecordDelegatedToolSource(ctx, "recallWorkHistory", args, pairs[0].result.Content))
	}
	for n := range 8 {
		content, _ := json.Marshal(map[string]any{"content": strings.Repeat("x", 3500), "path": fmt.Sprint("current/file-", n)})
		require.NoError(t, a.RecordDelegatedToolSource(ctx, "workbenchHost", map[string]any{"action": "fs_read"}, string(content)))
	}
	model := &replyReviewModel{}
	b.SetAIResolver(testAIResolver{fn: func(context.Context, airoute.ResolveRequest) (ResolvedProvider, error) {
		return ResolvedProvider{Client: model, Resolution: airoute.Resolution{ProviderName: "fleet:review", Model: "test"}}, nil
	}})
	_, err := b.workReviewReplyBuiltin(ctx, map[string]any{"draft": map[string]any{"reply": "The original sources were recovered."}}, 0)
	require.NoError(t, err)
	raw, _ := json.Marshal(model.messages)
	for n := range 9 {
		require.Contains(t, string(raw), fmt.Sprint("history/source-", n), "later file previews must not evict earlier recovery proofs")
	}
	require.Equal(t, 9, strings.Count(string(raw), `\"complete\":true`))
}

func TestProfileOnlyReplyDoesNotTriggerEvidenceModel(t *testing.T) {
	require.False(t, replyHasEvidenceSources([]any{map[string]any{"operation": "workViewerContext"}}))
	require.True(t, replyHasEvidenceSources([]any{map[string]any{"operation": "workViewerContext"}, map[string]any{"operation": "readStatus"}}))
	require.True(t, replyHasEvidenceSources([]any{nil}))
}
