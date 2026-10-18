package memql

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

func sourceArchiveCheckpoint(t *testing.T, messages []common.ChatMessage) map[string]any {
	t.Helper()
	raw, err := json.Marshal(messages)
	require.NoError(t, err)
	return map[string]any{"id": "checkpoint", "data": map[string]any{"messagesEncoding": "json-base64-v1", "messages": base64.StdEncoding.EncodeToString(raw)}}
}

func TestCheckpointSourceArchiveNeverPromotesPreviewAndCanUseOlderCompleteReceipt(t *testing.T) {
	original := common.ChatMessage{Role: "tool", Name: "readStatus", ToolCallId: "source", Content: strings.Repeat("a", 1000) + "item-A operational; cause unknown" + strings.Repeat("b", 14000)}
	paired := common.ChatMessage{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "source", Name: "readStatus", Arguments: `{}`}}}
	raw, err := json.Marshal([]common.ChatMessage{original})
	require.NoError(t, err)
	stub := original
	stub.Content = workContextArchivedResult([]common.ChatMessage{paired, original}, 1, workContextHash("tool-v2", raw))
	require.NotContains(t, stub.Content, "item-A operational")
	newer := sourceArchiveCheckpoint(t, []common.ChatMessage{paired, stub})
	older := sourceArchiveCheckpoint(t, []common.ChatMessage{paired, original})
	remaining := 24000
	sources, omitted := workCheckpointSources([]map[string]any{newer}, "run", &remaining)
	require.True(t, omitted)
	require.Empty(t, sources, "a compacted preview is not a complete source")
	require.Equal(t, 24000, remaining)
	sources, omitted = workCheckpointSources([]map[string]any{newer, older, older}, "run", &remaining)
	require.False(t, omitted, "the later complete receipt resolves the missing preview")
	require.Len(t, sources, 1)
	require.Equal(t, original.Content, sources[0]["value"], "a complete source over 8 KB fits the existing 24 KB total budget")
	require.Positive(t, remaining)
	remaining = 8000
	sources, omitted = workCheckpointSources([]map[string]any{older}, "run", &remaining)
	require.True(t, omitted)
	require.Empty(t, sources)
	remaining = 24000
	sources, omitted = projectWorkCheckpointSources([]map[string]any{newer}, "run", &remaining, func(common.ChatMessage) (common.ChatMessage, bool) {
		wrong := original
		wrong.ToolCallId = "another-call"
		return wrong, true
	})
	require.True(t, omitted)
	require.Empty(t, sources, "restoration must preserve the original tool-call binding")
}

func TestSourceArchiveValidatesExactToolMessageAndExcludesRecalledClaims(t *testing.T) {
	tool := common.ChatMessage{Role: "tool", Name: "readStatus", ToolCallId: "source", Content: "original source"}
	for _, tc := range []struct {
		name     string
		messages []common.ChatMessage
		valid    bool
	}{
		{"exact tool", []common.ChatMessage{tool}, true},
		{"mixed private prose", []common.ChatMessage{tool, {Role: "assistant", Content: "private reasoning"}}, false},
		{"assistant", []common.ChatMessage{{Role: "assistant", Name: "readStatus", ToolCallId: "source", Content: "claim"}}, false},
		{"unexecuted", []common.ChatMessage{{Role: "tool", Name: "readStatus", ToolCallId: "source", Content: common.UnexecutedWorkToolResult}}, false},
		{"nested archive", []common.ChatMessage{{Role: "tool", Name: "readStatus", ToolCallId: "source", Content: "[Archived tool result other]"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.messages)
			require.NoError(t, err)
			fingerprint := workContextHash("tool-v2", raw)
			_, ok := workSourceArchiveMessage(string(raw), fingerprint)
			require.Equal(t, tc.valid, ok)
			_, ok = workSourceArchiveMessage(string(raw)+" ", fingerprint)
			require.False(t, ok, "even valid JSON must match the saved content hash")
			require.Equal(t, fingerprint, workSourceArchiveFingerprint("[Archived tool result "+fingerprint+"]\npreview"))
		})
	}
	for _, content := range []string{"[Archived tool result (history page)]\npreview", "[Archived tool result abc]\npreview", "unrelated prose"} {
		require.Empty(t, workSourceArchiveFingerprint(content))
	}
	messages := []common.ChatMessage{
		{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "memory", Name: "recallMemory"}, {ID: "history", Name: "recallWorkHistory"}}},
		{Role: "tool", Name: "recallMemory", ToolCallId: "memory", Content: "An earlier assistant asserted an outage"},
		{Role: "tool", Name: "recallWorkHistory", ToolCallId: "history", Content: "An earlier assistant asserted an outage"},
	}
	remaining := 24000
	sources, omitted := workCheckpointSources([]map[string]any{sourceArchiveCheckpoint(t, messages)}, "run", &remaining)
	require.False(t, omitted)
	require.Empty(t, sources, "recalled assistant claims cannot become independent source evidence")
}

