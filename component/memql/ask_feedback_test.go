package memql

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

func TestAskFeedbackDistinguishesInformationFromRecovery(t *testing.T) {
	row := map[string]any{"id": "q", "kind": "feedback", "question": "What is your preference?", "subject": map[string]any{"kind": "choice"}, "options": []map[string]any{{"value": "a", "label": "First"}}, "decision": ""}
	q, a := askFeedbackProjection(row)
	require.NotNil(t, q)
	require.Nil(t, a)
	row["subject"] = map[string]any{"symptom": "transient"}
	q, a = askFeedbackProjection(row)
	require.Nil(t, q)
	require.Nil(t, a)
	row["subject"] = map[string]any{"kind": "choice"}
	row["decision"] = "answered"
	row["answer"] = map[string]any{"text": "My own answer"}
	row["decidedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
	q, a = askFeedbackProjection(row)
	require.Nil(t, q)
	require.NotNil(t, a)
	require.Contains(t, a.Prompt, "My own answer")
	require.True(t, a.AnswerOnly)
}

func TestAskWorkConversationAndContinuationSurviveReplicaChanges(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	ctx := askTestActor()
	ac, _ := auth.AccessFromContext(ctx)
	runID, goalID := id.NewShortId(), id.NewShortId()
	write := func(name string, args map[string]any) {
		t.Helper()
		call, err := parser.RenderCall(name, args)
		require.NoError(t, err)
		_, err = a.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
		require.NoError(t, err)
	}
	write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Find a preference", "origin": "user"})
	write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "feedback", "templateFingerprint": "test", "triggeredBy": "manual", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	first, err := a.askWorkConversationBuiltin(ctx, map[string]any{"runId": runID}, 0)
	require.NoError(t, err)
	second, err := b.askWorkConversationBuiltin(ctx, map[string]any{"runId": runID}, 0)
	require.NoError(t, err)
	require.Equal(t, first[0].ID, second[0].ID)
	_, err = b.askWorkConversationBuiltin(askTestActor(), map[string]any{"runId": runID}, 0)
	require.Error(t, err)
	row, err := b.askRead(ctx, first[0].ID)
	require.NoError(t, err)
	raw, _ := json.Marshal(row["transcript"])
	var transcript askTranscript
	require.NoError(t, json.Unmarshal(raw, &transcript))
	require.Len(t, transcript.Turns, 1)
	write("updateWorkRun", map[string]any{"runId": runID, "input": map[string]any{"conversation": map[string]any{"id": first[0].ID}}})
	origin, err := a.askWorkConversationBuiltin(ctx, map[string]any{"runId": runID}, 0)
	require.NoError(t, err)
	require.Equal(t, first[0].ID, origin[0].ID)
	runCtx := common.ContextWithRun(ctx, common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: ac.UserId, StepKey: "reason"})
	messages := []common.ChatMessage{{Role: "system", Content: "Old profile"}, {Role: "user", Content: "Find a preference"}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: "call", Name: "read", Arguments: "{}"}}}, {Role: "tool", ToolCallId: "call", Name: "read", Content: "No matching record"}}
	require.NoError(t, a.SaveWorkContinuation(runCtx, messages))
	restored, err := b.RestoreWorkContinuation(runCtx, []common.ChatMessage{{Role: "system", Content: "Current profile"}})
	require.NoError(t, err)
	require.Equal(t, "Current profile", restored[0].Content)
	require.Equal(t, messages[1:], restored[1:])
	require.NotContains(t, fmt.Sprint(restored), "Old profile")
}

func TestAskTextQuestionAlwaysSerializesAnOptionsArray(t *testing.T) {
	for _, options := range []any{nil, []any{}} {
		q, answer := askFeedbackProjection(map[string]any{"id": "q", "kind": "feedback", "question": "Which supplier?", "subject": map[string]any{"kind": "text"}, "options": options})
		require.NotNil(t, q)
		require.Nil(t, answer)
		raw, err := json.Marshal(q)
		require.NoError(t, err)
		require.Contains(t, string(raw), `"options":[]`)
	}
}

