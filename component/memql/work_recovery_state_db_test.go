package memql

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/core/common"
)

// Exercise the real compactor, PostgreSQL checkpoint and another execution
// replica. Prompt refresh may change the first system message; the recovery
// watermark and completed tool evidence must remain in their original order.
func TestWorkRecoveryStateSurvivesCompactionAndReplicaRestore(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, a)
	model := &checkpointModel{}
	contextTestModel(a, model)
	request := common.ChatMessage{Role: "user", Content: "Inspect the authorized sources and answer the original question."}
	messages := []common.ChatMessage{{Role: "system", Content: "Previous installed prompt"}, request}
	for n := range 24 {
		id := fmt.Sprintf("source-%d", n)
		messages = append(messages,
			common.ChatMessage{Role: "assistant", ToolCalls: []common.ToolCall{{ID: id, Name: "readSource", Arguments: fmt.Sprintf(`{"page":%d}`, n)}}},
			common.ChatMessage{Role: "tool", Name: "readSource", ToolCallId: id, Content: fmt.Sprintf("Source %d: %s", n, strings.Repeat("verified measurement ", 140))})
	}
	marker := common.ChatMessage{Role: "system", Name: "work_recovery_checkpoint", Content: `{"version":1,"discoveryRounds":3,"reviewActive":true,"stalledAfterReview":1,"reviewedLimit":12,"answeredAt":"2026-10-14T10:00:00Z","guidanceIds":["accepted-guidance"],"instruction":"Reconsider the current approach without granting access or resetting spend."}`}
	messages = append(messages, marker)
	compacted, err := a.CompactWorkContext(ctx, messages, nil, 7000)
	require.NoError(t, err)
	require.Less(t, len(compacted), len(messages))
	require.Positive(t, model.calls, "must exercise semantic compaction")
	require.Equal(t, marker, compacted[len(compacted)-1], "the positional evidence watermark must not move before old receipts")
	require.Equal(t, request, compacted[1])
	require.Equal(t, messages[len(messages)-3:len(messages)-1], compacted[len(compacted)-3:len(compacted)-1], "fresh tool exchange stays exact")
	require.NoError(t, a.SaveWorkContinuation(ctx, compacted))
	fresh := []common.ChatMessage{{Role: "system", Content: "New installed prompt"}, request}
	restored, err := b.RestoreWorkContinuation(ctx, fresh)
	require.NoError(t, err)
	require.Equal(t, fresh[0], restored[0])
	require.Equal(t, compacted[1:], restored[1:], "another replica must preserve recovery state and earlier evidence exactly")
	count := 0
	for _, message := range restored {
		if message.Name == marker.Name {
			count++
		}
	}
	require.Equal(t, 1, count)
}
