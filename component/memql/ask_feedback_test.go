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
