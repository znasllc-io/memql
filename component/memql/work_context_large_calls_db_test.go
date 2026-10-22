package memql

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/core/common"
)

func TestWorkContextArchivesOversizedOldCallsBeforeBuyingSummaries(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, a)
	model := &checkpointModel{fail: true}
	contextTestModel(a, model)
	messages := []common.ChatMessage{
		{Role: "system", Content: "Keep existing authority"},
		{Role: "user", Content: "Finish the requested artifact"},
		{Role: "user", Name: common.WorkInstructionMessageName, Content: "Current stage: verify the artifact"},
	}
	for n := range 3 {
		key := fmt.Sprintf("small-%d", n)
		messages = append(messages,
			common.ChatMessage{Role: "assistant", ToolCalls: []common.ToolCall{{ID: key, Name: "read", Arguments: `{}`}}},
			common.ChatMessage{Role: "tool", ToolCallId: key, Content: strings.Repeat("saved receipt ", 220)})
	}
	var originals [][]common.ChatMessage
	for n := range 3 {
		key := fmt.Sprintf("large-%d", n)
		pair := []common.ChatMessage{
			{Role: "assistant", ToolCalls: []common.ToolCall{{ID: key, Name: "write", Arguments: `{"content":"` + strings.Repeat("A", 825008) + `"}`}}},
			{Role: "tool", ToolCallId: key, Content: "Completed write receipt " + key},
		}
		originals = append(originals, pair)
		messages = append(messages, pair...)
	}
	messages = append(messages,
		common.ChatMessage{Role: "user", Content: "Keep the results open for review"},
		common.ChatMessage{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "fresh", Name: "read", Arguments: `{}`}}},
		common.ChatMessage{Role: "tool", ToolCallId: "fresh", Content: "Current status"})
	before := mustMarshalMessages(t, messages)
	compacted, err := a.CompactWorkContext(ctx, messages, nil, 6500)
	require.NoError(t, err)
	require.Zero(t, model.calls, "completed oversized arguments should not buy unrelated summaries first")
	require.LessOrEqual(t, WorkContextSize(compacted, nil), 6500)
	require.Equal(t, before, mustMarshalMessages(t, messages))
	require.Equal(t, messages[:9], compacted[:9], "small useful receipts and pinned instructions remain verbatim")
	require.Equal(t, messages[len(messages)-3:], compacted[len(compacted)-3:])
	for _, unit := range workContextUnits(compacted) {
		require.True(t, unit.complete, "call/result pairs cannot be split")
	}
	run, _ := common.RunFromContext(ctx)
	rows, err := b.workRows(ContextWithFreshRead(ctx), "workObservationsForOwnerRun", run.RunId)
	require.NoError(t, err)
	for _, pair := range originals {
		raw, err := json.Marshal(pair)
		require.NoError(t, err)
		found := false
		for _, row := range rows {
			data, _ := row["data"].(map[string]any)
			if data["sourceMessages"] == string(raw) {
				found = true
				require.Empty(t, data["summary"], "archiving bytes is not semantic verification")
			}
		}
		require.True(t, found, "another replica must recover each exact original exchange")
	}
}
