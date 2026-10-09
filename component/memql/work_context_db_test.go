package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

func contextTestRun(t *testing.T, e *MemQLEngine) context.Context {
	t.Helper()
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	ctx := askTestActor()
	access, _ := auth.AccessFromContext(ctx)
	runID, goalID := id.NewShortId(), id.NewShortId()
	for _, write := range []struct {
		name string
		args map[string]any
	}{
		{"createWorkGoal", map[string]any{"goalId": goalID, "statement": "Research a document", "origin": "user"}},
		{"createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "research", "templateFingerprint": "test", "triggeredBy": "manual", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)}},
	} {
		call, err := parser.RenderCall(write.name, write.args)
		require.NoError(t, err)
		_, err = e.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
		require.NoError(t, err)
	}
	return common.ContextWithRun(ctx, common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: access.UserId, StepKey: "research"})
}

func contextTestModel(e *MemQLEngine, model *checkpointModel) {
	e.SetAIResolver(testAIResolver{fn: func(context.Context, airoute.ResolveRequest) (ResolvedProvider, error) {
		return ResolvedProvider{Client: model, Resolution: airoute.Resolution{ProviderName: "fleet:test", Model: "test"}}, nil
	}})
}

func TestWorkContextOffloadIsDurableAcrossReplicasAndRecallIsBounded(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, a)
	model := &checkpointModel{fail: true}
	contextTestModel(a, model)
	original := common.ChatMessage{Role: "tool", Name: "extractPDF", ToolCallId: "pdf", Content: strings.Repeat("文献 nanoparticle evidence ", 3000) + "exact-final-receipt"}
	messages := []common.ChatMessage{{Role: "system", Content: "Follow the owner"}, {Role: "user", Content: "Compare sources, preserve uncertainty"}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: "pdf", Name: "extractPDF", Arguments: `{"path":"paper.pdf"}`}}}, original}
	before, _ := json.Marshal(messages)
	compacted, err := a.CompactWorkContext(ctx, messages, nil, 2500)
	require.NoError(t, err)
	require.LessOrEqual(t, WorkContextSize(compacted, nil), 2500)
	require.Zero(t, model.calls, "offloading must not spend a model call")
	require.Equal(t, messages[:3], compacted[:3])
	require.Equal(t, original.ToolCallId, compacted[3].ToolCallId)
	after, _ := json.Marshal(messages)
	require.Equal(t, before, after, "compaction must not mutate a saved continuation")
	raw, _ := json.Marshal([]common.ChatMessage{original})
	hash := workContextHash("tool-v2", raw)
	var exact strings.Builder
	for offset := 0; ; {
		result, err := b.recallWorkHistoryBuiltin(ctx, map[string]any{"checkpoint": hash, "messageIndex": 0, "offset": offset, "maxChars": 4000}, 0)
		require.NoError(t, err)
		require.Less(t, len(result[0].Payload), 18000, "even Unicode results stay bounded")
		var payload struct {
			Matches []struct {
				Source     string
				NextOffset int
			}
		}
		require.NoError(t, json.Unmarshal(result[0].Payload, &payload))
		require.Len(t, payload.Matches, 1)
		page := payload.Matches[0]
		exact.WriteString(page.Source)
		if page.NextOffset == 0 {
			break
		}
		require.Greater(t, page.NextOffset, offset)
		offset = page.NextOffset
	}
	want, _ := json.Marshal(original)
	require.Equal(t, string(want), exact.String())
	run, _ := common.RunFromContext(ctx)
	_, err = b.recallWorkHistoryBuiltin(common.ContextWithRun(askTestActor(), run), map[string]any{"checkpoint": hash}, 0)
	require.Error(t, err, "a guessed hash grants no cross-owner access")
	other := contextTestRun(t, b)
	result, err := b.recallWorkHistoryBuiltin(other, map[string]any{"checkpoint": hash}, 0)
	require.NoError(t, err)
	require.Contains(t, string(result[0].Payload), `"matches":[]`, "archives do not leak to another run")
}

func TestWorkContextRepeatedPressureBoundsActiveMemoryAndKeepsSources(t *testing.T) {
	e, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, e)
	model := &checkpointModel{}
	contextTestModel(e, model)
	messages := contextResearchTrace()[:2]
	originalRequest := messages[1]
	for round := range 40 {
		callID := fmt.Sprintf("research-%d", round)
		messages = append(messages,
			common.ChatMessage{Role: "assistant", ToolCalls: []common.ToolCall{{ID: callID, Name: "research", Arguments: `{}`}}},
			common.ChatMessage{Role: "tool", ToolCallId: callID, Content: fmt.Sprintf("Receipt %d https://example.test/paper/%d %s", round, round, strings.Repeat("measured evidence ", 300))})
		var err error
		messages, err = e.CompactWorkContext(ctx, messages, nil, 3000)
		require.NoError(t, err, "cycle %d", round)
		require.LessOrEqual(t, WorkContextSize(messages, nil), 3000)
		require.Equal(t, originalRequest, messages[1])
		for _, unit := range workContextUnits(messages) {
			require.True(t, unit.complete)
		}
	}
	require.Positive(t, model.calls)
	require.Less(t, model.calls, 40, "do not summarize on every turn")
	// Search pages are bounded and cover old sources, rather than reading every
	// saved continuation into memory or pretending the first page is exhaustive.
	cursor, found, pages := "", false, 0
	for {
		result, err := e.recallWorkHistoryBuiltin(ctx, map[string]any{"search": "Receipt 0", "cursor": cursor}, 0)
		require.NoError(t, err)
		var response struct {
			Matches []any
			Cursor  string
		}
		require.NoError(t, json.Unmarshal(result[0].Payload, &response))
		found = found || len(response.Matches) > 0
		pages++
		require.Less(t, pages, 60)
		if response.Cursor == "" {
			break
		}
		require.NotEqual(t, cursor, response.Cursor)
		cursor = response.Cursor
	}
	require.True(t, found, "the first receipt must remain retrievable after repeated checkpoints")
	require.Greater(t, pages, 1)
}

func TestWorkContextFailureRetainsOriginalsAndPendingCalls(t *testing.T) {
	e, db, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, e)
	model := &checkpointModel{fail: true}
	contextTestModel(e, model)
	messages := contextResearchTrace()
	before, _ := json.Marshal(messages)
	_, err := e.CompactWorkContext(ctx, messages, nil, 1800)
	require.Error(t, err)
	after, _ := json.Marshal(messages)
	require.Equal(t, before, after)
	// Storage outage: the replacement must not escape without the source.
	messages[3].Content = strings.Repeat("uncommitted source ", 5000)
	require.NoError(t, db.Close())
	_, err = e.CompactWorkContext(ctx, messages, nil, 2500)
	require.Error(t, err)
	require.NotContains(t, messages[3].Content, "[Archived tool result")
}