func TestSourceArchiveRestoresReviewAndFollowupAcrossReplicasWithOwnerRunScope(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, a)
	run, _ := common.RunFromContext(ctx)
	original := common.ChatMessage{Role: "tool", Name: "readStatus", ToolCallId: "status", Content: strings.Repeat("source preamble ", 80) + "item-A operational; no current incident; cause unknown" + strings.Repeat(" unrelated item details", 650)}
	messages := []common.ChatMessage{
		{Role: "system", Content: "PRIVATE_SYSTEM_NOT_EVIDENCE"},
		{Role: "assistant", Content: "PRIVATE_REASONING_NOT_EVIDENCE", ToolCalls: []common.ToolCall{{ID: "status", Name: "readStatus", Arguments: `{"item":"A"}`}}},
		original,
	}
	compacted, err := a.CompactWorkContext(ctx, messages, nil, 2500)
	require.NoError(t, err)
	require.NotContains(t, compacted[len(compacted)-1].Content, "item-A operational")
	require.Contains(t, compacted[len(compacted)-1].Content, "[Archived tool result ")
	require.NoError(t, a.SaveWorkContinuation(ctx, compacted))
	checkpoints, err := b.workRows(ContextWithFreshRead(ctx), "workSourceCheckpointsForOwnerRun", run.RunId)
	require.NoError(t, err)
	require.NotEmpty(t, checkpoints)
	remaining := 24000
	sources, omitted := b.workCheckpointSources(ctx, checkpoints, run.RunId, &remaining)
	require.False(t, omitted)
	require.Len(t, sources, 1)
	require.Equal(t, original.Content, sources[0]["value"], "receiving replica reads exact durable source without external work")

	model := &replyReviewModel{}
	b.SetAIResolver(testAIResolver{fn: func(context.Context, airoute.ResolveRequest) (ResolvedProvider, error) {
		return ResolvedProvider{Client: model, Resolution: airoute.Resolution{ProviderName: "fleet:review", Model: "test"}}, nil
	}})
	_, err = b.workReviewReplyBuiltin(ctx, map[string]any{"draft": map[string]any{"reply": "The item is slow because of an outage."}}, 0)
	require.NoError(t, err)
	require.Equal(t, 1, model.calls)
	raw, err := json.Marshal(model.messages)
	require.NoError(t, err)
	require.Contains(t, string(raw), "item-A operational; no current incident; cause unknown")
	require.NotContains(t, string(raw), "PRIVATE_REASONING_NOT_EVIDENCE")
	require.NotContains(t, string(raw), "PRIVATE_SYSTEM_NOT_EVIDENCE")
	require.NotContains(t, string(raw), "[Archived tool result ")

	conversation := askTestConversation(t, a, ctx)
	require.NoError(t, a.askSave(ctx, conversation, "Reuse source", askTranscript{Turns: []AskTurn{
		{ID: "prior", Prompt: "What is the item status?", State: "done", RunID: run.RunId, StartedAt: time.Now()},
		{ID: "current", Prompt: "What should I do?", State: "running", StartedAt: time.Now()},
	}}))
	nodes, err := b.workSourceContextBuiltin(ctx, map[string]any{"conversationId": conversation, "turnId": "current"}, 0)
	require.NoError(t, err)
	require.Contains(t, string(nodes[0].Payload), "item-A operational; no current incident; cause unknown")
	require.NotContains(t, string(nodes[0].Payload), "PRIVATE_REASONING_NOT_EVIDENCE")
	require.Equal(t, 1, model.calls, "archive restoration spends no model call")

	for _, tc := range []struct {
		name, runID string
		ctx         context.Context
	}{
		{"foreign owner", run.RunId, auth.ContextWithUserActor(context.Background(), "another-person")},
		{"different run", "different-run", ctx},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remaining := 24000
			sources, omitted := b.workCheckpointSources(tc.ctx, checkpoints, tc.runID, &remaining)
			require.True(t, omitted)
			require.Empty(t, sources, "the hash is not authority to read another owner's or run's archive")
		})
	}
	wrong := append([]common.ChatMessage(nil), compacted...)
	wrong[1].ToolCalls = []common.ToolCall{{ID: "other-call", Name: "readStatus"}}
	wrong[2].ToolCallId = "other-call"
	remaining = 24000
	sources, omitted = b.workCheckpointSources(ctx, []map[string]any{sourceArchiveCheckpoint(t, wrong)}, run.RunId, &remaining)
	require.True(t, omitted)
	require.Empty(t, sources, "even an owned archive must match the paired call")
}
