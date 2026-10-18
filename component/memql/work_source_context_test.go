package memql

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
)

func TestSourceContextReadsRecordedEvidenceAcrossReplicasAndPreservesOwnerScope(t *testing.T) {
	a, db, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := askTestActor()
	access, _ := auth.AccessFromContext(ctx)
	conversation := askTestConversation(t, a, ctx)
	runID := "v1:work:run:" + uniqueSuffix("source")
	foreignID := "v1:work:run:" + uniqueSuffix("foreign")
	insert := func(concept, id string, payload map[string]any) {
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		_, err = db.DB.ExecContext(ctx, `INSERT INTO "MemoryNodes"(id,concept,"createdAt","createdBy",schema,payload) VALUES( $1,$2,$3,'source-test','{}',$4)`, id, concept, time.Now().UTC(), string(raw))
		require.NoError(t, err)
	}
	insert("v1:work:run", runID, map[string]any{"ownerUserId": access.UserId, "status": "succeeded"})
	insert("v1:work:run", foreignID, map[string]any{"ownerUserId": "v1:identity:user:another", "status": "succeeded"})
	insert("v1:work:step", "v1:work:step:"+uniqueSuffix("receipt"), map[string]any{"ownerUserId": access.UserId, "runId": runID, "key": "viewer", "status": "done", "call": map[string]any{"name": "workViewerContext"}, "result": map[string]any{"value": map[string]any{"person": map[string]any{"primaryRole": "Founder"}, "organizationMemberships": []any{map[string]any{"name": "ZNAS LLC"}}, "source": "recorded authenticated profile"}}})
	insert("v1:work:step", "v1:work:step:"+uniqueSuffix("generation"), map[string]any{"ownerUserId": access.UserId, "runId": runID, "key": "reason", "status": "done", "call": map[string]any{"name": "ai"}, "result": map[string]any{"value": "Generated answer is not evidence"}})
	insert("v1:work:step", "v1:work:step:"+uniqueSuffix("review"), map[string]any{"ownerUserId": access.UserId, "runId": runID, "key": "review", "status": "done", "call": map[string]any{"name": "workReviewReply"}, "result": map[string]any{"value": "Reviewed prose is not evidence"}})
	insert("v1:work:step", "v1:work:step:"+uniqueSuffix("foreign-receipt"), map[string]any{"ownerUserId": "v1:identity:user:another", "runId": foreignID, "key": "viewer", "status": "done", "call": map[string]any{"name": "workViewerContext"}, "result": map[string]any{"value": "Foreign private evidence"}})
	checkpoint := func(record, owner, run string) {
		messages := []common.ChatMessage{
			{Role: "system", Content: "private system content"},
			{Role: "assistant", Content: "MODEL_REASONING_DO_NOT_EXPORT", ToolCalls: []common.ToolCall{{ID: "source-call", Name: "readStatus", Arguments: `{"entity":"service-A"}`}}},
			{Role: "tool", Name: "readStatus", ToolCallId: "source-call", Content: `{"status":"degraded","observedAt":"2026-10-17T04:39:30Z","timezone":"America/Phoenix","source":"https://status.example.test"}`},
		}
		encoded, err := json.Marshal(messages)
		require.NoError(t, err)
		insert("v1:work:observation", "v1:work:observation:"+record, map[string]any{"ownerUserId": owner, "runId": run, "kind": "note", "data": map[string]any{"continuation": true, "messagesEncoding": "json-base64-v1", "messages": base64.StdEncoding.EncodeToString(encoded)}})
	}
	checkpoint(uniqueSuffix("tools"), access.UserId, runID)
	checkpoint(uniqueSuffix("foreign-tools"), "v1:identity:user:another", runID)
	require.NoError(t, a.askSave(ctx, conversation, "Source explanation", askTranscript{Turns: []AskTurn{
		{ID: "prior", Prompt: "How can you help?", Answer: "Founder of ZNAS LLC", State: "done", RunID: runID, StartedAt: time.Now()},
		{ID: "unowned", Prompt: "Forged run", RunID: foreignID, State: "done", StartedAt: time.Now()},
		{ID: "current", Prompt: "Explain the source in a file", State: "running", StartedAt: time.Now()},
		{ID: "later", Prompt: "Later context must not enter", RunID: foreignID, State: "done", StartedAt: time.Now()},
	}}))
	nodes, err := b.workSourceContextBuiltin(ctx, map[string]any{"conversationId": conversation, "turnId": "current"}, 0)
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	text := string(nodes[0].Payload)
	require.Contains(t, text, "Founder")
	require.Contains(t, text, "ZNAS LLC")
	require.Contains(t, text, "workStepForOwnerRun")
	require.Contains(t, text, "recorded authenticated profile")
	require.NotContains(t, text, "Generated answer is not evidence")
	require.NotContains(t, text, "Reviewed prose is not evidence")
	require.NotContains(t, text, "Foreign private evidence")
	require.NotContains(t, text, foreignID)
	require.Contains(t, text, "2026-10-17T04:39:30Z")
	var projected struct {
		Clock struct {
			CurrentTimeUTC   string
			SourceZoneClocks map[string]string
		}
	}
	require.NoError(t, json.Unmarshal(nodes[0].Payload, &projected))
	current, err := time.Parse(time.RFC3339, projected.Clock.CurrentTimeUTC)
	require.NoError(t, err)
	zoned, err := time.Parse(time.RFC3339, projected.Clock.SourceZoneClocks["America/Phoenix"])
	require.NoError(t, err)
	require.True(t, current.Equal(zoned), "receiving replica must supply consistent current clocks")
	require.Contains(t, text, "service-A")
	require.Equal(t, 1, strings.Count(text, "source-call"))
	require.NotContains(t, text, "private system content")
	require.NotContains(t, text, "MODEL_REASONING_DO_NOT_EXPORT")
	input := b.askConversationInput(ctx, conversation, "current", "", []map[string]any{{"role": "assistant", "content": "An unsupported answer"}})
	carried, err := json.Marshal(input)
	require.NoError(t, err)
	require.Contains(t, string(carried), "sourceContext")
	require.Contains(t, string(carried), "sourceZoneClocks")
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(nodes[0].Payload, &decoded))
	ref := decoded["references"].([]any)[0].(map[string]any)["ref"].(string)
	resolved, resolveErr := b.Execute(ctx, "query "+ref)
	require.NoError(t, resolveErr)
	resolvedJSON, err := json.Marshal(MaterializeRows(resolved.OutputPayload()))
	require.NoError(t, err)
	require.Contains(t, string(resolvedJSON), "status.example.test")
	require.NotContains(t, string(resolvedJSON), "MODEL_REASONING_DO_NOT_EXPORT")
	require.Contains(t, string(carried), "https://status.example.test")
	require.Contains(t, string(carried), "2026-10-17T04:39:30Z")
	require.NotContains(t, string(carried), "MODEL_REASONING_DO_NOT_EXPORT")
	denied := b.askConversationInput(askTestActor(), conversation, "current", "", []map[string]any{{"role": "user", "content": runID}})
	deniedJSON, _ := json.Marshal(denied["sourceContext"])
	require.NotContains(t, string(deniedJSON), "status.example.test")
	require.Contains(t, string(deniedJSON), "could not be read")
	_, err = b.workSourceContextBuiltin(askTestActor(), map[string]any{"conversationId": conversation, "turnId": "current"}, 0)
	require.Error(t, err)
	_, err = b.workSourceContextBuiltin(ctx, map[string]any{"conversationId": conversation, "turnId": "missing"}, 0)
	require.Error(t, err)
}

