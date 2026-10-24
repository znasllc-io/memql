package memql

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

func TestLegacyOutcomeRecoveryReadsExactOwnedSessionSourcesOnPeer(t *testing.T) {
	a, b, ctx, _, runID, write := guidanceFixture(t)
	write("createWorkGoal", map[string]any{"goalId": "fixture-goal", "statement": "Save the requested output", "origin": "user"})
	runtime := WithActingAgentId(guidanceRunContext(ctx, runID), "assistant")
	run, _ := common.RunFromContext(runtime)
	source := []common.ChatMessage{{Role: "system", Content: "Native instructions"}, {Role: "user", Content: "Save once"}}
	scope := common.DelegatedTools{Run: run, AgentID: "assistant", AgentRole: "assistant", Messages: source, Tools: []common.ToolDefinition{{Name: common.DelegatedResponseTool}}}
	raw, err := json.Marshal(scope)
	require.NoError(t, err)
	var scoped map[string]any
	require.NoError(t, json.Unmarshal(raw, &scoped))
	session := id.NewShortId()
	start := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	write("createAppSession", map[string]any{"sessionId": session, "workerId": "worker", "app": "codex", "kind": "run", "runId": runID, "stepId": run.StepKey, "workspace": "/work", "prompt": "Private App prompt must not be used", "toolScope": scoped, "startedAt": start})
	require.NoError(t, a.RecordDelegatedToolSource(ContextWithDelegatedSession(runtime, session), "saveFile", map[string]any{"path": "result.txt"}, `{"saved":"result.txt","verified":true}`))
	write("endAppSession", map[string]any{"sessionId": session, "status": "ended", "exitCode": 0, "endedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	got, err := b.ReadLegacyWorkOutcome(runtime)
	require.NoError(t, err)
	require.Nil(t, got, "ordinary turns cannot borrow prior production")
	write("updateWorkRun", map[string]any{"runId": runID, "errorMessage": `function "runAgentTurn" execution failed: runAgentTurn: invalid delegated work outcome: use respondToUser`})
	got, err = b.ReadLegacyWorkOutcome(runtime)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, session, BareShortId(got.SessionID))
	require.Equal(t, "{}", got.Draft)
	require.Len(t, got.Messages, 4)
	require.Equal(t, source, got.Messages[:2])
	require.Contains(t, got.Messages[3].Content, "verified")
	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "Private App prompt")
	foreign := common.ContextWithRun(auth.ContextWithUserActor(ctx, "stranger"), run)
	_, err = b.ReadLegacyWorkOutcome(foreign)
	require.Error(t, err)
	write("updateWorkRun", map[string]any{"runId": runID, "cancelRequested": true})
	_, err = b.ReadLegacyWorkOutcome(runtime)
	require.Error(t, err)
	write("updateWorkRun", map[string]any{"runId": runID, "cancelRequested": false, "rerun": map[string]any{"requestId": "new", "stepKey": run.StepKey, "override": map[string]any{}}})
	run.RerunRequestId = "new"
	_, err = b.ReadLegacyWorkOutcome(common.ContextWithRun(runtime, run))
	require.ErrorContains(t, err, "binding unavailable")
}
