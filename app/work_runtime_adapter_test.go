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
	transcript := map[string]any{"turns": []any{map[string]any{"id": "origin", "runId": run, "prompt": "Synthetic work", "state": "queued", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)}}}
	call, err = parser.RenderCall("saveAskConversation", map[string]any{"id": conversation, "title": "Fixture", "transcript": transcript, "versionTime": time.Now().UTC().Add(time.Millisecond).Format(time.RFC3339Nano)})
	require.NoError(t, err)
	_, err = a.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
	require.NoError(t, err)
	snapshot := execute(a, "builtin", "askConversationSnapshot", map[string]any{"conversationId": conversation})[0]
	encoded, err := json.Marshal(snapshot["transcript"])
	require.NoError(t, err)
	var saved struct {
		Turns []memql.AskTurn `json:"turns"`
	}
	require.NoError(t, json.Unmarshal(encoded, &saved))
	require.True(t, saved.Turns[0].CanGuideWork)
	runtime := &CognitionEngineAdapter{Engine: b}
	execution := common.ContextWithRun(ctx, common.RunContext{RunId: run, GoalId: "adapter-goal", OwnerUserId: owner, StepKey: "nested/agent"})
	prior, err := runtime.ReadWorkGuidance(execution)
	require.NoError(t, err)
	require.Empty(t, prior)
	result := execute(a, "builtin", "askGuideWork", map[string]any{"runId": run, "conversationId": conversation, "targetEpoch": saved.Turns[0].GuidanceEpoch, "requestId": "note", "prompt": "Use the completed evidence."})
	require.Equal(t, "queued", result[0]["status"])
	received, err := runtime.ReadWorkGuidance(execution)
	require.NoError(t, err)
	require.Len(t, received, 1)
	require.NoError(t, runtime.AcknowledgeWorkGuidance(execution, []string{received[0].ID}))
	restored, err := (&CognitionEngineAdapter{Engine: a}).ReadWorkGuidance(execution)
	require.NoError(t, err)
	require.Equal(t, received, restored)
}
