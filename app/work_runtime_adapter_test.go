package app

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/agent"
)

func TestAgentRuntimeAdapterPreservesTheEnginesOwnershipBarrier(t *testing.T) {
	var runtime agent.WorkRuntime = &CognitionEngineAdapter{Engine: &memql.MemQLEngine{}}
	ctx := common.ContextWithRun(context.Background(), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner", StepKey: "section"})
	if _, err := runtime.ReadWorkGuidance(ctx); err == nil || !strings.Contains(err.Error(), "owned work") {
		t.Fatalf("adapter lost human guidance ownership gate: %v", err)
	}
	if err := runtime.AcknowledgeWorkGuidance(ctx, []string{"receipt"}); err == nil || !strings.Contains(err.Error(), "owned work") {
		t.Fatalf("adapter lost guidance receipt ownership gate: %v", err)
	}
	if err := runtime.PrepareWorkTool(ctx); err == nil || !strings.Contains(err.Error(), "owned work") {
		t.Fatalf("adapter lost workload ownership gate: %v", err)
	}
	if err := runtime.SaveWorkContinuation(ctx, nil); err == nil || !strings.Contains(err.Error(), "owned work") {
		t.Fatalf("adapter lost continuation ownership gate: %v", err)
	}
	if err := runtime.DelegatedWorkHumanWait(ctx, "session"); err == nil || !strings.Contains(err.Error(), "owned work") {
		t.Fatalf("adapter lost delegated wait ownership gate: %v", err)
	}
	bounded, cancel, err := runtime.ContextWithWorkCallDeadline(context.Background())
	defer cancel()
	if err != nil || bounded.Err() != nil {
		t.Fatalf("unowned call deadline: %v", err)
	}
}

// The executing replica has no originating Ask request or in-memory queue.
// Exercise the production adapter, public DSL ingress and peer read together.
func testAgentRuntimeAdapterReceivesGuidanceFromAnotherReplica(t *testing.T, a, b *memql.MemQLEngine) {
	old := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(old) })
	owner := id.NewShortId()
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	execute := func(e *memql.MemQLEngine, kind, name string, args map[string]any) []map[string]any {
		t.Helper()
		call, err := parser.RenderCall(name, args)
		require.NoError(t, err)
		result, err := e.Execute(ctx, kind+" "+call)
		require.NoError(t, err)
		return memql.MaterializeRows(result)
	}
	conversation := fmt.Sprint(execute(a, "mutation", "createAskConversation", map[string]any{"requestId": id.NewShortId(), "title": "Adapter guidance fixture"})[0]["id"])
	run := id.NewShortId()
	call, err := parser.RenderCall("createWorkRun", map[string]any{"runId": run, "goalId": "adapter-goal", "automationName": "fixture", "templateFingerprint": "stable", "triggeredBy": "manual", "status": "running", "mode": "live", "input": map[string]any{"conversation": map[string]any{"id": conversation}}, "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	require.NoError(t, err)
	_, err = a.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
	require.NoError(t, err)
	origin := map[string]any{"id": "origin", "runId": run, "prompt": "Synthetic work", "state": "queued", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)}
	version := time.Now().UTC()
	save := func(turns ...any) {
		t.Helper()
		version = version.Add(time.Millisecond)
		call, err := parser.RenderCall("saveAskConversation", map[string]any{"id": conversation, "title": "Fixture", "transcript": map[string]any{"turns": turns}, "versionTime": version.Format(time.RFC3339Nano)})
		require.NoError(t, err)
		_, err = a.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
		require.NoError(t, err)
	}
	save(origin)
	runtime := &CognitionEngineAdapter{Engine: b}
	execution := common.ContextWithRun(ctx, common.RunContext{RunId: run, GoalId: "adapter-goal", OwnerUserId: owner, StepKey: "nested/agent"})
	prior, err := runtime.ReadWorkGuidance(execution)
	require.NoError(t, err)
	require.Empty(t, prior)
	// A routed message, in its wire form: a turn with no goal of its own whose
	// target names this run.
	save(origin, map[string]any{"id": "note", "prompt": "Use the completed evidence.", "state": "done", "startedAt": time.Now().UTC().Format(time.RFC3339Nano), "routed": map[string]any{"targets": []any{
		map[string]any{"goalId": "adapter-goal", "runId": run, "title": "Synthetic work", "status": "queued", "text": "Use the completed evidence."},
	}}})
	received, err := runtime.ReadWorkGuidance(execution)
	require.NoError(t, err)
	require.Equal(t, []memql.WorkGuidance{{ID: "note", Prompt: "Use the completed evidence."}}, received)
	require.NoError(t, runtime.AcknowledgeWorkGuidance(execution, []string{received[0].ID}))
	snapshot := execute(a, "builtin", "askConversationSnapshot", map[string]any{"conversationId": conversation})[0]
	encoded, err := json.Marshal(snapshot["transcript"])
	require.NoError(t, err)
	var projected struct {
		Turns []memql.AskTurn `json:"turns"`
	}
	require.NoError(t, json.Unmarshal(encoded, &projected))
	require.Len(t, projected.Turns, 2)
	require.Equal(t, "received", projected.Turns[1].Routed.Targets[0].Status, "the writing replica sees the receipt")
	restored, err := (&CognitionEngineAdapter{Engine: a}).ReadWorkGuidance(execution)
	require.NoError(t, err)
	require.Equal(t, received, restored)
}
