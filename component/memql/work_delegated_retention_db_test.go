package memql

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

func TestDelegatedFeedbackReferencesLargeDurableResultsAcrossReplicas(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := WithActingAgentId(contextTestRun(t, a), "visual-agent")
	run, _ := common.RunFromContext(ctx)
	sessionID := id.NewShortId()
	runMutation(t, auth.ContextWithInternalOrigin(ctx), a, "createAppSession", map[string]any{"sessionId": sessionID, "workerId": "test-worker", "app": "codex", "kind": "run", "runId": run.RunId, "stepId": run.StepKey, "workspace": "/work", "prompt": "Inspect the interface", "startedAt": time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)})
	initial := []common.ChatMessage{{Role: "system", Content: "Current authority"}, {Role: "user", Name: common.WorkInstructionMessageName, Content: "Verify the current stage"}}
	// Each public receipt fits its existing 1 MiB boundary, but a normal
	// sequence of screenshots exceeds the 4 MiB continuation-content bound.
	// Their originals are already durable: references must not drop evidence
	// or force a new goal merely to ask the person a question.
	var originals []string
	for n := range 7 {
		result := fmt.Sprintf(`{"capture":%d,"data":"%s","tail":"exact-%d"}`, n, strings.Repeat("A", 700000), n)
		originals = append(originals, result)
		require.NoError(t, a.RecordDelegatedToolSource(ContextWithDelegatedSession(ctx, sessionID), "workerComputer", map[string]any{"action": "screenshot", "sequence": n}, result))
	}
	require.NoError(t, b.PrepareDelegatedFeedback(ctx, sessionID, initial, "requestUserFeedback", map[string]any{"question": "Review the visual result"}))
	restored, err := a.RestoreWorkContinuation(ctx, nil)
	require.NoError(t, err)
	require.Len(t, restored, 18)
	require.Equal(t, initial, restored[:2])
	require.Less(t, WorkContextSize(restored, nil), 5000)
	for n, original := range originals {
		call, result := restored[2+n*2], restored[3+n*2]
		require.Equal(t, call.ToolCalls[0].ID, result.ToolCallId)
		require.Contains(t, result.Content, "[Archived tool result (delegated source)]")
		// Fetch the original on another replica through the same authorized
		// public history route advertised by the retained reference.
		page, err := b.recallWorkHistoryBuiltin(ctx, map[string]any{"sourceRunId": run.RunId, "checkpoint": call.ToolCalls[0].ID, "messageIndex": 1, "search": fmt.Sprintf("exact-%d", n), "maxChars": 300}, 0)
		require.NoError(t, err)
		require.Contains(t, string(page[0].Payload), fmt.Sprintf("exact-%d", n))
		rows, err := b.workRows(ContextWithFreshRead(ctx), "workDelegatedSourcesForOwnerRun", run.RunId)
		require.NoError(t, err)
		found := false
		for _, row := range rows {
			if BareShortId(stringArg(row, "id")) != call.ToolCalls[0].ID {
				continue
			}
			data, _ := row["data"].(map[string]any)
			pair, err := decodeWorkContinuation(data)
			require.NoError(t, err)
			require.Equal(t, original, pair[1].Content)
			found = true
		}
		require.True(t, found)
	}
	for _, unit := range workContextUnits(restored) {
		require.True(t, unit.complete)
	}
	raw, err := json.Marshal(restored)
	require.NoError(t, err)
	require.NotContains(t, string(raw), strings.Repeat("A", 1000))
	require.Equal(t, "requestUserFeedback", restored[16].ToolCalls[0].Name)
	require.Equal(t, common.UnexecutedWorkToolResult, restored[17].Content)
}
