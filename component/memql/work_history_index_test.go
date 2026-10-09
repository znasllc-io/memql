package memql

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
)

func TestWorkHistoryIndexLocatesOriginalEvidenceAcrossReplicas(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, a)
	messages := []common.ChatMessage{
		{Role: "user", Content: "[Memory checkpoint older]\nDerived summary"},
		{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "read", Name: "readPaper", Arguments: `{"url":"https://example.test/paper"}`}}},
		{Role: "tool", Name: "readPaper", ToolCallId: "read", Content: "Measured evidence from the original paper"},
	}
	raw, err := json.Marshal(messages)
	require.NoError(t, err)
	hash := workContextHash("summary-v2", raw)
	require.NoError(t, a.saveWorkContextSource(ctx, hash, string(raw), ""))
	result, err := b.recallWorkHistoryBuiltin(ctx, map[string]any{"checkpoint": hash, "messageIndex": 0}, 0)
	require.NoError(t, err)
	var response struct {
		Sources []struct {
			Messages []struct {
				Index int
				Role  string
				Kind  string
				Tool  string
			}
		}
	}
	require.NoError(t, json.Unmarshal(result[0].Payload, &response))
	require.Len(t, response.Sources, 1)
	index := response.Sources[0].Messages
	require.Len(t, index, 3)
	require.Equal(t, "derived-checkpoint", index[0].Kind)
	require.Equal(t, "tool", index[2].Role)
	require.Equal(t, "readPaper", index[2].Tool)
	page, err := b.recallWorkHistoryBuiltin(ctx, map[string]any{"checkpoint": hash, "messageIndex": index[2].Index}, 0)
	require.NoError(t, err)
	require.Contains(t, string(page[0].Payload), "Measured evidence from the original paper")
	// The directory is descriptive metadata; stored source bytes stay exact.
	call, _ := parser.RenderCall("workCheckpointForOwner", map[string]any{"fingerprint": hash})
	saved, err := a.Execute(ContextWithFreshRead(ctx), "query "+call)
	require.NoError(t, err)
	require.Equal(t, string(raw), MaterializeRows(saved.OutputPayload())[0]["data"].(map[string]any)["sourceMessages"])
}

func TestWorkHistoryIndexBoundsMetadataAndAlwaysAdvances(t *testing.T) {
	messages := make([]common.ChatMessage, 30)
	for index := range messages {
		messages[index] = common.ChatMessage{Role: "assistant", Name: strings.Repeat("\u2028", 1000), ToolCalls: []common.ToolCall{
			{Name: strings.Repeat("\u2028", 1000), Arguments: strings.Repeat("\u2028", 1000)},
			{Name: strings.Repeat("\u2028", 1000), Arguments: strings.Repeat("\u2028", 1000)},
		}}
	}
	seen := 0
	for start := 0; ; {
		page := workHistorySourceIndex("checkpoint", messages, start)
		raw, err := json.Marshal(page)
		require.NoError(t, err)
		require.Less(t, len(raw), 2700)
		entries := page["messages"].([]any)
		require.NotEmpty(t, entries)
		for _, entry := range entries {
			require.Equal(t, seen, entry.(map[string]any)["index"])
			seen++
		}
		next, more := page["nextMessageIndex"].(int)
		if !more {
			break
		}
		require.Greater(t, next, start)
		start = next
	}
	require.Equal(t, len(messages), seen)
}