func TestExplicitMissingEvidenceAnswerWinsOverIntermediateAssessment(t *testing.T) {
	reply := "I could not find the source of that earlier claim, so I have not created a file."
	run := map[string]any{"outcome": map[string]any{"returned": map[string]any{"reply": reply}}}
	steps := []map[string]any{{"status": "done", "seq": 1, "result": map[string]any{"value": map[string]any{"reply": "internal JSON", "structured": map[string]any{"ready": false}}}}}
	require.Equal(t, reply, workResultText(run, steps, ""))
}

func TestCheckpointSourceProjectionBoundsDeduplicatesAndExcludesProse(t *testing.T) {
	messages := []common.ChatMessage{
		{Role: "assistant", Content: "not evidence", ToolCalls: []common.ToolCall{{ID: "ok", Name: "fetch", Arguments: `{"url":"https://example.test"}`}, {ID: "huge", Name: "fetch"}, {ID: "unexecuted", Name: "fetch"}, {ID: "review", Name: "workReviewReply"}}},
		{Role: "tool", Name: "fetch", ToolCallId: "ok", Content: `{"observedAt":"2020-01-01","location":"elsewhere","truncated":true}`},
		{Role: "tool", Name: "fetch", ToolCallId: "huge", Content: strings.Repeat("x", 9000)},
		{Role: "tool", Name: "fetch", ToolCallId: "unexecuted", Content: common.UnexecutedWorkToolResult},
		{Role: "tool", Name: "fetch", ToolCallId: "unpaired", Content: "fabricated"},
		{Role: "tool", Name: "workReviewReply", ToolCallId: "review", Content: "Reviewed prose is not evidence"},
		{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "history", Name: "recallWorkHistory", Arguments: `{"checkpoint":"conversation"}`}}},
		{Role: "tool", Name: "recallWorkHistory", ToolCallId: "history", Content: `{"messages":[{"role":"assistant","content":"An unsupported earlier claim"}]}`},
	}
	encoded, err := json.Marshal(messages)
	require.NoError(t, err)
	checkpoint := map[string]any{"id": "receipt", "createdAt": "2026-10-17", "data": map[string]any{"messagesEncoding": "json-base64-v1", "messages": base64.StdEncoding.EncodeToString(encoded)}}
	remaining := 1000
	sources, omitted := workCheckpointSources([]map[string]any{checkpoint, checkpoint}, "run", &remaining)
	require.True(t, omitted)
	require.Len(t, sources, 1)
	require.Equal(t, messages[1].Content, sources[0]["value"], "preserve old observation time, different scope and truncation; persistence must not make them fresh")
	require.Equal(t, "2026-10-17", sources[0]["checkpointAt"])
	require.Positive(t, remaining)
}
