package memql

import (
	"context"
	"encoding/json"
	"errors"
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
			reply, err := reviewReplyOnce(ctx, "Why is the service slow?", "It is slow because the provider is down.", `{"status":"degraded","cause":null}`, time.Now(), func(callCtx context.Context, prompt string, data map[string]any, _ string, schema json.RawMessage, strict bool) (string, error) {
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
				require.NotContains(t, reply, "provider is down")
			}
		})
	}
}

type replyReviewModel struct {
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
	return `{"reply":"The source reports degraded service. It does not identify a cause."}`, nil
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
	require.NoError(t, a.RecordDelegatedToolSource(WithActingAgentId(ctx, "coding-agent"), "workbenchHost", map[string]any{"action": "exec"}, `{"stdout":"Executed 12 tests; all passed","exitCode":0}`))
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
}
