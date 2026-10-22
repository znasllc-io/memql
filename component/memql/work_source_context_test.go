package memql

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	delegatedCtx := WithActingAgentId(common.ContextWithRun(ctx, common.RunContext{RunId: runID, OwnerUserId: access.UserId, StepKey: "coding"}), "coding-agent")
	require.NoError(t, a.RecordDelegatedToolSource(delegatedCtx, "workbenchHost", map[string]any{"action": "exec"}, `{"stdout":"Ran 12 tests: OK; LEDGER-CONTEXT-42","exitCode":0}`))
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
	require.Contains(t, text, "Ran 12 tests: OK; LEDGER-CONTEXT-42")
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

func TestDelegatedSourceReceiptsDoNotReplaceContinuationAndRequireOwnedRun(t *testing.T) {
	e, _, _ := readMergeTestEngine(t)
	ctx := WithActingAgentId(contextTestRun(t, e), "coding-agent")
	run, _ := common.RunFromContext(ctx)
	original := []common.ChatMessage{{Role: "user", Content: "Preserve the complete original request"}}
	require.NoError(t, e.RecordDelegatedToolSource(ctx, "workbenchHost", map[string]any{"action": "exec"}, `{"exitCode":0}`))
	restored, err := e.RestoreWorkContinuation(ctx, original)
	require.NoError(t, err)
	require.Equal(t, original, restored, "a source receipt must never replace resumable agent state")
	foreign := common.ContextWithRun(askTestActor(), run)
	foreign = WithActingAgentId(foreign, "coding-agent")
	require.Error(t, e.RecordDelegatedToolSource(foreign, "workbenchHost", nil, "private"))
	rows, err := e.workSourceReceipts(foreign, run.RunId)
	require.NoError(t, err)
	require.Empty(t, rows)
	require.NoError(t, e.RecordDelegatedToolSource(ctx, "workbenchHost", nil, strings.Repeat("x", 1024*1024+1)))
	rows, err = e.workSourceReceipts(ctx, run.RunId)
	require.NoError(t, err)
	remaining := replyReviewMaxBytes
	sources, omitted := workCheckpointSources(rows, run.RunId, &remaining)
	require.True(t, omitted, "oversized evidence must be explicitly unavailable")
	require.Len(t, sources, 1)
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
	value, err := json.Marshal(sources[0]["value"])
	require.NoError(t, err)
	require.JSONEq(t, messages[1].Content, string(value), "preserve old observation time, different scope and truncation; persistence must not make them fresh")
	require.Equal(t, "2026-10-17", sources[0]["checkpointAt"])
	require.Positive(t, remaining)
}

func TestCheckpointSourceProjectionExcludesWrappedRetrievals(t *testing.T) {
	messages := []common.ChatMessage{}
	for i, call := range []common.ToolCall{
		{Name: "workbenchHost", Arguments: `{"action":"fs_read","args":{"path":"design.md"}}`},
		{Name: "workSourceContext"},
		{Name: "executeCapability", Arguments: `{"name":"work.workSourceContext","arguments":{}}`},
		{Name: "executeCapability", Arguments: `{"name":"work.recallWorkHistory","arguments":{}}`},
		{Name: "executeCapability", Arguments: `{"name":"work.workReviewReply","arguments":{}}`},
		{Name: "executeCapability", Arguments: `{"name":"vault.readSecret","arguments":{}}`},
		{Name: "executeCapability", Arguments: `{"name":"library.libraryReadFile","arguments":{"fileId":"source"}}`},
	} {
		call.ID = fmt.Sprint(i)
		messages = append(messages,
			common.ChatMessage{Role: "assistant", ToolCalls: []common.ToolCall{call}},
			common.ChatMessage{Role: "tool", Name: call.Name, ToolCallId: call.ID, Content: "recorded result"})
	}
	raw, err := json.Marshal(messages)
	require.NoError(t, err)
	// Use the same encoded representation as delegated receipts.
	checkpoint := map[string]any{"data": map[string]any{"messagesEncoding": "json-base64-v1", "messages": base64.StdEncoding.EncodeToString(raw)}}
	pairs, omitted := workCheckpointPairs(checkpoint)
	require.False(t, omitted)
	require.Len(t, pairs, 2, "source previews must not nest earlier retrievals or generated/private content through a capability wrapper")
	require.JSONEq(t, `{"name":"library.libraryReadFile","arguments":{"fileId":"source"}}`, pairs[0].call.Arguments)
	require.Equal(t, 0, pairs[0].index)
	require.Equal(t, "workbenchHost", pairs[1].call.Name)
	require.Equal(t, 12, pairs[1].index, "older saved message indices must retain the same source")
}