func TestAskHumanAnswerReplacesDraftAcrossReplicasAndSavedHistory(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	ctx := askTestActor()
	ac, _ := auth.AccessFromContext(ctx)
	conversation := askTestConversation(t, a, ctx)
	runID, approvalID := id.NewShortId(), id.NewShortId()
	const draft = "I don't have a record of the synthetic project's launch month."
	const reply = "Recorded: the synthetic project launches in November."
	const ack = "I'll check the project notes."
	write := func(e *MemQLEngine, name string, args map[string]any) {
		t.Helper()
		call, err := parser.RenderCall(name, args)
		require.NoError(t, err)
		_, err = e.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
		require.NoError(t, err)
	}
	write(a, "createWorkRun", map[string]any{"runId": runID, "goalId": "goal", "automationName": "feedback", "templateFingerprint": "test", "triggeredBy": "manual", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	turn := AskTurn{ID: "request", RunID: runID, Prompt: "Which month is the synthetic project launching?", State: "queued", Background: true, Acknowledgement: ack, Answer: ack}
	require.NoError(t, a.askSave(ctx, conversation, "Launch month", askTranscript{Turns: []AskTurn{turn}}))
	runCtx := common.ContextWithRun(ctx, common.RunContext{RunId: runID, GoalId: "goal", OwnerUserId: ac.UserId, StepKey: "reason"})
	require.NoError(t, a.RecordWorkProgress(runCtx, WorkEvent{ID: "before-question", Kind: "response", Phase: "streaming", Text: draft}))
	write(a, "createWorkApproval", map[string]any{"approvalId": approvalID, "runId": runID, "stepKey": "reason", "kind": "feedback", "subject": map[string]any{"kind": "text"}, "artifactHash": "question", "question": "What is the launch month?", "requestedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	write(a, "updateWorkRun", map[string]any{"runId": runID, "status": "waiting", "waitingOn": map[string]any{"kind": "feedback", "subject": approvalID}})
	read := func(e *MemQLEngine) askTranscript {
		t.Helper()
		nodes, err := e.askConversationSnapshotBuiltin(ctx, map[string]any{"conversationId": conversation}, 0)
		require.NoError(t, err)
		var result struct {
			Transcript askTranscript `json:"transcript"`
		}
		require.NoError(t, json.Unmarshal(nodes[0].Payload, &result))
		return result.Transcript
	}
	waiting := read(b)
	require.Equal(t, "waiting", waiting.Turns[0].State)
	require.Equal(t, ack, waiting.Turns[0].Answer)
	require.NotNil(t, waiting.Turns[0].Question)
	// The answer and successful continuation land on a different engine.
	write(b, "decideWorkApproval", map[string]any{"approvalId": approvalID, "decision": "answered", "decidedBy": ac.UserId, "decidedAt": time.Now().UTC().Format(time.RFC3339Nano), "answer": map[string]any{"text": "November — synthetic test data."}})
	require.NoError(t, b.RecordWorkProgress(runCtx, WorkEvent{ID: "after-answer", Kind: "response", Phase: "completed", Text: reply}))
	write(b, "createWorkStep", map[string]any{"stepId": id.NewShortId(), "runId": runID, "key": "reason", "seq": 1, "stepType": "call", "status": "done", "attempt": 2, "result": responseStep(1, "done", reply)["result"]})
	write(b, "updateWorkRun", map[string]any{"runId": runID, "status": "succeeded", "finishedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	completed := read(a)
	require.Len(t, completed.Turns, 2)
	require.Equal(t, "done", completed.Turns[0].State)
	require.Equal(t, reply, completed.Turns[0].Answer)
	require.Nil(t, completed.Turns[0].Question)
	require.Contains(t, completed.Turns[1].Prompt, "November")
	require.True(t, completed.Turns[1].AnswerOnly)
	// Repair an already-saved aggregate too, including duplicate delivery and
	// the source text used by exact/semantic recall and subsequent messages.
	completed.Turns[0].Answer = draft + reply
	staleHash := conversationSourceHash(completed.Turns[0])
	require.NoError(t, a.askSave(ctx, conversation, "Launch month", completed))
	refreshed := read(b)
	require.Equal(t, reply, refreshed.Turns[0].Answer)
	require.NotEqual(t, staleHash, conversationSourceHash(refreshed.Turns[0]), "a previously indexed aggregate must fail source-hash validation")
	replayed, err := b.RunAsk(ctx, conversation, "request", "Duplicate request", "", AskRoute{}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, reply, replayed)
	memory, err := b.workSearchConversationsBuiltin(ctx, map[string]any{"search": "synthetic project"}, 0)
	require.NoError(t, err)
	require.NotContains(t, string(memory[0].Payload), draft)
	require.Contains(t, string(memory[0].Payload), reply)
	_, err = b.askConversationSnapshotBuiltin(askTestActor(), map[string]any{"conversationId": conversation}, 0)
	require.Error(t, err)
}
