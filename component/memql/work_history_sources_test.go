package memql

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

func TestWorkSourceHistoryPagesOlderLargeReceiptsAcrossReplicasWithoutPrivateContext(t *testing.T) {
	a, db, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, a)
	run, _ := common.RunFromContext(ctx)
	access, _ := auth.AccessFromContext(ctx)
	conversation := askTestConversation(t, a, ctx)
	prior, unrelated, later, foreign := "v1:work:run:"+uniqueSuffix("prior"), "v1:work:run:"+uniqueSuffix("unrelated"), "v1:work:run:"+uniqueSuffix("later"), "v1:work:run:"+uniqueSuffix("foreign")
	sequence := 0
	insert := func(concept, id string, payload map[string]any) {
		sequence++
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		_, err = db.DB.ExecContext(ctx, `INSERT INTO "MemoryNodes"(id,concept,"createdAt","createdBy",schema,payload) VALUES($1,$2,$3,'source-history-test','{}',$4)`, id, concept, time.Now().UTC().Add(time.Duration(sequence)*time.Millisecond), string(raw))
		require.NoError(t, err)
	}
	for _, id := range []string{prior, unrelated, later} {
		insert("v1:work:run", id, map[string]any{"ownerUserId": access.UserId, "status": "succeeded"})
	}
	insert("v1:work:run", foreign, map[string]any{"ownerUserId": "v1:identity:user:someone-else", "status": "succeeded"})
	insert("v1:work:run", "v1:work:run:"+BareShortId(run.RunId), map[string]any{"ownerUserId": access.UserId, "goalId": run.GoalId, "status": "running", "input": map[string]any{"conversation": map[string]any{"id": conversation, "turnId": "current"}}})
	require.NoError(t, a.askSave(ctx, conversation, "Source pages", askTranscript{Turns: []AskTurn{
		{ID: "original", RunID: prior, State: "done"},
		{ID: "forged", RunID: foreign, State: "done"},
		{ID: "current", RunID: run.RunId, State: "running"},
		{ID: "later", RunID: later, State: "done"},
	}}))
	original := common.ChatMessage{Role: "tool", Name: "workbenchHost", ToolCallId: "original-call", Content: "EXACT-ORIGINAL\n" + strings.Repeat("文献 original source line\n", 900) + "END-OF-ORIGINAL"}
	originalID := "v1:work:observation:" + uniqueSuffix("original")
	writeReceipt := func(id, sourceRun, owner string, messages []common.ChatMessage) {
		raw, err := json.Marshal(messages)
		require.NoError(t, err)
		insert("v1:work:observation", id, map[string]any{"runId": sourceRun, "ownerUserId": owner, "kind": "note", "data": map[string]any{"delegatedEvidence": true, "messagesEncoding": "json-base64-v1", "messages": base64.StdEncoding.EncodeToString(raw)}})
	}
	writeReceipt(originalID, prior, access.UserId, []common.ChatMessage{
		{Role: "system", Content: "PRIVATE-SYSTEM"},
		{Role: "assistant", Content: "PRIVATE-REASONING", ToolCalls: []common.ToolCall{{ID: "original-call", Name: "workbenchHost", Arguments: `{"action":"fs_read","args":{"path":"events.csv"}}`}, {ID: "secret", Name: "readSecret"}, {ID: "recall", Name: "recallWorkHistory"}, {ID: "pending", Name: "workbenchHost"}}},
		original,
		{Role: "tool", Name: "readSecret", ToolCallId: "secret", Content: "PRIVATE-SECRET"},
		{Role: "tool", Name: "recallWorkHistory", ToolCallId: "recall", Content: "RECALLED-CLAIM"},
		{Role: "tool", Name: "workbenchHost", ToolCallId: "pending", Content: common.UnexecutedWorkToolResult},
		{Role: "tool", Name: "workbenchHost", ToolCallId: "not-paired", Content: "UNPAIRED-RESULT"},
	})
	for n := range 20 {
		call := fmt.Sprintf("recent-%d", n)
		writeReceipt("v1:work:observation:"+uniqueSuffix(fmt.Sprintf("recent-%d", n)), prior, access.UserId, []common.ChatMessage{{Role: "assistant", ToolCalls: []common.ToolCall{{ID: call, Name: "workbenchHost"}}}, {Role: "tool", Name: "workbenchHost", ToolCallId: call, Content: "Recent capacity refusal"}})
	}
	preview, err := b.workSourceContextBuiltin(ctx, map[string]any{"conversationId": conversation, "turnId": "current"}, 0)
	require.NoError(t, err)
	require.NotContains(t, string(preview[0].Payload), "EXACT-ORIGINAL", "the original is outside the short preview")
	require.Contains(t, string(preview[0].Payload), `"sourceRunId":"`+BareShortId(prior)+`"`, "retrieval must remain discoverable even when original sources are omitted")
	var foundCheckpoint string
	foundIndex, pages := 0, 0
	for cursor := ""; ; {
		result, err := b.recallWorkHistoryBuiltin(ctx, map[string]any{"sourceRunId": BareShortId(prior), "search": "EXACT-ORIGINAL", "cursor": cursor}, 0)
		require.NoError(t, err)
		var page struct {
			Matches []struct {
				Checkpoint string
				Index      int
			}
			Cursor string
		}
		require.NoError(t, json.Unmarshal(result[0].Payload, &page))
		if len(page.Matches) > 0 {
			foundCheckpoint, foundIndex = page.Matches[0].Checkpoint, page.Matches[0].Index
		}
		pages++
		require.Less(t, pages, 20)
		if page.Cursor == "" {
			break
		}
		require.NotEqual(t, cursor, page.Cursor)
		cursor = page.Cursor
	}
	require.Greater(t, pages, 4, "must search beyond the former 16-receipt preview ceiling")
	require.Equal(t, BareShortId(originalID), foundCheckpoint)
	require.Equal(t, 1, foundIndex)
	var recovered strings.Builder
	for offset := 0; ; {
		args := map[string]any{"sourceRunId": prior, "checkpoint": foundCheckpoint, "messageIndex": foundIndex, "offset": offset, "maxChars": 4000}
		result, err := b.recallWorkHistoryBuiltin(ctx, args, 0)
		require.NoError(t, err)
		wire, err := json.Marshal(map[string]any{"nodes": result})
		require.NoError(t, err)
		require.NoError(t, b.RecordDelegatedToolSource(WithActingAgentId(ctx, "coding-agent"), "recallWorkHistory", args, string(wire)))
		text := string(result[0].Payload)
		for _, private := range []string{"PRIVATE-SYSTEM", "PRIVATE-REASONING", "PRIVATE-SECRET", "RECALLED-CLAIM", "UNPAIRED-RESULT"} {
			require.NotContains(t, text, private)
		}
		require.Less(t, len(text), 20000)
		var page struct {
			Matches []struct {
				Source     string
				NextOffset int
			}
		}
		require.NoError(t, json.Unmarshal(result[0].Payload, &page))
		require.Len(t, page.Matches, 1)
		recovered.WriteString(page.Matches[0].Source)
		if page.Matches[0].NextOffset == 0 {
			break
		}
		require.Greater(t, page.Matches[0].NextOffset, offset)
		offset = page.Matches[0].NextOffset
	}
	want, _ := json.Marshal(original)
	require.Equal(t, string(want), recovered.String(), "paged source remains byte-for-byte reconstructable")
	// Newer file checks must not push the earlier recovery out of review's
	// first receipt page. The finishing replica has none of the reader's state.
	for n := range 18 {
		require.NoError(t, b.RecordDelegatedToolSource(WithActingAgentId(ctx, "coding-agent"), "workbenchHost", map[string]any{"action": "fs_stat", "args": map[string]any{"path": fmt.Sprintf("output/file-%d", n)}}, `{"exists":true}`))
	}
	model := &replyReviewModel{}
	a.SetAIResolver(testAIResolver{fn: func(context.Context, airoute.ResolveRequest) (ResolvedProvider, error) {
		return ResolvedProvider{Client: model, Resolution: airoute.Resolution{ProviderName: "fleet:review", Model: "test"}}, nil
	}})
	_, err = a.workReviewReplyBuiltin(ctx, map[string]any{"draft": map[string]any{"reply": "The source was recovered."}}, 0)
	require.NoError(t, err)
	rendered, _ := json.Marshal(model.messages)
	require.Contains(t, string(rendered), `\"recoveredSources\"`)
	require.Contains(t, string(rendered), `\"complete\":true`)
	require.Contains(t, string(rendered), `\"path\":\"events.csv\"`)
	require.NotContains(t, string(rendered), "PRIVATE-REASONING")
	for _, denied := range []string{unrelated, later, foreign} {
		_, err := b.recallWorkHistoryBuiltin(ctx, map[string]any{"sourceRunId": denied, "checkpoint": foundCheckpoint}, 0)
		require.Error(t, err, "a guessed source ID cannot grant authority")
	}
	_, err = b.recallWorkHistoryBuiltin(common.ContextWithRun(askTestActor(), run), map[string]any{"sourceRunId": prior}, 0)
	require.Error(t, err)
	otherID := "v1:work:observation:" + uniqueSuffix("other")
	writeReceipt(otherID, unrelated, access.UserId, []common.ChatMessage{{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "other", Name: "workbenchHost"}}}, {Role: "tool", Name: "workbenchHost", ToolCallId: "other", Content: "UNRELATED-CONTENT"}})
	result, err := b.recallWorkHistoryBuiltin(ctx, map[string]any{"sourceRunId": prior, "checkpoint": BareShortId(otherID)}, 0)
	require.NoError(t, err)
	require.Contains(t, string(result[0].Payload), `"matches":[]`)
	require.NotContains(t, string(result[0].Payload), "UNRELATED-CONTENT")
}