func TestCheckpointSourceProjectionRetainsJSONWithoutDoubleEscaping(t *testing.T) {
	inner, err := json.Marshal(map[string]any{
		"exitCode": 0, "stderr": "",
		"stdout": strings.Repeat("{\"id\":\"e06\",\"cents\":8000}\n", 130) + "Ran 10 tests: OK",
	})
	require.NoError(t, err)
	result, err := json.Marshal(map[string]any{
		"content":  []any{map[string]any{"type": "text", "text": string(inner)}},
		"sequence": int64(9007199254740993), "isError": false,
	})
	require.NoError(t, err)
	messages := []common.ChatMessage{
		{Role: "assistant", Content: "PRIVATE_REASONING", ToolCalls: []common.ToolCall{{ID: "exec", Name: "workbenchHost", Arguments: strings.Repeat("large script", 1000)}}},
		{Role: "tool", Name: "workbenchHost", ToolCallId: "exec", Content: string(result)},
	}
	encoded, err := json.Marshal(messages)
	require.NoError(t, err)
	checkpoint := map[string]any{"id": "receipt", "data": map[string]any{"messagesEncoding": "json-base64-v1", "messages": base64.StdEncoding.EncodeToString(encoded)}}
	remaining := replyReviewMaxBytes
	sources, omitted := workCheckpointSources([]map[string]any{checkpoint}, "run", &remaining)
	require.False(t, omitted, "JSON escaping overhead must not hide an execution receipt that fits the preview")
	require.Len(t, sources, 1)
	require.Equal(t, true, sources[0]["argumentsOmitted"])
	projected, err := json.Marshal(sources[0]["value"])
	require.NoError(t, err)
	require.Contains(t, string(projected), "9007199254740993", "do not round source numbers")
	require.Contains(t, string(projected), "Ran 10 tests: OK")
	require.NotContains(t, string(projected), "PRIVATE_REASONING")
	// The compact transport projection must reconstruct the exact JSON values.
	preview := workSourcePreviewValue(string(result)).(map[string]any)
	block := preview["content"].([]any)[0].(map[string]any)
	textJSON, err := json.Marshal(block["textJSON"])
	require.NoError(t, err)
	block["text"] = string(textJSON)
	delete(block, "textJSON")
	reconstructed, err := json.Marshal(preview)
	require.NoError(t, err)
	require.JSONEq(t, string(result), string(reconstructed))
	raw, err := json.Marshal(sources[0])
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw), 8000)
	require.Equal(t, replyReviewMaxBytes-len(raw), remaining)
	remaining = len(raw) - 1
	sources, omitted = workCheckpointSources([]map[string]any{checkpoint}, "run", &remaining)
	require.True(t, omitted)
	require.Empty(t, sources, "the existing total preview budget still applies")
}

func TestSourcePreviewPreservesTransportErrorsAndOpaqueText(t *testing.T) {
	value := workSourcePreviewValue(`{"isError":true,"content":[{"type":"text","text":"{\"truncated\":true,\"exitCode\":1,\"stdout\":\"{not nested JSON}\"}"},{"type":"text","text":"opaque prose"},{"type":"image","data":"opaque image"},{"type":"text","text":"{}","textJSON":"preexisting"}]}`).(map[string]any)
	require.Equal(t, true, value["isError"])
	blocks := value["content"].([]any)
	result := blocks[0].(map[string]any)["textJSON"].(map[string]any)
	require.Equal(t, true, result["truncated"])
	require.Equal(t, json.Number("1"), result["exitCode"])
	require.Equal(t, "{not nested JSON}", result["stdout"])
	require.Equal(t, "opaque prose", blocks[1].(map[string]any)["text"])
	require.Equal(t, "opaque image", blocks[2].(map[string]any)["data"])
	require.Equal(t, "{}", blocks[3].(map[string]any)["text"])
	require.Equal(t, "preexisting", blocks[3].(map[string]any)["textJSON"])
}
