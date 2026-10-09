package memql

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/core/common"
)

// This is the failed live research trace's protocol shape: a six-message
// protected tail landed BETWEEN the two results of the first parallel call.
func contextResearchTrace() []common.ChatMessage {
	return []common.ChatMessage{
		{Role: "system", Content: "Platform instructions"},
		{Role: "user", Content: "Research and rewrite the document; retain exact source citations"},
		{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "a", Name: "discover"}, {ID: "b", Name: "recall"}}},
		{Role: "tool", ToolCallId: "a", Content: strings.Repeat("contract ", 300)},
		{Role: "tool", ToolCallId: "b", Content: strings.Repeat("evidence ", 300)},
		{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "c", Name: "research"}}},
		{Role: "tool", ToolCallId: "c", Content: strings.Repeat("source ", 300)},
		{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "d", Name: "read"}, {ID: "e", Name: "read"}}},
		{Role: "tool", ToolCallId: "d", Content: strings.Repeat("article ", 300)},
		{Role: "tool", ToolCallId: "e", Content: "Current receipt"},
	}
}

func TestWorkContextChunkKeepsCompleteParallelExchanges(t *testing.T) {
	messages := contextResearchTrace()
	start, end := workContextChunk(messages, workCheckpointInputBytes-len(workContextTask(messages)))
	require.Equal(t, 2, start)
	require.Equal(t, 5, end)
	for _, u := range workContextUnits(messages) {
		require.True(t, u.complete)
	}
	// A short transcript may retire an older exchange, still preserving the
	// newest one; a pending or mismatched group must never be compacted.
	short := append(append([]common.ChatMessage{}, messages[:5]...), messages[7:]...)
	start, end = workContextChunk(short, workCheckpointInputBytes-len(workContextTask(short)))
	require.Equal(t, []int{2, 5}, []int{start, end})
	for _, broken := range []string{"missing", "duplicate", "unrelated"} {
		t.Run(broken, func(t *testing.T) {
			bad := append([]common.ChatMessage{}, messages...)
			switch broken {
			case "missing":
				bad = append(bad[:4], bad[5:]...)
			case "duplicate":
				bad[4].ToolCallId = "a"
			case "unrelated":
				bad[4].ToolCallId = "other"
			}
			require.False(t, workContextUnits(bad)[2].complete)
			s, _ := workContextChunk(bad, workCheckpointInputBytes-len(workContextTask(bad)))
			require.NotEqual(t, 2, s)
		})
	}
}

func TestWorkContextChunkPinsAuthorityRequestAndLatestCorrection(t *testing.T) {
	messages := contextResearchTrace()
	messages = append(messages, common.ChatMessage{Role: "user", Content: "Correction: use Morgan, not Taylor"}, common.ChatMessage{Role: "assistant", Content: "Working"})
	for range 3 {
		s, e := workContextChunk(messages, workCheckpointInputBytes-len(workContextTask(messages)))
		if s == e {
			break
		}
		for _, m := range messages[s:e] {
			require.NotEqual(t, "system", m.Role)
			require.NotContains(t, m.Content, "Correction:")
			require.NotContains(t, m.Content, "Research and rewrite")
		}
		memory := common.ChatMessage{Role: "user", Content: "[Memory checkpoint ref]\nPrevious facts"}
		messages = append(append(append([]common.ChatMessage{}, messages[:s]...), memory), messages[e:]...)
	}
}

func TestWorkHistoryPagesReconstructExactUnicodeAndToolArguments(t *testing.T) {
	message := common.ChatMessage{Role: "tool", Content: strings.Repeat("文献🧪 café ", 1000), ToolCalls: []common.ToolCall{{ID: "receipt", Arguments: `{"file":"résumé.md"}`}}}
	want, err := json.Marshal(message)
	require.NoError(t, err)
	var reconstructed strings.Builder
	for offset := 0; ; {
		page, ok := workHistoryPage(message, "", offset, 333, true)
		require.True(t, ok)
		part := page["source"].(string)
		require.True(t, utf8.ValidString(part))
		require.LessOrEqual(t, utf8.RuneCountInString(part), 333)
		reconstructed.WriteString(part)
		next, more := page["nextOffset"].(int)
		if !more {
			break
		}
		require.Greater(t, next, offset)
		offset = next
	}
	require.Equal(t, string(want), reconstructed.String())
	page, ok := workHistoryPage(message, "résumé", 0, 333, false)
	require.True(t, ok)
	require.Contains(t, page["source"], "résumé")
	_, ok = workHistoryPage(message, "nonexistent fact", 0, 333, false)
	require.False(t, ok)
}
